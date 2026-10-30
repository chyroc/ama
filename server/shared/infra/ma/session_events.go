// Session events: sending and receiving (non-streaming).
// API: POST /api/v3/sessions/{id}/events, GET /api/v3/sessions/{id}/events
//
// Events are the atomic messages exchanged between the app and the Agent: user messages,
// thinking, tool calls, tool results, status updates. The server persists them append-only; they
// can be subscribed to live (see session_stream.go) or replayed later (ListEvents in this file).

package ma

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/volcengine/ark-runtime-go/arkruntime/model/session"
)

// SendText sends a plain-text user message to a Session. It is the most common entry point.
//
//	_, err := ins.Sessions().SendText(ctx, sess.ID, "Help me analyze this data")
//
// Using the SDK directly needs three nested levels of union constructors (EventParams →
// UserMessageEventParams → MessageContentBlock → TextBlock); this turns it into one line.
func (s *SessionService) SendText(
	ctx context.Context,
	sessionID, text string,
) (*session.SendSessionEventsResponse, error) {
	if text == "" {
		return nil, errors.New("ma: 消息文本不能为空")
	}
	return s.SendEvents(ctx, sessionID, UserTextEvent(text))
}

// SendEvents sends several events at once. Build events with constructors like UserTextEvent,
// or build session.ManagedAgentsEventParams yourself.
func (s *SessionService) SendEvents(
	ctx context.Context,
	sessionID string,
	events ...session.ManagedAgentsEventParams,
) (*session.SendSessionEventsResponse, error) {
	if len(events) == 0 {
		return nil, errors.New("ma: 至少需要一个事件")
	}
	return s.SendEventsRaw(ctx, sessionID, &session.SendSessionEventsRequest{
		Events: events,
	})
}

// SendEventsRaw sends events using the SDK's own request body.
func (s *SessionService) SendEventsRaw(
	ctx context.Context,
	sessionID string,
	req *session.SendSessionEventsRequest,
) (*session.SendSessionEventsResponse, error) {
	return s.client.SendSessionEvents(ctx, sessionID, req)
}

// UserTextEvent builds a plain-text user message event.
//
// Use it when one request sends several events, or mixes them with other event types:
//
//	ins.Sessions().SendEvents(ctx, sid,
//	    ma.UserTextEvent("first part"),
//	    ma.UserTextEvent("second part"),
//	)
func UserTextEvent(text string) session.ManagedAgentsEventParams {
	return session.ManagedAgentsEventParams{
		OneOf: session.NewManagedAgentsUserMessageEventParamsManagedAgentsEventParamsSum(
			session.ManagedAgentsUserMessageEventParams{
				Content: []session.ManagedAgentsMessageContentBlock{
					{
						OneOf: session.NewManagedAgentsTextBlockManagedAgentsMessageContentBlockSum(
							session.ManagedAgentsTextBlock{Text: text},
						),
					},
				},
			},
		),
	}
}

// EventQuery holds the filters for querying past events.
type EventQuery struct {
	// Limit is the max number of items per page; 0 uses the server default.
	Limit int32
	// Page is the page cursor; leave empty for the first page, then use next_page from the
	// previous response.
	Page string
	// Types returns only events of these types, e.g. []string{"agent.message"}; empty returns all.
	Types []string
	// CreatedAtGte / CreatedAtLte filter by time, in RFC 3339 format.
	CreatedAtGte string
	CreatedAtLte string
}

// ListEvents replays the past events of a Session.
//
// The returned Data is the raw event union (30+ variants); each element is a map[string]any,
// dispatched on ev["type"]. For typed handling use the streaming API (StreamEvents).
func (s *SessionService) ListEvents(
	ctx context.Context,
	sessionID string,
	q EventQuery,
) (*session.ListSessionEventsResponse, error) {
	return s.ListEventsRaw(ctx, sessionID, &session.SessionEventsListEventsParams{
		SessionId:    sessionID,
		Limit:        optSessionInt32(q.Limit),
		Page:         optSessionString(q.Page),
		Types:        q.Types,
		CreatedAtGte: optSessionString(q.CreatedAtGte),
		CreatedAtLte: optSessionString(q.CreatedAtLte),
	})
}

// ListEventsRaw replays past events using the SDK's own query parameters.
func (s *SessionService) ListEventsRaw(
	ctx context.Context,
	sessionID string,
	params *session.SessionEventsListEventsParams,
) (*session.ListSessionEventsResponse, error) {
	return s.client.ListSessionEvents(ctx, sessionID, params)
}

// EventRawPayload extracts the raw JSON bytes of one Ark event.
//
// For types unknown to the SDK (*session.ManagedAgentsUnknownSessionEvent) it takes RawPayload
// directly (the SDK keeps the original bytes when decoding); for known struct types it
// serializes to standard JSON with json.Marshal.
func EventRawPayload(ev session.ManagedAgentsSessionEvent) ([]byte, error) {
	if unk, ok := ev.(*session.ManagedAgentsUnknownSessionEvent); ok && len(unk.RawPayload) > 0 {
		return append([]byte(nil), unk.RawPayload...), nil
	}
	return json.Marshal(ev)
}

// RawEventPayloads extracts every event in an event list response as raw JSON bytes.
func RawEventPayloads(resp *session.ListSessionEventsResponse) ([][]byte, error) {
	if resp == nil || len(resp.Events) == 0 {
		return nil, nil
	}
	out := make([][]byte, 0, len(resp.Events))
	for _, ev := range resp.Events {
		raw, err := EventRawPayload(ev)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// ListAllRawEvents fetches all past events of a Session page by page and returns them as a list
// of raw JSON byte slices.
//
// It follows the NextPage cursor automatically until all events are fetched.
func (s *SessionService) ListAllRawEvents(ctx context.Context, sessionID string) ([][]byte, error) {
	var all [][]byte
	var page string
	for {
		resp, err := s.ListEvents(ctx, sessionID, EventQuery{
			Limit: 100,
			Page:  page,
		})
		if err != nil {
			return nil, err
		}
		raws, err := RawEventPayloads(resp)
		if err != nil {
			return nil, err
		}
		all = append(all, raws...)

		if resp == nil || !resp.NextPage.IsSet() || resp.NextPage.Value == "" {
			break
		}
		page = resp.NextPage.Value
	}
	return all, nil
}
