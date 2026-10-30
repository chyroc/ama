// GitHub login: the OAuth device flow (RFC 8628); it only needs a client_id, no client secret.
//
// # Two steps
//
//	StartGitHubLogin  ask GitHub for a device_code + user_code, store the device_code,
//	                  give the browser a flow id + a binding cookie + the user_code
//	PollGitHubLogin   the browser polls at the interval: the poll that wins the chance takes the
//	                  device_code and exchanges it for a token; once it has one, it queries /user
//	                  and /user/emails, checks admission, and issues a session
//
// # All state is in PG (auth_github_flows)
//
// Starting on instance A and polling on instances B/C/D is normal. The device_code, the current
// interval and the next allowed poll time are all in the database, so any instance can continue
// polling, and **all instances share one interval**: N instances polling on their own would not
// multiply the poll rate to GitHub by N (which would bring slow_down).
//
// # One flow creates only one session
//
// Two gates, each a single SQL statement:
//
//  1. ClaimGitHubPoll: the `next_poll_at <= now` check and pushing it back are in the same UPDATE,
//     so among concurrent polls in one interval only one gets the device_code to ask GitHub;
//  2. ConsumeGitHubFlow: after getting the token, first `delete ... returning`; only the one that
//     deleted the row goes on to create a session.
//     Even if gate 1 is bypassed by something like clock skew, gate 2 still lets only one through.
//
// # The flow id cannot be claimed by someone else
//
// The flow id is sent to the browser (it needs it to poll), but **the flow id alone is not
// enough**: at start we also set an HttpOnly cookie (`ama_gh_<flow id>`, whose value is another
// 32 random bytes; the database stores only the sha256). A poll counts as the same person only
// when both the flow id and the cookie match. So it does not matter if the flow id shows up in
// logs, screenshots or browser history: anyone polling with it always gets "expired" and cannot
// take the login. The cookie uses SameSite=Strict + Path=/api/auth/github: requests started by
// other sites cannot carry it, and other endpoints of this site cannot see it. The cookie name
// includes the flow id, so two tabs in the same browser logging in separately do not overwrite
// each other.
//
// ⚠️ This does not stop the "code phishing" built into the device flow: an attacker starts a
// login themselves and sends the user_code to a victim to enter on GitHub; in that case the
// attacker holds the cookie anyway. The only mitigations are the admission allow list (a victim
// not on the list cannot get in) and the app name shown on GitHub's authorization page, so **do
// not borrow someone else's client_id for real users** (the authorization page would show their
// app name). When no client id is configured the default is exactly the GitHub CLI's
// (github.DefaultClientID), so the startup log reminds you to switch to your own.
//
// # Admission: only allowed when an allow list is configured
//
// Anyone can register a GitHub account, and the email is the Actor: no admission rule would open
// the platform to the whole world. So **when none of** allowedUsers / allowedEmailDomains /
// allowedOrgs **is configured, nobody gets in** (the startup log says so at Error level); with any
// of them set, "matching any one lets you in".
// It is deliberately not tied to the local accounts' "allow self sign-up": that switch controls
// "can people create their own accounts", while GitHub login does not create local accounts at
// all. Tying them together would make "open sign-up" accidentally mean "any GitHub user can get
// in".
//
// # Email
//
// Take the one in /user/emails that is **primary and verified**, lower-case it and use it as the
// Actor. If there is none, reject (do not fall back to the login or a noreply address): the email
// is the only basis for recognizing the same person across login methods.
//
// # The GitHub token is thrown away after use
//
// It only lives in local variables of PollGitHubLogin for a few API calls; it is not stored, not
// logged, not put in responses. The device flow has no client secret, so we cannot revoke it after
// use either. It stays in the user's GitHub "Authorized OAuth Apps", where the user can revoke it
// themselves without affecting the AMA session.

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chyroc/ama/server/shared/infra/github"
)

// Errors of GitHub login. The handler maps them to status codes and codes.
var (
	// ErrGitHubDisabled means GitHub login is not enabled. → 404
	ErrGitHubDisabled = errors.New("auth: 未启用 GitHub 登录")

	// ErrGitHubFlowExpired means this login can no longer be used: the flow does not exist / has
	// expired / the cookie does not match / GitHub says the device code expired / it was used up by
	// another poll. → 410
	//
	// Merged into one for the same reason as ErrLoginExpired: for the user it is always "start
	// again".
	ErrGitHubFlowExpired = errors.New("auth: 本次 GitHub 登录已失效，请重新开始")

	// ErrGitHubDenied means the user clicked cancel on GitHub's authorization page. → 403
	ErrGitHubDenied = errors.New("auth: 你在 GitHub 上取消了授权")

	// ErrGitHubNoVerifiedEmail means the GitHub account has no "primary and verified" email. → 403
	ErrGitHubNoVerifiedEmail = errors.New("auth: GitHub 账号没有已验证的主邮箱")

	// ErrGitHubNotAllowed means not on the admission allow list (or no allow list at all). → 403
	ErrGitHubNotAllowed = errors.New("auth: 这个 GitHub 账号没有登录本平台的权限")

	// ErrPollTooSoon is Store.ClaimGitHubPoll's "the row exists, but it is not yet time for the
	// next poll". Service turns it into a pending response; it never reaches the handler.
	ErrPollTooSoon = errors.New("auth: 还没到下一次轮询时间")
)

// githubSlowDownStep is how many seconds slow_down adds to the interval (RFC 8628 3.5 says 5).
const githubSlowDownStep = 5

// GitHubCookiePrefix is the name prefix of the binding cookie, followed by the flow id.
const GitHubCookiePrefix = "ama_gh_"

// GitHubClient is the GitHub capability Service depends on; the production implementation is
// *github.Client.
type GitHubClient interface {
	RequestDeviceCode(ctx context.Context) (github.DeviceCode, error)
	PollToken(ctx context.Context, deviceCode string) (github.TokenResult, error)
	GetUser(ctx context.Context, token string) (github.User, error)
	ListEmails(ctx context.Context, token string) ([]github.Email, error)
	IsOrgMember(ctx context.Context, token, org string) (bool, error)
	// ClientID is the client id actually used, only for the startup log.
	ClientID() string
}

// GitHubConfig is the GitHub login config. Client nil = disabled.
type GitHubConfig struct {
	// Client is the GitHub client. ⚠️ The wiring code must not assign a nil *github.Client to it
	// (same as Config.OIDC).
	Client GitHubClient

	// DisplayName is the name on the login page button; "GitHub" if empty.
	DisplayName string

	// Admission allow lists; matching any one lets you in; **all empty = nobody gets in**; see the
	// file header.
	//
	// AllowedUsers are GitHub user names (login), case-insensitive.
	AllowedUsers []string
	// AllowedEmailDomains match the part after @ of the primary email, e.g. example.com
	// (subdomains not included).
	AllowedEmailDomains []string
	// AllowedOrgs are GitHub organization names; full membership is required. **Needs the
	// read:org scope**.
	AllowedOrgs []string
}

// githubAuth is the GitHub part of Service; read-only after construction.
type githubAuth struct {
	client      GitHubClient
	displayName string
	users       map[string]bool
	domains     map[string]bool
	orgs        []string
}

func newGitHubAuth(cfg GitHubConfig) *githubAuth {
	if cfg.Client == nil {
		return nil
	}
	g := &githubAuth{
		client:      cfg.Client,
		displayName: strings.TrimSpace(cfg.DisplayName),
		users:       lowerSet(cfg.AllowedUsers, ""),
		domains:     lowerSet(cfg.AllowedEmailDomains, "@"),
	}
	if g.displayName == "" {
		g.displayName = "GitHub"
	}
	for _, o := range cfg.AllowedOrgs {
		if o = strings.TrimSpace(o); o != "" {
			g.orgs = append(g.orgs, o)
		}
	}
	return g
}

// lowerSet turns a list of values into a set after dropping empties, lower-casing and trimming
// prefixes (such as an @ written by mistake before a domain).
func lowerSet(in []string, trim string) map[string]bool {
	out := map[string]bool{}
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		v = strings.TrimPrefix(v, trim)
		if v != "" {
			out[v] = true
		}
	}
	return out
}

// GitHubEnabled reports whether GitHub login is enabled.
func (s *Service) GitHubEnabled() bool { return s.github != nil }

// GitHubClientID returns the GitHub client id actually used; empty when disabled. Only for the
// startup log.
func (s *Service) GitHubClientID() string {
	if s.github == nil {
		return ""
	}
	return s.github.client.ClientID()
}

// GitHubHasAllowlist reports whether at least one admission allow list is configured. Without
// one, nobody can log in with GitHub.
func (s *Service) GitHubHasAllowlist() bool {
	g := s.github
	return g != nil && (len(g.users) > 0 || len(g.domains) > 0 || len(g.orgs) > 0)
}

// GitHubStart is the result of starting a GitHub login.
type GitHubStart struct {
	// FlowID is sent to the browser and included when polling.
	FlowID string
	// BindingToken is the **plain value** of the binding cookie; it exists only at this moment
	// (the database has only the hash).
	BindingToken    string
	UserCode        string
	VerificationURI string
	Interval        time.Duration
	ExpiresAt       time.Time
}

// StartGitHubLogin asks GitHub for a device code and stores it.
func (s *Service) StartGitHubLogin(ctx context.Context) (GitHubStart, error) {
	if s.github == nil {
		return GitHubStart{}, ErrGitHubDisabled
	}
	dc, err := s.github.client.RequestDeviceCode(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "申请 GitHub device code 失败", slog.String("error", err.Error()))
		return GitHubStart{}, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
	}
	flowID, err := newToken()
	if err != nil {
		return GitHubStart{}, err
	}
	binding, err := newToken()
	if err != nil {
		return GitHubStart{}, err
	}
	now := s.now()
	interval := int(dc.Interval / time.Second)
	if interval <= 0 {
		interval = 5
	}
	flow := GitHubFlow{
		FlowID:          flowID,
		BindingHash:     hashToken(binding),
		DeviceCode:      dc.DeviceCode,
		UserCode:        dc.UserCode,
		VerificationURI: dc.VerificationURI,
		IntervalSeconds: interval,
		// The first poll also waits a full interval: the user has not entered the code on GitHub
		// yet, and asking early only earns a slow_down.
		NextPollAt: now.Add(time.Duration(interval) * time.Second),
		CreatedAt:  now,
		ExpiresAt:  now.Add(dc.ExpiresIn),
	}
	if err := s.store.PutGitHubFlow(ctx, flow); err != nil {
		return GitHubStart{}, err
	}
	return GitHubStart{
		FlowID:          flowID,
		BindingToken:    binding,
		UserCode:        dc.UserCode,
		VerificationURI: dc.VerificationURI,
		Interval:        time.Duration(interval) * time.Second,
		ExpiresAt:       flow.ExpiresAt,
	}, nil
}

// GitHubPoll is the result of one poll: when Pending is true the browser comes back after
// Interval; otherwise Login has a value.
type GitHubPoll struct {
	Pending  bool
	Interval time.Duration
	Login    LoginResult
}

// PollGitHubLogin handles one poll. Both flowID and bindingToken (the plain cookie value) must match.
func (s *Service) PollGitHubLogin(ctx context.Context, flowID, bindingToken string) (GitHubPoll, error) {
	g := s.github
	if g == nil {
		return GitHubPoll{}, ErrGitHubDisabled
	}
	flowID = strings.TrimSpace(flowID)
	bindingToken = strings.TrimSpace(bindingToken)
	if flowID == "" || bindingToken == "" {
		return GitHubPoll{}, ErrGitHubFlowExpired
	}

	now := s.now()
	flow, err := s.store.ClaimGitHubPoll(ctx, flowID, hashToken(bindingToken), now)
	switch {
	case errors.Is(err, ErrPollTooSoon):
		return GitHubPoll{Pending: true, Interval: seconds(flow.IntervalSeconds)}, nil
	case errors.Is(err, ErrNotFound):
		return GitHubPoll{}, ErrGitHubFlowExpired
	case err != nil:
		return GitHubPoll{}, err
	}

	tr, err := g.client.PollToken(ctx, flow.DeviceCode)
	if err != nil {
		s.log.ErrorContext(ctx, "GitHub 换 token 失败", slog.String("error", err.Error()))
		return GitHubPoll{}, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
	}
	switch tr.Error {
	case github.ErrCodeAuthorizationPending:
		return GitHubPoll{Pending: true, Interval: seconds(flow.IntervalSeconds)}, nil
	case github.ErrCodeSlowDown:
		n, err := s.store.SlowDownGitHubFlow(ctx, flowID, int(tr.Interval/time.Second), now)
		if errors.Is(err, ErrNotFound) {
			return GitHubPoll{}, ErrGitHubFlowExpired
		}
		if err != nil {
			return GitHubPoll{}, err
		}
		s.log.InfoContext(ctx, "GitHub 要求放慢轮询", slog.Int("interval", n))
		return GitHubPoll{Pending: true, Interval: seconds(n)}, nil
	case github.ErrCodeExpiredToken:
		s.dropGitHubFlow(ctx, flowID)
		return GitHubPoll{}, ErrGitHubFlowExpired
	case github.ErrCodeAccessDenied:
		s.dropGitHubFlow(ctx, flowID)
		return GitHubPoll{}, ErrGitHubDenied
	}

	// We have a token. **Delete the row before anything else**: only the one that deleted it
	// creates a session; see "One flow creates only one session" in the file header.
	won, err := s.store.ConsumeGitHubFlow(ctx, flowID, now)
	if err != nil {
		return GitHubPoll{}, err
	}
	if !won {
		return GitHubPoll{}, ErrGitHubFlowExpired
	}
	identity, err := s.githubIdentity(ctx, tr.AccessToken)
	if err != nil {
		return GitHubPoll{}, err
	}
	res, err := s.finishLogin(ctx, identity, "github")
	if err != nil {
		return GitHubPoll{}, err
	}
	return GitHubPoll{Login: res}, nil
}

// githubIdentity uses the token to look up the user and emails and checks admission. The token is
// only used inside this function.
func (s *Service) githubIdentity(ctx context.Context, token string) (Identity, error) {
	g := s.github
	user, err := g.client.GetUser(ctx, token)
	if err != nil {
		s.log.ErrorContext(ctx, "查 GitHub 用户失败", slog.String("error", err.Error()))
		return Identity{}, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
	}
	emails, err := g.client.ListEmails(ctx, token)
	if err != nil {
		// A 403 is most likely user:email missing from the scopes.
		s.log.ErrorContext(ctx, "查 GitHub 邮箱失败（scopes 里有 user:email 吗）",
			slog.String("login", user.Login), slog.String("error", err.Error()))
		return Identity{}, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
	}
	email := ""
	for _, e := range emails {
		if e.Primary && e.Verified {
			email = normalizeEmail(e.Email)
			break
		}
	}
	if email == "" || !strings.Contains(email, "@") {
		s.log.WarnContext(ctx, "GitHub 账号没有已验证的主邮箱，拒绝登录", slog.String("login", user.Login))
		return Identity{}, ErrGitHubNoVerifiedEmail
	}

	ok, err := s.githubAdmitted(ctx, token, user.Login, email)
	if err != nil {
		return Identity{}, err
	}
	if !ok {
		s.log.WarnContext(ctx, "GitHub 账号不在准入白名单里，拒绝登录",
			slog.String("login", user.Login), slog.String("email", email),
			slog.Bool("配了白名单", s.GitHubHasAllowlist()))
		return Identity{}, ErrGitHubNotAllowed
	}

	name := strings.TrimSpace(user.Name)
	if name == "" {
		name = user.Login
	}
	return Identity{Email: email, Name: name, AvatarURL: user.AvatarURL}, nil
}

// githubAdmitted decides admission: matching any allow list lets you in, all empty rejects.
// Cheap checks first; organization membership needs extra GitHub API calls, so it goes last.
func (s *Service) githubAdmitted(ctx context.Context, token, login, email string) (bool, error) {
	g := s.github
	if g.users[strings.ToLower(login)] {
		return true, nil
	}
	if i := strings.LastIndex(email, "@"); i >= 0 && g.domains[email[i+1:]] {
		return true, nil
	}
	for _, org := range g.orgs {
		member, err := g.client.IsOrgMember(ctx, token, org)
		if err != nil {
			s.log.ErrorContext(ctx, "查 GitHub 组织成员失败", slog.String("org", org), slog.String("error", err.Error()))
			return false, fmt.Errorf("%w: %v", ErrLoginUnavailable, err)
		}
		if member {
			return true, nil
		}
	}
	return false, nil
}

// dropGitHubFlow deletes a flow that is no longer needed. A failure is only logged: the row would
// be cleaned up when it expires anyway.
func (s *Service) dropGitHubFlow(ctx context.Context, flowID string) {
	if err := s.store.DeleteGitHubFlow(ctx, flowID); err != nil {
		s.log.WarnContext(ctx, "删除 GitHub 登录流程失败", slog.String("error", err.Error()))
	}
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }
