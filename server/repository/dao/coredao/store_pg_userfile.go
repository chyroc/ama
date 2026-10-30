// PostgreSQL implementation of the user_files table. Style matches store_pg_host.go case by case:
// column list constants, $N placeholders, fmt.Errorf("core: ...: %w"),
// sql.ErrNoRows → persistmodel.ErrNotFound, lists return a non-nil empty slice.
//
// ⚠️ **Every method's where includes owner_email**: these routes have no workspace dimension, so owner isolation
// lives entirely in the SQL layer (see the file header of repository/schema/user_files.sql).
package coredao

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// userFileColumns matches the Scan arguments of scanUserFile one to one.
const userFileColumns = `id, owner_email, bucket, object_key, filename, content_type,
	        size, status, upload_mode, upload_id, created_at`

func scanUserFile(scan func(dest ...any) error) (persistmodel.UserFile, error) {
	var f persistmodel.UserFile
	if err := scan(&f.ID, &f.OwnerEmail, &f.Bucket, &f.ObjectKey, &f.Filename,
		&f.ContentType, &f.Size, &f.Status, &f.UploadMode, &f.UploadID,
		&f.CreatedAt); err != nil {
		return persistmodel.UserFile{}, err
	}
	return f, nil
}

// CreateUserFile inserts a row. A clash on the unique key (owner_email, object_key) returns ErrConflict.
func (s *PGStore) CreateUserFile(ctx context.Context, row persistmodel.UserFile) error {
	_, err := s.client.Exec(ctx,
		`insert into user_files
		   (id, owner_email, bucket, object_key, filename, content_type, size,
		    status, upload_mode, upload_id, created_at)
		 values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		row.ID, row.OwnerEmail, row.Bucket, row.ObjectKey, row.Filename,
		row.ContentType, row.Size, row.Status, row.UploadMode, row.UploadID,
		row.CreatedAt)
	if err != nil {
		if pg.IsUniqueViolation(err) {
			return fmt.Errorf("core: 创建用户文件撞唯一键: %w", persistmodel.ErrConflict)
		}
		return fmt.Errorf("core: 创建用户文件失败: %w", err)
	}
	return nil
}

// GetUserFile fetches by (owner, id). "Belongs to another owner" and "does not exist" cannot be told apart; both are ErrNotFound.
func (s *PGStore) GetUserFile(
	ctx context.Context, ownerEmail, id string,
) (persistmodel.UserFile, error) {
	row, err := scanUserFile(s.client.QueryRow(ctx,
		`select `+userFileColumns+` from user_files
		 where owner_email = $1 and id = $2`,
		ownerEmail, id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistmodel.UserFile{},
				fmt.Errorf("core: 用户文件不存在 owner=%s id=%s: %w",
					ownerEmail, id, persistmodel.ErrNotFound)
		}
		return persistmodel.UserFile{}, fmt.Errorf("core: 查用户文件失败: %w", err)
	}
	return row, nil
}

// ListCompletedUserFiles lists completed files, created_at descending; the caller has already bounded limit.
func (s *PGStore) ListCompletedUserFiles(
	ctx context.Context, ownerEmail string, limit int,
) ([]persistmodel.UserFile, error) {
	rows, err := s.client.Query(ctx,
		`select `+userFileColumns+` from user_files
		 where owner_email = $1 and status = $2
		 order by created_at desc, id desc
		 limit $3`,
		ownerEmail, persistmodel.UserFileStatusCompleted, limit)
	if err != nil {
		return nil, fmt.Errorf("core: 列用户文件失败: %w", err)
	}
	defer rows.Close()

	out := make([]persistmodel.UserFile, 0, 16)
	for rows.Next() {
		row, err := scanUserFile(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 扫用户文件行失败: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历用户文件失败: %w", err)
	}
	return out, nil
}

// MarkUserFileCompleted uses CAS to set an uploading row to completed.
//
// The where includes status='uploading': repeated callbacks / concurrent completion will not write an already completed row again.
// When 0 rows are affected it tells two cases apart: the row exists but is not uploading (409), or the row does not exist (404).
func (s *PGStore) MarkUserFileCompleted(
	ctx context.Context, ownerEmail, id string, size int64,
) error {
	res, err := s.client.Exec(ctx,
		`update user_files
		    set status = $3, size = $4
		  where owner_email = $1 and id = $2 and status = $5`,
		ownerEmail, id, persistmodel.UserFileStatusCompleted, size,
		persistmodel.UserFileStatusUploading)
	if err != nil {
		return fmt.Errorf("core: 标记用户文件完成失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("core: 取标记完成影响行数失败: %w", err)
	}
	if n == 0 {
		// Tell "row exists but in the wrong status" from "row missing" so the caller gets the right status code.
		var currentStatus string
		err := s.client.QueryRow(ctx,
			`select status from user_files where owner_email = $1 and id = $2`,
			ownerEmail, id).Scan(&currentStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("core: 用户文件不存在: %w", persistmodel.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("core: 回查用户文件状态失败: %w", err)
		}
		return fmt.Errorf("core: 用户文件已不是上传中状态（当前 %s）: %w",
			currentStatus, persistmodel.ErrConflict)
	}
	return nil
}

// DeleteUserFile deletes the row; missing is not an error (idempotent).
func (s *PGStore) DeleteUserFile(ctx context.Context, ownerEmail, id string) error {
	_, err := s.client.Exec(ctx,
		`delete from user_files where owner_email = $1 and id = $2`,
		ownerEmail, id)
	if err != nil {
		return fmt.Errorf("core: 删用户文件失败: %w", err)
	}
	return nil
}
