// In-memory implementation of the Issue domain.
//
// **The interface declaration stays in core/store_issue.go** (a domain's declaration goes with the domain and is embedded by Store);
// only the implementation is here. It matches the neighboring coredao/store_pg_issue.go case by case,
// and coredao's runStoreSuite runs once each against the two implementations.
//
// # The in-memory version is the reference behavior, but "reference" does not mean "write it any way"
//
// Any difference between the two implementations shows up in the worst way: unit tests run the in-memory version, so **wrong
// behavior on PG goes unwatched**. The skill_files case (the in-memory version stored the caller's Size as given, PG
// recomputed it as len(content)) was caught exactly that way, by SkillFilesRoundTrip.

package coremem

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// -- Issues --------------------------------------------------------------------

func (s *MemStore) ListIssues(
	_ context.Context, workspaceID string, status apimodel.IssueStatus,
) ([]apimodel.Issue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Non-nil empty slice: a Store convention (nil encodes as null, and the contract requires []).
	out := make([]apimodel.Issue, 0)
	// Iterating backwards = newest first, matching PG's `order by ord desc`.
	for i := len(s.data.issues) - 1; i >= 0; i-- {
		iss := s.data.issues[i]
		if iss.WorkspaceID != workspaceID {
			continue
		}
		if status != "" && iss.Status != status {
			continue
		}
		out = append(out, s.withIssueCountsLocked(iss))
	}
	return out, nil
}

func (s *MemStore) GetIssue(_ context.Context, id string) (apimodel.Issue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, iss := range s.data.issues {
		if iss.ID == id {
			return s.withIssueCountsLocked(iss), nil
		}
	}
	return apimodel.Issue{}, fmt.Errorf("%w: 问题不存在", persistmodel.ErrNotFound)
}

func (s *MemStore) CreateIssue(_ context.Context, iss apimodel.Issue) (apimodel.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// ⚠️ **Matches PG's partial unique index exactly** (same reason PutFeedback matches dedup_key).
	//
	// If the two stores dedup differently it shows up in the worst way: unit tests run the in-memory version, so
	// a drift like "PG has a unique index while the in-memory version keeps piling up duplicates" goes unwatched, and as soon as
	// AMA_STORE=mem is used again the problem comes back. The condition must match the index's where: **only rows with both columns non-empty are deduped**.
	if iss.Origin != "" && iss.Fingerprint != "" {
		for _, cur := range s.data.issues {
			if cur.WorkspaceID == iss.WorkspaceID &&
				cur.Origin == iss.Origin && cur.Fingerprint == iss.Fingerprint {
				return apimodel.Issue{}, fmt.Errorf("%w: 同指纹的问题已存在", persistmodel.ErrConflict)
			}
		}
	}

	s.data.issues = append(s.data.issues, iss)
	// On create both counts are always 0, but still go through the same function; bypassing it would make
	// "the object returned by create" and "the object later returned by Get" differ in shape.
	return s.withIssueCountsLocked(iss), nil
}

func (s *MemStore) SaveIssue(_ context.Context, iss apimodel.Issue) (apimodel.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.data.issues {
		if cur.ID != iss.ID {
			continue
		}
		s.data.issues[i] = iss
		return s.withIssueCountsLocked(iss), nil
	}
	return apimodel.Issue{}, fmt.Errorf("%w: 问题不存在", persistmodel.ErrNotFound)
}

// DeleteIssue deletes an issue together with its comments and attachments. Missing is not an error (idempotent).
func (s *MemStore) DeleteIssue(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.data.issueAttachments, id)
	delete(s.data.issueComments, id)

	s.data.issues = filterSlice(s.data.issues, func(iss apimodel.Issue) bool {
		return iss.ID != id
	})
	return nil
}

// withIssueCountsLocked fills in the two derived counts. **The caller must already hold the lock.**
//
// The counts are derived and not stored (same as Run.AgentDeleted). Storing them would mean updating them on every comment
// and attachment add/delete, and missing one spot shows as "the list says 3 comments, opening it shows only 2".
// That inconsistency has no error at all, and both numbers look plausible.
func (s *MemStore) withIssueCountsLocked(iss apimodel.Issue) apimodel.Issue {
	out := iss
	out.CommentCount = len(s.data.issueComments[iss.ID])
	out.AttachmentCount = len(s.data.issueAttachments[iss.ID])
	return out
}

// -- Comments --------------------------------------------------------------------

func (s *MemStore) ListIssueComments(
	_ context.Context, issueID string,
) ([]apimodel.IssueComment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Ascending: a conversation reads top to bottom (the opposite of the issue list's descending order, on purpose).
	out := make([]apimodel.IssueComment, 0, len(s.data.issueComments[issueID]))
	out = append(out, s.data.issueComments[issueID]...)
	return out, nil
}

func (s *MemStore) GetIssueComment(_ context.Context, id string) (apimodel.IssueComment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, list := range s.data.issueComments {
		for _, c := range list {
			if c.ID == id {
				return c, nil
			}
		}
	}
	return apimodel.IssueComment{}, fmt.Errorf("%w: 评论不存在", persistmodel.ErrNotFound)
}

func (s *MemStore) CreateIssueComment(
	_ context.Context, c apimodel.IssueComment,
) (apimodel.IssueComment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.issueComments[c.IssueID] = append(s.data.issueComments[c.IssueID], c)
	return c, nil
}

func (s *MemStore) SaveIssueComment(
	_ context.Context, c apimodel.IssueComment,
) (apimodel.IssueComment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.data.issueComments[c.IssueID]
	for i, cur := range list {
		if cur.ID != c.ID {
			continue
		}
		list[i] = c
		return c, nil
	}
	return apimodel.IssueComment{}, fmt.Errorf("%w: 评论不存在", persistmodel.ErrNotFound)
}

func (s *MemStore) DeleteIssueComment(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for issueID, list := range s.data.issueComments {
		kept := filterSlice(list, func(c apimodel.IssueComment) bool { return c.ID != id })
		if len(kept) == len(list) {
			continue
		}
		if len(kept) == 0 {
			// When emptied, remove the key, closer to PG (there, no rows means no rows).
			delete(s.data.issueComments, issueID)
		} else {
			s.data.issueComments[issueID] = kept
		}
		return nil
	}
	// Missing is not an error (idempotent).
	return nil
}

// -- Attachments --------------------------------------------------------------------

func (s *MemStore) ListIssueAttachments(
	_ context.Context, issueID string,
) ([]apimodel.IssueAttachment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Metadata never contains bytes (bytes are in another map), so copying the value is enough.
	out := make([]apimodel.IssueAttachment, 0, len(s.data.issueAttachments[issueID]))
	out = append(out, s.data.issueAttachments[issueID]...)
	return out, nil
}

func (s *MemStore) GetIssueAttachment(
	_ context.Context, id string,
) (apimodel.IssueAttachment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, list := range s.data.issueAttachments {
		for _, att := range list {
			if att.ID == id {
				return att, nil
			}
		}
	}
	return apimodel.IssueAttachment{}, fmt.Errorf("%w: 附件不存在", persistmodel.ErrNotFound)
}

func (s *MemStore) CreateIssueAttachment(
	_ context.Context, att apimodel.IssueAttachment,
) (apimodel.IssueAttachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Only metadata is stored: the bytes were already written into BlobStore by the caller (att.FileID points to them).
	// A missing file_id must be rejected, giving the same answer as the PG version (not null on the column).
	att.FileID = strings.TrimSpace(att.FileID)
	if att.FileID == "" {
		return apimodel.IssueAttachment{}, fmt.Errorf("%w: 附件缺 file_id（字节没写进对象存储）", persistmodel.ErrInvalidArgument)
	}
	s.data.issueAttachments[att.IssueID] = append(s.data.issueAttachments[att.IssueID], att)
	return att, nil
}

func (s *MemStore) DeleteIssueAttachment(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for issueID, list := range s.data.issueAttachments {
		kept := filterSlice(list, func(a apimodel.IssueAttachment) bool { return a.ID != id })
		if len(kept) == len(list) {
			continue
		}
		if len(kept) == 0 {
			delete(s.data.issueAttachments, issueID)
		} else {
			s.data.issueAttachments[issueID] = kept
		}
		return nil
	}
	return nil
}
