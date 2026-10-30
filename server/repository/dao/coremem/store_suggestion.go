// In-memory implementation of the product Suggestion domain.
//
// **The interface declaration stays in core/store_suggestion.go** (a domain's declaration goes with the domain and is embedded by Store);
// only the implementation is here. It matches the neighboring coredao/store_pg_suggestion.go case by case,
// and coredao's runStoreSuite runs once each against the two implementations.
//
// # The in-memory version is the reference behavior, but "reference" does not mean "write it any way"
//
// Unit tests run the in-memory version, so **wrong behavior on PG goes unwatched**. Four things decided by SQL on the PG side
// are deliberately matched here, and each of them really drifted once on issue or skill_files:
//
//	ordering         suggestions by ord descending, replies and attachments by ord ascending
//	byte isolation   neither list nor Get returns content (the PG side never selects it)
//	Size             computed from the content length, not trusting the caller's value
//	paging           Limit/Offset are cut **after filtering**, not cut first and filtered after
//
// The last one is specific to this domain and the easiest to get backwards: if you cut the page first and then filter by author,
// a user turning to page two sees an almost empty list, because most rows on that page belonged to other people
// and were filtered out. And it does not error; it is just "I clearly submitted 20, but page two has only 1".

package coremem

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// -- Suggestions --------------------------------------------------------------------

func (s *MemStore) ListSuggestions(
	_ context.Context, q persistmodel.SuggestionQuery,
) ([]apimodel.Suggestion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Non-nil empty slice: a Store convention (nil encodes as null, and the contract requires []).
	matched := make([]apimodel.Suggestion, 0)
	// Iterating backwards = newest first, matching PG's `order by ord desc`.
	for i := len(s.data.suggestions) - 1; i >= 0; i-- {
		sg := s.data.suggestions[i]
		// ⚠️ The Author filter **is also the implementation of authorization** (non-staff only see their own),
		// not an optional filter. Compare with equality, not case-insensitively:
		// the write side already lowercases (suggestionActor); only with both sides normalized will you avoid
		// "I can't see my own suggestions". That failure has no error; the list is just empty.
		if q.Author != "" && sg.Author != q.Author {
			continue
		}
		if q.Status != "" && sg.Status != q.Status {
			continue
		}
		matched = append(matched, s.withSuggestionCountsLocked(sg))
	}

	// **Cut the page after filtering**; see the file header.
	return pageSlice(matched, q.Offset, q.Limit), nil
}

// pageSlice cuts one page by offset/limit; out of range always returns an empty slice instead of panicking.
//
// It is a separate function because there are four edge cases (offset too large, limit is 0,
// offset+limit overflow, both are 0), and writing them at each call site is sure to miss one,
// and the missed one shows up as a panic, which is a 500, not an empty list.
func pageSlice(in []apimodel.Suggestion, offset, limit int) []apimodel.Suggestion {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(in) {
		return make([]apimodel.Suggestion, 0)
	}
	rest := in[offset:]
	// limit <= 0 means no limit (the caller has already applied the default; this is only a fallback).
	if limit > 0 && limit < len(rest) {
		rest = rest[:limit]
	}
	out := make([]apimodel.Suggestion, 0, len(rest))
	out = append(out, rest...)
	return out
}

func (s *MemStore) GetSuggestion(_ context.Context, id string) (apimodel.Suggestion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sg := range s.data.suggestions {
		if sg.ID == id {
			return s.withSuggestionCountsLocked(sg), nil
		}
	}
	return apimodel.Suggestion{}, fmt.Errorf("%w: 建议不存在", persistmodel.ErrNotFound)
}

// CreateSuggestion stores a new suggestion.
//
// ⚠️ **Deliberately no dedup check at all**, compare with the partial unique index on the CreateIssue side:
// that one exists for "a bot rescanning the same material N times", while suggestions are all typed by people.
// Two people submitting the same suggestion are two separate requests; merging them loses "how many people want it".
func (s *MemStore) CreateSuggestion(
	_ context.Context, sg apimodel.Suggestion,
) (apimodel.Suggestion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.suggestions = append(s.data.suggestions, sg)
	// On create both counts are always 0, but still go through the same function; bypassing it would make
	// "the object returned by create" and "the object later returned by Get" differ in shape.
	return s.withSuggestionCountsLocked(sg), nil
}

// SaveSuggestion overwrites a suggestion.
//
// ⚠️ **author and createdAt are not overwritten**, matching the update column list on the PG side exactly
// (that SQL only sets title/body/status/updated_at).
//
// author is **the basis of authorization** in this domain (non-staff can only read rows where author = themselves);
// allowing it to be overwritten opens a privilege escalation path: put your own suggestion under someone else's name, or the other way around,
// claim someone else's as yours. If either implementation misses this, as soon as `AMA_STORE=mem` is used again
// the hole comes back, and unit tests run exactly the in-memory version.
func (s *MemStore) SaveSuggestion(
	_ context.Context, sg apimodel.Suggestion,
) (apimodel.Suggestion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.data.suggestions {
		if cur.ID != sg.ID {
			continue
		}
		next := sg
		// These two columns are decided by the stored copy; the argument cannot change them.
		next.Author = cur.Author
		next.CreatedAt = cur.CreatedAt
		s.data.suggestions[i] = next
		return s.withSuggestionCountsLocked(next), nil
	}
	return apimodel.Suggestion{}, fmt.Errorf("%w: 建议不存在", persistmodel.ErrNotFound)
}

// DeleteSuggestion deletes a suggestion together with its replies and attachments. Missing is not an error (idempotent).
func (s *MemStore) DeleteSuggestion(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.data.suggestionAttachments, id)
	delete(s.data.suggestionReplies, id)

	s.data.suggestions = filterSlice(s.data.suggestions, func(sg apimodel.Suggestion) bool {
		return sg.ID != id
	})
	return nil
}

// withSuggestionCountsLocked fills in the two derived counts. **The caller must already hold the lock.**
//
// The counts are derived and not stored (same as on the issue side). Storing them would mean updating them on every reply and attachment
// add/delete, and missing one spot shows as "the list says 3 replies, opening it shows only 2".
// That inconsistency has no error at all, and both numbers look plausible.
func (s *MemStore) withSuggestionCountsLocked(sg apimodel.Suggestion) apimodel.Suggestion {
	out := sg
	out.ReplyCount = len(s.data.suggestionReplies[sg.ID])
	out.AttachmentCount = len(s.data.suggestionAttachments[sg.ID])
	return out
}

// -- Replies --------------------------------------------------------------------

func (s *MemStore) ListSuggestionReplies(
	_ context.Context, suggestionID string,
) ([]apimodel.SuggestionReply, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Ascending: a conversation reads top to bottom (the opposite of the suggestion list's descending order, on purpose).
	out := make([]apimodel.SuggestionReply, 0, len(s.data.suggestionReplies[suggestionID]))
	out = append(out, s.data.suggestionReplies[suggestionID]...)
	return out, nil
}

func (s *MemStore) GetSuggestionReply(
	_ context.Context, id string,
) (apimodel.SuggestionReply, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, list := range s.data.suggestionReplies {
		for _, r := range list {
			if r.ID == id {
				return r, nil
			}
		}
	}
	return apimodel.SuggestionReply{}, fmt.Errorf("%w: 回复不存在", persistmodel.ErrNotFound)
}

func (s *MemStore) CreateSuggestionReply(
	_ context.Context, r apimodel.SuggestionReply,
) (apimodel.SuggestionReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.suggestionReplies[r.SuggestionID] = append(s.data.suggestionReplies[r.SuggestionID], r)
	return r, nil
}

// SaveSuggestionReply overwrites a reply.
//
// ⚠️ **Only the body and updatedAt change**, matching the update column list on the PG side exactly.
//
// is_staff records "whether they were staff at the moment they wrote this", a historical fact.
// Editing the body must not recompute it, otherwise after someone changes role an old reply would turn from an official answer into a user comment.
// The same goes for author and origin.
func (s *MemStore) SaveSuggestionReply(
	_ context.Context, r apimodel.SuggestionReply,
) (apimodel.SuggestionReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.data.suggestionReplies[r.SuggestionID]
	for i, cur := range list {
		if cur.ID != r.ID {
			continue
		}
		next := cur
		next.Body = r.Body
		next.UpdatedAt = r.UpdatedAt
		list[i] = next
		return next, nil
	}
	return apimodel.SuggestionReply{}, fmt.Errorf("%w: 回复不存在", persistmodel.ErrNotFound)
}

func (s *MemStore) DeleteSuggestionReply(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sugID, list := range s.data.suggestionReplies {
		kept := filterSlice(list, func(r apimodel.SuggestionReply) bool { return r.ID != id })
		if len(kept) == len(list) {
			continue
		}
		if len(kept) == 0 {
			// When emptied, remove the key, closer to PG (there, no rows means no rows).
			delete(s.data.suggestionReplies, sugID)
		} else {
			s.data.suggestionReplies[sugID] = kept
		}
		return nil
	}
	// Missing is not an error (idempotent).
	return nil
}

// -- Attachments --------------------------------------------------------------------

func (s *MemStore) ListSuggestionAttachments(
	_ context.Context, suggestionID string,
) ([]apimodel.SuggestionAttachment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Metadata never contains bytes (bytes are in another map), so copying the value is enough.
	out := make([]apimodel.SuggestionAttachment, 0, len(s.data.suggestionAttachments[suggestionID]))
	out = append(out, s.data.suggestionAttachments[suggestionID]...)
	return out, nil
}

func (s *MemStore) GetSuggestionAttachment(
	_ context.Context, id string,
) (apimodel.SuggestionAttachment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, list := range s.data.suggestionAttachments {
		for _, att := range list {
			if att.ID == id {
				return att, nil
			}
		}
	}
	return apimodel.SuggestionAttachment{}, fmt.Errorf("%w: 附件不存在", persistmodel.ErrNotFound)
}

func (s *MemStore) CreateSuggestionAttachment(
	_ context.Context, att apimodel.SuggestionAttachment,
) (apimodel.SuggestionAttachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Only metadata is stored: the bytes were already written into BlobStore by the caller (att.FileID points to them).
	// A missing file_id must be rejected, giving the same answer as the PG version (not null on the column).
	att.FileID = strings.TrimSpace(att.FileID)
	if att.FileID == "" {
		return apimodel.SuggestionAttachment{}, fmt.Errorf("%w: 附件缺 file_id（字节没写进对象存储）", persistmodel.ErrInvalidArgument)
	}
	s.data.suggestionAttachments[att.SuggestionID] = append(
		s.data.suggestionAttachments[att.SuggestionID], att)
	return att, nil
}

func (s *MemStore) DeleteSuggestionAttachment(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sugID, list := range s.data.suggestionAttachments {
		kept := filterSlice(list, func(a apimodel.SuggestionAttachment) bool { return a.ID != id })
		if len(kept) == len(list) {
			continue
		}
		if len(kept) == 0 {
			delete(s.data.suggestionAttachments, sugID)
		} else {
			s.data.suggestionAttachments[sugID] = kept
		}
		return nil
	}
	return nil
}
