// Package requestid gives each HTTP request a request id, puts it in ctx, writes it back in the
// response header, and passes it on to downstream services.
//
// # It turns "a user reported an id" into something you can look up
//
//  1. **amad's own logs carry it**: a slog handler wrapped by xlog.NewHandler takes it from ctx
//     and fills the `request_id` field, so one search by it returns every log line of that request;
//  2. **the response header returns it to the caller**: when the frontend reports a problem
//     (issue feedback) it attaches this id, so nobody has to describe "roughly what time";
//  3. **it is passed downstream**: an outbound transport can put it in a request header of the same name.
//
// It lives in one package so that there is **only one copy** of the literal: separate copies for
// inbound and outbound drift sooner or later,
// and after drifting each side is "correct" on its own; they just no longer link up.
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Header is the request/response header name for the request id.
//
// It uses the de facto standard `X-Request-Id` (nginx and most gateways recognize it), so when a
// reverse proxy sits in front,
// we can reuse the id it generated.
const Header = "X-Request-Id"

type ctxKey struct{}

// With puts id into ctx. An empty string returns ctx unchanged (no "empty id" is stored).
func With(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// From gets the request id from ctx. Returns ("", false) when there is none.
func From(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok && id != ""
}

// New generates a new request id: a 14-digit time prefix (UTC, to the second) + 16 random hex digits.
//
// The time prefix lets someone estimate when the request happened from the id alone; the random part
// keeps concurrent ids from colliding.
// It does not depend on any central id issuer: with many instances, each one generates its own.
func New() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102150405") + hex.EncodeToString(b[:])
}
