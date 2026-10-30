// A **pass-through layer** for the poll step backoff: the implementation moved to
// service/arkbackoff, and only aliases stay here.
//
// # Why keep this file instead of changing every reference to arkbackoff.X
//
// Same as how the first eight domains were split out (plan A: core keeps the pass-throughs it
// needs). The step backoff constants and types are referenced about thirty times by core's
// pollLoop and four worker test files. Rewriting all of them with the package name only spreads
// the same diff over more lines and adds no clarity.
//
// Everything left here is a **type alias / the same value** (`=`, not a new type), so:
//
//   - `backoffLadder` and `arkbackoff.Ladder` **are the same type** and can be passed to each other;
//   - `workerBackoffLevels` and `arkbackoff.Levels` share the same underlying array, so
//     "two ladders that disagree with each other" cannot happen.
//
// ⚠️ **Do not build a second implementation here.** This file used to hold 140 lines of real
// logic and now holds none. The step values, the jitter ratio, and the reasoning about the silent
// bug "you must Reset when you get work" all live in the file header of
// service/arkbackoff/backoff.go. Change behavior over there.
package core

import (
	"context"
	"time"

	"github.com/chyroc/ama/server/service/arkbackoff"
)

// The four step backoff constants, passed through as is. Reasoning is in the file header of
// service/arkbackoff/backoff.go.
const (
	workerBackoffStep   = arkbackoff.Step
	workerBackoffMax    = arkbackoff.Max
	workerBackoffJitter = arkbackoff.JitterRatio
)

// workerBackoffLevels is the idle backoff ladder. It is **the same underlying array** as
// arkbackoff.Levels.
//
// ⚠️ Read only. Changing it changes the poll pace of every environment at once, and nothing
// would report an error about it.
var workerBackoffLevels = arkbackoff.Levels

// backoffLadder is an alias of arkbackoff.Ladder: **the same type**, not a wrapper.
type backoffLadder = arkbackoff.Ladder

// jitterAround adds ±20% jitter to an interval. Passes through to arkbackoff.Jitter.
func jitterAround(d time.Duration) time.Duration { return arkbackoff.Jitter(d) }

// arkSleep waits for a while, and returns false early when ctx is canceled.
//
// **There is only one implementation** (arkbackoff.Sleep): with two ctx-aware sleeps, one of
// them will sooner or later miss ctx.Done(), and that bug shows up as "the service will not shut
// down", which does not point at sleep at all.
func arkSleep(ctx context.Context, d time.Duration) bool { return arkbackoff.Sleep(ctx, d) }
