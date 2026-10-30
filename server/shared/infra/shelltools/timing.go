// Logs one duration line for each tool call.
//
// # Why it is needed
//
// "Tool execution is slow" used to be something we could only work out afterwards by digging
// through the database: there was not a single timing point on the worker path, and the only
// number available was the **wall-clock difference** the frontend computed by subtracting the
// `processed_at` of the `tool_use` and `tool_result` events. That includes queueing on the Ark
// side and the ten-plus seconds of resuming the container from pause, so "how slow is the
// command itself" could not be computed at all (the comment in `web/src/lib/eventFacets.ts`
// already admits this).
//
// The decorator sits on the tools in `toolset.Set`, so it measures the time "from the SDK handing
// this call to us until we hand the result back", which naturally excludes Ark queueing. Together
// with the container build/resume/rebuild durations in pause.go (linked by the same ark_session),
// it answers the real question: **of these 25 seconds, how much is waiting for the container to
// wake up, and how much is the command running.**
//
// # Two lines not to cross
//
//  1. **`Name()` must be forwarded as-is.** `Set.Register` uses `tool.Name()` as the map key, and
//     `Set.Execute` looks it up again by name (SDK toolset/types.go). If the name changes, when
//     Ark sends `bash` it gets "unknown tool: bash", which is treated as a tool execution failure
//     and is hard to trace.
//
//  2. **Never wrap idleHook.** `Set.Close()` decides whether to call Close through the type
//     assertion `tool.(ClosableTool)` (SDK toolset/types.go), and the decorator **hides** the
//     wrapped object's Close: once wrapped, the assertion no longer holds, idleHook.Close is never
//     called, and so **the container never pauses again**. That failure has zero symptoms: no
//     compile error, no returned error, just one missing log line, while the container burns
//     until TTL. So provider.go only wraps the six real tools (none of them has Close), and the
//     sentinel is Registered as-is. `TestTimedToolsDoNotBreakIdlePause` guards this.
package shelltools

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// slowToolThreshold is the line where "this call is worth a higher log level".
//
// Above it we log Warn instead of Info: in a normal conversation there are several Info lines
// per round, and the really slow call would be buried. 10 seconds was chosen because, measured,
// once the container is warm the P50 of calls in the same run is only 1.2 seconds, while
// building/resuming a container takes ten to twenty-some seconds. 10 seconds roughly separates
// "the container is waking up" from "the command really is slow", without flooding the log with
// normal commands that are slow anyway, like installing dependencies or running builds.
const slowToolThreshold = 10 * time.Second

// timedTool wraps a tool only to log one duration around its execution.
//
// It deliberately **does not implement Close**: none of the six wrapped tools has Close, and an
// extra one on the decorator would make `Set.Close()` call it once for each of them (see point 2
// in the file header).
type timedTool struct {
	inner toolset.Tool
	log   *slog.Logger
}

// newTimedTool wraps a tool with timing. When logger is nil it falls back to slog.Default().
func newTimedTool(inner toolset.Tool, logger *slog.Logger) toolset.Tool {
	if logger == nil {
		logger = slog.Default()
	}
	return &timedTool{inner: inner, log: logger}
}

// Name is forwarded as-is. **Do not change it**; see point 1 in the file header.
func (t *timedTool) Name() string { return t.inner.Name() }

// Execute runs the wrapped tool and logs one duration line.
//
// "Did it time out" is judged by ctx: the SDK wraps each call with
// `context.WithTimeout(ctx, ToolTimeout)` (120 seconds by default), and on timeout it returns
// its own "tool execution timed out" error result. That result has no duration and does not tell
// "the command ran too long" from "the HTTP layer broke first", so we judge it here ourselves.
func (t *timedTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	start := time.Now()
	res := t.inner.Execute(ctx, input)
	took := time.Since(start).Round(time.Millisecond)

	// Also detect "failed to fetch Feishu history" while we are here (see larkdegrade.go).
	//
	// Riding along here instead of adding a separate decorator: this spot already has all three
	// things needed, (name, input, result); one more wrapper would only add one more way to fail
	// ("forgot to wrap it"). It is purely on the side: it only looks and changes nothing, does not
	// touch res, and does not affect any return value.
	logLarkHistoryFailure(t.log, t.inner.Name(), input, res)

	// ctx already expired → this call was cut off by the timeout, not a normal return.
	// Use ctx.Err() instead of parsing the result text: the text is built separately by the SDK and
	// by us, and it changes.
	timedOut := ctx.Err() != nil

	attrs := []any{
		slog.String("tool", t.inner.Name()),
		slog.Duration("took", took),
		slog.Bool("失败", res.IsError),
	}
	if timedOut {
		attrs = append(attrs, slog.Bool("超时", true), slog.Any("ctx_err", ctx.Err()))
		t.log.Warn("工具执行被超时中断", attrs...)
		return res
	}
	if took >= slowToolThreshold {
		t.log.Warn("工具执行偏慢", attrs...)
		return res
	}
	t.log.Info("工具执行完成", attrs...)
	return res
}
