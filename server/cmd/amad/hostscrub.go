package main

import (
	"github.com/chyroc/ama/server/core"
	hostsvc "github.com/chyroc/ama/server/service/host"
	publichost "github.com/chyroc/ama/server/service/host"
)

func newHostResultScrubber(svc *core.Service) hostsvc.ExecResultScrubber {
	return publichost.NewExecResultScrubber(svc)
}

func newHostToolResultScrubber(svc *core.Service) hostsvc.ToolResultScrubber {
	return publichost.NewToolResultScrubber(svc)
}
