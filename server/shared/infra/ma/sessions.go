// Session: an Agent instance running in an Environment, used to carry out one concrete task.
// API: /api/v3/sessions
//
// This file only covers the Session's own lifecycle. For sending and receiving events see
// session_events.go and session_stream.go; for mounted resources see session_resources.go; for
// threads see session_threads.go.

package ma

import (
	"context"
	"errors"

	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/session"
)

// SessionService manages the Session lifecycle and event interaction.
// Get it via Instance.Sessions().
type SessionService struct {
	client *arkruntime.Client
	// streamClient is only used for long-lived SSE connections (see session_stream.go). It
	// points to a different http.Client from client: that one has a timeout, this one does not.
	// For why they must be separate, see newStreamHTTPClient.
	streamClient *arkruntime.Client
}

// stream returns the client to open streams with.
//
// The NewWithClient path only has one client, so streamClient is the same one. The nil fallback
// here keeps tests that build SessionService{client: ...} directly from panicking.
func (s *SessionService) stream() *arkruntime.Client {
	if s.streamClient != nil {
		return s.streamClient
	}
	return s.client
}

// SessionSpec is the convenience input for creating a Session.
type SessionSpec struct {
	// AgentID is the ID of the Agent to run. Required.
	AgentID string
	// EnvironmentID is the ID of the environment to run in. Required.
	EnvironmentID string
	// Title is the session title, to make it easy to spot in the console.
	Title string
	// VaultIDs lists the credential vault IDs this session may access.
	VaultIDs []string
}

// toRequest turns the convenience input into the SDK request body.
//
// In the SDK main branch (545df4a, the self-hosted worker commit), EnvironmentID changed from
// string to OptString: the environment is now optional (if not sent, Ark picks the default
// environment). So this goes through optSessionString, and an empty string becomes "field not
// sent" instead of an empty string being sent.
func (s SessionSpec) toRequest() *session.CreateSessionRequest {
	return &session.CreateSessionRequest{
		Agent:         session.NewStringAgentIdentifier(s.AgentID),
		EnvironmentID: optSessionString(s.EnvironmentID),
		Title:         optSessionString(s.Title),
		VaultIds:      s.VaultIDs,
	}
}

// Create creates a Session, binding the Agent to the Environment so it starts working.
//
//	sess, err := ins.Sessions().Create(ctx, ma.SessionSpec{
//	    AgentID:       ag.ID,
//	    EnvironmentID: env.ID,
//	})
func (s *SessionService) Create(ctx context.Context, spec SessionSpec) (*session.Session, error) {
	if spec.AgentID == "" {
		return nil, errors.New("ma: SessionSpec.AgentID 必填")
	}
	if spec.EnvironmentID == "" {
		return nil, errors.New("ma: SessionSpec.EnvironmentID 必填")
	}
	return s.CreateRaw(ctx, spec.toRequest())
}

// CreateRaw creates a Session using the SDK's own request body, for fields that SessionSpec does
// not cover (mounting resources at creation, pinning an agent version, tags, etc.).
func (s *SessionService) CreateRaw(
	ctx context.Context,
	req *session.CreateSessionRequest,
) (*session.Session, error) {
	return s.client.CreateSession(ctx, req)
}

// Get returns Session details by ID. The Status field reflects the current run state.
func (s *SessionService) Get(ctx context.Context, sessionID string) (*session.Session, error) {
	return s.client.GetSession(ctx, sessionID)
}

// List lists Sessions, page by page.
func (s *SessionService) List(ctx context.Context, page Page) (*session.ListSessionsResponse, error) {
	return s.ListRaw(ctx, &session.SessionsListParams{
		Limit:        optSessionInt32(page.Limit),
		Page:         optSessionString(page.Page),
		CreatedAtGte: optSessionString(page.CreatedAtGte),
		CreatedAtLte: optSessionString(page.CreatedAtLte),
	})
}

// ListRaw paginates using the SDK's own query parameters.
func (s *SessionService) ListRaw(
	ctx context.Context,
	params *session.SessionsListParams,
) (*session.ListSessionsResponse, error) {
	return s.client.ListSessions(ctx, params)
}

// Update updates the Session title.
func (s *SessionService) Update(
	ctx context.Context,
	sessionID, title string,
) (*session.Session, error) {
	return s.UpdateRaw(ctx, sessionID, &session.UpdateSessionRequest{
		Title: optSessionString(title),
	})
}

// UpdateRaw updates a Session using the SDK's own request body.
func (s *SessionService) UpdateRaw(
	ctx context.Context,
	sessionID string,
	req *session.UpdateSessionRequest,
) (*session.Session, error) {
	return s.client.UpdateSession(ctx, sessionID, req)
}

// Delete deletes a Session. Its sandbox is released with it.
func (s *SessionService) Delete(
	ctx context.Context,
	sessionID string,
) (*session.DeleteSessionResponse, error) {
	return s.client.DeleteSession(ctx, sessionID)
}
