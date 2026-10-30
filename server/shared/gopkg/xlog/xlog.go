// Package xlog is the repo-wide ctx logging entry point, built on the standard library log/slog.
//
// # Why not use slog.InfoContext directly
//
// Logs must be able to "link a whole call chain with one session id". xlog.With puts KVs into the
// context, and every log line written with that ctx afterwards carries these fields, so one
// search by ma_session_id / ama_run_id returns all related logs.
//
// This relies on NewHandler: it wraps the real slog handler and, before each record is emitted,
// adds the KVs in ctx and the request id (shared/gopkg/requestid). **So slog's *Context calls also
// carry these fields**, provided cmd/amad wrapped the global logger with NewHandler.
//
// # Usage
//
//	// 1) At the entry of a call chain, put entity ids into ctx; the whole chain carries them:
//	ctx = xlog.WithRun(ctx, runID)
//	ctx = xlog.With(ctx, xlog.KMEnvironmentID, envID, xlog.KWorkerID, workerID)
//
//	// 2) Write logs in the simple printf style:
//	xlog.Info(ctx, "worker starts polling, concurrency=%d", concurrency)
//	xlog.Warn(ctx, "poll work failed: %v", err)
//
// # Redaction
//
// xlog itself does not depend on a specific redaction implementation (keeps gopkg pure). At
// startup cmd/amad attaches shared/infra/secret.Redact with SetRedactor; every message and every
// string KV value passes through it before being emitted. When none is attached (local tests),
// output is unchanged.
package xlog

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/chyroc/ama/server/shared/gopkg/requestid"
)

// LevelNotice is a level between info and warn, for important lifecycle events.
const LevelNotice = slog.Level(2)

// redactor is injected by the wiring layer (secret.Redact in production). By default it returns input unchanged.
var redactor = func(s string) string { return s }

// SetRedactor attaches the shared redaction function. Call it only once at process startup (cmd/amad).
func SetRedactor(r func(string) string) {
	if r != nil {
		redactor = r
	}
}

// Debug writes a debug-level log line (printf style).
func Debug(ctx context.Context, format string, args ...any) {
	emit(ctx, slog.LevelDebug, format, args...)
}

// Info writes an info-level log line.
func Info(ctx context.Context, format string, args ...any) {
	emit(ctx, slog.LevelInfo, format, args...)
}

// Notice writes a notice-level log line (more visible than info, for important lifecycle events).
func Notice(ctx context.Context, format string, args ...any) {
	emit(ctx, LevelNotice, format, args...)
}

// Warn writes a warn-level log line.
func Warn(ctx context.Context, format string, args ...any) {
	emit(ctx, slog.LevelWarn, format, args...)
}

// Error writes an error-level log line.
func Error(ctx context.Context, format string, args ...any) {
	emit(ctx, slog.LevelError, format, args...)
}

// emit sends one record through the global slog logger, recording the call site as xlog's caller
// rather than this file.
func emit(ctx context.Context, level slog.Level, format string, args ...any) {
	if ctx == nil {
		ctx = context.Background()
	}
	l := slog.Default()
	if !l.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:]) // skip Callers / emit / the Info layer
	r := slog.NewRecord(time.Now(), level, render(format, args...), pcs[0])
	_ = l.Handler().Handle(ctx, r)
}

// render renders and redacts the message text.
//
// It first Sprintfs into the final string and then redacts the whole thing, rather than redacting
// each arg: redaction recognizes "key name + value" and shapes like JWT/Bearer, which only show up
// in the fully assembled text.
func render(format string, args ...any) string {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return redactor(msg)
}

type kvKey struct{}

// With puts several "key, value" pairs into ctx and returns the new ctx. Logs written with it
// afterwards carry these fields.
//
// Arguments must come in pairs; keys with an empty string value are skipped (so logs are not full
// of ama_run_id=). For duplicate keys, the later one overrides the earlier one.
func With(ctx context.Context, kvs ...any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	cleaned := make([]slog.Attr, 0, len(kvs)/2)
	for i := 0; i+1 < len(kvs); i += 2 {
		key, _ := kvs[i].(string)
		if key == "" {
			continue
		}
		val := kvs[i+1]
		if s, ok := val.(string); ok {
			if s == "" {
				continue
			}
			val = redactorKV(key, s)
		}
		cleaned = append(cleaned, slog.Any(key, val))
	}
	if len(cleaned) == 0 {
		return ctx
	}
	prev := attrsFrom(ctx)
	merged := make([]slog.Attr, 0, len(prev)+len(cleaned))
	for _, a := range prev {
		if !hasKey(cleaned, a.Key) {
			merged = append(merged, a)
		}
	}
	merged = append(merged, cleaned...)
	return context.WithValue(ctx, kvKey{}, merged)
}

func attrsFrom(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(kvKey{}).([]slog.Attr)
	return attrs
}

func hasKey(attrs []slog.Attr, key string) bool {
	for _, a := range attrs {
		if a.Key == key {
			return true
		}
	}
	return false
}

// redactorKV redacts a string KV value by "key name + value" (credential-like keys get their value wiped).
func redactorKV(key, val string) any {
	// Reuse the message redactor's verdict on "key=val": if it was rewritten, it matched a sensitive shape.
	masked := redactor(key + "=" + val)
	if masked == key+"="+val {
		return val // no match, original value
	}
	return masked
}

// NewHandler wraps a slog.Handler: before each record is emitted it adds the KVs in ctx and the request id.
func NewHandler(next slog.Handler) slog.Handler { return ctxHandler{next: next} }

type ctxHandler struct{ next slog.Handler }

func (h ctxHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if id, ok := requestid.From(ctx); ok {
			r.AddAttrs(slog.String("request_id", id))
		}
		if attrs := attrsFrom(ctx); len(attrs) > 0 {
			r.AddAttrs(attrs...)
		}
	}
	return h.next.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ctxHandler{next: h.next.WithAttrs(attrs)}
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	return ctxHandler{next: h.next.WithGroup(name)}
}

// -- Named shortcuts for frequently used entities (empty values are skipped) --

// WithWorkspace attaches the workspace id.
func WithWorkspace(ctx context.Context, id string) context.Context {
	return With(ctx, KWorkspaceID, id)
}

// WithAgent attaches the AMA agent id.
func WithAgent(ctx context.Context, id string) context.Context {
	return With(ctx, KAgentID, id)
}

// WithRun attaches the run id (sesn-xxx), one of the two most important keys.
func WithRun(ctx context.Context, id string) context.Context {
	return With(ctx, KRunID, id)
}

// WithThread attaches the AMA thread id.
func WithThread(ctx context.Context, id string) context.Context {
	return With(ctx, KThreadID, id)
}

// WithSkill attaches the AMA skill id.
func WithSkill(ctx context.Context, id string) context.Context {
	return With(ctx, KSkillID, id)
}

// WithSandboxSession attaches the session identifier of the execution environment container.
func WithSandboxSession(ctx context.Context, id string) context.Context {
	return With(ctx, KSandboxSessionID, id)
}

// WithWorker attaches the worker instance id.
func WithWorker(ctx context.Context, id string) context.Context {
	return With(ctx, KWorkerID, id)
}

// WithEnvironment attaches the Ark self_hosted environment id.
func WithEnvironment(ctx context.Context, id string) context.Context {
	return With(ctx, KMEnvironmentID, id)
}

// WithMAgent attaches the Ark agent id.
func WithMAgent(ctx context.Context, id string) context.Context {
	return With(ctx, KMAgentID, id)
}

// WithMSession attaches the Ark session id, one of the two most important ma keys.
func WithMSession(ctx context.Context, id string) context.Context {
	return With(ctx, KMSessionID, id)
}

// WithWork attaches the Ark work queue item id.
func WithWork(ctx context.Context, id string) context.Context {
	return With(ctx, KWorkID, id)
}

// WithTool attaches the tool name and one tool call id (either is skipped if empty).
func WithTool(ctx context.Context, name, toolUseID string) context.Context {
	return With(ctx, KToolName, name, KToolUseID, toolUseID)
}
