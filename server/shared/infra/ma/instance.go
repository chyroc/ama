// Package ma wraps Volcengine Ark Managed Agents. It adds a user-facing layer of organization on
// top of the official SDK github.com/volcengine/ark-runtime-go.
//
// Usage: create an Instance with an API Key; every operation then hangs off that instance.
//
//	ins, err := ma.New("your-api-key")
//	if err != nil {
//	    return err
//	}
//
//	ag, err := ins.Agents().Create(ctx, ma.AgentSpec{
//	    Name:   "triage",
//	    Model:  "doubao-seed-2-1-pro-260628",
//	    System: "You are a helpful assistant.",
//	    Tools:  []string{ma.ToolsetDefault},
//	})
//
// Each of the six resource kinds has its own sub-service, split along Managed Agents concepts:
//
//	ins.Agents()        Agent definitions (versioned, reusable agent configuration)
//	ins.Environments()  The environment a Session runs in (network, dependencies, env vars)
//	ins.Sessions()      Agent instances that carry out one concrete task; events are here too
//	ins.Vaults()        Credential vaults, letting the Agent call third parties as the user
//	ins.MemoryStores()  Persistent memory shared across Sessions
//	ins.Skills()        Reusable skill packages
//
// Design trade-off: this package is a **thin wrapper**. Convenience entry points (such as
// AgentSpec, SendText) cover the common cases; every sub-service also offers *Raw methods that
// take and return the SDK's own request/response types, so fields the wrapper does not cover
// never get in the way. Return values are always the SDK's own structs (*agent.Agent etc.) with
// no second mapping. That way new fields from SDK upgrades show up automatically, and there is
// no parallel set of DTOs to maintain.
package ma

import (
	"errors"
	"net/http"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime"
)

// ErrMissingAPIKey is returned when no API Key is given.
var ErrMissingAPIKey = errors.New("ma: 缺少 API Key")

// Instance is the entry point to this package and holds a configured Ark client.
//
// An Instance is safe to share between goroutines: the inner *arkruntime.Client reuses one
// http.Client, and the sub-services are just stateless method holders. One per process is
// usually enough.
type Instance struct {
	client *arkruntime.Client
	// streamClient is only for long-lived SSE connections. Its only difference from client is
	// that the underlying http.Client has no Timeout. For why they must be two, see
	// newStreamHTTPClient.
	streamClient *arkruntime.Client

	agents       *AgentService
	environments *EnvironmentService
	sessions     *SessionService
	vaults       *VaultService
	memoryStores *MemoryStoreService
	skills       *SkillService

	// raw is the byte-for-byte pass-through channel. Only New (which has apiKey and baseURL) sets
	// it up; for an Instance built by NewWithClient it is nil, and the pass-through methods return
	// ErrRawUnsupported.
	raw *rawClient
}

// New creates an instance from an API Key. The API Key can be found on the "API Key management"
// page of the Ark console.
//
// Optional Options override the defaults, e.g. a different region or a timeout:
//
//	ins, err := ma.New(key, ma.WithRegion("cn-beijing"), ma.WithTimeout(2*time.Minute))
func New(apiKey string, opts ...Option) (*Instance, error) {
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}

	cfg := newOptions(opts...)
	client := arkruntime.NewClientWithApiKey(apiKey, cfg.configOptions()...)
	streamClient := arkruntime.NewClientWithApiKey(apiKey, cfg.streamConfigOptions()...)

	ins := newInstance(client, streamClient)
	ins.configureRaw(apiKey, cfg)
	return ins, nil
}

// NewWithClient creates an instance from an *arkruntime.Client the caller built.
//
// Use it when New's options do not cover what you need, e.g. AK/SK authentication
// (arkruntime.NewClientWithAkSk) or BytePlus endpoints
// (arkruntime.NewByteplusClientWithApiKey).
//
// **Note**: this path has only one client, and long-lived SSE connections use it too, so its
// http.Client.Timeout becomes a hard limit on streams (the SDK default is 10 minutes; see
// newStreamHTTPClient). To run long streams, set the passed-in client's Timeout to 0 and control
// the lifetime with a context.
func NewWithClient(client *arkruntime.Client) (*Instance, error) {
	if client == nil {
		return nil, errors.New("ma: client 不能为 nil")
	}
	return newInstance(client, client), nil
}

// newStreamHTTPClient builds an http.Client only for long-lived SSE connections: **no Timeout**.
//
// # Why it must be overridden explicitly
//
// http.Client.Timeout limits the total time "from connect to finishing reading the body", and an
// SSE body never finishes. A normal long stream gets cut off as a timeout. The SDK's
// StreamSessionEvents uses config.HTTPClient (arkruntime/sessions.go), the same one as ordinary
// requests.
//
// And **"not passing WithTimeout" does not mean "no timeout"**: the SDK's NewClientConfig comes
// with defaultTimeout = 10 * time.Minute (arkruntime/config.go), which applies unless
// overridden. This trap really fired: production logs showed repeated "chat stream interrupted"
// errors reporting `context deadline exceeded (Client.Timeout or context cancellation while
// reading body)`, every one lasting exactly 10m0.00x s. Any chat longer than 10 minutes was
// sure to break, and the frontend got session.error and rendered it as an error entry. So
// Timeout: 0 must be given explicitly here.
//
// The stream's lifetime is instead controlled by context (the caller passes ctx; cancelling it
// closes the stream).
//
// The Transport is not built here: leaving it nil uses http.DefaultTransport, which has its own
// connection pool, is safe for concurrent use, and shares connection reuse with the ordinary
// client.
func newStreamHTTPClient() *http.Client {
	return &http.Client{Timeout: 0}
}

// newInstance assembles the instance and its sub-services. The sub-services are built once at
// construction, so accessors like ins.Agents() need no lock and have no race.
//
// streamClient is only given to SessionService: it is the only one with long-lived SSE
// connections.
func newInstance(client, streamClient *arkruntime.Client) *Instance {
	ins := &Instance{client: client, streamClient: streamClient}
	ins.agents = &AgentService{client: client}
	ins.environments = &EnvironmentService{client: client}
	ins.sessions = &SessionService{client: client, streamClient: streamClient}
	ins.vaults = &VaultService{client: client}
	ins.memoryStores = &MemoryStoreService{client: client}
	ins.skills = &SkillService{client: client}
	return ins
}

// Agents returns operations on Agent definitions.
func (i *Instance) Agents() *AgentService { return i.agents }

// Environments returns operations on environments.
func (i *Instance) Environments() *EnvironmentService { return i.environments }

// Sessions returns operations on sessions, including sending/receiving events and streaming.
func (i *Instance) Sessions() *SessionService { return i.sessions }

// Vaults returns operations on credential vaults.
func (i *Instance) Vaults() *VaultService { return i.vaults }

// MemoryStores returns operations on persistent memory.
func (i *Instance) MemoryStores() *MemoryStoreService { return i.memoryStores }

// Skills returns operations on skill packages.
func (i *Instance) Skills() *SkillService { return i.skills }

// Client exposes the underlying Ark client, for APIs this package does not wrap yet
// (such as model inference Chat Completions, Responses, etc.).
func (i *Instance) Client() *arkruntime.Client { return i.client }

// Option configures an Instance.
type Option func(*options)

// options collects the settings New understands.
//
// It deliberately does not store []arkruntime.ConfigOption directly: those are opaque closures.
// Storing named fields lets configOptions decide per field "was it set" before passing it on,
// so a zero value is not mistaken for the user's explicit intent.
type options struct {
	region     string
	baseURL    string
	timeout    time.Duration
	retryTimes int
	hasRetry   bool
	httpClient *http.Client
}

func newOptions(opts ...Option) options {
	var o options
	for _, apply := range opts {
		if apply != nil {
			apply(&o)
		}
	}
	return o
}

// configOptions translates this package's options into the SDK's ConfigOption.
// Unset items are not passed on at all, so the SDK's own defaults apply.
func (o options) configOptions() []arkruntime.ConfigOption {
	var out []arkruntime.ConfigOption
	if o.region != "" {
		out = append(out, arkruntime.WithRegion(o.region))
	}
	if o.baseURL != "" {
		out = append(out, arkruntime.WithBaseUrl(o.baseURL))
	}
	if o.timeout > 0 {
		out = append(out, arkruntime.WithTimeout(o.timeout))
	}
	if o.hasRetry {
		out = append(out, arkruntime.WithRetryTimes(o.retryTimes))
	}
	if o.httpClient != nil {
		out = append(out, arkruntime.WithHTTPClient(o.httpClient))
	}
	return out
}

// streamConfigOptions is the streaming version of configOptions: region / address / retry stay
// the same, but **timeout is dropped** and an http.Client with no Timeout is used instead.
//
// Order matters: WithHTTPClient must come last. The SDK's WithTimeout writes a field on
// config.HTTPClient (config.HTTPClient.Timeout = timeout), so WithHTTPClient followed by
// WithTimeout would write the timeout back onto our streaming client and undo the change. Here
// WithTimeout is simply not passed on, and the order adds one more safeguard.
//
// When the caller explicitly injected an httpClient, it wins: that is a clear "I manage the
// connections myself" intent. But its Timeout also becomes the stream's limit; see the comment
// on WithHTTPClient.
func (o options) streamConfigOptions() []arkruntime.ConfigOption {
	var out []arkruntime.ConfigOption
	if o.region != "" {
		out = append(out, arkruntime.WithRegion(o.region))
	}
	if o.baseURL != "" {
		out = append(out, arkruntime.WithBaseUrl(o.baseURL))
	}
	if o.hasRetry {
		out = append(out, arkruntime.WithRetryTimes(o.retryTimes))
	}

	streamHTTP := o.httpClient
	if streamHTTP == nil {
		streamHTTP = newStreamHTTPClient()
	}
	return append(out, arkruntime.WithHTTPClient(streamHTTP))
}

// WithRegion sets the region, e.g. "cn-beijing".
func WithRegion(region string) Option {
	return func(o *options) { o.region = region }
}

// WithBaseURL overrides the API root address. Usually only needed for integration testing or
// private deployments.
func WithBaseURL(url string) Option {
	return func(o *options) { o.baseURL = url }
}

// WithTimeout sets the per-request timeout.
//
// It only applies to ordinary requests. Long-lived SSE connections (StreamEvents /
// StreamThreadEvents) use a different client with no Timeout, are not affected by this, and
// their lifetime is controlled by the context passed in.
//
// Note this is **not** because "http.Client.Timeout does not reach SSE": it does, and the SDK
// comes with a 10-minute default. This package explicitly overrides it so long streams survive;
// see newStreamHTTPClient.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithRetryTimes sets how many times to retry on failure. 0 means no retry.
func WithRetryTimes(n int) Option {
	return func(o *options) {
		o.retryTimes = n
		o.hasRetry = true // recorded explicitly, otherwise 0 cannot be told apart from "not set"
	}
}

// WithHTTPClient replaces the underlying http.Client, e.g. for a custom connection pool or to
// inject tracing.
//
// **The client passed in is also used for long-lived SSE connections**, so its Timeout becomes
// a hard limit on streams (see newStreamHTTPClient). To run long streams, set Timeout to 0.
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) { o.httpClient = c }
}
