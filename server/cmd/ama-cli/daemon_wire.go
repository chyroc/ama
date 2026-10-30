// The daemon's transport abstraction: two implementations, HTTP long polling and WebSocket
// (daemon_wire_http.go / daemon_wire_ws.go); the supervisor (daemon_supervisor.go) decides which one
// to use and when to switch.
//
// A transport only does I/O: downstream it puts batches into the inbound channel (the single worker
// does the execution), upstream it synchronously submits one ResultsRequest batch. Execution, the
// three buffers and the cursor all live on daemonRuntime, so the transport can be swapped at any
// time without affecting a 120s exec in progress.
package main

import (
	"context"
	"encoding/json"

	"github.com/chyroc/ama/server/shared/hostproto"
)

const (
	transportNameWS   = "ws"
	transportNameHTTP = "http"

	// inboundQueueBatches is the queue limit for downstream batches (the unit is "batches", at most
	// 64 calls per batch). The worker runs them serially, and the cursor only advances after a batch
	// is done; when full, the transport's read loop blocks, the server's write timeout disconnects,
	// and unacknowledged rows heal themselves via lease redelivery.
	inboundQueueBatches = 8
)

// inboundBatch is one downstream batch the transport hands to the worker. Host revocation does not
// go here: after the transport receives a revoked frame/close 4001 it makes serve return
// errDaemonUnauthorized directly.
type inboundBatch struct {
	calls []hostproto.Dispatch
}

// transport is one channel of transport between the daemon and amad.
type transport interface {
	name() string
	// serve runs until ctx is cancelled or the connection ends; downstream batches are delivered to in.
	// When it returns errDaemonUnauthorized the supervisor takes the wipe-credentials exit; other
	// errors are handled as reconnect/fallback.
	serve(ctx context.Context, in chan<- inboundBatch) error
	// submitResults sends one batch of results synchronously: HTTP = POST and wait for 2xx; WS = send a frame and wait for results_ack.
	submitResults(ctx context.Context, req hostproto.ResultsRequest) error
	close() error
}

// workerLoop is the only dispatch executor: take batches serially → execute → advance the cursor and
// persist it.
//
// A single worker is the key: during a switch window both transports may each deliver a batch, and a
// single consumer guarantees exec stays serial (the serial semantics of agent tools) and the cursor is
// never advanced concurrently.
func (rt *daemonRuntime) workerLoop(ctx context.Context, in <-chan inboundBatch) {
	for {
		select {
		case <-ctx.Done():
			return
		case batch := <-in:
			rt.handleBatch(ctx, batch.calls)
		}
	}
}

// splitResults splits the three buffers into several ResultsRequests by a JSON byte budget (only
// cutting between entries; a single result, however big, gets a block to itself; the 4MiB per-result
// limit leaves room under the server's 8MiB read limit). Order is kept: exec before frames/acks; on
// retransmission the server's CAS/idempotency covers ordering differences.
func splitResults(
	exec []hostproto.ExecResultItem, tools []hostproto.ToolResultItem,
	frames []hostproto.TermFrame, acks []int64, maxBytes int,
) []hostproto.ResultsRequest {
	var chunks []hostproto.ResultsRequest

	cur := hostproto.ResultsRequest{}
	curBytes := 0
	flush := func() {
		if len(cur.ExecResults) == 0 && len(cur.ToolResults) == 0 &&
			len(cur.TermFrames) == 0 && len(cur.AckSeqs) == 0 {
			return
		}
		chunks = append(chunks, cur)
		cur = hostproto.ResultsRequest{}
		curBytes = 0
	}
	addItem := func(size int) {
		if len(cur.ExecResults) > 0 || len(cur.ToolResults) > 0 {
			if curBytes+size > maxBytes {
				flush()
			}
		}
		curBytes += size
	}

	for _, item := range exec {
		b, _ := json.Marshal(item)
		addItem(len(b))
		cur.ExecResults = append(cur.ExecResults, item)
	}
	for _, item := range tools {
		b, _ := json.Marshal(item)
		addItem(len(b))
		cur.ToolResults = append(cur.ToolResults, item)
	}
	for _, f := range frames {
		b, _ := json.Marshal(f)
		if curBytes+len(b) > maxBytes {
			flush()
		}
		cur.TermFrames = append(cur.TermFrames, f)
		curBytes += len(b)
	}
	// acks is []int64, tiny, attached as a whole to the last block; it must be sent even as a block on its own.
	if len(acks) > 0 {
		b, _ := json.Marshal(acks)
		if curBytes+len(b) > maxBytes {
			flush()
		}
		cur.AckSeqs = append(cur.AckSeqs, acks...)
	}
	flush()
	return chunks
}
