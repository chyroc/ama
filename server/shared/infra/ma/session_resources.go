// Session resources.
// API: /api/v3/sessions/{id}/resources
//
// Resources are input material for the Agent (e.g. uploaded files). Once mounted, the Agent can
// read them inside the sandbox.

package ma

import (
	"context"

	"github.com/volcengine/ark-runtime-go/arkruntime/model/session"
)

// AddResource mounts a resource on a Session.
//
// The resource shape (file ID, mount path, etc.) is defined by the SDK's
// CreateSessionResourceRequest and is passed through as is.
func (s *SessionService) AddResource(
	ctx context.Context,
	sessionID string,
	req *session.CreateSessionResourceRequest,
) (*session.SessionResource, error) {
	return s.client.CreateSessionResource(ctx, sessionID, req)
}

// ListResources lists the resources mounted on a Session. This API is not paginated.
func (s *SessionService) ListResources(
	ctx context.Context,
	sessionID string,
) (*session.ListSessionResourcesResponse, error) {
	return s.client.ListSessionResources(ctx, sessionID)
}
