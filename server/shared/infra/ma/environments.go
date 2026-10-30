// Environment: the configuration a Session runs in (network, preinstalled dependencies, env vars).
// API: /api/v3/environments

package ma

import (
	"context"
	"errors"

	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/environment"
)

// EnvironmentService creates, reads, updates and deletes environments. Get it via
// Instance.Environments().
type EnvironmentService struct {
	client *arkruntime.Client
}

// EnvironmentSpec is the convenience input for creating an Environment.
//
// Use CreateRaw when you need preinstalled dependencies, env vars, a self-hosted Worker, etc.
type EnvironmentSpec struct {
	// Name is the environment name. Required.
	Name string
	// Type is the environment type: ma.EnvCloud (Ark-hosted sandbox) or ma.EnvSelfHosted
	// (self-hosted Worker). Defaults to ma.EnvCloud when empty.
	Type string
	// Networking is the network policy: ma.NetworkUnrestricted or ma.NetworkLimited.
	// Defaults to ma.NetworkUnrestricted (all outbound allowed) when empty.
	//
	// **Only valid when Type == ma.EnvCloud.** For self_hosted environments, outbound traffic is
	// managed by your own runtime; Ark does not accept this field and returns 400 if it is sent.
	// So this field is ignored when Type is EnvSelfHosted.
	Networking string
	// Description is the environment description.
	Description string
}

// toRequest turns the convenience input into the SDK request body.
func (s EnvironmentSpec) toRequest() *environment.CreateEnvironmentRequest {
	envType := s.Type
	if envType == "" {
		envType = EnvCloud
	}

	cfg := environment.EnvConfig{
		Type: environment.EnvConfigType(envType),
	}

	// self_hosted does not accept networking: outbound traffic is managed by your own runtime,
	// and Ark rejects it if sent (400 InvalidParameter: config.networking: not allowed when
	// config.type is "self_hosted"). So it is only set for cloud, and a default must not be set
	// unconditionally; otherwise EnvSelfHosted could never be created through EnvironmentSpec.
	if envType == EnvCloud {
		netType := s.Networking
		if netType == "" {
			netType = NetworkUnrestricted
		}
		cfg.Networking = environment.NewOptNetworkingConfig(environment.NetworkingConfig{
			Type: environment.NetworkingType(netType),
		})
	}

	return &environment.CreateEnvironmentRequest{
		Name:        s.Name,
		Config:      environment.NewOptEnvConfig(cfg),
		Description: optEnvString(s.Description),
	}
}

// Create creates an environment.
//
//	env, err := ins.Environments().Create(ctx, ma.EnvironmentSpec{Name: "default"})
func (s *EnvironmentService) Create(
	ctx context.Context,
	spec EnvironmentSpec,
) (*environment.Environment, error) {
	if spec.Name == "" {
		return nil, errors.New("ma: EnvironmentSpec.Name 必填")
	}
	return s.CreateRaw(ctx, spec.toRequest())
}

// CreateRaw creates an environment using the SDK's own request body, for fields that
// EnvironmentSpec does not cover (preinstalled dependencies, env vars, metadata, etc.).
func (s *EnvironmentService) CreateRaw(
	ctx context.Context,
	req *environment.CreateEnvironmentRequest,
) (*environment.Environment, error) {
	return s.client.CreateEnvironment(ctx, req)
}

// Get returns environment details by ID.
func (s *EnvironmentService) Get(
	ctx context.Context,
	environmentID string,
) (*environment.Environment, error) {
	return s.client.GetEnvironment(ctx, environmentID)
}

// List lists environments, page by page.
//
// Note: the environment list only supports Limit / Page; the time filter fields in Page have no
// effect on this API.
func (s *EnvironmentService) List(
	ctx context.Context,
	page Page,
) (*environment.ListEnvironmentsResponse, error) {
	return s.ListRaw(ctx, &environment.EnvironmentsListParams{
		Limit: optEnvInt32(page.Limit),
		Page:  optEnvString(page.Page),
	})
}

// ListRaw paginates using the SDK's own query parameters.
func (s *EnvironmentService) ListRaw(
	ctx context.Context,
	params *environment.EnvironmentsListParams,
) (*environment.ListEnvironmentsResponse, error) {
	return s.client.ListEnvironments(ctx, params)
}

// Update updates the environment's name and description. Empty fields keep their current value.
//
// Use UpdateRaw to change config (network, dependencies, etc.).
func (s *EnvironmentService) Update(
	ctx context.Context,
	environmentID, name, description string,
) (*environment.Environment, error) {
	return s.UpdateRaw(ctx, environmentID, &environment.UpdateEnvironmentRequest{
		Name:        optEnvString(name),
		Description: optEnvString(description),
	})
}

// UpdateRaw updates an environment using the SDK's own request body.
func (s *EnvironmentService) UpdateRaw(
	ctx context.Context,
	environmentID string,
	req *environment.UpdateEnvironmentRequest,
) (*environment.Environment, error) {
	return s.client.UpdateEnvironment(ctx, environmentID, req)
}

// Delete deletes an environment.
func (s *EnvironmentService) Delete(
	ctx context.Context,
	environmentID string,
) (*environment.DeleteEnvironmentResponse, error) {
	return s.client.DeleteEnvironment(ctx, environmentID)
}
