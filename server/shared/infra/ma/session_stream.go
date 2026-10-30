// Session event stream (long-lived SSE connection).
// API: GET /api/v3/sessions/{id}/events/stream
//
// Besides passing the SDK decoder through, this file offers Run, a combined "send one message
// and wait for the Agent to finish" operation. It hides two easy-to-hit traps:
//
//  1. You must **open the stream before sending the message**, or the Agent's early events are
//     missed;
//  2. You must detect the terminal events (status_idle / terminated / error) to leave the loop,
//     or it blocks on Next() until the context times out.

package ma

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/session"
)

// streamAttachDelay is the wait between opening the stream and sending the message.
//
// When the SDK's StreamSessionEvents returns, the HTTP response headers have arrived, but the
// server-side subscription may not be fully ready yet; the official example leaves the same
// buffer. The value is kept small so it does not slow down time to first token.
const streamAttachDelay = 500 * time.Millisecond

// StreamEvents opens the event stream of a Session.
//
// Optionally pass eventDeltas to choose which event types get per-token delta frames (e.g.
// "agent.message", "agent.thinking").
//
// The caller **must** Close the returned decoder, or the underlying HTTP connection leaks:
//
//	dec, err := ins.Sessions().StreamEvents(ctx, sess.ID, "agent.message", "agent.thinking")
//	if err != nil {
//	    return err
//	}
//	defer dec.Close()
//
//	for dec.Next() {
//	    switch ev := dec.Event().Data.(type) {
//	    case *session.ManagedAgentsAgentMessageEvent:
//	        // ...
//	    }
//	}
//	if err := dec.Err(); err != nil {
//	    return err
//	}
//
// The stream's lifetime is controlled by ctx.
//
// This method uses the dedicated streamClient (an http.Client with no Timeout), so the stream is
// not cut off after some fixed duration. **This requires explicitly overriding the SDK's
// 10-minute default timeout**; see newStreamHTTPClient. The WithTimeout option does not affect
// this method.
func (s *SessionService) StreamEvents(
	ctx context.Context,
	sessionID string,
	eventDeltas ...string,
) (*session.StreamDecoder, error) {
	if len(eventDeltas) > 0 {
		return s.stream().StreamSessionEvents(ctx, sessionID,
			arkruntime.WithQuery("event_deltas", strings.Join(eventDeltas, ",")))
	}
	return s.stream().StreamSessionEvents(ctx, sessionID)
}

// RunResult is the result of Run.
type RunResult struct {
	// Text is the Agent's reply text, joined in event order.
	Text string
	// TerminalType is the type of the terminal event that ended this turn, such as
	// "session.status_idle" / "session.status_terminated" / "session.error".
	TerminalType string
	// Events records the types of all events received in this turn, in order, to help trace
	// what the Agent did.
	Events []string
}

// Idle reports whether this turn ended normally in idle (rather than being terminated or
// failing).
func (r RunResult) Idle() bool {
	return r.TerminalType == "session.status_idle"
}

// Run sends one text message, then blocks until the Agent finishes this turn, and returns the
// joined reply.
//
// It fits synchronous "one question, one answer" use. When you need to render as output arrives
// (typewriter effect) or handle tool call details, consume the event stream yourself with
// StreamEvents.
//
//	res, err := ins.Sessions().Run(ctx, sess.ID, "What is the highest mountain? Answer in one sentence")
//	fmt.Println(res.Text)
//
// ctx must have a timeout, or Run waits forever when the Agent misbehaves.
func (s *SessionService) Run(
	ctx context.Context,
	sessionID, text string,
) (*RunResult, error) {
	if text == "" {
		return nil, errors.New("ma: 消息文本不能为空")
	}

	// Open the stream first, then send the message; the other order misses the Agent's earliest
	// events.
	dec, err := s.StreamEvents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dec.Close() }()

	// Send asynchronously: SendSessionEvents is a synchronous HTTP call. If it were sent on the
	// current goroutine, reading the stream would have to wait for it to return, which may miss
	// early events.
	sendErr := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			sendErr <- ctx.Err()
			return
		case <-time.After(streamAttachDelay):
		}
		_, err := s.SendText(ctx, sessionID, text)
		sendErr <- err
	}()

	res := &RunResult{}
	var sb strings.Builder

	for dec.Next() {
		frame := dec.Event()
		res.Events = append(res.Events, frame.Type)

		switch ev := frame.Data.(type) {
		case *session.ManagedAgentsAgentMessageEvent:
			for _, block := range ev.Content {
				if block.Text != "" {
					sb.WriteString(block.Text)
				}
			}
		case *session.ManagedAgentsSessionStatusIdleEvent,
			*session.ManagedAgentsSessionStatusTerminatedEvent,
			*session.ManagedAgentsSessionErrorEvent:
			_ = ev
			res.TerminalType = frame.Type
			res.Text = strings.TrimSpace(sb.String())
			// A send failure must be reported, or "no reply" gets misread as an Agent problem.
			if err := <-sendErr; err != nil {
				return res, err
			}
			return res, dec.Err()
		}
	}

	res.Text = strings.TrimSpace(sb.String())

	// The stream ended without a terminal event: report the stream error first, then the send
	// error.
	if err := dec.Err(); err != nil {
		return res, err
	}
	if err := <-sendErr; err != nil {
		return res, err
	}
	return res, nil
}
