// Business logic for upload / complete / abort / list / detail / delete.
//
// The source of truth for the state machine is PostgreSQL (user_files.status); the source of truth
// for the part list is object storage. This layer caches nothing across requests.
package userfile

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// StartUpload starts an upload.
//
// If size is given and <= PartSize -> simple (then PUT content); otherwise multipart
// (then PUT parts + complete). In multipart mode this step already registers the upload with
// object storage and saves uploadId to the DB; simple does not touch object storage.
func (s *Service) StartUpload(
	ctx context.Context, owner string, in apimodel.StartUserFileUpload,
) (apimodel.UserFile, error) {
	email, err := normalizeOwner(owner)
	if err != nil {
		return apimodel.UserFile{}, err
	}
	filename, err := validateFilename(in.Filename)
	if err != nil {
		return apimodel.UserFile{}, err
	}
	if in.Size < 0 {
		return apimodel.UserFile{}, fmt.Errorf("%w: size 不能为负", ErrInvalidArgument)
	}
	if in.Size > MaxFileSize {
		return apimodel.UserFile{}, fmt.Errorf(
			"%w: 文件不能超过 %d 字节（2 GiB）", ErrInvalidArgument, MaxFileSize)
	}

	mode := persistmodel.UserFileModeMultipart
	if in.Size > 0 && in.Size <= PartSize {
		mode = persistmodel.UserFileModeSimple
	}

	id := newID()
	row := persistmodel.UserFile{
		ID:          id,
		OwnerEmail:  email,
		Bucket:      s.objects.Bucket(),
		ObjectKey:   objectKey(email, id),
		Filename:    filename,
		ContentType: in.ContentType,
		Status:      persistmodel.UserFileStatusUploading,
		UploadMode:  mode,
		CreatedAt:   apimodel.NowISO(),
	}

	if mode == persistmodel.UserFileModeMultipart {
		// Register with object storage first, then write the row: uploadId is required in this row,
		// so it must exist when the row is written.
		// Registered but the row write fails -> object storage is left with an unfinished upload,
		// which the bucket lifecycle aborts and reclaims; we also make a best-effort abort here, as
		// a second safety net.
		uploadID, err := s.objects.InitMultipartUpload(ctx, row.ObjectKey, row.ContentType)
		if err != nil {
			return apimodel.UserFile{}, fmt.Errorf("初始化分片上传失败: %w", err)
		}
		row.UploadID = uploadID
		if err := s.store.CreateUserFile(ctx, row); err != nil {
			_ = s.objects.AbortMultipartUpload(ctx, row.ObjectKey, uploadID)
			return apimodel.UserFile{}, err
		}
	} else {
		if err := s.store.CreateUserFile(ctx, row); err != nil {
			return apimodel.UserFile{}, err
		}
	}
	return view(row), nil
}

// PutSimpleContent writes the whole object in simple mode. content must have exactly size bytes
// (the HTTP handler provides it accurately via Content-Length).
func (s *Service) PutSimpleContent(
	ctx context.Context, owner, id string, size int64, content io.Reader,
) (apimodel.UserFile, error) {
	row, err := s.getOwnedUploading(ctx, owner, id, persistmodel.UserFileModeSimple)
	if err != nil {
		return apimodel.UserFile{}, err
	}
	if size <= 0 {
		return apimodel.UserFile{}, fmt.Errorf("%w: 文件内容为空", ErrInvalidArgument)
	}
	if size > PartSize {
		// Safety net: the handler side already blocks with MaxBytesReader; we block again here so
		// other callers (e.g. tests, a future daemon entry point) cannot bypass the limit and blow
		// up memory.
		return apimodel.UserFile{}, fmt.Errorf(
			"%w: simple 模式只能传不超过 %d 字节的文件", ErrInvalidArgument, PartSize)
	}
	// The simple path is <= 8 MiB, so reading it into memory is acceptable (matches the []byte
	// signature of ObjectStore.Put).
	data, err := io.ReadAll(io.LimitReader(content, size+1))
	if err != nil {
		return apimodel.UserFile{}, fmt.Errorf("读取文件内容失败: %w", err)
	}
	if int64(len(data)) != size {
		return apimodel.UserFile{}, fmt.Errorf(
			"%w: 声明 %d 字节，实际收到 %d 字节", ErrInvalidArgument, size, len(data))
	}

	if err := s.objects.Put(ctx, row.ObjectKey, data, row.ContentType); err != nil {
		return apimodel.UserFile{}, fmt.Errorf("写对象失败: %w", err)
	}
	if err := s.store.MarkUserFileCompleted(ctx, row.OwnerEmail, row.ID, int64(len(data))); err != nil {
		// The object is written but the status was not saved: retrying the same content is an
		// idempotent overwrite and does no damage. Return the error as is; do not pretend success.
		return apimodel.UserFile{}, fmt.Errorf("标记上传完成失败: %w", err)
	}
	row.Status = persistmodel.UserFileStatusCompleted
	row.Size = int64(len(data))
	return view(row), nil
}

// UploadPart receives one multipart part and streams it to object storage. Nothing is written to
// the DB (the part list's source of truth is object storage).
func (s *Service) UploadPart(
	ctx context.Context, owner, id string, partNumber int, size int64, content io.Reader,
) error {
	row, err := s.getOwnedUploading(ctx, owner, id, persistmodel.UserFileModeMultipart)
	if err != nil {
		return err
	}
	if partNumber < 1 || partNumber > 10000 {
		return fmt.Errorf("%w: 片号必须在 1~10000 之间", ErrInvalidArgument)
	}
	if size <= 0 {
		return fmt.Errorf("%w: 空片不允许上传", ErrInvalidArgument)
	}
	if size > PartSize {
		return fmt.Errorf("%w: 单片不能超过 %d 字节", ErrInvalidArgument, PartSize)
	}
	if err := s.objects.UploadPart(
		ctx, row.ObjectKey, row.UploadID, partNumber, size, content); err != nil {
		return fmt.Errorf("上传分片失败: %w", err)
	}
	return nil
}

// CompleteUpload merges the parts, reads back the size to check it, and sets completed.
func (s *Service) CompleteUpload(
	ctx context.Context, owner, id string,
) (apimodel.UserFile, error) {
	row, err := s.getOwnedUploading(ctx, owner, id, persistmodel.UserFileModeMultipart)
	if err != nil {
		return apimodel.UserFile{}, err
	}

	if err := s.objects.CompleteMultipartUpload(ctx, row.ObjectKey, row.UploadID); err != nil {
		return apimodel.UserFile{}, fmt.Errorf("合并分片失败: %w", err)
	}

	// Trust the size read back from object storage, not the value the client declared in
	// StartUpload.
	size, err := s.objects.Size(ctx, row.ObjectKey)
	if err != nil {
		return apimodel.UserFile{}, fmt.Errorf("回读对象大小失败: %w", err)
	}
	if size > MaxFileSize {
		// Over the limit: the object is already assembled. Delete it and return an error; do not
		// leave an oversized object plus a completed row.
		_ = s.objects.Delete(ctx, row.ObjectKey)
		return apimodel.UserFile{}, fmt.Errorf(
			"%w: 文件不能超过 %d 字节（2 GiB），实际 %d 字节",
			ErrInvalidArgument, MaxFileSize, size)
	}

	if err := s.store.MarkUserFileCompleted(ctx, row.OwnerEmail, row.ID, size); err != nil {
		return apimodel.UserFile{}, fmt.Errorf("标记上传完成失败: %w", err)
	}
	row.Status = persistmodel.UserFileStatusCompleted
	row.Size = size
	return view(row), nil
}

// AbortUpload abandons an upload (idempotent: a missing row also counts as success).
func (s *Service) AbortUpload(ctx context.Context, owner, id string) error {
	email, err := normalizeOwner(owner)
	if err != nil {
		return err
	}
	row, err := s.store.GetUserFile(ctx, email, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return nil
		}
		return err
	}
	if row.UploadMode == persistmodel.UserFileModeMultipart && row.UploadID != "" {
		if err := s.objects.AbortMultipartUpload(ctx, row.ObjectKey, row.UploadID); err != nil {
			return fmt.Errorf("废弃分片失败: %w", err)
		}
	}
	if err := s.store.DeleteUserFile(ctx, email, id); err != nil {
		return fmt.Errorf("删除上传记录失败: %w", err)
	}
	return nil
}

// ListFiles lists the owner's completed files, newest first by creation time.
func (s *Service) ListFiles(
	ctx context.Context, owner string, limit int,
) ([]apimodel.UserFile, error) {
	email, err := normalizeOwner(owner)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	rows, err := s.store.ListCompletedUserFiles(ctx, email, limit)
	if err != nil {
		return nil, err
	}
	out := make([]apimodel.UserFile, 0, len(rows))
	for _, row := range rows {
		out = append(out, view(row))
	}
	return out, nil
}

// GetFile returns file details; when completed it attaches a freshly signed, time-limited
// download URL.
//
// uploading rows can be fetched too (the uploader uses this to check status / get partSize), they
// just have no downloadUrl.
func (s *Service) GetFile(ctx context.Context, owner, id string) (apimodel.UserFile, error) {
	email, err := normalizeOwner(owner)
	if err != nil {
		return apimodel.UserFile{}, err
	}
	row, err := s.store.GetUserFile(ctx, email, id)
	if err != nil {
		return apimodel.UserFile{}, err
	}
	out := view(row)
	if row.Status == persistmodel.UserFileStatusCompleted {
		url, err := s.objects.PresignedGetURL(row.ObjectKey, downloadTTL)
		if err != nil {
			return apimodel.UserFile{}, fmt.Errorf("签下载链接失败: %w", err)
		}
		out.DownloadURL = url
		out.ExpiresInSeconds = int(downloadTTL.Seconds())
	}
	return out, nil
}

// OpenFile opens the content of a completed file (downloaded through amad,
// `GET /api/files/{id}/content`).
//
// # Why this is needed in addition to the presigned URL
//
// A presigned URL has the object storage Host signed into it, so only a party that can reach
// s3.publicEndpoint can use it. But `ama files download` often runs in a **Docker session
// container** or on a **user's own machine**: the former is inside the container network (the
// compose default publicEndpoint is the host's localhost, which does not work inside a container),
// and the latter may not reach object storage at all. The only thing they can always reach is amad
// itself (AMA_BASE_URL), so the CLI uses this path while the browser keeps using downloadUrl to go
// straight to object storage.
//
// Owner isolation is the same as the other methods: another person's id is always ErrNotFound.
// uploading rows are rejected (the bytes are not fully merged yet).
func (s *Service) OpenFile(
	ctx context.Context, owner, id string,
) (apimodel.UserFile, io.ReadCloser, error) {
	email, err := normalizeOwner(owner)
	if err != nil {
		return apimodel.UserFile{}, nil, err
	}
	row, err := s.store.GetUserFile(ctx, email, id)
	if err != nil {
		return apimodel.UserFile{}, nil, err
	}
	if row.Status != persistmodel.UserFileStatusCompleted {
		return apimodel.UserFile{}, nil, fmt.Errorf("%w: 文件还没传完", ErrConflict)
	}
	body, size, err := s.objects.Open(ctx, row.ObjectKey)
	if err != nil {
		return apimodel.UserFile{}, nil, fmt.Errorf("读文件内容失败: %w", err)
	}
	out := view(row)
	out.Size = size
	return out, body, nil
}

// DeleteFile deletes a completed file: first the object storage object (idempotent), then the DB
// row. uploading rows are rejected: that is "abandon an upload" and should go through AbortUpload.
func (s *Service) DeleteFile(ctx context.Context, owner, id string) error {
	email, err := normalizeOwner(owner)
	if err != nil {
		return err
	}
	row, err := s.store.GetUserFile(ctx, email, id)
	if err != nil {
		return err
	}
	if row.Status != persistmodel.UserFileStatusCompleted {
		return fmt.Errorf("%w: 文件还没传完，取消请调 abort", ErrConflict)
	}
	if err := s.objects.Delete(ctx, row.ObjectKey); err != nil &&
		!errors.Is(err, ErrObjectNotFound) {
		// If the object is already gone, keep deleting the row (the target state is "does not
		// exist" either way). Any other error must stop here and keep the row, so a user retry
		// does not double-charge.
		return fmt.Errorf("删对象失败: %w", err)
	}
	if err := s.store.DeleteUserFile(ctx, email, id); err != nil {
		return fmt.Errorf("删文件记录失败: %w", err)
	}
	return nil
}

// getOwnedUploading fetches a row and confirms it "belongs to this person, is still uploading, and
// the mode matches".
//
// Each of the three invalid cases has its own result: missing / not this person's -> ErrNotFound
// (404, does not reveal existence); already completed -> ErrConflict (409, stop sending
// parts/content); wrong mode -> ErrInvalidArgument (400, using a simple id to upload parts means
// the caller used the wrong endpoint).
func (s *Service) getOwnedUploading(
	ctx context.Context, owner, id, mode string,
) (persistmodel.UserFile, error) {
	email, err := normalizeOwner(owner)
	if err != nil {
		return persistmodel.UserFile{}, err
	}
	row, err := s.store.GetUserFile(ctx, email, id)
	if err != nil {
		return persistmodel.UserFile{}, err
	}
	if row.Status != persistmodel.UserFileStatusUploading {
		return persistmodel.UserFile{}, fmt.Errorf(
			"%w: 文件 %s 已完成，不能再传内容", ErrConflict, id)
	}
	if row.UploadMode != mode {
		return persistmodel.UserFile{}, fmt.Errorf(
			"%w: 文件 %s 是 %s 上传，不能调 %s 模式的接口",
			ErrInvalidArgument, id, row.UploadMode, mode)
	}
	return row, nil
}
