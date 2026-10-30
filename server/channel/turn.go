package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

// subscribeGrace is how long RunTurn waits after starting the subscription before sending.
//
// No handshake is needed: StreamRunEvents first replays the session's stored events, so sending a
// little early loses nothing. The pause only makes the common case cleaner.
const subscribeGrace = 50 * time.Millisecond

// TurnConfig describes one turn on an existing session.
type TurnConfig struct {
	// Conversations is used for AppendRunEvents and StreamRunEvents only.
	Conversations Conversations
	// RunID is the session to run on.
	RunID string
	// BuildEvent returns the user.message to send. It is called after the subscription has started,
	// so slow work (fetching platform context, downloading media) can happen here without missing events.
	BuildEvent func(ctx context.Context) (json.RawMessage, error)
	// OnFrame, when set, sees every frame (for live progress such as a chain-of-thought view).
	OnFrame func(ctx context.Context, raw []byte)
	// Answer collects the answer. Optional; a fresh buffer is used when nil. Pass one to set Persist.
	Answer *AnswerBuffer
}

// TurnResult is how a turn ended.
type TurnResult struct {
	// Answer is the final answer text; may be empty.
	Answer string
	// Interrupted means a newer message redirected this turn: nothing should be sent for it.
	Interrupted bool
	// FailedUpstream means Ark gave up (the model side kept failing, usually rate limiting).
	FailedUpstream bool
	// StopReason is the last stop_reason seen, for logs.
	StopReason string
	// Frames and Messages count all frames and agent.message frames, for logs.
	Frames, Messages int64
	// StreamErr is a subscription error that was not caused by the turn ending normally.
	StreamErr error
}

// SendError means the user message never reached the session (building or appending it failed).
// The caller should tell the user; nothing ran.
type SendError struct{ Err error }

func (e *SendError) Error() string { return e.Err.Error() }
func (e *SendError) Unwrap() error { return e.Err }

// RunTurn runs one turn: subscribe first, then send, then wait for the final frame.
//
// # Order: subscribe first, then send
//
// core stores a session's events only while someone is subscribed, so sending before subscribing
// loses the start of the reply. The subscription stops by itself at the final frame (Ark keeps the
// stream open between turns, see arkframe.TurnEnded), or when ctx is done.
//
// The returned error is always a *SendError; everything that happens after the message was sent is
// reported in TurnResult, because by then the turn ran and the caller still owes the user an answer.
func RunTurn(ctx context.Context, cfg TurnConfig) (TurnResult, error) {
	answer := cfg.Answer
	if answer == nil {
		answer = &AnswerBuffer{}
	}

	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()

	var (
		res            TurnResult
		streamErr      error
		frames, msgs   atomic.Int64
		interrupted    atomic.Bool
		failedUpstream atomic.Bool
		lastStop       atomic.Value
		wg             sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamErr = cfg.Conversations.StreamRunEvents(streamCtx, cfg.RunID, func(raw []byte) error {
			frames.Add(1)
			if text, ok := arkframe.AgentText(raw); ok && text != "" {
				msgs.Add(1)
				answer.Write(text)
			}
			if arkframe.StartsToolCall(raw) {
				answer.Reset()
			}
			if arkframe.Interrupted(raw) {
				interrupted.Store(true)
			}
			if arkframe.FailedUpstream(raw) {
				failedUpstream.Store(true)
			}
			if s := arkframe.StopReason(raw); s != "" {
				lastStop.Store(s)
			}
			if cfg.OnFrame != nil {
				cfg.OnFrame(streamCtx, raw)
			}
			if arkframe.TurnEnded(raw) {
				stopStream()
			}
			return nil
		})
		// We dropped the stream ourselves at the final frame: that is a normal end, not an error.
		if streamErr != nil && streamCtx.Err() != nil && ctx.Err() == nil {
			streamErr = nil
		}
	}()

	sleepCtx(ctx, subscribeGrace)

	event, err := cfg.BuildEvent(ctx)
	if err != nil {
		stopStream()
		wg.Wait()
		return res, &SendError{Err: err}
	}
	if _, err := cfg.Conversations.AppendRunEvents(ctx, cfg.RunID, []json.RawMessage{event}); err != nil {
		stopStream()
		wg.Wait()
		return res, &SendError{Err: fmt.Errorf("发消息到会话失败: %w", err)}
	}

	wg.Wait()
	res.Answer = answer.Text()
	res.Interrupted = interrupted.Load()
	res.FailedUpstream = failedUpstream.Load()
	res.StopReason, _ = lastStop.Load().(string)
	res.Frames = frames.Load()
	res.Messages = msgs.Load()
	res.StreamErr = streamErr
	return res, nil
}

// sleepCtx sleeps for d or until ctx is done. It reports whether the full sleep happened.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
