// Look up the state of a single work item on the Ark side.
//
// The SDK (selfhosted.API) only exposes the five endpoints a worker needs to do its job
// (Poll/Ack/Heartbeat/Stop/GetSession). It does **not** expose the "get a single work"
// endpoint from the docs (GET /environments/:id/work/:work_id; see the "get a single work"
// page of the self-hosted worker section under docs/ark-managed-agents/api/02-Environments/).
// The rescue sweep (runrescue.go) must know whether a work is really stopped or is still
// being run normally by some worker: sending a tool_result only because "it is quiet on our
// side" can collide with a long tool that is still running, giving two results for the same
// call. So this file uses a minimal GET that does not depend on the SDK.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// arkWorkState only takes the fields the sweep needs; everything else is ignored.
type arkWorkState struct {
	State string `json:"state"`
}

// arkWork state values, matching the Ark docs page "long-poll for work".
const (
	arkWorkStateQueued   = "queued"
	arkWorkStateStarting = "starting"
	arkWorkStateActive   = "active"
	arkWorkStateStopping = "stopping"
	arkWorkStateStopped  = "stopped"
)

// arkWorkGetTimeout is the limit for a single lookup. It runs in the sweep's ticker
// goroutine, so getting stuck only delays this round of rescue a little; 15 seconds is enough.
const arkWorkGetTimeout = 15 * time.Second

// fetchArkWorkState looks up the current state of a work item on the Ark side.
//
// 404 returns ("", false, nil): the environment or work no longer exists on the Ark side
// (resources were rebuilt after a credential change, etc.). The caller should **skip the
// rescue** and not treat it as stopped: sending a result for something we could not find
// may send it to a resource that does not belong to this key.
func fetchArkWorkState(
	ctx context.Context, baseURL, apiKey, environmentID, workID string,
) (state string, found bool, err error) {
	u := strings.TrimRight(baseURL, "/") +
		"/environments/" + environmentID + "/work/" + workID

	reqCtx, cancel := context.WithTimeout(ctx, arkWorkGetTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode/100 != 2 {
		return "", false, fmt.Errorf("查 work 状态返回 %d: %s",
			resp.StatusCode, truncateForLog(string(body), 300))
	}
	var got arkWorkState
	if err := json.Unmarshal(body, &got); err != nil {
		return "", false, fmt.Errorf("解析 work 状态失败: %w", err)
	}
	return got.State, true, nil
}

// truncateForLog shortens an error response body so a whole HTML page / long JSON does not end up in the log.
func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
