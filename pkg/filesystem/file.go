package filesystem

import (
	"context"
	"fmt"
	"io"

	model "github.com/cloudreve/Cloudreve/v3/models"
	"github.com/cloudreve/Cloudreve/v3/pkg/cache"
	"github.com/cloudreve/Cloudreve/v3/pkg/conf"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/fsctx"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/response"
	"github.com/cloudreve/Cloudreve/v3/pkg/serializer"
	"github.com/cloudreve/Cloudreve/v3/pkg/util"
	"github.com/juju/ratelimit"
)

/* ============
	 文件相关
   ============
*/

// 限速后的ReaderSeeker
type lrs struct {
	response.RSCloser
	r io.Reader
}

func (r lrs) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

// withSpeedLimit 给原有的ReadSeeker加上限速
func (fs *FileSystem) withSpeedLimit(rs response.RSCloser) response.RSCloser {
	// 如果用户组有速度限制，就返回限制流速的ReaderSeeker
	if fs.User.Group.SpeedLimit != 0 {
		speed := fs.User.Group.SpeedLimit
		bucket := ratelimit.NewBucketWithRate(float64(speed), int64(speed))
		lrs := lrs{rs, ratelimit.Reader(rs, bucket)}
		return lrs
	}
	// 否则返回原始流
	return rs

}

// AddFile 新增文件记录
func (fs *FileSystem) AddFile(ctx context.Context, parent *model.Folder, file fsctx.FileHeader) (*model.File, error) {
	// 添加文件记录前的钩子
	err := fs.Trigger(ctx, "BeforeAddFile", file)
	if err != nil {
		return nil, err
	}

	uploadInfo := file.Info()
	newFile := model.File{
		Name:               uploadInfo.FileName,
		SourceName:         uploadInfo.SavePath,
		UserID:             fs.User.ID,
		Size:               uploadInfo.Size,
		FolderID:           parent.ID,
		PolicyID:           fs.Policy.ID,
		MetadataSerialized: uploadInfo.Metadata,
		UploadSessionID:    uploadInfo.UploadSessionID,
	}

	err = newFile.Create()

	if err != nil {
		if err := fs.Trigger(ctx, "AfterValidateFailed", file); err != nil {
			util.Log().Debug("AfterValidateFailed hook execution failed: %s", err)
		}
		return nil, ErrFileExisted.WithError(err)
	}

	fs.User.Storage += newFile.Size
	return &newFile, nil
}

// GetPhysicalFileContent 根据文件物理路径获取文件流
func (fs *FileSystem) GetPhysicalFileContent(ctx context.Context, path string) (response.RSCloser, error) {
	// 重设上传策略
	fs.Policy = &model.Policy{Type: "local"}
	_ = fs.DispatchHandler()

	// 获取文件流
	rs, err := fs.Handler.Get(ctx, path)
	if err != nil {
		return nil, err
	}

	return fs.withSpeedLimit(rs), nil
}

// Preview 预览文件
//
//	path   -   文件虚拟路径
//	isText -   是否为文本文件，文本文件会忽略重定向，直接由
//	           服务端拉取中转给用户，故会对文件大小进行限制
func (fs *FileSystem) Preview(ctx context.Context, id uint, isText bool) (*response.ContentResponse, error) {
	err := fs.resetFileIDIfNotExist(ctx, id)
	if err != nil {
		return nil, err
	}

	// 如果是文本文件预览，需要检查大小限制
	sizeLimit := model.GetIntSetting("maxEditSize", 2<<20)
	if isText && fs.FileTarget[0].Size > uint64(sizeLimit) {
		return nil, ErrFileSizeTooBig
	}

	// 是否直接返回文件内容
	if isText || fs.Policy.IsDirectlyPreview() {
		resp, err := fs.GetDownloadContent(ctx, id)
		if err != nil {
			return nil, err
		}
		return &response.ContentResponse{
			Redirect: false,
			Content:  resp,
		}, nil
	}

	// 否则重定向到签名的预览URL
	ttl := model.GetIntSetting("preview_timeout", 60)
	previewURL, err := fs.SignURL(ctx, &fs.FileTarget[0], int64(ttl), false)
	if err != nil {
		return nil, err
	}
	return &response.ContentResponse{
		Redirect: true,
		URL:      previewURL,
		MaxAge:   ttl,
	}, nil

}

// GetDownloadContent 获取用于下载的文件流
func (fs *FileSystem) GetDownloadContent(ctx context.Context, id uint) (response.RSCloser, error) {
	// 获取原始文件流
	rs, err := fs.GetContent(ctx, id)
	if err != nil {
		return nil, err
	}

	// 返回限速处理后的文件流
	return fs.withSpeedLimit(rs), nil

}

// GetContent 获取文件内容，path为虚拟路径
func (fs *FileSystem) GetContent(ctx context.Context, id uint) (response.RSCloser, error) {
	err := fs.resetFileIDIfNotExist(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, fsctx.FileModelCtx, fs.FileTarget[0])

	// 获取文件流
	rs, err := fs.Handler.Get(ctx, fs.FileTarget[0].SourceName)
	if err != nil {
		return nil, ErrIO.WithError(err)
	}

	return rs, nil
}

// deleteGroupedFile 对分组好的文件执行删除操作，
// 返回每个分组失败的文件列表
func (fs *FileSystem) deleteGroupedFile(ctx context.Context, files map[uint][]*model.File) map[uint][]string {
	// 失败的文件列表
	// TODO 并行删除
	failed := make(map[uint][]string, len(files))
	thumbs := make([]string, 0)

	for policyID, toBeDeletedFiles := range files {
		// 列举出需要物理删除的文件的物理路径
		sourceNamesAll := make([]string, 0, len(toBeDeletedFiles))
		uploadSessions := make([]*serializer.UploadSession, 0, len(toBeDeletedFiles))

		for i := 0; i < len(toBeDeletedFiles); i++ {
			sourceNamesAll = append(sourceNamesAll, toBeDeletedFiles[i].SourceName)

			if toBeDeletedFiles[i].UploadSessionID != nil {
				if session, ok := cache.Get(UploadSessionCachePrefix + *toBeDeletedFiles[i].UploadSessionID); ok {
					uploadSession := session.(serializer.UploadSession)
					uploadSessions = append(uploadSessions, &uploadSession)
				}
			}

			// Check if sidecar thumb file exist
			if model.IsTrueVal(toBeDeletedFiles[i].MetadataSerialized[model.ThumbSidecarMetadataKey]) {
				thumbs = append(thumbs, toBeDeletedFiles[i].ThumbFile())
			}
		}

		// 切换上传策略
		fs.Policy = toBeDeletedFiles[0].GetPolicy()
		err := fs.DispatchHandler()
		if err != nil {
			failed[policyID] = sourceNamesAll
			continue
		}

		// 取消上传会话
		for _, upSession := range uploadSessions {
			if err := fs.Handler.CancelToken(ctx, upSession); err != nil {
				util.Log().Warning("Failed to cancel upload session for %q: %s", upSession.Name, err)
			}

			cache.Deletes([]string{upSession.Key}, UploadSessionCachePrefix)
		}

		// 执行删除
		toBeDeletedSrcs := append(sourceNamesAll, thumbs...)
		failedFile, _ := fs.Handler.Delete(ctx, toBeDeletedSrcs)

		// Exclude failed results related to thumb file
		failed[policyID] = util.SliceDifference(failedFile, thumbs)
	}

	return failed
}

// GroupFileByPolicy 将目标文件按照存储策略分组
func (fs *FileSystem) GroupFileByPolicy(ctx context.Context, files []model.File) map[uint][]*model.File {
	var policyGroup = make(map[uint][]*model.File)

	for key := range files {
		if file, ok := policyGroup[files[key].PolicyID]; ok {
			// 如果已存在分组，直接追加
			policyGroup[files[key].PolicyID] = append(file, &files[key])
		} else {
			// 分组不存在，创建
			policyGroup[files[key].PolicyID] = make([]*model.File, 0)
			policyGroup[files[key].PolicyID] = append(policyGroup[files[key].PolicyID], &files[key])
		}
	}

	return policyGroup
}

// GetDownloadURL 创建文件下载链接, timeout 为数据库中存储过期时间的字段
func (fs *FileSystem) GetDownloadURL(ctx context.Context, id uint, timeout string) (string, error) {
	err := fs.resetFileIDIfNotExist(ctx, id)
	if err != nil {
		return "", err
	}
	fileTarget := &fs.FileTarget[0]

	// 生成下載地址
	ttl := model.GetIntSetting(timeout, 60)
	source, err := fs.SignURL(
		ctx,
		fileTarget,
		int64(ttl),
		true,
	)
	if err != nil {
		return "", err
	}

	return source, nil
}

// GetSource 获取可直接访问文件的外链地址
func (fs *FileSystem) GetSource(ctx context.Context, fileID uint) (string, error) {
	// 查找文件记录
	err := fs.resetFileIDIfNotExist(ctx, fileID)
	if err != nil {
		return "", ErrObjectNotExist.WithError(err)
	}

	// 检查存储策略是否可以获得外链
	if !fs.Policy.IsOriginLinkEnable {
		return "", serializer.NewError(
			serializer.CodePolicyNotAllowed,
			"This policy is not enabled for getting source link",
			nil,
		)
	}

	// MODIFY START

	fs.Policy = &fs.FileTarget[0].Policy
	fs.DispatchHandler()

	// MODIFY END

	source, err := fs.SignURL(ctx, &fs.FileTarget[0], 0, false)
	if err != nil {

		// MODIFY START

		util.Log().Error("Failed to get source link: %w", err)

		// MODIFY END

		return "", serializer.NewError(serializer.CodeNotSet, "Failed to get source link: "+err.Error(), err)
	}

	return source, nil
}

// SignURL 签名文件原始 URL
func (fs *FileSystem) SignURL(ctx context.Context, file *model.File, ttl int64, isDownload bool) (string, error) {
	fs.FileTarget = []model.File{*file}
	ctx = context.WithValue(ctx, fsctx.FileModelCtx, *file)

	err := fs.resetPolicyToFirstFile(ctx)
	if err != nil {
		return "", err
	}

	// 签名最终URL
	// 生成外链地址
	
	source, err := fs.Handler.Source(ctx, fs.FileTarget[0].SourceName, ttl, isDownload, fs.User.Group.SpeedLimit)
	if err != nil {

		// MODIFY START

		util.Log().Error("Failed to get source link: %w", err)

		// MODIFY END

		return "", serializer.NewError(serializer.CodeNotSet, "Failed to get source link", err)
	}

	return source, nil
}

// ResetFileIfNotExist 重设当前目标文件为 path，如果当前目标为空
func (fs *FileSystem) ResetFileIfNotExist(ctx context.Context, path string) error {
	// 找到文件
	if len(fs.FileTarget) == 0 {
		exist, file := fs.IsFileExist(path)
		if !exist {
			return ErrObjectNotExist
		}
		fs.FileTarget = []model.File{*file}
	}

	// 将当前存储策略重设为文件使用的
	return fs.resetPolicyToFirstFile(ctx)
}

// ResetFileIfNotExist 重设当前目标文件为 id，如果当前目标为空
func (fs *FileSystem) resetFileIDIfNotExist(ctx context.Context, id uint) error {
	// 找到文件
	if len(fs.FileTarget) == 0 {
		file, err := model.GetFilesByIDs([]uint{id}, fs.User.ID)
		if err != nil || len(file) == 0 {
			return ErrObjectNotExist
		}
		fs.FileTarget = []model.File{file[0]}
	}

	// 如果上下文限制了父目录，则进行检查
	if parent, ok := ctx.Value(fsctx.LimitParentCtx).(*model.Folder); ok {
		if parent.ID != fs.FileTarget[0].FolderID {
			return ErrObjectNotExist
		}
	}

	// 将当前存储策略重设为文件使用的
	return fs.resetPolicyToFirstFile(ctx)
}

// resetPolicyToFirstFile 将当前存储策略重设为第一个目标文件文件使用的
func (fs *FileSystem) resetPolicyToFirstFile(ctx context.Context) error {
	if len(fs.FileTarget) == 0 {
		return ErrObjectNotExist
	}

	// 从机模式不进行操作
	if conf.SystemConfig.Mode == "slave" {
		return nil
	}

	fs.Policy = fs.FileTarget[0].GetPolicy()
	err := fs.DispatchHandler()
	if err != nil {
		return err
	}
	return nil
}

// Search 搜索文件
func (fs *FileSystem) Search(ctx context.Context, keywords ...interface{}) ([]serializer.Object, error) {
	parents := make([]uint, 0)

	// 如果限定了根目录，则只在这个根目录下搜索。
	if fs.Root != nil {
		allFolders, err := model.GetRecursiveChildFolder([]uint{fs.Root.ID}, fs.User.ID, true)
		if err != nil {
			return nil, fmt.Errorf("failed to list all folders: %w", err)
		}

		for _, folder := range allFolders {
			parents = append(parents, folder.ID)
		}
	}

	files, _ := model.GetFilesByKeywords(fs.User.ID, parents, keywords...)
	fs.SetTargetFile(&files)

	return fs.listObjects(ctx, "/", files, nil, nil), nil
}
