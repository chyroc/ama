package main

import (
	"context"

	"github.com/chyroc/ama/server/core"
	publichost "github.com/chyroc/ama/server/service/host"
)

type hostTermSink struct{ svc *core.Service }

func (h hostTermSink) DeliverTermFrame(ctx context.Context, terminalID, frameType, data, dataB64 string) error {
	return publichost.NewTerminalSink(h.svc).DeliverTermFrame(ctx, terminalID, frameType, data, dataB64)
}
