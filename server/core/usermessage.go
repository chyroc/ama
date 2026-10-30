// The prefix-stripping function the business layer uses to turn an error into "the sentence
// shown to the user".
//
// # ⚠️ This repo now has four nearly identical copies of the stripping logic, **on purpose**
//
//	apihandler/handler.go  userMessage      last step at the HTTP boundary, output goes straight to the frontend
//	core/usermessage.go    userMessage      this file, stored in the failure_reason column
//	core/agentsync.go      userMessageOf    same as above, but a nil error gives an empty string
//	core/skillmarket.go    stripErrPrefix   intermediate step when the business layer builds a message
//
// The precedent and the reason are written on stripErrPrefix in skillmarket.go: **sharing one
// function means "changing how the HTTP boundary strips" accidentally changes the business
// layer's wording**, and that kind of change gives zero errors. It only makes the text stored
// in some column quietly look different.
//
// # Why this copy was split out at this step
//
// It used to live in core/handler.go. That whole file moved to apihandler (one step of the
// package split), while this package's pendingmessage.go and three test files still use it.
// **So it is copied with the same name and body**, instead of changing the call sites to use
// userMessageOf: the two functions give different answers when the error has no ": " (this
// one returns fixed text, that one returns the original). Switching would quietly change the
// text stored in failure_reason, and no test would go red.
package core

import "strings"

// userMessage takes the part of an error that is shown to the user.
//
// The core error text convention is "sentinel: Chinese sentence", so take the last segment.
// When nothing can be taken, return a fixed fallback sentence instead of exposing the raw text
// with the sentinel prefix.
func userMessage(err error) string {
	msg := err.Error()
	// Like "core: <not found>: <workspace not found>"; take the last segment
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		if tail := strings.TrimSpace(msg[i+2:]); tail != "" {
			return tail
		}
	}
	return "请求无法完成"
}
