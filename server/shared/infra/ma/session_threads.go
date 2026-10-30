// Session threads.
// API: /api/v3/sessions/{id}/threads
//
// A thread is an execution branch inside a Session (e.g. one per sub-Agent in multi-Agent
// orchestration). Its events can be queried or streamed on their own.

package ma

import (
	"context"

	"github.com/volcengine/ark-runtime-go/arkruntime/model/session"
)

// ListThreads lists the threads of a Session, page by page.
func (s *SessionService) ListThreads(
	ctx context.Context,
	sessionID string,
	page Page,
) (*session.ListSessionThreadsResponse, error) {
	return s.ListThreadsRaw(ctx, sessionID, &session.SessionThreadsListParams{
		SessionId: sessionID,
		Limit:     optSessionInt32(page.Limit),
		Page:      optSessionString(page.Page),
	})
}

// ListThreadsRaw lists threads using the SDK's own query parameters.
func (s *SessionService) ListThreadsRaw(
	ctx context.Context,
	sessionID string,
	params *session.SessionThreadsListParams,
) (*session.ListSessionThreadsResponse, error) {
	return s.client.ListSessionThreads(ctx, sessionID, params)
}

// GetThread returns the details of one thread.
func (s *SessionService) GetThread(
	ctx context.Context,
	sessionID, threadID string,
) (*session.SessionThread, error) {
	return s.client.GetSessionThread(ctx, sessionID, threadID)
}

// ListThreadEvents replays the past events of one thread.
func (s *SessionService) ListThreadEvents(
	ctx context.Context,
	sessionID, threadID string,
	q EventQuery,
) (*session.ListSessionEventsResponse, error) {
	return s.ListThreadEventsRaw(ctx, sessionID, threadID, &session.SessionThreadsListEventsParams{
		SessionId: sessionID,
		ThreadId:  threadID,
		Limit:     optSessionInt32(q.Limit),
		Page:      optSessionString(q.Page),
	})
}

// ListThreadEventsRaw replays thread events using the SDK's own query parameters.
func (s *SessionService) ListThreadEventsRaw(
	ctx context.Context,
	sessionID, threadID string,
	params *session.SessionThreadsListEventsParams,
) (*session.ListSessionEventsResponse, error) {
	return s.client.ListSessionThreadEvents(ctx, sessionID, threadID, params)
}

// StreamThreadEvents opens the event stream of one thread.
//
// As with StreamEvents, the caller must Close the returned decoder, and it also goes through the
// streamClient that has no Timeout (see newStreamHTTPClient).
func (s *SessionService) StreamThreadEvents(
	ctx context.Context,
	sessionID, threadID string,
) (*session.StreamDecoder, error) {
	return s.stream().StreamSessionThreadEvents(ctx, sessionID, threadID)
}
