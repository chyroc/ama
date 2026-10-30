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
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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
	// EnvArkSecretKey is the master key that encrypts users' Ark credentials. **It is not
	// ARK_API_KEY**, see Ark.SecretKey.
	EnvArkSecretKey = "ARK_SECRET_KEY"

	EnvDockerEnabled      = "AMA_DOCKER_ENABLED"
	EnvDockerHost         = "AMA_DOCKER_HOST"
	EnvDockerImage        = "AMA_DOCKER_IMAGE"
	EnvDockerWorkdir      = "AMA_DOCKER_WORKDIR"
	EnvDockerPrefix       = "AMA_DOCKER_PREFIX"
	EnvDockerCPUs         = "AMA_DOCKER_CPUS"
	EnvDockerMemory       = "AMA_DOCKER_MEMORY"
	EnvDockerPidsLimit    = "AMA_DOCKER_PIDS_LIMIT"
	EnvDockerNetwork      = "AMA_DOCKER_NETWORK"
	EnvDockerIdleAction   = "AMA_DOCKER_IDLE_ACTION"
	EnvDockerContainerTTL = "AMA_DOCKER_CONTAINER_TTL"
	EnvDockerWorkspaceTTL = "AMA_DOCKER_WORKSPACE_TTL"
	EnvWorkerConcurrency  = "AMA_WORKER_CONCURRENCY"

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
