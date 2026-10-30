// PostgreSQL implementation of product Suggestions.
//
// A separate file matching coremem/store_suggestion.go (the in-memory version) case by case,
// run once each by runStoreSuite.
//
// # This domain uses real columns, not doc jsonb
//
// Same rule as for issue: **anything queried/aggregated becomes a real column, the rest goes into doc**, and every
// field of a suggestion is queried (author / status filtered, ord sorted, title and body displayed),
// so not one field is left and doc would be empty.
//
// # The two counts are queried, not stored
//
// ReplyCount / AttachmentCount are computed by subqueries, same as issue. Storing them would mean updating them on every add/delete,
// and missing one spot shows as "the list says 3 replies, opening it shows only 2", with no error.
//
// # ⚠️ The where clauses in this domain have no workspace_id; that is not an omission
//
// Suggestions are **platform-level**; those three tables have no such column at all (see the header of core_suggestion_pg.sql).
// The authorization dimension instead is **author**: non-staff only see their own.
// Before adding a `where workspace_id = $x`, read core/README-suggestion.md first.

package coredao

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// suggestionSelectColumns is the fixed column list for listing suggestions, including the two computed counts.
//
// It is a constant because ListSuggestions and GetSuggestion must be **exactly the same**: one column different,
// and the two paths return different objects, an inconsistency only noticed when "it looks right in the list but a number is missing when opened".
const suggestionSelectColumns = `
    s.id, s.title, s.body, s.status, s.author, s.created_at, s.updated_at,
    (select count(*) from suggestion_replies r where r.suggestion_id = s.id),
    (select count(*) from suggestion_attachments a where a.suggestion_id = s.id)`

// scanSuggestion reads one row in the order of suggestionSelectColumns.
func scanSuggestion(row interface{ Scan(...any) error }) (apimodel.Suggestion, error) {
	var sg apimodel.Suggestion
	err := row.Scan(
		&sg.ID, &sg.Title, &sg.Body, &sg.Status, &sg.Author, &sg.CreatedAt, &sg.UpdatedAt,
		&sg.ReplyCount, &sg.AttachmentCount,
	)
	return sg, err
}

// suggestionAttachmentColumns is the metadata columns of attachments. The bytes are not in PG (they are in object storage; file_id points there).
const suggestionAttachmentColumns = `
    id, suggestion_id, filename, content_type, size, author, created_at,
    file_id`

func scanSuggestionAttachment(
	row interface{ Scan(...any) error },
) (apimodel.SuggestionAttachment, error) {
	var att apimodel.SuggestionAttachment
	err := row.Scan(
		&att.ID, &att.SuggestionID, &att.Filename,
		&att.ContentType, &att.Size, &att.Author, &att.CreatedAt, &att.FileID,
	)
	return att, err
}

// -- Suggestions --------------------------------------------------------------------

func (s *PGStore) ListSuggestions(
	ctx context.Context, q persistmodel.SuggestionQuery,
) ([]apimodel.Suggestion, error) {
	// Both filters use `($n = '' or column = $n)` instead of building SQL in Go: the string-building version
	// will sooner or later get a variable stuffed in, while this form shares one statement for all four combinations.
	//
	// ⚠️ **limit/offset apply after filtering** (SQL does that naturally; it is noted here because the in-memory version
	// is easy to get backwards): cutting the page first and then filtering makes page two look almost empty,
	// and it does not error; it is just "I clearly submitted 20, but page two has only 1".
	//
	// ⚠️ `order by s.ord desc` must not be replaced with created_at: two rows created in the same millisecond sort
	// unstably by time, so refreshing the list swaps two rows.
	rows, err := s.client.Query(ctx, `
        select `+suggestionSelectColumns+`
        from suggestions s
        where ($1 = '' or s.author = $1) and ($2 = '' or s.status = $2)
        order by s.ord desc
        limit $3 offset $4`,
		q.Author, string(q.Status), q.Limit, q.Offset)
	if err != nil {
		return nil, fmt.Errorf("core: 列建议失败: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice: the contract requires [], nil encodes as null.
	out := make([]apimodel.Suggestion, 0)
	for rows.Next() {
		sg, err := scanSuggestion(rows)
		if err != nil {
			return nil, fmt.Errorf("core: 读取建议失败: %w", err)
		}
		out = append(out, sg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 列建议失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) GetSuggestion(ctx context.Context, id string) (apimodel.Suggestion, error) {
	sg, err := scanSuggestion(s.client.QueryRow(ctx,
		`select `+suggestionSelectColumns+` from suggestions s where s.id = $1`, id))
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.Suggestion{}, fmt.Errorf("%w: 建议不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Suggestion{}, fmt.Errorf("core: 查建议失败: %w", err)
	}
	return sg, nil
}

// CreateSuggestion stores a new suggestion.
//
// ⚠️ **This table has no unique constraint at all, so there is no ErrConflict branch here**,
// compare with CreateIssue: that one has a partial unique index to dedup bot output,
// while suggestions are all typed by people. Two people submitting the same suggestion are two separate requests.
func (s *PGStore) CreateSuggestion(
	ctx context.Context, sg apimodel.Suggestion,
) (apimodel.Suggestion, error) {
	if _, err := s.client.Exec(ctx, `
        insert into suggestions
            (id, title, body, status, author, created_at, updated_at)
        values ($1, $2, $3, $4, $5, $6, $7)`,
		sg.ID, sg.Title, sg.Body, string(sg.Status), sg.Author,
		sg.CreatedAt, sg.UpdatedAt); err != nil {
		return apimodel.Suggestion{}, fmt.Errorf("core: 提建议失败: %w", err)
	}
	// On create both counts are always 0, so return the argument with the counts cleared, without querying again.
	out := sg
	out.ReplyCount = 0
	out.AttachmentCount = 0
	return out, nil
}

// SaveSuggestion overwrites the whole row.
//
// **author is not updated**: a suggestion never changes owner, and that column is the basis of authorization. Allowing it to change
// would allow "putting your own suggestion under someone else's name", or the other way around, claiming someone else's as yours.
// Updating one column less costs nothing; updating one more opens a privilege escalation path.
// (Same as SaveIssue not updating workspace_id.)
func (s *PGStore) SaveSuggestion(
	ctx context.Context, sg apimodel.Suggestion,
) (apimodel.Suggestion, error) {
	res, err := s.client.Exec(ctx, `
        update suggestions
        set title = $2, body = $3, status = $4, updated_at = $5
        where id = $1`,
		sg.ID, sg.Title, sg.Body, string(sg.Status), sg.UpdatedAt)
	if err != nil {
		return apimodel.Suggestion{}, fmt.Errorf("core: 存建议失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apimodel.Suggestion{}, fmt.Errorf("%w: 建议不存在", persistmodel.ErrNotFound)
	}
	// Query again to get the counts: the caller (Service) returns the result straight to the frontend,
	// and the counts in the argument were filled by the caller and are likely stale.
	return s.GetSuggestion(ctx, sg.ID)
}

// DeleteSuggestion deletes a suggestion and its replies and attachments.
//
// Three DELETEs in one transaction: run separately, a failure in the middle would leave orphan replies and orphan attachments,
// and orphan data is never found by any read path (reads always start from the suggestion, then fetch children),
// it just keeps taking up space, and **the attachments are especially costly** since they hold bytes.
func (s *PGStore) DeleteSuggestion(ctx context.Context, id string) error {
	err := s.client.WithTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`delete from suggestion_attachments where suggestion_id = $1`,
			`delete from suggestion_replies where suggestion_id = $1`,
			`delete from suggestions where id = $1`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("core: 删建议失败: %w", err)
	}
	return nil
}

// -- Replies --------------------------------------------------------------------

func (s *PGStore) ListSuggestionReplies(
	ctx context.Context, suggestionID string,
) ([]apimodel.SuggestionReply, error) {
	// Ascending: a conversation reads top to bottom (the opposite of the suggestion list's descending order, on purpose).
	rows, err := s.client.Query(ctx, `
        select id, suggestion_id, body, author, is_staff, origin, created_at, updated_at
        from suggestion_replies
        where suggestion_id = $1
        order by ord`, suggestionID)
	if err != nil {
		return nil, fmt.Errorf("core: 列回复失败: %w", err)
	}
	defer rows.Close()

	out := make([]apimodel.SuggestionReply, 0)
	for rows.Next() {
		var r apimodel.SuggestionReply
		if err := rows.Scan(&r.ID, &r.SuggestionID, &r.Body, &r.Author,
			&r.IsStaff, &r.Origin, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("core: 读取回复失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 列回复失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) GetSuggestionReply(
	ctx context.Context, id string,
) (apimodel.SuggestionReply, error) {
	var r apimodel.SuggestionReply
	err := s.client.QueryRow(ctx, `
        select id, suggestion_id, body, author, is_staff, origin, created_at, updated_at
        from suggestion_replies where id = $1`, id).
		Scan(&r.ID, &r.SuggestionID, &r.Body, &r.Author,
			&r.IsStaff, &r.Origin, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.SuggestionReply{}, fmt.Errorf("%w: 回复不存在", persistmodel.ErrNotFound)
		}
		return apimodel.SuggestionReply{}, fmt.Errorf("core: 查回复失败: %w", err)
	}
	return r, nil
}

func (s *PGStore) CreateSuggestionReply(
	ctx context.Context, r apimodel.SuggestionReply,
) (apimodel.SuggestionReply, error) {
	if _, err := s.client.Exec(ctx, `
        insert into suggestion_replies
            (id, suggestion_id, body, author, is_staff, origin, created_at, updated_at)
        values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.ID, r.SuggestionID, r.Body, r.Author,
		r.IsStaff, string(r.Origin), r.CreatedAt, r.UpdatedAt); err != nil {
		return apimodel.SuggestionReply{}, fmt.Errorf("core: 回复失败: %w", err)
	}
	return r, nil
}

// SaveSuggestionReply updates only the body and time.
//
// **is_staff is not updated**: that column records "whether they were staff at the moment they wrote this",
// a historical fact. Editing the body must not recompute it against the current allowlist; otherwise after someone changes role
// an old reply would turn from an official answer into a user comment (see apimodel.SuggestionReply.IsStaff).
// The same goes for author and origin; neither is updated.
func (s *PGStore) SaveSuggestionReply(
	ctx context.Context, r apimodel.SuggestionReply,
) (apimodel.SuggestionReply, error) {
	res, err := s.client.Exec(ctx, `
        update suggestion_replies set body = $2, updated_at = $3 where id = $1`,
		r.ID, r.Body, r.UpdatedAt)
	if err != nil {
		return apimodel.SuggestionReply{}, fmt.Errorf("core: 存回复失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apimodel.SuggestionReply{}, fmt.Errorf("%w: 回复不存在", persistmodel.ErrNotFound)
	}
	return r, nil
}

func (s *PGStore) DeleteSuggestionReply(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx,
		`delete from suggestion_replies where id = $1`, id); err != nil {
		return fmt.Errorf("core: 删回复失败: %w", err)
	}
	// Missing is not an error (idempotent); RowsAffected is not checked.
	return nil
}

// -- Attachments --------------------------------------------------------------------

func (s *PGStore) ListSuggestionAttachments(
	ctx context.Context, suggestionID string,
) ([]apimodel.SuggestionAttachment, error) {
	rows, err := s.client.Query(ctx,
		`select `+suggestionAttachmentColumns+`
         from suggestion_attachments where suggestion_id = $1 order by ord`, suggestionID)
	if err != nil {
		return nil, fmt.Errorf("core: 列附件失败: %w", err)
	}
	defer rows.Close()

	out := make([]apimodel.SuggestionAttachment, 0)
	for rows.Next() {
		att, err := scanSuggestionAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("core: 读取附件失败: %w", err)
		}
		out = append(out, att)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 列附件失败: %w", err)
	}
	return out, nil
}

// GetSuggestionAttachment gets metadata.
//
// Authorization goes through this: after getting SuggestionID, the Service looks up the suggestion to check author,
// without fetching bytes from object storage.
func (s *PGStore) GetSuggestionAttachment(
	ctx context.Context, id string,
) (apimodel.SuggestionAttachment, error) {
	att, err := scanSuggestionAttachment(s.client.QueryRow(ctx,
		`select `+suggestionAttachmentColumns+` from suggestion_attachments where id = $1`, id))
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.SuggestionAttachment{}, fmt.Errorf("%w: 附件不存在", persistmodel.ErrNotFound)
		}
		return apimodel.SuggestionAttachment{}, fmt.Errorf("core: 查附件失败: %w", err)
	}
	return att, nil
}

func (s *PGStore) CreateSuggestionAttachment(
	ctx context.Context, att apimodel.SuggestionAttachment,
) (apimodel.SuggestionAttachment, error) {
	// file_id must be non-empty, for the same reason as on the issue side (the bytes are only in object storage).
	att.FileID = strings.TrimSpace(att.FileID)
	if att.FileID == "" {
		return apimodel.SuggestionAttachment{}, fmt.Errorf("%w: 附件缺 file_id（字节没写进对象存储）", persistmodel.ErrInvalidArgument)
	}
	if _, err := s.client.Exec(ctx, `
        insert into suggestion_attachments
            (id, suggestion_id, filename, content_type, size,
             author, created_at, file_id)
        values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		att.ID, att.SuggestionID, att.Filename, att.ContentType,
		att.Size, att.Author, att.CreatedAt, att.FileID); err != nil {
		return apimodel.SuggestionAttachment{}, fmt.Errorf("core: 存附件失败: %w", err)
	}
	return att, nil
}

func (s *PGStore) DeleteSuggestionAttachment(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx,
		`delete from suggestion_attachments where id = $1`, id); err != nil {
		return fmt.Errorf("core: 删附件失败: %w", err)
	}
	// Missing is not an error (idempotent).
	return nil
}
