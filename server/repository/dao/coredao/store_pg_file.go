// PostgreSQL implementation of the files table.
//
// The contract is declared in core/store_file.go; the in-memory implementation is in repository/dao/coremem/store_file.go.
// The table definition and all design reasons are in repository/schema/files.sql.
package coredao

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// fileSelectColumns is all the columns of files.
//
// **Selecting *all* columns is fine here**: this table has no byte column (the bytes are in object storage);
// the largest field is a 64-character sha256.
const fileSelectColumns = `
    id, bucket, object_key, filename, content_type, size, content_sha256, author, created_at`

func scanFile(row interface{ Scan(...any) error }) (persistmodel.FileRecord, error) {
	var f persistmodel.FileRecord
	err := row.Scan(&f.ID, &f.Bucket, &f.ObjectKey, &f.Filename,
		&f.ContentType, &f.Size, &f.ContentSHA256, &f.Author, &f.CreatedAt)
	return f, err
}

func (s *PGStore) CreateFile(ctx context.Context, rec persistmodel.FileRecord) error {
	if _, err := s.client.Exec(ctx, `
        insert into files
            (id, bucket, object_key, filename, content_type, size,
             content_sha256, author, created_at)
        values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		rec.ID, rec.Bucket, rec.ObjectKey, rec.Filename, rec.ContentType,
		rec.Size, rec.ContentSHA256, rec.Author, rec.CreatedAt); err != nil {
		if pg.IsUniqueViolation(err) {
			return fmt.Errorf("%w: 文件已存在", persistmodel.ErrConflict)
		}
		return fmt.Errorf("core: 存文件元信息失败: %w", err)
	}
	return nil
}

func (s *PGStore) GetFile(
	ctx context.Context, id string,
) (persistmodel.FileRecord, error) {
	f, err := scanFile(s.client.QueryRow(ctx,
		`select `+fileSelectColumns+` from files where id = $1`, id))
	if err != nil {
		if pg.IsNoRows(err) {
			return persistmodel.FileRecord{}, fmt.Errorf("%w: 文件不存在", persistmodel.ErrNotFound)
		}
		return persistmodel.FileRecord{}, fmt.Errorf("core: 查文件元信息失败: %w", err)
	}
	return f, nil
}

// GetFiles fetches metadata in bulk with **one SQL**. Contract (including "missing ids are not an error") in core/store_file.go.
//
// ⚠️ **Use expanded $1,$2,… instead of = any($1)**: this repo does not import lib/pq,
// so there is no pq.Array, and database/sql does not turn []string into a PG array
// (passing it gives `unsupported type []string`). Expanding placeholders needs no dependency.
//
// The number of ids is bounded so no batching is needed: the caller is skill files (at most maxSkillFiles=64 per skill),
// three orders of magnitude below PostgreSQL's limit of 65535 parameters.
func (s *PGStore) GetFiles(
	ctx context.Context, ids []string,
) (map[string]persistmodel.FileRecord, error) {
	out := make(map[string]persistmodel.FileRecord, len(ids))
	if len(ids) == 0 {
		// Do not hit the database. An empty in () is a syntax error in PG, and this branch is taken every time
		// "a skill has not moved a single file".
		return out, nil
	}

	// Dedup only **saves a few parameters**; it is not needed for correctness. Placeholder numbers come from len(args),
	// so they cannot mismatch the parameter count (verified by mutation: with dedup removed the tests stay green).
	// It is kept because ids repeat when the same bytes are referenced by several paths, and passing duplicate parameters is pointless.
	seen := make(map[string]struct{}, len(ids))
	args := make([]any, 0, len(ids))
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	if len(args) == 0 {
		return out, nil
	}

	rows, err := s.client.Query(ctx,
		`select `+fileSelectColumns+` from files where id in (`+
			strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("core: 批量查文件元信息失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("core: 读取文件元信息失败: %w", err)
		}
		out[f.ID] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历文件元信息失败: %w", err)
	}
	return out, nil
}
