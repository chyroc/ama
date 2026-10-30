package channel

import (
	"strings"
	"sync"
)

// AnswerBuffer collects the agent's text during a turn and decides which part is the answer.
//
// The text the agent writes right before a tool call is usually narration ("let me look that up"),
// not the answer, so Reset starts a new segment at every tool call and Text returns the last segment.
//
// ⚠️ Reset keeps the dropped segment as a backup. A turn may end right after a tool call with no new
// text; without the backup Text would be empty and the user would be told "no reply was produced"
// although a real answer was written before the tool call. Sending one extra sentence of narration is
// much better than losing the answer.
type AnswerBuffer struct {
	// Persist, when set, is called with the current segment after every change, so a turn interrupted
	// by a restart can be finished later. It must be cheap and must not block.
	Persist func(text string)

	mu   sync.Mutex
	b    strings.Builder
	prev string
}

// Write appends text to the current segment.
func (a *AnswerBuffer) Write(text string) {
	a.mu.Lock()
	a.b.WriteString(text)
	current := a.b.String()
	a.mu.Unlock()
	if a.Persist != nil {
		a.Persist(current)
	}
}

// Reset starts a new segment and keeps the old one (if it has content) as the backup.
func (a *AnswerBuffer) Reset() {
	a.mu.Lock()
	if cur := a.b.String(); strings.TrimSpace(cur) != "" {
		a.prev = cur
	}
	a.b.Reset()
	a.mu.Unlock()
	// What is persisted is the state after the reset, not the backup: a recovered turn must not send
	// narration that is already out of date as the start of its answer.
	if a.Persist != nil {
		a.Persist("")
	}
}

// Text returns the current segment, or the backup when the current segment is empty.
func (a *AnswerBuffer) Text() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur := a.b.String(); strings.TrimSpace(cur) != "" {
		return cur
	}
	return a.prev
}
