// hostTermSink forwards the host domain's upstream terminal frames to core.Service (it owns the
// terminalHub and the pgmq relay, so it knows whether a frame goes straight to this instance or
// to the instance that holds the browser connection).
package main

import (
	"context"

	"github.com/chyroc/ama/server/core"
)

type hostTermSink struct {
	svc *core.Service
}

func (h hostTermSink) DeliverTermFrame(ctx context.Context, terminalID, frameType, data, dataB64 string) error {
	return h.svc.DeliverHostTermFrame(ctx, terminalID, frameType, data, dataB64)
}
