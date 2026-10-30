// The real implementation of ArkVerifier.
//
// # Ark has no "test this key" endpoint
//
// The auth sections of the 60 API reference pages describe only one method:
// `Authorization: Bearer $ARK_API_KEY`. There is no endpoint to "validate this key". So the
// implementation calls one **read-only** endpoint with the key (list Agents, limit=1) and
// judges by the status code: 401/403 → not usable; any other success → usable.
//
// # WithRegion has no effect for BytePlus
//
// The frontend supports two regions: Volcano cn-beijing and BytePlus ap-southeast-1. But the
// SDK's arkruntime.WithRegion only changes the region field; it does **not** switch the base
// URL to the BytePlus domain. ma.New goes through NewClientWithApiKey, which hardcodes
// Volcano's volces.com. So verifying a valid BytePlus key would hit the Volcano endpoint,
// get a 401, and be wrongly judged "invalid key".
//
// So here the base URL is given explicitly per region (via ma.WithBaseURL). The URLs come
// from ARK_REGIONS in the frontend's web/src/types/domain.ts; the two sides must match.

package core

import (
	"context"
	"fmt"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	arkcredsvc "github.com/chyroc/ama/server/service/arkcred"
	"github.com/chyroc/ama/server/service/arkrun"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// The API root URLs per region have moved to service/arkrun (the real home of BaseURL).

// arkVerifyTimeout is the timeout for a single verification.
//
// It is required: this is a synchronous action the user is waiting on after clicking a
// button. If Ark hangs, the handler must not hang with it and leave the frontend spinner
// spinning forever.
const arkVerifyTimeout = 8 * time.Second

// LiveArkVerifier is a verifier that calls the real Ark.
type LiveArkVerifier struct {
	// BaseURLOverride, when non-empty, is used directly, ignoring the region. Only a hook for integration testing.
	BaseURLOverride string
}

// NewLiveArkVerifier builds a verifier that calls the real Ark.
func NewLiveArkVerifier() *LiveArkVerifier { return &LiveArkVerifier{} }

// Verify lists Agents once with the key and returns a structured three-level result.
//
// Ark has no "test this key" endpoint, so it uses one **read-only** call
// (GET /api/v3/agents?limit=1) that creates no resources and has no side effects:
//
//   - 401/403                          → InvalidKey (the key is invalid or was deleted);
//   - 404 + code=ManagedAgentNotOpen   → NotActivated (the key is valid but the account has
//     not enabled Managed Agents); the account id is pulled from the raw text;
//   - everything else (timeout, 5xx, 429, unrecognized errors, including failure to build
//     the client) → Skipped, with the technical reason in Cause, **logged only**.
//
// ⚠️ **Do not judge "not enabled" from 404 alone**: the same 404 with an empty code also
// means "resource not found" (ma.IsNotFound). The code is the contract; see the comment on
// ma.IsManagedAgentNotOpen.
//
// ⚠️ **Skipped is deliberately broad**: if the probe misses NotActivated, the worst case is
// just one missing "not enabled" warning. A valid key must never be misjudged as bad and
// blocked from being saved.
func (v *LiveArkVerifier) Verify(ctx context.Context, apiKey string, region apimodel.ArkRegion) arkcredsvc.VerifyResult {
	baseURL := v.BaseURLOverride
	if baseURL == "" {
		var err error
		if baseURL, err = arkBaseURL(region); err != nil {
			// Unknown region: this is not Ark's answer, so count it as not verified (normal callers already filter with validRegions).
			return arkcredsvc.VerifyResult{Status: arkcredsvc.VerifyStatusSkipped, Cause: err}
		}
	}

	// Set both the SDK's per-request timeout and a context timeout: the former covers
	// connecting and read/write; the latter caps any retries that pile up inside the SDK.
	ins, err := ma.New(apiKey, ma.WithBaseURL(baseURL), ma.WithTimeout(arkVerifyTimeout))
	if err != nil {
		return arkcredsvc.VerifyResult{
			Status: arkcredsvc.VerifyStatusSkipped,
			Cause:  fmt.Errorf("构造方舟客户端失败: %w", err),
		}
	}

	ctx, cancel := context.WithTimeout(ctx, arkVerifyTimeout)
	defer cancel()

	// Listing one is enough; we only want to see whether auth passes and the account is enabled.
	if _, err := ins.Agents().List(ctx, ma.Page{Limit: 1}); err != nil {
		switch {
		case ma.IsAuthError(err):
			return arkcredsvc.VerifyResult{
				Status:    arkcredsvc.VerifyStatusInvalidKey,
				RequestID: ma.RequestID(err),
			}
		case ma.IsManagedAgentNotOpen(err):
			return arkcredsvc.VerifyResult{
				Status:    arkcredsvc.VerifyStatusNotActivated,
				AccountID: ma.NotOpenAccountID(err),
				RequestID: ma.RequestID(err),
			}
		default:
			return arkcredsvc.VerifyResult{Status: arkcredsvc.VerifyStatusSkipped, Cause: err}
		}
	}
	return arkcredsvc.VerifyResult{Status: arkcredsvc.VerifyStatusOK}
}

// arkBaseURL gives the API root URL for a region.
//
// ⚠️ **The real definition is service/arkrun.BaseURL; this only forwards to it.** The two
// places (verifying a key, actually starting a chat) must share one region map. If each
// kept its own copy, forgetting one side when adding a region would show up as "key
// verification says this region works, but starting a chat says unknown region", with
// zero errors.
func arkBaseURL(region apimodel.ArkRegion) (string, error) {
	return arkrun.BaseURL(region)
}

// Let the compiler confirm the interface is implemented.
var _ ArkVerifier = (*LiveArkVerifier)(nil)
