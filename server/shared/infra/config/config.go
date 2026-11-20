// Package config loads amad's configuration: one YAML file + environment variable overrides.
//
// # Why YAML instead of piling up more environment variables
//
// A database connection is 6~10 related values (host / port / database / user / password /
// sslmode / pool settings). Spread out as environment variables, it is hard to see how they
// relate, and there is no place for a comment explaining "why this IPv6 address must not have
// square brackets". A commented YAML file explains itself.
//
// # No viper
//
// viper's implicit behavior (automatic search paths, AutomaticEnv, config hot reload) conflicts
// with this repo's "thin wrapper, explicit, no magic" style: when something goes wrong you do not
// know where a value came from. Here it is just yaml.Decode + one explicit environment variable
// override function; reading the code tells you exactly where each value comes from.
//
// # Priority
//
//	environment variables > YAML file > built-in defaults
//
// Environment variables stay at the highest priority so that when production breaks, changing
// one environment variable and restarting fixes it; it also makes it easy to keep secrets in
// environment variables instead of files.
//
// ⚠️ The cost: **a leftover environment variable with the same name keeps winning**, so "I changed
// the file but it did not take effect" becomes a pitfall with no error. For this,
// Result.EnvOverridden lists every field overridden by an environment variable, and cmd/amad
// prints them in the startup log.
//
// # Stacking multiple YAML files
//
// ParseLayers stacks several YAML files in order (later ones override earlier ones; fields not
// mentioned keep their values), which makes it easy to keep secrets in a separate file. cmd/amad
// currently reads only one (AMA_CONFIG, default config.yaml).
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/chyroc/ama/server/shared/infra/datasources"
	"github.com/chyroc/ama/server/shared/infra/github"
	"github.com/chyroc/ama/server/shared/infra/pg"
	"github.com/chyroc/ama/server/shared/infra/redis"
	"github.com/chyroc/ama/server/shared/infra/s3"
)

// ErrInvalidConfig means the configuration is invalid (missing required items, bad format, etc.).
var ErrInvalidConfig = errors.New("config: 配置不合法")

// Config is amad's complete configuration.
//
// When stacking several YAML files, a later file only needs the few fields it cares about:
// yaml.Decode into an already filled struct **merges** (fields not mentioned keep their values
// and are not wiped by zero values). A unit test pins this down.
type Config struct {
	PG    PG    `yaml:"pg"`
	Redis Redis `yaml:"redis"`
	S3    S3    `yaml:"s3"`

	Server   Server   `yaml:"server"`
	APIKey   APIKey   `yaml:"apiKey"`
	Auth     Auth     `yaml:"auth"`
	Ark      Ark      `yaml:"ark"`
	Docker   Docker   `yaml:"docker"`
	Worker   Worker   `yaml:"worker"`
	Feedback Feedback `yaml:"feedback"`

	// DataSources is the worker-side datasource tools (query_sql / read_object). Optional: when no
	// source is enabled, amad declares no custom tool at all, and the Ark Agent is exactly what it
	// was before this feature existed.
	//
	// Why credentials live here (the amad process) and not in the agent's env: see
	// shared/infra/datasources/README.md, section "Why the worker side".
	DataSources DataSources `yaml:"dataSources"`

	// UI holds optional branding and empty-chat starter prompts served to the web frontend.
	// Leaving it empty keeps AMA's default name, title, and clean empty chat state.
	UI UI `yaml:"ui"`

	// OpenViking configures optional long-term memory recall and capture hooks.
	OpenViking OpenViking `yaml:"openViking"`

	// CUA configures the optional Volcengine Computer Use Agent cloud desktop reverse proxy.
	CUA CUA `yaml:"cua"`

	// Seedance configures optional credentials and target Agent/Environment IDs injected for the
	// built-in seedance-ma video/image generation skill.
	Seedance Seedance `yaml:"seedance"`
}

// Feedback is the read permission for issue feedback: who counts as "the dev team".
//
// # Why "not open to users" has to be this, rather than "no entry point in the UI"
//
// Feedback contains other people's emails, the errors they ran into, and their route traces.
// "No entry point in the UI" stops nobody who can use curl: that is not access control, it is
// **pretending** to have access control, and it fails silently: no error at all, just nobody
// knowing who read the data.
//
// # Why an email allowlist, not an admin flag on API keys
//
// Adding a flag to keys (option a) runs into an invariant **already written in three places**:
// a key strictly inherits its creator's permissions (top of auth/apikey.go,
// repository/schema/auth_pg.sql, and the reasoning for the level of the two api-keys routes in
// authz/matrix.go). Those two routes can be ActionRead precisely because "a key created by a
// viewer can only read".
//
// Once keys can carry an admin flag, **a viewer can mint an admin key for themselves**: a real
// privilege escalation path, and it would immediately break the level of those two api-keys
// routes (the comments there say explicitly "if scopes are ever added, these two must be
// re-leveled"). Changing the foundation of the whole key model for one internal read endpoint
// costs far more than it gains.
//
// An email allowlist is **independent of the key mechanism and works with it automatically**: the
// identity a key resolves to is its creator's email (auth.Identity.Email), so dev team members
// pass naturally with their own keys, and people not on the list cannot pass with any key. One
// check covers both kinds of credential.
//
// # Empty means deny all (fail-closed)
//
// This is a direction this repo has confirmed again and again: feishu.Config.Authorizer denies
// everything when empty, and authz's AllowAllAuthorizer has been deleted. With allow as the
// fallback, "forgot to configure" means the gate does not exist at all, with zero errors; with
// deny as the fallback, it is noticed on the first click. **Authorization should fail on the
// noisy side.**
//
// So by default, local development cannot read the feedback list; to read it, add yourself in
// config.yaml.
type Feedback struct {
	// Readers is the list of emails that may read feedback. **Empty means nobody can read.**
	//
	// Comparison is case-insensitive (Emails are lowercased at the auth entry, but these values
	// are typed into the config by hand and may be written as LiWei@example.com). See
	// Feedback.CanRead.
	Readers []string `yaml:"readers"`
}

// CanRead reports whether this actor may read feedback.
//
// Case-insensitive, and both sides are TrimSpace'd: the config is typed by hand, and an extra
// space after a YAML list item is completely invisible, yet it would make that person **silently**
// unable to read (it shows up as "but I am on the list").
//
// An empty actor is always denied, whether or not the list contains an empty string: if an empty
// item slips into the list (a `- ""` in YAML, or a line with just `-`), it would become "readable
// without login".
func (f Feedback) CanRead(actor string) bool {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return false
	}
	for _, r := range f.Readers {
		if r := strings.TrimSpace(r); r != "" && strings.EqualFold(r, actor) {
			return true
		}
	}
	return false
}

// Server is about the process itself: where it listens, who may make cross-origin requests,
// which Store it uses.
type Server struct {
	// Addr is the listen address; empty means 0.0.0.0:9999 (all interfaces, including IPv6).
	//
	// It can also be set with the AMA_ADDR environment variable, which has the highest priority;
	// if set, the value here is overridden.
	Addr string `yaml:"addr"`

	// PublicURL is the public access address. **It is only used to decide whether cookies get
	// Secure**: only when the value starts with https://. Locally it is http://, and with Secure
	// the browser does not store the cookie at all; it shows up as "login succeeds, but a refresh
	// asks to log in again", with no error.
	PublicURL string `yaml:"publicURL"`

	// AllowedOrigin is the origin allowed for cross-origin requests, like http://localhost:5273.
	//
	// Empty means no CORS headers are sent. **It cannot be `*`** (httpd rejects it), because
	// cookies are sent. When running the root dev.sh locally, frontend and backend are same-origin
	// and this is not needed.
	AllowedOrigin string `yaml:"allowedOrigin"`

	// TerminalWSOrigins is the allowlist of page origins that may open a **terminal WebSocket**.
	//
	// # Why it is a separate field from AllowedOrigin
	//
	// AllowedOrigin covers CORS for normal `/api/` requests, while the terminal WS is a different
	// path: the page is on `ama.example.com`, and the WS connects to **`ama-ws.example.com`** (the
	// gateway for that entry does not use h2, so the WS handshake works). So the Origin is always
	// different, same-origin cannot be required, and only an allowlist works. The two paths may
	// have different lists; merging them into one field would make "open up CORS" also open up
	// "who can open a terminal", and the latter can run arbitrary commands in the container.
	//
	// ⚠️ **When empty, the terminal WS rejects every connection that carries an Origin**
	// (fail-closed), so the browser cannot connect and silently falls back to SSE+POST. That
	// fallback works, but fast typing echoes out of order and costs one RTT per character. **In
	// other words, missing this setting gives no error; those two bugs just come back as they
	// were.** The check is whether the terminal status bar shows a "compatibility mode" badge.
	//
	// Not failing open is on purpose: "allow everything when not configured" means opening the
	// terminal to the whole network (CSWSH), and that hole gives zero errors. Better a slow
	// terminal (with a visible badge) than a silent opening.
	//
	// In production use `https://ama.example.com`; when running the root dev.sh locally, the
	// frontend and the WS are same-origin, use `http://127.0.0.1:5273` (the port follows dev.sh's
	// PORT).
	// **`*` is not supported**, for the same reason as AllowedOrigin.
	TerminalWSOrigins []string `yaml:"terminalWSOrigins"`

	// DevRoutes, when true, mounts /api/_debug/reset|clear, used by e2e to prepare data.
	//
	// ⚠️ **Never turn it on in production**: one request can wipe the whole database.
	DevRoutes bool `yaml:"devRoutes"`

	// Store picks the data layer: pg (default) or mem.
	//
	// ⚠️ mem only suits a single local process: with multiple instances each instance knows only
	// its own sessions, so if the login request lands on A and the next request lands on B, the
	// user is logged out; and data is gone when the process restarts.
	Store string `yaml:"store"`
}

// APIKey is the fixed-key channel for e2e / CI / scripts / probes: they have no browser and
// cannot go through an OAuth redirect.
//
// **Enabled only when both Key and Email are non-empty**; in production it is not configured
// by default and so does not exist. The local dev frontend still goes through real OAuth; this
// bypass is only for callers that "are not people".
//
// ⚠️ Key is a credential and belongs in a separate secrets config or an environment variable. It
// is a standing key that bypasses OAuth login; put in the wrong place it is a backdoor, so
// cmd/amad logs a warn saying so when it is enabled.
type APIKey struct {
	Key   string `yaml:"key"`
	Email string `yaml:"email"`
	Name  string `yaml:"name"`
}

// Ark is the process-level credential for Ark Managed Agents.
//
// ⚠️ **The chat path does not use this APIKey**: each workspace configures its own Ark
// credential and uses that. These two fields are for probes and the old self-built Worker path;
// they are kept here to keep config in one place. APIKey belongs in the encrypted config.
type Ark struct {
	APIKey        string `yaml:"apiKey"`
	EnvironmentID string `yaml:"environmentID"`

	// VaultIDs is an optional list of Ark Vault IDs (e.g. vlt-example) attached when creating
	// sessions so Ark can authenticate Vault-managed MCP credentials without exposing plaintext
	// secrets inside session containers. Empty attaches no Vault. AMA_ARK_VAULT_IDS.
	VaultIDs []string `yaml:"vaultIDs"`

	// SecretKey is the master key for **encrypting users' Ark credentials**: 32 bytes in base64
	// (AES-256-GCM).
	//
	// It is a completely different thing from APIKey above: APIKey is our own Ark key, while
	// SecretKey is used to encrypt **the keys users configure** before storing them. Do not mix
	// them up: mixing them up gives no error (both are opaque strings); encryption just silently
	// does not work.
	//
	// **Empty degrades to storing plaintext**, and the service starts as usual (that was the
	// behavior before this field was added). A hard error would keep the service from starting,
	// which is far worse than storing plaintext. cmd/amad states the current mode in the startup
	// log; that line is the only way ops can know "is encryption actually on", because both modes
	// work exactly the same and only the shape of the stored data differs.
	//
	// Generate one: `openssl rand -base64 32`. It belongs in a separate secrets config or an
	// environment variable.
	//
	// ⚠️ **Replacing this value is not "rotating the key"**; it makes every encrypted row
	// undecryptable, and the symptom is everyone's Ark credentials failing at once. Rotation must
	// go through the version-prefix approach described in shared/infra/cryptobox.
	SecretKey string `yaml:"secretKey"`
}

// Docker is the config of the Docker executor (the default place tools run): one container per
// Ark session.
//
// The whole section can be omitted; all defaults work (local /var/run/docker.sock, image
// ama-sandbox:latest). If the daemon is unreachable the service starts as usual, but agents that
// use container execution fail as soon as they call a tool; the startup log says so.
//
// ⚠️ **amad having access to docker.sock equals having root on the host** (it can start
// privileged containers and mount host directories). Treat it as a high-privilege service when
// deploying, see docs/guides/docker-executor.md.
type Docker struct {
	// Enabled false means the Docker executor is not wired (only the host executor remains).
	// Empty defaults to true.
	Enabled *bool `yaml:"enabled"`

	// Host is the daemon address: unix:///var/run/docker.sock, tcp://host:2375.
	// Empty falls back to the DOCKER_HOST environment variable, then to
	// unix:///var/run/docker.sock. **TLS is not supported.**
	Host string `yaml:"host"`

	// Image is the session container image; empty means ama-sandbox:latest (the Dockerfile
	// under sandbox-image/ in the repo). A custom image needs bash, coreutils (with timeout),
	// findutils (GNU find) and grep.
	Image string `yaml:"image"`

	// Workdir is the working directory inside the container; the workspace volume is mounted
	// here, and skills are installed under its skills/. Empty means /workspace.
	Workdir string `yaml:"workdir"`

	// Prefix is the prefix for container names, volume names and labels; empty means ama.
	//
	// ⚠️ **Two unrelated AMA deployments on the same daemon must each set their own**: the
	// cleanup sweep identifies "its own" containers by label, and with the same prefix it would
	// delete the other's. Multiple instances of the same AMA deployment must set the **same** one.
	Prefix string `yaml:"prefix"`

	// CPUs is the CPU quota per container (cores, may be fractional); 0 means no limit.
	CPUs float64 `yaml:"cpus"`
	// Memory is the memory limit per container, like 512m / 2g; empty means no limit.
	Memory string `yaml:"memory"`
	// PidsLimit is the process count limit per container; 0 means no limit.
	PidsLimit int64 `yaml:"pidsLimit"`
	// Network is the container network (bridge / none / a custom network name); empty uses the
	// daemon default (bridge). none cuts the network completely, but then the agent cannot clone
	// code or install dependencies.
	Network string `yaml:"network"`

	// Runtime is the OCI runtime for session containers, for example runsc (gVisor). It must be
	// registered with the daemon (/etc/docker/daemon.json "runtimes"). Empty uses the daemon
	// default (runc). With many sessions sharing one big host, runsc gives each session its own
	// user-space kernel instead of sharing the host kernel.
	Runtime string `yaml:"runtime"`
	// CapDrop lists Linux capabilities dropped from session containers, for example [ALL]. Empty
	// keeps the Docker default set. Dropping ALL breaks things that need root inside the
	// container (apt install, chown), so check the image before turning it on.
	CapDrop []string `yaml:"capDrop"`
	// DataMounts are host directories bind-mounted into every session container, for shared
	// business data (for example a directory on an EBS disk). Every session sees the same files;
	// keep readOnly true unless sessions really must write.
	DataMounts []DockerDataMount `yaml:"dataMounts"`

	// IdleAction is what to do with the container after a chat turn goes idle; empty means pause:
	//
	//	pause  freeze processes, keep memory; resumes in milliseconds when a message comes (default)
	//	stop   stop the container, free memory; restarts when a message comes (a second or two), background processes do not come back
	//	none   leave it alone, it runs until the cleanup sweep deletes it
	IdleAction string `yaml:"idleAction"`

	// ContainerTTL is how long a container may sit idle before it is deleted (the workspace
	// volume is kept, and the container is rebuilt automatically on the next message).
	// Empty means 24h; 0 or negative means never delete.
	ContainerTTL *time.Duration `yaml:"containerTTL"`
	// WorkspaceTTL is how long a workspace volume may sit idle before it is deleted (this is
	// when the workspace is really wiped). Empty means 168h (7 days); 0 or negative means never
	// delete.
	WorkspaceTTL *time.Duration `yaml:"workspaceTTL"`
}

// DockerDataMount is one entry of docker.dataMounts.
type DockerDataMount struct {
	// Source is the absolute path on the Docker host.
	Source string `yaml:"source"`
	// Target is the absolute path inside the container; it must not overlap docker.workdir.
	Target string `yaml:"target"`
	// ReadOnly mounts the directory read-only. Empty means true.
	ReadOnly *bool `yaml:"readOnly"`
}

// ReadOnlyOrDefault reports whether the mount is read-only (true when empty).
func (m DockerDataMount) ReadOnlyOrDefault() bool { return m.ReadOnly == nil || *m.ReadOnly }

// DockerEnabled reports whether the Docker executor is enabled (enabled by default when empty).
func (d Docker) DockerEnabled() bool { return d.Enabled == nil || *d.Enabled }

// MemoryBytes parses Memory into bytes. Empty or unparsable returns 0 (no limit); the latter also
// returns an error.
func (d Docker) MemoryBytes() (int64, error) {
	return parseByteSize(d.Memory)
}

// ContainerTTLOrDefault / WorkspaceTTLOrDefault return the effective cleanup duration (<=0 means
// no cleanup).
func (d Docker) ContainerTTLOrDefault() time.Duration {
	if d.ContainerTTL == nil {
		return 24 * time.Hour
	}
	return *d.ContainerTTL
}

// WorkspaceTTLOrDefault: see ContainerTTLOrDefault.
func (d Docker) WorkspaceTTLOrDefault() time.Duration {
	if d.WorkspaceTTL == nil {
		return 7 * 24 * time.Hour
	}
	return *d.WorkspaceTTL
}

// parseByteSize parses 512m / 2g / 1024k / a plain number (bytes).
func parseByteSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(s, "b")
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: docker.memory %q 看不懂（要形如 512m / 2g）", ErrInvalidConfig, s)
	}
	return int64(n * float64(mult)), nil
}

// Worker is the config of the self_hosted worker (the loop that polls Ark's work queue).
type Worker struct {
	// Concurrency is how many works **each Ark environment** handles at the same time; empty
	// takes the default 20.
	//
	// One work is one chat turn. Each Ark poll returns at most one, so concurrency can only come
	// from "take one, hand it off, go back right away for the next", see runOne in
	// core/worker.go.
	//
	// ⚠️ **Setting it to 1 falls back to serial**: a work only exits after the session has been
	// idle for MaxIdle (default 60 seconds), and meanwhile other sessions of the same environment
	// **wait in line the whole time**. The symptom is "I sent a message and nothing happened for
	// a long time".
	//
	// Each running work may hold its own container, so this also indirectly limits how many
	// containers run at once; use a smaller value on a small machine.
	// It is a pointer to tell "not set" from "explicitly set to 1".
	Concurrency *int `yaml:"concurrency"`

	// AdoptExternalSessions, when true, registers a Run automatically when the self_hosted worker
	// picks up work for an Ark session created outside AMA (for example from the Ark Console or SDK)
	// whose agent ID matches a bound AMA agent. AMA_WORKER_ADOPT_EXTERNAL_SESSIONS.
	AdoptExternalSessions bool `yaml:"adoptExternalSessions"`
}

// DefaultWorkerConcurrency is the number of concurrent works per environment when
// worker.concurrency is not set.
const DefaultWorkerConcurrency = 20

// ConcurrencyOrDefault returns the effective per-environment concurrency. **Anything below 1 is
// treated as 1** (falls back to serial).
func (w Worker) ConcurrencyOrDefault() int {
	if w.Concurrency == nil {
		return DefaultWorkerConcurrency
	}
	if *w.Concurrency < 1 {
		return 1
	}
	return *w.Concurrency
}

// Auth is the config related to login and credentials.
//
// Kept separate from APIKey above on purpose: that one is a **process-level singleton** bypass key
// (for CI/probes), this is something else. Merging them into one section would make "which one
// to rotate" unclear.
type Auth struct {
	// ManagedKeySecret is the derivation secret for "platform-managed API keys".
	//
	// Purpose: the `ama` command inside containers needs an AMA API key (AMA_TOKEN). The platform
	// derives it as HMAC(this secret, user email), so the plaintext is never stored and every
	// instance computes the same string. See server/auth/managedkey.go.
	//
	// ⚠️ **This and ark.secretKey are two different keys; do not reuse one for the other.** That
	// one is used by cryptobox to encrypt users' Ark keys (decryptable storage); this one only
	// does HMAC derivation. Reusing gives no error, but forces their rotation schedules together,
	// and rotating them has completely different consequences (changing that one makes existing
	// ciphertext undecryptable; changing this one only makes everyone get a token again).
	//
	// **Empty means the ama command in containers is unavailable**; the service starts as usual
	// (cmd/amad states the consequence in the startup log).
	// Generate one: `openssl rand -base64 32`. It belongs in a separate secrets config or an
	// environment variable.
	//
	// Consequence of rotation: AMA_TOKEN in every container becomes invalid at once; the next
	// command run derives a new string automatically and creates the row. **Acceptable, no
	// downtime needed**: see "key rotation" in managedkey.go.
	ManagedKeySecret string `yaml:"managedKeySecret"`

	// Local is local account (email + password) login.
	Local AuthLocal `yaml:"local"`

	// OIDC is generic OpenID Connect login (Authorization Code + PKCE).
	OIDC AuthOIDC `yaml:"oidc"`

	// GitHub is GitHub OAuth device flow login (needs only a client id, no secret).
	GitHub AuthGitHub `yaml:"github"`

	// Admins is the list of platform admin emails: they can create local accounts when
	// self-signup is off. It is OR'ed with is_admin on local accounts: the first registered local
	// account is automatically an admin, and people logging in through OIDC can only become
	// admins by being listed here.
	Admins []string `yaml:"admins"`
}

// AuthLocal is the config of local account login.
//
// ⚠️ When **neither** local nor oidc is on, people cannot log in (the fixed API key and user API
// keys still work); cmd/amad says so in the startup log at Error level.
type AuthLocal struct {
	// Enabled turns on email + password login.
	Enabled bool `yaml:"enabled"`

	// AllowSignup opens self-signup. **When off, registering the first account is still allowed**
	// (when there are no users in the database at all, that person automatically becomes the
	// platform admin); after that only a platform admin can create accounts via
	// POST /api/admin/users.
	AllowSignup bool `yaml:"allowSignup"`

	// MaxFailures / FailureWindowSeconds rate-limit password login: after N failures within the
	// window, logins are refused; the count is in PG (shared across instances). Empty means
	// 10 attempts / 900 seconds.
	MaxFailures          int `yaml:"maxFailures"`
	FailureWindowSeconds int `yaml:"failureWindowSeconds"`
}

// AuthOIDC is the config of OIDC login. It is enabled only when both issuer and clientId are set.
//
// The redirect_uri to register at the IdP is `<server.publicURL>/api/auth/oidc/callback`.
type AuthOIDC struct {
	// Issuer looks like https://accounts.google.com (the iss of the ID token must equal it
	// exactly).
	Issuer string `yaml:"issuer"`

	ClientID string `yaml:"clientId"`

	// ClientSecret is a secret; may be empty (public clients rely on PKCE only).
	ClientSecret string `yaml:"clientSecret"`

	// Scopes empty means openid email profile. openid is always added.
	Scopes []string `yaml:"scopes"`

	// DisplayName is the name on the login page button ("Sign in with <it>"); empty means "SSO".
	DisplayName string `yaml:"displayName"`

	// AllowUnverifiedEmail lets accounts whose email_verified is not true log in. **Default
	// false**: the email is the Actor, and an IdP that does not verify emails lets anyone enter
	// any email, which means anyone can impersonate any member.
	AllowUnverifiedEmail bool `yaml:"allowUnverifiedEmail"`
}

// Enabled reports whether OIDC is fully configured (issuer and clientId both non-empty).
func (o AuthOIDC) Enabled() bool {
	return strings.TrimSpace(o.Issuer) != "" && strings.TrimSpace(o.ClientID) != ""
}

// AuthGitHub is the config of GitHub login (OAuth device flow). enabled true turns it on.
//
// When clientId is empty, the public client id of GitHub CLI (github.DefaultClientID) is used, so
// it works out of the box. For a real deployment it is better to create your own under GitHub →
// Settings → Developer settings → OAuth Apps, check "Enable Device Flow", and put it here (no
// client secret needed). The reason is in the comment on that constant.
//
// ⚠️ When **none** of the admission allowlists (allowedUsers / allowedEmailDomains / allowedOrgs)
// is set, nobody can get in: anyone can register a GitHub account, so no admission rule would open
// the platform to the whole world. Matching any one item lets the user in.
type AuthGitHub struct {
	Enabled bool `yaml:"enabled"`

	// ClientID empty means the public client id of GitHub CLI, see above.
	ClientID string `yaml:"clientId"`

	// Scopes empty means read:user user:email. read:org is added automatically when allowedOrgs
	// is set.
	Scopes []string `yaml:"scopes"`

	// DisplayName is the name on the login page button ("Sign in with <it>"); empty means
	// "GitHub".
	DisplayName string `yaml:"displayName"`

	// Proxy is an HTTP proxy **used only for github.com / api.github.com**, like
	// http://127.0.0.1:8080. It does not affect other outbound traffic such as Ark; empty uses the
	// process's HTTPS_PROXY and similar environment variables (the standard library default).
	Proxy string `yaml:"proxy"`

	// AllowedUsers are GitHub usernames (login), case-insensitive.
	AllowedUsers []string `yaml:"allowedUsers"`

	// AllowedEmailDomains match the domain of the GitHub primary email (must be verified), like
	// example.com. Subdomains are not included.
	AllowedEmailDomains []string `yaml:"allowedEmailDomains"`

	// AllowedOrgs are GitHub organization names; the user must be a full member (needs the
	// read:org scope, added automatically).
	// ⚠️ When the organization has "third-party application access restrictions" on, an org admin
	// must approve this OAuth App, otherwise membership cannot be read.
	AllowedOrgs []string `yaml:"allowedOrgs"`
}

// Active reports whether GitHub login is to be enabled. The client id may be empty (default is
// used), so only the switch matters.
func (g AuthGitHub) Active() bool { return g.Enabled }

// HasAllowlist reports whether at least one admission allowlist is set.
func (g AuthGitHub) HasAllowlist() bool {
	return len(g.AllowedUsers) > 0 || len(g.AllowedEmailDomains) > 0 || len(g.AllowedOrgs) > 0
}

// Redis is the Redis connection config; fields map one to one to redis.Config.
//
// **It is only used for leader election of the Feishu long connection** (one Feishu app allows
// only one long connection; multiple instances connecting directly evict each other and randomly
// drop inbound messages). No cache, no queue. See the package comment of shared/infra/redis.
//
// The whole section may be empty: then the long connection is not enabled, the service starts as
// usual, it just receives no Feishu messages; the startup log says so.
type Redis struct {
	// Addr is host:port. Locally it is 127.0.0.1:16379 (docker-compose maps the container's 6379
	// to host port 16379, avoiding a 6379 that may already be taken on the machine).
	Addr string `yaml:"addr"`

	// Password empty means no auth (as in local docker-compose).
	//
	// In production do not write it here; use the AMA_REDIS_PASSWORD environment variable, which
	// overrides this field.
	Password string `yaml:"password"`

	// DB is the database index; empty means 0.
	DB int `yaml:"db"`

	// DialTimeout is the connect timeout, like "5s". Empty means 5s.
	DialTimeout time.Duration `yaml:"dialTimeout"`

	// OpTimeout is the read/write timeout of a single command, like "1s". Empty means 1s.
	OpTimeout time.Duration `yaml:"opTimeout"`
}

// Enabled reports whether Redis is configured, so the long connection can be enabled. A
// non-empty Addr counts as configured.
func (r Redis) Enabled() bool {
	return strings.TrimSpace(r.Addr) != ""
}

// S3 is the config of object storage (S3 compatible); fields map one to one to
// shared/infra/s3.Config.
//
// **All bytes of attachments, skill files, skill packages and personal files live here; PG only
// stores metadata.** In PG mode (server.store=pg, the default) this section **must be set**;
// without it amad refuses to start. It does not silently write bytes back to PG. mem mode may
// leave it unset (then bytes are kept in process memory, and personal file endpoints answer 503).
//
// Local docker compose runs RustFS (see docker-compose.yml at the repo root); production can use
// any S3 compatible service.
//
// # Three endpoints
//
// Presigned links have the Host signed in, so "who opens this link" decides which address to sign
// with:
//
//	endpoint          amad's own reads and writes (http://s3:9000 in compose)
//	publicEndpoint    signed for browsers / users' own machines (http://localhost:19000 in compose)
//	sandboxEndpoint   signed for Docker session containers fetching skill packages (http://s3:9000 in compose)
//
// The last two fall back to endpoint when empty. See the package comment of shared/infra/s3.
type S3 struct {
	Endpoint        string `yaml:"endpoint"`
	PublicEndpoint  string `yaml:"publicEndpoint"`
	SandboxEndpoint string `yaml:"sandboxEndpoint"`

	// Region empty means us-east-1 (most S3 compatible services do not care, but the signature
	// needs one).
	Region string `yaml:"region"`
	// Bucket is the bucket name. **Empty = object storage not configured.**
	Bucket string `yaml:"bucket"`

	// AccessKey / SecretKey are credentials; in production use AMA_S3_ACCESS_KEY /
	// AMA_S3_SECRET_KEY.
	AccessKey string `yaml:"accessKey"`
	SecretKey string `yaml:"secretKey"`

	// UsePathStyle empty means true: RustFS / MinIO / local setups all need path-style
	// (virtual-host style needs wildcard DNS). Set false explicitly when using AWS S3 with
	// virtual-host style.
	UsePathStyle *bool `yaml:"usePathStyle"`

	// CreateBucket empty means true: create the bucket at startup if it does not exist
	// (concurrent creation by multiple instances tolerates "already exists"). Set false when the
	// production bucket is created by ops and amad's credentials cannot create buckets.
	CreateBucket *bool `yaml:"createBucket"`

	// Timeout is the limit for a single call, like "2m". Empty means 2 minutes.
	Timeout time.Duration `yaml:"timeout"`
}

// Enabled reports whether object storage is configured. A non-empty bucket name counts; whether
// it is reachable is decided by HeadBucket at startup, and a failure there makes amad refuse to
// start.
func (s S3) Enabled() bool { return strings.TrimSpace(s.Bucket) != "" }

// ToS3Config converts to shared/infra/s3's Config and fills the two nullable booleans with
// their defaults.
func (s S3) ToS3Config() s3.Config {
	pathStyle, create := true, true
	if s.UsePathStyle != nil {
		pathStyle = *s.UsePathStyle
	}
	if s.CreateBucket != nil {
		create = *s.CreateBucket
	}
	return s3.Config{
		Endpoint:        s.Endpoint,
		PublicEndpoint:  s.PublicEndpoint,
		SandboxEndpoint: s.SandboxEndpoint,
		Region:          s.Region,
		Bucket:          s.Bucket,
		AccessKey:       s.AccessKey,
		SecretKey:       s.SecretKey,
		UsePathStyle:    pathStyle,
		CreateBucket:    create,
		Timeout:         s.Timeout,
	}
}

// DataSources is the configuration of the worker-side datasource tools.
//
// # Why the worker side instead of the agent's environment
//
// The agent's env goes into the session container and into the Ark Agent config, so credentials
// put there are readable by the agent itself (one curl and they are gone) and visible in plaintext
// in the Agent definition. amad holding them means the model only ever sees "a read-only account"
// and "a prefix in one bucket"; the tool layer also owns the row/byte/timeout limits and writes an
// audit line per call.
//
// # enabled but incomplete = startup failure
//
// Every field except the ones with defaults is required once `enabled: true`. Leaving enabled on
// with a missing host does **not** degrade to "no tool": it fails at startup with ErrInvalidConfig,
// because "looks enabled in config while the model has no tool" is a zero-error failure, and the
// first person to notice it would be a user staring at an agent that never queries anything.
//
// # Defaults (0 / empty means the value in brackets)
//
//	mysql.port        (3306)     mysql.maxRows   (200, hard cap 1000)
//	mysql.timeout     (30s)      tos.maxBytes    (1 MiB, hard cap 8 MiB)
//	tos.timeout       (20s)      tos.prefix      (required; a trailing "/" is added when missing)
type DataSources struct {
	MySQL  DataSourceMySQL   `yaml:"mysql"`
	TOS    DataSourceTOS     `yaml:"tos"`
	Redact map[string]string `yaml:"redact"`
}

// DataSourceMySQL is the MySQL source behind the query_sql tool.
//
// In production the password should come from AMA_DATA_MYSQL_PASSWORD and the account should be
// **SELECT-only** on the business database: that account is the real security boundary, the SQL
// guard in the tool is only a "don't shoot yourself in the foot" layer.
type DataSourceMySQL struct {
	// Enabled turns the source (and the query_sql declaration) on. AMA_DATA_MYSQL_ENABLED.
	Enabled bool `yaml:"enabled"`

	// Host is the hostname or IP, e.g. mysql.example.com. AMA_DATA_MYSQL_HOST.
	Host string `yaml:"host"`
	// Port empty means 3306. AMA_DATA_MYSQL_PORT.
	Port int `yaml:"port"`
	// Database is the schema the read-only account may query. AMA_DATA_MYSQL_DATABASE.
	Database string `yaml:"database"`
	// User is the read-only account. AMA_DATA_MYSQL_USER.
	User string `yaml:"user"`
	// Password: **use AMA_DATA_MYSQL_PASSWORD in production**, not this field.
	Password string `yaml:"password"`
	// MaxRows is the per-call row cap; empty means 200, values above 1000 are clamped to 1000.
	// The tool's max_rows parameter can only lower it. AMA_DATA_MYSQL_MAX_ROWS.
	MaxRows int `yaml:"maxRows"`
	// Timeout is the total per-call timeout; empty means 30s. AMA_DATA_MYSQL_TIMEOUT.
	Timeout time.Duration `yaml:"timeout"`
}

// DataSourceTOS is the TOS (S3-compatible) source behind the read_object tool.
//
// The bucket is fixed and keys must start with Prefix: the tool can only narrow what the
// credentials already allow, never widen it. A missing Prefix fails startup (fail-closed) rather
// than defaulting to "the whole bucket".
type DataSourceTOS struct {
	// Enabled turns the source (and the read_object declaration) on. AMA_DATA_TOS_ENABLED.
	Enabled bool `yaml:"enabled"`

	// Endpoint is the S3-compatible endpoint, e.g. https://tos-s3-cn-beijing.volces.com.
	// Without a scheme, https:// is added. AMA_DATA_TOS_ENDPOINT.
	Endpoint string `yaml:"endpoint"`
	// Region is the signing region, e.g. cn-beijing. AMA_DATA_TOS_REGION.
	Region string `yaml:"region"`
	// Bucket is the only bucket the tool may read. AMA_DATA_TOS_BUCKET.
	Bucket string `yaml:"bucket"`
	// Prefix is the only key prefix the tool may read, e.g. example-prefix/.
	// A trailing "/" is added when missing, so "a" does not also match "a-secret/...".
	// AMA_DATA_TOS_PREFIX.
	Prefix string `yaml:"prefix"`
	// AccessKey: **use AMA_DATA_TOS_ACCESS_KEY in production**.
	AccessKey string `yaml:"accessKey"`
	// SecretKey: **use AMA_DATA_TOS_SECRET_KEY in production**.
	SecretKey string `yaml:"secretKey"`
	// MaxBytes is the per-call byte cap; empty means 1 MiB, above 8 MiB is clamped to 8 MiB.
	// AMA_DATA_TOS_MAX_BYTES.
	MaxBytes int64 `yaml:"maxBytes"`
	// Timeout is per-call timeout; empty means 20s. AMA_DATA_TOS_TIMEOUT.
	Timeout time.Duration `yaml:"timeout"`
	// UsePathStyle sends http://host/bucket/key instead of http://bucket.host/key. Empty means
	// false: TOS's S3-compatible domain is virtual-hosted. AMA_DATA_TOS_USE_PATH_STYLE.
	UsePathStyle bool `yaml:"usePathStyle"`
}

// Enabled reports whether at least one datasource is turned on.
func (d DataSources) Enabled() bool { return d.MySQL.Enabled || d.TOS.Enabled }

// ToConfig converts to shared/infra/datasources' Config.
//
// The direction is config → datasources (the same one as ToStdConfig / ToS3Config): the infra
// package does not need to know about YAML or this package.
func (d DataSources) ToConfig() datasources.Config {
	return datasources.Config{
		MySQL: datasources.MySQLConfig{
			Enabled:  d.MySQL.Enabled,
			Host:     d.MySQL.Host,
			Port:     d.MySQL.Port,
			Database: d.MySQL.Database,
			User:     d.MySQL.User,
			Password: d.MySQL.Password,
			MaxRows:  d.MySQL.MaxRows,
			Timeout:  d.MySQL.Timeout,
		},
		TOS: datasources.TOSConfig{
			Enabled:      d.TOS.Enabled,
			Endpoint:     d.TOS.Endpoint,
			Region:       d.TOS.Region,
			Bucket:       d.TOS.Bucket,
			Prefix:       d.TOS.Prefix,
			AccessKey:    d.TOS.AccessKey,
			SecretKey:    d.TOS.SecretKey,
			MaxBytes:     d.TOS.MaxBytes,
			Timeout:      d.TOS.Timeout,
			UsePathStyle: d.TOS.UsePathStyle,
		},
		Redact: d.Redact,
	}
}

// validate checks the datasource section. Disabled sections are not looked at at all (so a
// half-written `tos:` block left over from an experiment does not block startup); enabled ones
// must be complete.
//
// The rules themselves live in datasources.Config.Validate: one implementation, so "config says it
// is valid" and "the registry can actually be built" cannot drift apart.
func (d DataSources) validate() error {
	if !d.Enabled() {
		return nil
	}
	if err := d.ToConfig().Validate(); err != nil {
		return fmt.Errorf("%w: dataSources.%v", ErrInvalidConfig, err)
	}
	return nil
}

// UI configures optional white-label branding and empty-chat starter prompts for the web UI.
// All fields default to empty, which preserves AMA's built-in brand and clean empty chat screen.
type UI struct {
	// BrandName overrides the platform display name (e.g. "Acme Agents"). AMA_UI_BRAND_NAME.
	BrandName string `yaml:"brandName"`
	// BrandBadge renders a small pill next to the brand name on the landing page. AMA_UI_BRAND_BADGE.
	BrandBadge string `yaml:"brandBadge"`
	// Title overrides document.title in the browser tab. AMA_UI_TITLE.
	Title string `yaml:"title"`
	// StarterPrompts lists optional prompt cards shown when starting a new conversation.
	StarterPrompts []UIStarterPrompt `yaml:"starterPrompts"`
}

// UIStarterPrompt is one suggested starter card on the empty chat screen.
type UIStarterPrompt struct {
	Label  string `yaml:"label" json:"label"`
	Prompt string `yaml:"prompt" json:"prompt"`
}

// DefaultOpenVikingURL is the default Volcengine VikingDB OpenViking endpoint when openViking.url is empty.
const DefaultOpenVikingURL = "https://api.vikingdb.cn-beijing.volces.com/openviking"

// OpenViking configures optional long-term memory recall and capture.
type OpenViking struct {
	// Enabled turns server-side OpenViking memory hooks on. Individual agents still opt in via
	// Agent.OpenVikingEnabled. AMA_OPENVIKING_ENABLED.
	Enabled bool `yaml:"enabled"`
	// URL is the OpenViking HTTP base URL; empty defaults to DefaultOpenVikingURL. AMA_OPENVIKING_URL.
	URL string `yaml:"url"`
	// APIKey is the X-API-Key credential; required when enabled is true. AMA_OPENVIKING_API_KEY.
	APIKey string `yaml:"apiKey"`
	// ResourceID is the X-OpenViking-Resource-Id header; required when enabled is true. AMA_OPENVIKING_RESOURCE_ID.
	ResourceID string `yaml:"resourceID"`
	// RecallTimeout bounds pre-turn context search; empty defaults to 3500ms. AMA_OPENVIKING_RECALL_TIMEOUT.
	RecallTimeout time.Duration `yaml:"recallTimeout"`
	// CaptureTimeout bounds post-turn session commit; empty defaults to 15s. AMA_OPENVIKING_CAPTURE_TIMEOUT.
	CaptureTimeout time.Duration `yaml:"captureTimeout"`
}

// URLOrDefault returns the trimmed OpenViking base URL without trailing slash.
func (o OpenViking) URLOrDefault() string {
	if u := strings.TrimRight(strings.TrimSpace(o.URL), "/"); u != "" {
		return u
	}
	return DefaultOpenVikingURL
}

// RecallTimeoutOrDefault returns the recall timeout (3500ms when unset or non-positive).
func (o OpenViking) RecallTimeoutOrDefault() time.Duration {
	if o.RecallTimeout > 0 {
		return o.RecallTimeout
	}
	return 3500 * time.Millisecond
}

// CaptureTimeoutOrDefault returns the capture timeout (15s when unset or non-positive).
func (o OpenViking) CaptureTimeoutOrDefault() time.Duration {
	if o.CaptureTimeout > 0 {
		return o.CaptureTimeout
	}
	return 15 * time.Second
}

func (o OpenViking) validate() error {
	if !o.Enabled {
		return nil
	}
	var missing []string
	if strings.TrimSpace(o.APIKey) == "" {
		missing = append(missing, "apiKey")
	}
	if strings.TrimSpace(o.ResourceID) == "" {
		missing = append(missing, "resourceID")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: openViking.enabled=true 但缺少 %s", ErrInvalidConfig, strings.Join(missing, " / "))
	}
	return nil
}

// DefaultCUAUpstreamURL is the default Volcengine CUA desktop web origin when cua.upstreamURL is empty.
const DefaultCUAUpstreamURL = "https://cua.ark.cn-beijing.volces.com"

// CUA configures the optional Computer Use Agent cloud desktop reverse proxy and skill defaults.
type CUA struct {
	// Enabled mounts the authenticated /desktops/ and /cua-app/ reverse proxy routes. AMA_CUA_ENABLED.
	Enabled bool `yaml:"enabled"`
	// UpstreamURL is the CUA desktop web origin; empty defaults to DefaultCUAUpstreamURL. AMA_CUA_UPSTREAM_URL.
	UpstreamURL string `yaml:"upstreamURL"`
	// GatewayURL is the CUA control-plane API base URL (used to mint desktop tickets and injected as
	// AP_CUA_SKILL_API_BASE_URL for the ark-cua skill). Required when enabled is true. AMA_CUA_GATEWAY_URL.
	GatewayURL string `yaml:"gatewayURL"`
	// APIKey is the optional server-level CUA API key used to mint desktop access tickets. AMA_CUA_API_KEY.
	APIKey string `yaml:"apiKey"`
	// ArtifactBaseURL overrides CUA_CREDENTIAL_ARTIFACT_BASE_URL for CUA credential helper scripts. AMA_CUA_ARTIFACT_BASE_URL.
	ArtifactBaseURL string `yaml:"artifactBaseURL"`
}

// UpstreamURLOrDefault returns the CUA upstream origin without trailing slash.
func (c CUA) UpstreamURLOrDefault() string {
	if u := strings.TrimRight(strings.TrimSpace(c.UpstreamURL), "/"); u != "" {
		return u
	}
	return DefaultCUAUpstreamURL
}

func (c CUA) validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.GatewayURL) == "" {
		return fmt.Errorf("%w: cua.enabled=true 但缺少 gatewayURL", ErrInvalidConfig)
	}
	return nil
}

// DefaultSeedanceAPIBaseURL is the default Ark API base URL for the seedance-ma skill.
const DefaultSeedanceAPIBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

// Seedance configures optional server-managed defaults injected for agents using the seedance-ma skill.
type Seedance struct {
	// Enabled turns server-managed Seedance environment injection on. AMA_SEEDANCE_ENABLED.
	Enabled bool `yaml:"enabled"`
	// APIBaseURL is the Ark API base URL; empty defaults to DefaultSeedanceAPIBaseURL. AMA_SEEDANCE_API_BASE_URL.
	APIBaseURL string `yaml:"apiBaseURL"`
	// APIKey is the dedicated Ark API key for Seedance media generation; required when enabled is true. AMA_SEEDANCE_API_KEY.
	APIKey string `yaml:"apiKey"`
	// AgentID is the target Ark Agent ID for Seedance sessions; required when enabled is true. AMA_SEEDANCE_AGENT_ID.
	AgentID string `yaml:"agentID"`
	// EnvironmentID is the optional Ark cloud Environment ID for Seedance sessions. AMA_SEEDANCE_ENVIRONMENT_ID.
	EnvironmentID string `yaml:"environmentID"`
	// VideoModel is the optional video model identifier passed to seedance-ma. AMA_SEEDANCE_VIDEO_MODEL.
	VideoModel string `yaml:"videoModel"`
	// ImageModel is the optional image model identifier passed to seedance-ma. AMA_SEEDANCE_IMAGE_MODEL.
	ImageModel string `yaml:"imageModel"`
}

// APIBaseURLOrDefault returns the Seedance API base URL without trailing slash.
func (s Seedance) APIBaseURLOrDefault() string {
	if u := strings.TrimRight(strings.TrimSpace(s.APIBaseURL), "/"); u != "" {
		return u
	}
	return DefaultSeedanceAPIBaseURL
}

func (s Seedance) validate() error {
	if !s.Enabled {
		return nil
	}
	var missing []string
	if strings.TrimSpace(s.APIKey) == "" {
		missing = append(missing, "apiKey")
	}
	if strings.TrimSpace(s.AgentID) == "" {
		missing = append(missing, "agentID")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: seedance.enabled=true 但缺少 %s", ErrInvalidConfig, strings.Join(missing, " / "))
	}
	return nil
}

// PG is the connection config for standard PostgreSQL; fields map one to one to pg.StdConfig.
//
// Production is any standard PostgreSQL; locally it is postgres:17-alpine from docker-compose.
// Both share this section; switching only changes host / port / user / password.
type PG struct {
	// Host is a hostname or IP.
	//
	// **Do not put square brackets around IPv6.** The rule "IPv6 must be wrapped in square
	// brackets" in many docs only applies to URI-form DSNs; we build libpq's key=value form, where
	// brackets are taken as part of the hostname and reported as
	// `lookup [fdbd:...]: no such host`. See the comment on pg.StdConfig.Host.
	Host string `yaml:"host"`

	// Port is the port; empty means 5432.
	Port int `yaml:"port"`

	// Database is the database name.
	Database string `yaml:"database"`

	// User is the login user name.
	User string `yaml:"user"`

	// Password is the login password.
	//
	// In production do not write it here; use the AMA_PG_PASSWORD environment variable, which
	// overrides this field.
	Password string `yaml:"password"`

	// SSLMode maps to libpq's sslmode; empty means disable (internal network). Over the public
	// internet it must be require or stricter.
	SSLMode string `yaml:"sslmode"`

	// ConnectTimeout is the timeout for a single connect, like "5s". Empty means it is not passed
	// down.
	//
	// Recommended: when the address is unreachable (for example an allowlist does not let it
	// through), by default it hangs until the system TCP timeout before returning an error, tens of
	// seconds, which is painful when debugging locally.
	ConnectTimeout time.Duration `yaml:"connectTimeout"`

	// The pool settings below, when empty, use the standard PG defaults in shared/infra/pg
	// (25 / 5 / 5m / 30m).
	MaxOpenConns    int           `yaml:"maxOpenConns"`
	MaxIdleConns    int           `yaml:"maxIdleConns"`
	ConnMaxIdleTime time.Duration `yaml:"connMaxIdleTime"`
	ConnMaxLifetime time.Duration `yaml:"connMaxLifetime"`

	// StatementTimeout is the server-side execution limit for each SQL statement, like "10s".
	// Empty means 10s.
	//
	// ⚠️ **Do not raise it for normal workloads, and never turn it off.** It is the safety net for
	// "one stuck query holds a connection forever and does not return even when ctx is
	// cancelled". Schema migrations automatically use an exempt connection and are not limited by
	// it.
	StatementTimeout time.Duration `yaml:"statementTimeout"`

	// LockTimeout is the limit for waiting on row locks, like "2s". Empty means 2s.
	LockTimeout time.Duration `yaml:"lockTimeout"`
}

// Environment variable names. They keep the AMA_PG_ prefix already set by shared/infra/pg/example.
//
// **These are an "override layer", not the source of config**: when non-empty they override the
// field of the same name in the file. When adding a field, remember to update applyEnv and the
// comments in config.example.yaml too; missing one shows up as "I set the environment variable
// but it has no effect", exactly the kind of silent failure this package keeps guarding against.
const (
	EnvPGHost     = "AMA_PG_HOST"
	EnvPGPort     = "AMA_PG_PORT"
	EnvPGDatabase = "AMA_PG_DATABASE"
	EnvPGUser     = "AMA_PG_USER"
	EnvPGPassword = "AMA_PG_PASSWORD"
	EnvPGSSLMode  = "AMA_PG_SSLMODE"

	EnvRedisAddr     = "AMA_REDIS_ADDR"
	EnvRedisPassword = "AMA_REDIS_PASSWORD"

	EnvS3Endpoint        = "AMA_S3_ENDPOINT"
	EnvS3PublicEndpoint  = "AMA_S3_PUBLIC_ENDPOINT"
	EnvS3SandboxEndpoint = "AMA_S3_SANDBOX_ENDPOINT"
	EnvS3Region          = "AMA_S3_REGION"
	EnvS3Bucket          = "AMA_S3_BUCKET"
	EnvS3AccessKey       = "AMA_S3_ACCESS_KEY"
	EnvS3SecretKey       = "AMA_S3_SECRET_KEY"
	EnvS3UsePathStyle    = "AMA_S3_USE_PATH_STYLE"
	EnvS3CreateBucket    = "AMA_S3_CREATE_BUCKET"

	// Worker-side datasource tools (query_sql / read_object). They exist **only as an override
	// layer**, same as everything else here: the credentials belong in environment variables so
	// they never have to be written into a config file that is checked in.
	EnvDataMySQLEnabled  = "AMA_DATA_MYSQL_ENABLED"
	EnvDataMySQLHost     = "AMA_DATA_MYSQL_HOST"
	EnvDataMySQLPort     = "AMA_DATA_MYSQL_PORT"
	EnvDataMySQLDatabase = "AMA_DATA_MYSQL_DATABASE"
	EnvDataMySQLUser     = "AMA_DATA_MYSQL_USER"
	EnvDataMySQLPassword = "AMA_DATA_MYSQL_PASSWORD"
	EnvDataMySQLMaxRows  = "AMA_DATA_MYSQL_MAX_ROWS"
	EnvDataMySQLTimeout  = "AMA_DATA_MYSQL_TIMEOUT"

	EnvDataTOSEnabled      = "AMA_DATA_TOS_ENABLED"
	EnvDataTOSEndpoint     = "AMA_DATA_TOS_ENDPOINT"
	EnvDataTOSRegion       = "AMA_DATA_TOS_REGION"
	EnvDataTOSBucket       = "AMA_DATA_TOS_BUCKET"
	EnvDataTOSPrefix       = "AMA_DATA_TOS_PREFIX"
	EnvDataTOSAccessKey    = "AMA_DATA_TOS_ACCESS_KEY"
	EnvDataTOSSecretKey    = "AMA_DATA_TOS_SECRET_KEY"
	EnvDataTOSMaxBytes     = "AMA_DATA_TOS_MAX_BYTES"
	EnvDataTOSTimeout      = "AMA_DATA_TOS_TIMEOUT"
	EnvDataTOSUsePathStyle = "AMA_DATA_TOS_USE_PATH_STYLE"

	EnvAddr          = "AMA_ADDR"
	EnvPublicURL     = "AMA_PUBLIC_URL"
	EnvAllowedOrigin = "AMA_ALLOWED_ORIGIN"
	// EnvTerminalWSOrigins is several origins separated by commas, see Server.TerminalWSOrigins.
	EnvTerminalWSOrigins = "AMA_TERMINAL_WS_ORIGINS"
	EnvDevRoutes         = "AMA_DEV_ROUTES"
	EnvStore             = "AMA_STORE"

	EnvAPIKey      = "AMA_API_KEY"
	EnvAPIKeyEmail = "AMA_API_KEY_EMAIL"
	EnvAPIKeyName  = "AMA_API_KEY_NAME"

	// EnvFeedbackReaders is the feedback read allowlist, a **comma-separated** list of emails.
	//
	// Environment variables can only be strings, while this field is a list, so it is parsed
	// separately (see applyEnv). The reason to keep this door is the same as for other fields:
	// when production needs to temporarily grant someone access to debug a problem, change one
	// environment variable and restart, without editing the config file.
	EnvFeedbackReaders = "AMA_FEEDBACK_READERS"

	EnvArkAPIKey        = "ARK_API_KEY"
	EnvArkEnvironmentID = "ARK_ENVIRONMENT_ID"
	EnvArkVaultIDs      = "AMA_ARK_VAULT_IDS"
	// EnvArkSecretKey is the master key that encrypts users' Ark credentials. **It is not
	// ARK_API_KEY**, see Ark.SecretKey.
	EnvArkSecretKey = "ARK_SECRET_KEY"

	EnvDockerEnabled               = "AMA_DOCKER_ENABLED"
	EnvDockerHost                  = "AMA_DOCKER_HOST"
	EnvDockerImage                 = "AMA_DOCKER_IMAGE"
	EnvDockerWorkdir               = "AMA_DOCKER_WORKDIR"
	EnvDockerPrefix                = "AMA_DOCKER_PREFIX"
	EnvDockerCPUs                  = "AMA_DOCKER_CPUS"
	EnvDockerMemory                = "AMA_DOCKER_MEMORY"
	EnvDockerPidsLimit             = "AMA_DOCKER_PIDS_LIMIT"
	EnvDockerNetwork               = "AMA_DOCKER_NETWORK"
	EnvDockerRuntime               = "AMA_DOCKER_RUNTIME"
	EnvDockerIdleAction            = "AMA_DOCKER_IDLE_ACTION"
	EnvDockerContainerTTL          = "AMA_DOCKER_CONTAINER_TTL"
	EnvDockerWorkspaceTTL          = "AMA_DOCKER_WORKSPACE_TTL"
	EnvWorkerConcurrency           = "AMA_WORKER_CONCURRENCY"
	EnvWorkerAdoptExternalSessions = "AMA_WORKER_ADOPT_EXTERNAL_SESSIONS"

	EnvUIBrandName  = "AMA_UI_BRAND_NAME"
	EnvUIBrandBadge = "AMA_UI_BRAND_BADGE"
	EnvUITitle      = "AMA_UI_TITLE"

	EnvOpenVikingEnabled        = "AMA_OPENVIKING_ENABLED"
	EnvOpenVikingURL            = "AMA_OPENVIKING_URL"
	EnvOpenVikingAPIKey         = "AMA_OPENVIKING_API_KEY"
	EnvOpenVikingResourceID     = "AMA_OPENVIKING_RESOURCE_ID"
	EnvOpenVikingRecallTimeout  = "AMA_OPENVIKING_RECALL_TIMEOUT"
	EnvOpenVikingCaptureTimeout = "AMA_OPENVIKING_CAPTURE_TIMEOUT"

	EnvCUAEnabled         = "AMA_CUA_ENABLED"
	EnvCUAUpstreamURL     = "AMA_CUA_UPSTREAM_URL"
	EnvCUAGatewayURL      = "AMA_CUA_GATEWAY_URL"
	EnvCUAAPIKey          = "AMA_CUA_API_KEY"
	EnvCUAArtifactBaseURL = "AMA_CUA_ARTIFACT_BASE_URL"

	EnvSeedanceEnabled       = "AMA_SEEDANCE_ENABLED"
	EnvSeedanceAPIBaseURL    = "AMA_SEEDANCE_API_BASE_URL"
	EnvSeedanceAPIKey        = "AMA_SEEDANCE_API_KEY"
	EnvSeedanceAgentID       = "AMA_SEEDANCE_AGENT_ID"
	EnvSeedanceEnvironmentID = "AMA_SEEDANCE_ENVIRONMENT_ID"
	EnvSeedanceVideoModel    = "AMA_SEEDANCE_VIDEO_MODEL"
	EnvSeedanceImageModel    = "AMA_SEEDANCE_IMAGE_MODEL"

	EnvAuthManagedKeySecret = "AMA_AUTH_MANAGED_KEY_SECRET"

	EnvAuthLocalEnabled     = "AMA_AUTH_LOCAL_ENABLED"
	EnvAuthLocalAllowSignup = "AMA_AUTH_LOCAL_ALLOW_SIGNUP"
	EnvAuthOIDCIssuer       = "AMA_AUTH_OIDC_ISSUER"
	EnvAuthOIDCClientID     = "AMA_AUTH_OIDC_CLIENT_ID"
	EnvAuthOIDCClientSecret = "AMA_AUTH_OIDC_CLIENT_SECRET"
	EnvAuthOIDCDisplayName  = "AMA_AUTH_OIDC_DISPLAY_NAME"
	EnvAuthAdmins           = "AMA_AUTH_ADMINS"

	EnvAuthGitHubEnabled             = "AMA_AUTH_GITHUB_ENABLED"
	EnvAuthGitHubClientID            = "AMA_AUTH_GITHUB_CLIENT_ID"
	EnvAuthGitHubScopes              = "AMA_AUTH_GITHUB_SCOPES"
	EnvAuthGitHubDisplayName         = "AMA_AUTH_GITHUB_DISPLAY_NAME"
	EnvAuthGitHubProxy               = "AMA_AUTH_GITHUB_PROXY"
	EnvAuthGitHubAllowedUsers        = "AMA_AUTH_GITHUB_ALLOWED_USERS"
	EnvAuthGitHubAllowedEmailDomains = "AMA_AUTH_GITHUB_ALLOWED_EMAIL_DOMAINS"
	EnvAuthGitHubAllowedOrgs         = "AMA_AUTH_GITHUB_ALLOWED_ORGS"
)

// Load reads a YAML file, applies environment variable overrides, then validates.
//
// A missing file returns a clear error instead of silently using defaults: a wrong config file
// path is a very common slip, and silently falling back to "connect to localhost" only leaves
// people staring at a puzzling connection error.
//
// Callers that only care about *Config use this; those that need to know "which fields were
// overridden by environment variables" use LoadFile.
func Load(path string) (*Config, error) {
	res, err := LoadFile(path)
	if err != nil {
		return nil, err
	}
	return res.Config, nil
}

// LoadFile reads a YAML file and returns a Result with source information.
func LoadFile(path string) (*Result, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf(
				"%w: 配置文件 %s 不存在（可以从 config.example.yaml 复制一份）",
				ErrInvalidConfig, path,
			)
		}
		return nil, fmt.Errorf("读配置文件 %s 失败: %w", path, err)
	}

	res, err := ParseLayers("file:"+path, raw)
	if err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return res, nil
}

// Parse parses config from YAML bytes, applies environment variable overrides, then validates.
//
// It is exported separately so tests do not have to write files.
func Parse(raw []byte) (*Config, error) {
	res, err := ParseLayers("inline", raw)
	if err != nil {
		return nil, err
	}
	return res.Config, nil
}

// Result is the result of one config load; besides the config itself, it carries "where the
// values came from".
//
// With this information the startup log can state clearly where the config came from. **This is
// not a nice-to-have**: environment variables have the highest priority, so "I changed the file
// but it did not take effect" is the number one silent pitfall, and EnvOverridden is what makes it
// visible at a glance in the startup log.
type Result struct {
	// Config is the config that finally takes effect.
	Config *Config

	// Source describes where the config came from, like "file:config.yaml". Goes straight into
	// the startup log.
	Source string

	// EnvOverridden are the field names overridden by environment variables (like "pg.host"), in
	// applyEnv order.
	//
	// **Only field names, never values**: secrets must not go into logs.
	EnvOverridden []string

	// UnknownFields are fields that appear in the YAML but the struct does not know (like
	// "line 153: field maxOpenConn not found in type config.PG").
	//
	// **They do not block startup**; they only produce a warning in the startup log; see the
	// note about ordering in ParseLayers. The list is kept to still guard the pitfall strict mode
	// was meant to prevent: a misspelled key (`maxOpenConn` missing an s) means "you think it is
	// set but it is not", and this warning line lets people see it.
	UnknownFields []string
}

// ParseLayers stacks several YAML documents in order, then applies environment variable overrides
// and validates.
//
// "Stacking" relies on yaml.Decode into the same struct: **fields not mentioned keep their values
// and are not wiped by zero values** (a unit test pins this behavior). So secrets can live in a
// separate YAML stacked on top of the plaintext one, and the secrets file only needs the few
// fields it cares about.
//
// Empty byte slices in layers are skipped: a config that exists but is empty is a valid
// intermediate state and should not keep the service from starting. But if **all of them are
// empty**, validate reports "config is empty".
//
// # Unknown fields only warn, they are not fatal
//
// Fields the struct does not know are collected into Result.UnknownFields, and **the service
// starts as usual**.
//
// This used to fail outright with KnownFields(true), to prevent "misspelled key → you think it is
// set but it is not". But that rule turned **the release order of config and code** into a hard
// constraint: once config went live before the code, the old binary could not parse the new
// field and exited. And config is global, so **all instances restart over and over**, and not a
// single instance survives.
//
// Adding a field to config first and shipping the code afterwards is a completely normal order
// (and unavoidable during canary and rollback, when old and new binaries run side by side: old
// instances will certainly read the new field). Turning that into an outage is the wrong
// tradeoff; the correct meaning of an unknown field is "this version does not know it yet,
// ignore it".
//
// The misspelled-key pitfall is still guarded: it is now a WARN in the startup log instead of a
// production incident.
func ParseLayers(source string, layers ...[]byte) (*Result, error) {
	var cfg Config
	var unknown []string
	decoded := false
	for _, raw := range layers {
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		// KnownFields(true) is still on, but only to **detect** unknown fields, not to veto
		// startup: below, "unknown field" is picked out of the error and downgraded to a warning;
		// everything else (type mismatch, YAML syntax errors) stays fatal.
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			// For a file with only comments, Decode returns a bare io.EOF, an error message that
			// means nothing to users. Skip it as "this layer has no content" and let validate judge
			// whether the final result is enough.
			if errors.Is(err, io.EOF) {
				continue
			}
			// yaml.v3's TypeError is a **soft error**: it collects all problems of this layer,
			// while the struct **has already been filled normally** (all known fields took
			// effect). So unknown fields can safely just be recorded and not returned; real type
			// errors ("cannot unmarshal !!str into int") must still be stopped, otherwise a wrong
			// type silently becomes a zero value.
			var te *yaml.TypeError
			if errors.As(err, &te) {
				var fatal []string
				for _, msg := range te.Errors {
					if strings.Contains(msg, "not found in type") {
						unknown = append(unknown, msg)
						continue
					}
					fatal = append(fatal, msg)
				}
				if len(fatal) > 0 {
					return nil, &yaml.TypeError{Errors: fatal}
				}
				decoded = true
				continue
			}
			return nil, err
		}
		decoded = true
	}
	if !decoded {
		return nil, fmt.Errorf("%w: 配置是空的，至少要有 pg.host / pg.database / pg.user", ErrInvalidConfig)
	}

	overridden := cfg.applyEnv()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Result{Config: &cfg, Source: source, EnvOverridden: overridden, UnknownFields: unknown}, nil
}

// applyEnv overrides YAML values with environment variables and returns the overridden field
// names.
//
// Only non-empty environment variables override: an empty value cannot be used to "clear" a
// field. So if the config.yaml in the build output ships with a local redis address,
// AMA_REDIS_ADDR="" cannot save it, and instances get rebuilt over and over.
func (c *Config) applyEnv() []string {
	var hit []string
	// str is the common shape "override if non-empty and record it". trim=false is for secrets:
	// in theory a password may start/end with spaces, trimming them breaks the connection, and
	// that kind of failure is very hard to locate.
	str := func(env, field string, dst *string, trim bool) {
		v := os.Getenv(env)
		if trim {
			v = strings.TrimSpace(v)
		}
		if v == "" {
			return
		}
		*dst = v
		hit = append(hit, field)
	}

	str(EnvPGHost, "pg.host", &c.PG.Host, true)
	if v := strings.TrimSpace(os.Getenv(EnvPGPort)); v != "" {
		// If parsing fails, treat it as unset: a typo in one environment variable should not keep
		// the service from starting, and validate catches the "invalid port" case.
		if n, err := strconv.Atoi(v); err == nil {
			c.PG.Port = n
			hit = append(hit, "pg.port")
		}
	}
	str(EnvPGDatabase, "pg.database", &c.PG.Database, true)
	str(EnvPGUser, "pg.user", &c.PG.User, true)
	str(EnvPGPassword, "pg.password", &c.PG.Password, false)
	str(EnvPGSSLMode, "pg.sslmode", &c.PG.SSLMode, true)

	str(EnvRedisAddr, "redis.addr", &c.Redis.Addr, true)
	str(EnvRedisPassword, "redis.password", &c.Redis.Password, false)

	str(EnvS3Endpoint, "s3.endpoint", &c.S3.Endpoint, true)
	str(EnvS3PublicEndpoint, "s3.publicEndpoint", &c.S3.PublicEndpoint, true)
	str(EnvS3SandboxEndpoint, "s3.sandboxEndpoint", &c.S3.SandboxEndpoint, true)
	str(EnvS3Region, "s3.region", &c.S3.Region, true)
	str(EnvS3Bucket, "s3.bucket", &c.S3.Bucket, true)
	str(EnvS3AccessKey, "s3.accessKey", &c.S3.AccessKey, true)
	str(EnvS3SecretKey, "s3.secretKey", &c.S3.SecretKey, false)
	// The two booleans: unrecognized values are treated as unset (same handling as boolEnv
	// below).
	for _, b := range []struct {
		env, field string
		dst        **bool
	}{
		{EnvS3UsePathStyle, "s3.usePathStyle", &c.S3.UsePathStyle},
		{EnvS3CreateBucket, "s3.createBucket", &c.S3.CreateBucket},
	} {
		if v := strings.TrimSpace(os.Getenv(b.env)); v != "" {
			if parsed, err := strconv.ParseBool(v); err == nil {
				*b.dst = &parsed
				hit = append(hit, b.field)
			}
		}
	}

	str(EnvAddr, "server.addr", &c.Server.Addr, true)
	str(EnvPublicURL, "server.publicURL", &c.Server.PublicURL, true)
	str(EnvAllowedOrigin, "server.allowedOrigin", &c.Server.AllowedOrigin, true)
	// The terminal WS origin allowlist has **multiple values**, comma-separated in the
	// environment variable.
	//
	// Why it gets an environment variable: to verify locally that "the terminal really uses WS",
	// you need a same-origin address with a port (`http://127.0.0.1:<PORT>`), and that port
	// follows dev.sh's PORT. Putting it in config.yaml would mean editing the file every time the
	// port changes, and that file is checked in.
	if v := strings.TrimSpace(os.Getenv(EnvTerminalWSOrigins)); v != "" {
		var out []string
		for _, part := range strings.Split(v, ",") {
			if o := strings.TrimSpace(part); o != "" {
				out = append(out, o)
			}
		}
		// ⚠️ When it is all blanks, **do not** override with an empty slice: that would clear the
		// list configured in YAML, so the terminal silently falls back to SSE, when the user just
		// wrote the environment variable as `AMA_...=,,`.
		if len(out) > 0 {
			c.Server.TerminalWSOrigins = out
			hit = append(hit, "server.terminalWSOrigins")
		}
	}
	// DevRoutes keeps its historical meaning: **only "1" turns it on**. true / yes do not; this
	// is on purpose: a switch that can wipe the whole database should not have several spellings,
	// and any ambiguous value counts as off.
	if v := strings.TrimSpace(os.Getenv(EnvDevRoutes)); v != "" {
		c.Server.DevRoutes = v == "1"
		hit = append(hit, "server.devRoutes")
	}
	str(EnvStore, "server.store", &c.Server.Store, true)

	str(EnvAPIKey, "apiKey.key", &c.APIKey.Key, false)
	str(EnvAPIKeyEmail, "apiKey.email", &c.APIKey.Email, true)
	str(EnvAPIKeyName, "apiKey.name", &c.APIKey.Name, true)

	// Feedback read allowlist: comma-separated. List fields cannot use the str closure above (it
	// writes a *string).
	//
	// **It replaces the whole list, it does not merge with the list in the config file**: with
	// merging, "remove one person via the environment variable" cannot be expressed, and that is
	// exactly the operation needed most when something goes wrong. It also matches the override
	// behavior of other fields.
	if v := strings.TrimSpace(os.Getenv(EnvFeedbackReaders)); v != "" {
		var readers []string
		for _, part := range strings.Split(v, ",") {
			if e := strings.TrimSpace(part); e != "" {
				readers = append(readers, e)
			}
		}
		// When all items are empty (for example the value is just ","), do not override: that is
		// clearly a typo, and overriding with an empty list would **quietly remove everyone's read
		// access**, showing up as "configured, yet nobody can read". Better to treat it as unset.
		if len(readers) > 0 {
			c.Feedback.Readers = readers
			hit = append(hit, "feedback.readers")
		}
	}

	str(EnvArkAPIKey, "ark.apiKey", &c.Ark.APIKey, true)
	str(EnvArkEnvironmentID, "ark.environmentID", &c.Ark.EnvironmentID, true)
	str(EnvArkSecretKey, "ark.secretKey", &c.Ark.SecretKey, true)

	if v := strings.TrimSpace(os.Getenv(EnvDockerEnabled)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Docker.Enabled = &b
			hit = append(hit, "docker.enabled")
		}
	}
	str(EnvDockerHost, "docker.host", &c.Docker.Host, true)
	str(EnvDockerImage, "docker.image", &c.Docker.Image, true)
	str(EnvDockerWorkdir, "docker.workdir", &c.Docker.Workdir, true)
	str(EnvDockerPrefix, "docker.prefix", &c.Docker.Prefix, true)
	str(EnvDockerMemory, "docker.memory", &c.Docker.Memory, true)
	str(EnvDockerNetwork, "docker.network", &c.Docker.Network, true)
	str(EnvDockerRuntime, "docker.runtime", &c.Docker.Runtime, true)
	str(EnvDockerIdleAction, "docker.idleAction", &c.Docker.IdleAction, true)
	// Numbers and durations: if parsing fails, treat as unset, same as pg.port (a typo in one
	// environment variable should not keep the service from starting).
	if v := strings.TrimSpace(os.Getenv(EnvDockerCPUs)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			c.Docker.CPUs = f
			hit = append(hit, "docker.cpus")
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvDockerPidsLimit)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			c.Docker.PidsLimit = n
			hit = append(hit, "docker.pidsLimit")
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvDockerContainerTTL)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Docker.ContainerTTL = &d
			hit = append(hit, "docker.containerTTL")
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvDockerWorkspaceTTL)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Docker.WorkspaceTTL = &d
			hit = append(hit, "docker.workspaceTTL")
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvWorkerConcurrency)); v != "" {
		// Same as above: if parsing fails, treat as unset. **Accepting 1** is on purpose:
		// `AMA_WORKER_CONCURRENCY=1` falls back to serial, a way to stop the bleeding without a
		// release when concurrent scheduling goes wrong.
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			c.Worker.Concurrency = &n
			hit = append(hit, "worker.concurrency")
		}
	}

	str(EnvAuthManagedKeySecret, "auth.managedKeySecret", &c.Auth.ManagedKeySecret, true)
	// Boolean switches: only true/false/1/0 are accepted (strconv.ParseBool); unrecognized values
	// are treated as unset. A typo in one environment variable should not keep the service from
	// starting, nor be silently treated as "off".
	boolEnv := func(env, field string, dst *bool) {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = b
				hit = append(hit, field)
			}
		}
	}
	boolEnv(EnvAuthLocalEnabled, "auth.local.enabled", &c.Auth.Local.Enabled)
	boolEnv(EnvAuthLocalAllowSignup, "auth.local.allowSignup", &c.Auth.Local.AllowSignup)
	str(EnvAuthOIDCIssuer, "auth.oidc.issuer", &c.Auth.OIDC.Issuer, true)
	str(EnvAuthOIDCClientID, "auth.oidc.clientId", &c.Auth.OIDC.ClientID, true)
	str(EnvAuthOIDCClientSecret, "auth.oidc.clientSecret", &c.Auth.OIDC.ClientSecret, false)
	str(EnvAuthOIDCDisplayName, "auth.oidc.displayName", &c.Auth.OIDC.DisplayName, true)
	// Admin list: comma-separated, replaces the whole list (same as feedback.readers).
	if v := strings.TrimSpace(os.Getenv(EnvAuthAdmins)); v != "" {
		var admins []string
		for _, part := range strings.Split(v, ",") {
			if e := strings.TrimSpace(part); e != "" {
				admins = append(admins, e)
			}
		}
		if len(admins) > 0 {
			c.Auth.Admins = admins
			hit = append(hit, "auth.admins")
		}
	}

	boolEnv(EnvAuthGitHubEnabled, "auth.github.enabled", &c.Auth.GitHub.Enabled)
	str(EnvAuthGitHubClientID, "auth.github.clientId", &c.Auth.GitHub.ClientID, true)
	str(EnvAuthGitHubDisplayName, "auth.github.displayName", &c.Auth.GitHub.DisplayName, true)
	str(EnvAuthGitHubProxy, "auth.github.proxy", &c.Auth.GitHub.Proxy, true)
	// Lists: comma (or space) separated, replace the whole list, same as auth.admins.
	list := func(env, field string, dst *[]string) {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
			if len(parts) > 0 {
				*dst = parts
				hit = append(hit, field)
			}
		}
	}
	list(EnvAuthGitHubScopes, "auth.github.scopes", &c.Auth.GitHub.Scopes)
	list(EnvAuthGitHubAllowedUsers, "auth.github.allowedUsers", &c.Auth.GitHub.AllowedUsers)
	list(EnvAuthGitHubAllowedEmailDomains, "auth.github.allowedEmailDomains", &c.Auth.GitHub.AllowedEmailDomains)
	list(EnvAuthGitHubAllowedOrgs, "auth.github.allowedOrgs", &c.Auth.GitHub.AllowedOrgs)

	// Worker-side datasource tools. Credentials go through environment variables; the
	// non-secret fields are here too so that "which environment is this pod talking to" can be
	// fixed without editing the file.
	boolEnv(EnvDataMySQLEnabled, "dataSources.mysql.enabled", &c.DataSources.MySQL.Enabled)
	str(EnvDataMySQLHost, "dataSources.mysql.host", &c.DataSources.MySQL.Host, true)
	str(EnvDataMySQLDatabase, "dataSources.mysql.database", &c.DataSources.MySQL.Database, true)
	str(EnvDataMySQLUser, "dataSources.mysql.user", &c.DataSources.MySQL.User, true)
	str(EnvDataMySQLPassword, "dataSources.mysql.password", &c.DataSources.MySQL.Password, false)
	if v := strings.TrimSpace(os.Getenv(EnvDataMySQLPort)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.DataSources.MySQL.Port = n
			hit = append(hit, "dataSources.mysql.port")
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvDataMySQLMaxRows)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.DataSources.MySQL.MaxRows = n
			hit = append(hit, "dataSources.mysql.maxRows")
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvDataMySQLTimeout)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.DataSources.MySQL.Timeout = d
			hit = append(hit, "dataSources.mysql.timeout")
		}
	}

	boolEnv(EnvDataTOSEnabled, "dataSources.tos.enabled", &c.DataSources.TOS.Enabled)
	str(EnvDataTOSEndpoint, "dataSources.tos.endpoint", &c.DataSources.TOS.Endpoint, true)
	str(EnvDataTOSRegion, "dataSources.tos.region", &c.DataSources.TOS.Region, true)
	str(EnvDataTOSBucket, "dataSources.tos.bucket", &c.DataSources.TOS.Bucket, true)
	str(EnvDataTOSPrefix, "dataSources.tos.prefix", &c.DataSources.TOS.Prefix, true)
	str(EnvDataTOSAccessKey, "dataSources.tos.accessKey", &c.DataSources.TOS.AccessKey, true)
	str(EnvDataTOSSecretKey, "dataSources.tos.secretKey", &c.DataSources.TOS.SecretKey, false)
	boolEnv(EnvDataTOSUsePathStyle, "dataSources.tos.usePathStyle", &c.DataSources.TOS.UsePathStyle)
	if v := strings.TrimSpace(os.Getenv(EnvDataTOSMaxBytes)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.DataSources.TOS.MaxBytes = n
			hit = append(hit, "dataSources.tos.maxBytes")
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvDataTOSTimeout)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.DataSources.TOS.Timeout = d
			hit = append(hit, "dataSources.tos.timeout")
		}
	}

	list(EnvArkVaultIDs, "ark.vaultIDs", &c.Ark.VaultIDs)
	boolEnv(EnvWorkerAdoptExternalSessions, "worker.adoptExternalSessions", &c.Worker.AdoptExternalSessions)

	str(EnvUIBrandName, "ui.brandName", &c.UI.BrandName, true)
	str(EnvUIBrandBadge, "ui.brandBadge", &c.UI.BrandBadge, true)
	str(EnvUITitle, "ui.title", &c.UI.Title, true)

	boolEnv(EnvOpenVikingEnabled, "openViking.enabled", &c.OpenViking.Enabled)
	str(EnvOpenVikingURL, "openViking.url", &c.OpenViking.URL, true)
	str(EnvOpenVikingAPIKey, "openViking.apiKey", &c.OpenViking.APIKey, false)
	str(EnvOpenVikingResourceID, "openViking.resourceID", &c.OpenViking.ResourceID, true)
	if v := strings.TrimSpace(os.Getenv(EnvOpenVikingRecallTimeout)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.OpenViking.RecallTimeout = d
			hit = append(hit, "openViking.recallTimeout")
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvOpenVikingCaptureTimeout)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.OpenViking.CaptureTimeout = d
			hit = append(hit, "openViking.captureTimeout")
		}
	}

	boolEnv(EnvCUAEnabled, "cua.enabled", &c.CUA.Enabled)
	str(EnvCUAUpstreamURL, "cua.upstreamURL", &c.CUA.UpstreamURL, true)
	str(EnvCUAGatewayURL, "cua.gatewayURL", &c.CUA.GatewayURL, true)
	str(EnvCUAAPIKey, "cua.apiKey", &c.CUA.APIKey, false)
	str(EnvCUAArtifactBaseURL, "cua.artifactBaseURL", &c.CUA.ArtifactBaseURL, true)

	boolEnv(EnvSeedanceEnabled, "seedance.enabled", &c.Seedance.Enabled)
	str(EnvSeedanceAPIBaseURL, "seedance.apiBaseURL", &c.Seedance.APIBaseURL, true)
	str(EnvSeedanceAPIKey, "seedance.apiKey", &c.Seedance.APIKey, false)
	str(EnvSeedanceAgentID, "seedance.agentID", &c.Seedance.AgentID, true)
	str(EnvSeedanceEnvironmentID, "seedance.environmentID", &c.Seedance.EnvironmentID, true)
	str(EnvSeedanceVideoModel, "seedance.videoModel", &c.Seedance.VideoModel, true)
	str(EnvSeedanceImageModel, "seedance.imageModel", &c.Seedance.ImageModel, true)

	return hit
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.PG.Host) == "" {
		return fmt.Errorf("%w: pg.host 不能为空", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.PG.Database) == "" {
		return fmt.Errorf("%w: pg.database 不能为空", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.PG.User) == "" {
		return fmt.Errorf("%w: pg.user 不能为空", ErrInvalidConfig)
	}
	if c.PG.Port < 0 || c.PG.Port > 65535 {
		return fmt.Errorf("%w: pg.port %d 不在 0~65535 范围内", ErrInvalidConfig, c.PG.Port)
	}
	// The whole Redis section may be omitted (then the long connection is not enabled), but if
	// set, it must be set consistently.
	if c.Redis.DB < 0 {
		return fmt.Errorf("%w: redis.db 不能是负数", ErrInvalidConfig)
	}
	// store has only two valid values. A misspelling ("memory" / "PG") would **silently run as
	// pg**, leading to "I clearly set mem, why is it still connecting to the database"; so it is
	// treated as an error here instead of falling back to the default.
	if s := strings.TrimSpace(c.Server.Store); s != "" && s != StorePG && s != StoreMem {
		return fmt.Errorf("%w: server.store 只能是 %s 或 %s，实际 %q",
			ErrInvalidConfig, StorePG, StoreMem, s)
	}
	// httpd explicitly rejects "*" (cookies are sent, and browsers reject the two together). It
	// is also stopped here, so it fails when loading config instead of waiting for httpd.New to
	// return a later error.
	if strings.TrimSpace(c.Server.AllowedOrigin) == "*" {
		return fmt.Errorf("%w: server.allowedOrigin 不能是 *，请写明具体来源", ErrInvalidConfig)
	}
	// Setting only half of OIDC is almost certainly a mistake, and its consequence is the login
	// page silently missing a button. Report it as an error.
	if o := c.Auth.OIDC; (strings.TrimSpace(o.Issuer) == "") != (strings.TrimSpace(o.ClientID) == "") {
		return fmt.Errorf("%w: auth.oidc.issuer 与 auth.oidc.clientId 要么都配、要么都不配", ErrInvalidConfig)
	}
	// A wrong proxy (missing http://) shows up as "GitHub login never works", and the error does
	// not point at the proxy, so it is stopped here.
	if p := strings.TrimSpace(c.Auth.GitHub.Proxy); p != "" {
		if _, err := github.ParseProxy(p); err != nil {
			return fmt.Errorf("%w: auth.github.proxy: %v", ErrInvalidConfig, err)
		}
	}
	if c.Auth.Local.MaxFailures < 0 || c.Auth.Local.FailureWindowSeconds < 0 {
		return fmt.Errorf("%w: auth.local.maxFailures / failureWindowSeconds 不能是负数", ErrInvalidConfig)
	}
	// Worker-side datasource tools: enabled but incomplete is a startup error, not "silently one
	// tool fewer". See DataSources.
	if err := c.DataSources.validate(); err != nil {
		return err
	}
	if err := c.OpenViking.validate(); err != nil {
		return err
	}
	if err := c.CUA.validate(); err != nil {
		return err
	}
	if err := c.Seedance.validate(); err != nil {
		return err
	}
	return c.Docker.validateDataMounts()
}

// validateDataMounts rejects data mounts the daemon would only refuse at the first tool call (a
// relative path), or that would silently hide the workspace (a target on or around workdir).
func (d Docker) validateDataMounts() error {
	workdir := strings.TrimSpace(d.Workdir)
	if workdir == "" {
		workdir = "/workspace"
	}
	workdir = path.Clean(workdir)
	for i, m := range d.DataMounts {
		src, dst := strings.TrimSpace(m.Source), strings.TrimSpace(m.Target)
		if !path.IsAbs(src) || !path.IsAbs(dst) {
			return fmt.Errorf("%w: docker.dataMounts[%d] 的 source / target 都要是绝对路径（source=%q target=%q）",
				ErrInvalidConfig, i, src, dst)
		}
		dst = path.Clean(dst)
		if dst == "/" || dst == workdir || strings.HasPrefix(workdir, dst+"/") || strings.HasPrefix(dst, workdir+"/") {
			return fmt.Errorf("%w: docker.dataMounts[%d].target %q 和工作目录 %q 重叠，会盖住工作区",
				ErrInvalidConfig, i, dst, workdir)
		}
	}
	return nil
}

// The two valid values of Store.
const (
	StorePG  = "pg"
	StoreMem = "mem"
)

// StoreOrDefault returns the effective store type; pg when empty.
//
// **Defaulting to pg instead of mem is on purpose**: the in-memory Store looks perfectly fine
// until a restart reveals all data is gone. To run without a database, set mem explicitly.
func (s Server) StoreOrDefault() string {
	if v := strings.TrimSpace(s.Store); v != "" {
		return v
	}
	return StorePG
}

// UseMemStore reports whether the in-memory Store is used.
func (s Server) UseMemStore() bool { return s.StoreOrDefault() == StoreMem }

// AddrOrDefault returns the listen address; 0.0.0.0:9999 when empty.
//
// It writes 0.0.0.0 explicitly instead of just `:9999`: the two are equivalent in Go (both listen
// on all interfaces, both also accept IPv6); writing it in full just makes it obvious in the
// startup log that "it is open to the outside".
//
// 9999 must match the port agreed with the deployment side (docker-compose port mapping, health
// check probes); at deploy time it can also be set explicitly with `AMA_ADDR`, which takes
// priority.
func (s Server) AddrOrDefault() string {
	if v := strings.TrimSpace(s.Addr); v != "" {
		return v
	}
	return "0.0.0.0:9999"
}

// SecureCookie reports whether cookies should carry Secure: only when the public address is
// https.
//
// Locally it is http://, and with Secure the browser does not store the cookie at all; it shows
// up as "login succeeds, but a refresh asks to log in again", with no error at all.
func (s Server) SecureCookie() bool {
	return strings.HasPrefix(strings.TrimSpace(s.PublicURL), "https://")
}

// Enabled reports whether the fixed API key channel is fully configured. Both Key and Email must
// be non-empty.
func (k APIKey) Enabled() bool {
	return strings.TrimSpace(k.Key) != "" && strings.TrimSpace(k.Email) != ""
}

// ToRedisConfig converts to shared/infra/redis's Config. Same direction as ToStdConfig; the
// dependency goes one way.
func (r Redis) ToRedisConfig() redis.Config {
	return redis.Config{
		Addr:        r.Addr,
		Password:    r.Password,
		DB:          r.DB,
		DialTimeout: r.DialTimeout,
		OpTimeout:   r.OpTimeout,
	}
}

// ToStdConfig converts to shared/infra/pg's StdConfig.
//
// The direction is config → infra, so infra does not need to know this package; the dependency
// goes one way.
func (p PG) ToStdConfig() pg.StdConfig {
	return pg.StdConfig{
		Host:             p.Host,
		Port:             p.Port,
		Database:         p.Database,
		User:             p.User,
		Password:         p.Password,
		SSLMode:          p.SSLMode,
		ConnectTimeout:   p.ConnectTimeout,
		MaxOpenConns:     p.MaxOpenConns,
		MaxIdleConns:     p.MaxIdleConns,
		ConnMaxIdleTime:  p.ConnMaxIdleTime,
		ConnMaxLifetime:  p.ConnMaxLifetime,
		StatementTimeout: p.StatementTimeout,
		LockTimeout:      p.LockTimeout,
	}
}
