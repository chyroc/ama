// PostgreSQL implementation of Issue.
//
// A separate file matching store_issue.go (the in-memory version) case by case; runStoreSuite runs both.
//
// # This domain uses real columns, not doc jsonb
//
// Unlike the skills / agents / runs pattern of "scalar columns + doc jsonb holding the whole entity",
// all three tables here are real columns. The rule is the one set by the runs table: **anything queried/aggregated becomes a real column,
// the rest goes into doc**, and every field of issue is queried:
//
//	workspace_id / status   lists filter by them
//	ord                     sorted on
//	title / body            displayed, and searched later
//	author / two timestamps displayed
//
// Not one field is left, so doc would be empty. Forcing that pattern would only add a layer of encoding,
// and turn "filter by status" into something like doc->>'status' that cannot use an index.
//
// # The two counts are queried, not stored
//
// CommentCount / AttachmentCount are computed by subqueries (same approach as Run.AgentDeleted:
// if it can be computed it should not be stored). Storing them would mean updating them on every comment and attachment add/delete,
// and missing one spot shows as "the list says 3 comments, opening it shows only 2", with no error at all,
// and both numbers look plausible.
//
// The cost is two extra subqueries per list query. At issue volumes (tens to hundreds per workspace) that is no problem;
// if it ever needs optimizing, the right way is a count column + trigger, not maintaining it in Go.

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

// issueSelectColumns is the fixed column list for listing issues, including the two computed counts.
//
// It is a constant because ListIssues and GetIssue must be **exactly the same**: one column different,
// and the two paths return different objects, an inconsistency only noticed when "it looks right in the list but a number is missing when opened".
const issueSelectColumns = `
    i.id, i.workspace_id, i.title, i.body, i.status, i.priority,
    i.author, i.created_at, i.updated_at,
    i.origin, i.fingerprint,
    (select count(*) from issue_comments c where c.issue_id = i.id),
    (select count(*) from issue_attachments a where a.issue_id = i.id)`

// scanIssue reads one row in the order of issueSelectColumns.
func scanIssue(row interface{ Scan(...any) error }) (apimodel.Issue, error) {
	var iss apimodel.Issue
	err := row.Scan(
		&iss.ID, &iss.WorkspaceID, &iss.Title, &iss.Body, &iss.Status, &iss.Priority,
		&iss.Author, &iss.CreatedAt, &iss.UpdatedAt,
		&iss.Origin, &iss.Fingerprint,
		&iss.CommentCount, &iss.AttachmentCount,
	)
	return iss, err
}

// -- Issues --------------------------------------------------------------------

func (s *PGStore) ListIssues(
	ctx context.Context, workspaceID string, status apimodel.IssueStatus,
) ([]apimodel.Issue, error) {
	// An empty status means no filter. Use `($2 = '' or i.status = $2)` instead of building two
	// SQL strings in Go: the string-building version will sooner or later get a variable stuffed in, while this form shares one statement for both cases.
	//
	// ⚠️ `order by i.ord desc` must not be dropped or replaced with created_at: two rows created in the same millisecond
	// sort unstably by time, so refreshing the list swaps two rows.
	rows, err := s.client.Query(ctx, `
        select `+issueSelectColumns+`
        from issues i
        where i.workspace_id = $1 and ($2 = '' or i.status = $2)
        order by i.ord desc`, workspaceID, string(status))
	if err != nil {
		return nil, fmt.Errorf("core: 列问题失败: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice: the contract requires [], nil encodes as null.
	out := make([]apimodel.Issue, 0)
	for rows.Next() {
		iss, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("core: 读取问题失败: %w", err)
		}
		out = append(out, iss)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 列问题失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) GetIssue(ctx context.Context, id string) (apimodel.Issue, error) {
	iss, err := scanIssue(s.client.QueryRow(ctx,
		`select `+issueSelectColumns+` from issues i where i.id = $1`, id))
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.Issue{}, fmt.Errorf("%w: 问题不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Issue{}, fmt.Errorf("core: 查问题失败: %w", err)
	}
	return iss, nil
}

func (s *PGStore) CreateIssue(ctx context.Context, iss apimodel.Issue) (apimodel.Issue, error) {
	if _, err := s.client.Exec(ctx, `
        insert into issues
            (id, workspace_id, title, body, status, priority, author, created_at, updated_at,
             origin, fingerprint)
        values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		iss.ID, iss.WorkspaceID, iss.Title, iss.Body,
		string(iss.Status), string(iss.Priority), iss.Author,
		iss.CreatedAt, iss.UpdatedAt,
		string(iss.Origin), iss.Fingerprint); err != nil {
		// Hit the partial unique index = one with the same source and fingerprint already exists (two instances scanned it at once).
		// **Translate it to ErrConflict instead of passing it through**: the caller (scheduler.deliver) falls back to
		// "append a comment", a normal branch of this path, not a failure. Passing it through would count it as
		// "failed to create issue", while this round actually lost nothing.
		if pg.IsUniqueViolation(err) {
			return apimodel.Issue{}, fmt.Errorf("%w: 同指纹的问题已存在", persistmodel.ErrConflict)
		}
		return apimodel.Issue{}, fmt.Errorf("core: 建问题失败: %w", err)
	}
	// On create both counts are always 0, so return the argument with the counts cleared, without querying again.
	out := iss
	out.CommentCount = 0
	out.AttachmentCount = 0
	return out, nil
}

// SaveIssue overwrites the whole row.
//
// **workspace_id is not updated**: an issue never changes workspace, and that column is the basis of authorization.
// Allowing it to change would allow "moving an issue from your workspace into someone else's".
// Updating one column less costs nothing; updating one more opens a privilege escalation path.
func (s *PGStore) SaveIssue(ctx context.Context, iss apimodel.Issue) (apimodel.Issue, error) {
	res, err := s.client.Exec(ctx, `
        update issues
        set title = $2, body = $3, status = $4, priority = $5, updated_at = $6
        where id = $1`,
		iss.ID, iss.Title, iss.Body,
		string(iss.Status), string(iss.Priority), iss.UpdatedAt)
	if err != nil {
		return apimodel.Issue{}, fmt.Errorf("core: 存问题失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apimodel.Issue{}, fmt.Errorf("%w: 问题不存在", persistmodel.ErrNotFound)
	}
	// Query again to get the counts: the caller (Service) returns the result straight to the frontend,
	// and the counts in the argument were filled by the caller and are likely stale.
	return s.GetIssue(ctx, iss.ID)
}

// DeleteIssue deletes an issue and its comments and attachments.
//
// Three DELETEs in one transaction: run separately, a failure in the middle would leave orphan comments and orphan attachments,
// and orphan data is never found by any read path (reads always start from the issue, then fetch children),
// it just keeps taking up space, and **the attachments are especially costly** since they hold bytes.
func (s *PGStore) DeleteIssue(ctx context.Context, id string) error {
	err := s.client.WithTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`delete from issue_attachments where issue_id = $1`,
			`delete from issue_comments where issue_id = $1`,
			`delete from issues where id = $1`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("core: 删问题失败: %w", err)
	}
	return nil
}

// -- Comments --------------------------------------------------------------------

func (s *PGStore) ListIssueComments(
	ctx context.Context, issueID string,
) ([]apimodel.IssueComment, error) {
	// Ascending: a conversation reads top to bottom (the opposite of the issue list's descending order, on purpose).
	rows, err := s.client.Query(ctx, `
        select id, issue_id, workspace_id, body, author, created_at, updated_at
        from issue_comments where issue_id = $1 order by ord`, issueID)
	if err != nil {
		return nil, fmt.Errorf("core: 列评论失败: %w", err)
	}
	defer rows.Close()

	out := make([]apimodel.IssueComment, 0)
	for rows.Next() {
		var c apimodel.IssueComment
		if err := rows.Scan(&c.ID, &c.IssueID, &c.WorkspaceID,
			&c.Body, &c.Author, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("core: 读取评论失败: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 列评论失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) GetIssueComment(ctx context.Context, id string) (apimodel.IssueComment, error) {
	var c apimodel.IssueComment
	err := s.client.QueryRow(ctx, `
        select id, issue_id, workspace_id, body, author, created_at, updated_at
        from issue_comments where id = $1`, id).
		Scan(&c.ID, &c.IssueID, &c.WorkspaceID, &c.Body, &c.Author, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.IssueComment{}, fmt.Errorf("%w: 评论不存在", persistmodel.ErrNotFound)
		}
		return apimodel.IssueComment{}, fmt.Errorf("core: 查评论失败: %w", err)
	}
	return c, nil
}

func (s *PGStore) CreateIssueComment(
	ctx context.Context, c apimodel.IssueComment,
) (apimodel.IssueComment, error) {
	if _, err := s.client.Exec(ctx, `
        insert into issue_comments
            (id, issue_id, workspace_id, body, author, created_at, updated_at)
        values ($1, $2, $3, $4, $5, $6, $7)`,
		c.ID, c.IssueID, c.WorkspaceID, c.Body, c.Author, c.CreatedAt, c.UpdatedAt); err != nil {
		return apimodel.IssueComment{}, fmt.Errorf("core: 建评论失败: %w", err)
	}
	return c, nil
}

// SaveIssueComment changes the comment body. Like SaveIssue, **issue_id and workspace_id are not updated**:
// a comment never changes owner, and opening those two columns opens a privilege escalation path.
func (s *PGStore) SaveIssueComment(
	ctx context.Context, c apimodel.IssueComment,
) (apimodel.IssueComment, error) {
	res, err := s.client.Exec(ctx,
		`update issue_comments set body = $2, updated_at = $3 where id = $1`,
		c.ID, c.Body, c.UpdatedAt)
	if err != nil {
		return apimodel.IssueComment{}, fmt.Errorf("core: 存评论失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apimodel.IssueComment{}, fmt.Errorf("%w: 评论不存在", persistmodel.ErrNotFound)
	}
	return c, nil
}

func (s *PGStore) DeleteIssueComment(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `delete from issue_comments where id = $1`, id); err != nil {
		return fmt.Errorf("core: 删评论失败: %w", err)
	}
	return nil
}

// -- Attachments --------------------------------------------------------------------

// attachmentSelectColumns is the metadata columns of attachments. The bytes are not in PG (they are in object storage; file_id points there).
const attachmentSelectColumns = `
    id, issue_id, workspace_id, filename, content_type, size, author, created_at,
    file_id`

func scanAttachment(row interface{ Scan(...any) error }) (apimodel.IssueAttachment, error) {
	var a apimodel.IssueAttachment
	err := row.Scan(&a.ID, &a.IssueID, &a.WorkspaceID,
		&a.Filename, &a.ContentType, &a.Size, &a.Author, &a.CreatedAt, &a.FileID)
	return a, err
}

func (s *PGStore) ListIssueAttachments(
	ctx context.Context, issueID string,
) ([]apimodel.IssueAttachment, error) {
	rows, err := s.client.Query(ctx,
		`select `+attachmentSelectColumns+
			` from issue_attachments where issue_id = $1 order by ord`, issueID)
	if err != nil {
		return nil, fmt.Errorf("core: 列附件失败: %w", err)
	}
	defer rows.Close()

	out := make([]apimodel.IssueAttachment, 0)
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("core: 读取附件失败: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 列附件失败: %w", err)
	}
	return out, nil
}

// GetIssueAttachment gets metadata (no bytes). **Authorization resolving goes through this.**
func (s *PGStore) GetIssueAttachment(
	ctx context.Context, id string,
) (apimodel.IssueAttachment, error) {
	a, err := scanAttachment(s.client.QueryRow(ctx,
		`select `+attachmentSelectColumns+` from issue_attachments where id = $1`, id))
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.IssueAttachment{}, fmt.Errorf("%w: 附件不存在", persistmodel.ErrNotFound)
		}
		return apimodel.IssueAttachment{}, fmt.Errorf("core: 查附件失败: %w", err)
	}
	return a, nil
}

func (s *PGStore) CreateIssueAttachment(
	ctx context.Context, att apimodel.IssueAttachment,
) (apimodel.IssueAttachment, error) {
	// file_id must be non-empty: the bytes are only in object storage, and an attachment without file_id cannot be read back.
	// The column has not null as a backstop too; checking here first is so the error message is human-readable.
	att.FileID = strings.TrimSpace(att.FileID)
	if att.FileID == "" {
		return apimodel.IssueAttachment{}, fmt.Errorf("%w: 附件缺 file_id（字节没写进对象存储）", persistmodel.ErrInvalidArgument)
	}
	if _, err := s.client.Exec(ctx, `
        insert into issue_attachments
            (id, issue_id, workspace_id, filename, content_type, size,
             author, created_at, file_id)
        values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		att.ID, att.IssueID, att.WorkspaceID, att.Filename,
		att.ContentType, att.Size, att.Author,
		att.CreatedAt, att.FileID); err != nil {
		return apimodel.IssueAttachment{}, fmt.Errorf("core: 存附件失败: %w", err)
	}
	return att, nil
}

func (s *PGStore) DeleteIssueAttachment(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `delete from issue_attachments where id = $1`, id); err != nil {
		return fmt.Errorf("core: 删附件失败: %w", err)
	}
	return nil
}
