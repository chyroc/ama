// Passthrough service for the raw HTTP API of Ark Managed Agents.
//
// # What this layer does
//
// AMA itself only wraps Ark's main chat path (create Agent / create session / send and
// receive events). Ark also has a whole group of resource APIs that AMA has no model of its
// own for: session resources (attaching files), session threads, Files upload, Vaults and
// credentials, Memory Stores and memory entries, session upgrade. AMA passes these APIs
// through **unchanged**: the request body is sent byte for byte, the response body is
// returned as read, and field names, nesting and pagination wrappers all match Ark
// (snake_case, data/next_page, etc.), so callers can use the Ark docs directly.
//
// The reason for not mapping them again is the same as for the Raw column of RunEvent: one
// more unmarshal → marshal and fields newly added by Ark are silently dropped, and that kind
// of drift raises no error at all.
//
// # Credentials are resolved by resource ownership, in two scopes
//
// No Ark key appears in the passthrough API itself. Each AMA route works out which key to
// use from the AMA resource it hangs off:
//
//	run       /api/runs/{runId}/ark/...       three-level lookup for the workspace/agent that owns this session
//	workspace /api/workspaces/{id}/ark/...    workspace-level/global lookup for that workspace
//
// "File upload" hangs off run because files in the Files API belong to a cloud account, and
// a file attached to a session must be in **the same account** as that session. Resolving
// by run guarantees this, so files are never uploaded into someone else's account.
//
// ⚠️ **These are paid credentials**: write operations through the passthrough really use Ark
// quota (creating Vaults, writing memories, uploading files), so route permission levels
// must be chosen with care (see the comment before each group in authz/matrix.go).

package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/shared/infra/ma"
)

// ArkProxyDoForRun forwards one normal (non-streaming) Ark request with the credentials of a given session.
func (s *Service) ArkProxyDoForRun(
	ctx context.Context, runID string, req ma.RawRequest,
) (*ma.RawResponse, error) {
	return s.arkProxyDo(ctx, req, func() (ArkBindingKey, error) {
		run, err := s.store.GetRun(ctx, strings.TrimSpace(runID))
		if err != nil {
			return ArkBindingKey{}, err
		}
		return s.arkKey(ctx, run.WorkspaceID, run.AgentID)
	})
}

// ArkProxyStreamForRun forwards one SSE streaming request with the credentials of a given session.
func (s *Service) ArkProxyStreamForRun(
	ctx context.Context, runID string, req ma.RawRequest,
) (*ma.RawStream, error) {
	return s.arkProxyStream(ctx, req, func() (ArkBindingKey, error) {
		run, err := s.store.GetRun(ctx, strings.TrimSpace(runID))
		if err != nil {
			return ArkBindingKey{}, err
		}
		return s.arkKey(ctx, run.WorkspaceID, run.AgentID)
	})
}

// ArkProxyDoForWorkspace forwards one normal Ark request with the credentials of a given
// workspace (the workspace-level key; if not set, it falls back to the global key that the
// workspace refers to).
func (s *Service) ArkProxyDoForWorkspace(
	ctx context.Context, workspaceID string, req ma.RawRequest,
) (*ma.RawResponse, error) {
	return s.arkProxyDo(ctx, req, func() (ArkBindingKey, error) {
		return s.arkKey(ctx, strings.TrimSpace(workspaceID), "")
	})
}

// ArkProxyStreamForWorkspace forwards one SSE streaming request with the credentials of a given workspace.
func (s *Service) ArkProxyStreamForWorkspace(
	ctx context.Context, workspaceID string, req ma.RawRequest,
) (*ma.RawStream, error) {
	return s.arkProxyStream(ctx, req, func() (ArkBindingKey, error) {
		return s.arkKey(ctx, strings.TrimSpace(workspaceID), "")
	})
}

func (s *Service) arkProxyDo(
	ctx context.Context, req ma.RawRequest, resolve func() (ArkBindingKey, error),
) (*ma.RawResponse, error) {
	if s.runner == nil {
		return nil, fmt.Errorf("%w: 服务端未接入方舟", ErrPrecondition)
	}
	key, err := resolve()
	if err != nil {
		return nil, err
	}
	return s.runner.RawDo(ctx, key, req)
}

func (s *Service) arkProxyStream(
	ctx context.Context, req ma.RawRequest, resolve func() (ArkBindingKey, error),
) (*ma.RawStream, error) {
	if s.runner == nil {
		return nil, fmt.Errorf("%w: 服务端未接入方舟", ErrPrecondition)
	}
	key, err := resolve()
	if err != nil {
		return nil, err
	}
	return s.runner.RawStream(ctx, key, req)
}
