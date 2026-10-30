// Local accounts: email + password.
//
// # Three entry points
//
//	Signup             self sign-up. With open sign-up anyone can register; when closed, a first
//	                   account can only be registered at the moment "there are no users at all in
//	                   the database" (it automatically becomes the platform admin)
//	LoginWithPassword  password login, rate limited by failure count
//	CreateLocalUser    a platform admin creates an account for someone (the only way to add people
//	                   when self sign-up is closed)
//
// # Only the bcrypt hash of the password is stored
//
// bcrypt includes the salt and cost; the string in the database is enough to verify, not enough
// to recover. The default cost is 12 (about 200~300ms per check): it makes offline brute force
// expensive without making the login endpoint so slow it can be used as a DoS amplifier.
// bcrypt only looks at the first 72 bytes, so the password length limit is 72 bytes. Anything
// beyond that would be **silently ignored**, which would make the user believe they set a longer
// password, so we reject it outright.
//
// # Rate limiting must live outside the process
//
// Failure counts are stored in PG (auth_login_failures), counted per email, in a fixed window.
// With an in-process map, N instances give N times the attempt budget and every restart resets
// it, which is the same as no rate limit. See the DDL comment of that table.
//
// ⚠️ Counted only per email, **not per source IP**: in production we are behind a load balancer,
// so RemoteAddr is the gateway's address; counting by it would make everyone share one budget
// (one person typing a wrong password a few times locks the whole site out), and
// X-Forwarded-For can be forged by the caller. The cost of counting per email is "someone else
// can lock you out for 15 minutes on purpose by typing wrong passwords"; it recovers by itself
// when the window passes.

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Errors related to local accounts. The handler maps them to HTTP status codes and error codes.
var (
	// ErrLocalDisabled means local account login is not enabled. → 404
	ErrLocalDisabled = errors.New("auth: 未启用邮箱密码登录")

	// ErrInvalidCredentials means the email or password is wrong. → 401
	//
	// **It does not tell "no such account" apart from "wrong password"**: doing so would give
	// attackers an endpoint to enumerate emails.
	ErrInvalidCredentials = errors.New("auth: 邮箱或密码不正确")

	// ErrTooManyAttempts means too many failures; further attempts are rejected inside the window. → 429
	ErrTooManyAttempts = errors.New("auth: 尝试次数过多，请稍后再试")

	// ErrSignupDisabled means self sign-up is closed (and the database already has users). → 403
	ErrSignupDisabled = errors.New("auth: 未开放注册，请联系平台管理员创建账号")

	// ErrEmailTaken means this email already has a local account. → 409
	ErrEmailTaken = errors.New("auth: 该邮箱已注册")

	// ErrWeakPassword means the password does not meet the length requirement. → 400
	ErrWeakPassword = errors.New("auth: 密码长度需要在 8 到 72 字节之间")

	// ErrForbidden means logged in but not a platform admin. → 403
	ErrForbidden = errors.New("auth: 只有平台管理员可以执行这个操作")
)

const (
	// defaultBcryptCost: see the file header.
	defaultBcryptCost = 12

	minPasswordBytes = 8
	// maxPasswordBytes is bcrypt's input limit; see the file header.
	maxPasswordBytes = 72

	defaultMaxFailures   = 10
	defaultFailureWindow = 15 * time.Minute
)

// LocalConfig is the local account config. Zero value = disabled.
type LocalConfig struct {
	// Enabled turns on email + password login.
	Enabled bool

	// AllowSignup opens self sign-up. **When it is off, registering the first account is still
	// allowed** (when there are no users at all in the database); otherwise a fresh deployment
	// could never let in its first person.
	AllowSignup bool

	// MaxFailures is the number of failures allowed in the window; 10 if empty.
	MaxFailures int

	// FailureWindow is the counting window; 15 minutes if empty.
	FailureWindow time.Duration
}

func (c LocalConfig) withDefaults() LocalConfig {
	if c.MaxFailures <= 0 {
		c.MaxFailures = defaultMaxFailures
	}
	if c.FailureWindow <= 0 {
		c.FailureWindow = defaultFailureWindow
	}
	return c
}

// failureKey is the key of the rate-limit counter. It has a prefix; see the comment on
// auth_login_failures.key.
func failureKey(email string) string { return "email:" + email }

// validateEmail only checks "it looks like an email" and returns the lower-cased value.
//
// It uses net/mail instead of a regular expression: it accepts RFC 5322 addresses. It also
// requires no display name (something like `John Doe <a@b>` is accepted by ParseAddress but
// should not be stored as an email).
func validateEmail(raw string) (string, error) {
	e := strings.TrimSpace(raw)
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Name != "" || addr.Address != e || !strings.Contains(e, "@") {
		return "", fmt.Errorf("%w: 邮箱格式不正确", ErrInvalidArgument)
	}
	return normalizeEmail(e), nil
}

func validatePassword(pw string) error {
	if n := len(pw); n < minPasswordBytes || n > maxPasswordBytes {
		return ErrWeakPassword
	}
	return nil
}

// Signup registers a local account by self sign-up and logs in on success (returns a session).
//
// Order of decisions:
//
//  1. First try "register the first account" (when there are no users in the database); it
//     becomes the platform admin;
//  2. Not the first: with open sign-up create a normal account, otherwise ErrSignupDisabled.
//
// Step 1 is not limited by AllowSignup; otherwise a fresh deployment with sign-up closed could
// never let in its first person.
// "Is this the first one" is decided by the single statement in CreateFirstLocalAccount; see the
// Store comment.
func (s *Service) Signup(ctx context.Context, email, password, name string) (LoginResult, error) {
	if !s.local.Enabled {
		return LoginResult{}, ErrLocalDisabled
	}
	e, err := validateEmail(email)
	if err != nil {
		return LoginResult{}, err
	}
	if err := validatePassword(password); err != nil {
		return LoginResult{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.bcryptCost)
	if err != nil {
		return LoginResult{}, fmt.Errorf("计算密码哈希失败: %w", err)
	}
	now := s.now()
	acct := LocalAccount{Email: e, PasswordHash: string(hash), CreatedAt: now, UpdatedAt: now}

	first := acct
	first.Admin = true
	created, err := s.store.CreateFirstLocalAccount(ctx, first)
	if err != nil {
		return LoginResult{}, err
	}
	if created {
		s.log.InfoContext(ctx, "注册了第一个本地账号，它是平台管理员", slog.String("email", e))
	} else {
		if !s.local.AllowSignup {
			return LoginResult{}, ErrSignupDisabled
		}
		created, err = s.store.CreateLocalAccount(ctx, acct)
		if err != nil {
			return LoginResult{}, err
		}
		if !created {
			return LoginResult{}, ErrEmailTaken
		}
	}
	return s.finishLogin(ctx, localIdentity(e, name), "local_signup")
}

// LoginWithPassword logs in with email + password.
//
// Rate limiting is "record one failure first, then verify the password, clear on success"; see
// Store.RecordLoginFailure for why.
// Over the limit, the **password is not verified**: it saves one bcrypt run and makes "guessed
// right while over the limit" impossible.
func (s *Service) LoginWithPassword(ctx context.Context, email, password string) (LoginResult, error) {
	if !s.local.Enabled {
		return LoginResult{}, ErrLocalDisabled
	}
	e := normalizeEmail(email)
	if e == "" || password == "" {
		return LoginResult{}, fmt.Errorf("%w: 邮箱与密码都不能为空", ErrInvalidArgument)
	}
	key := failureKey(e)
	n, err := s.store.RecordLoginFailure(ctx, key, s.now(), s.local.FailureWindow)
	if err != nil {
		// If the counter cannot be written, reject the login: letting people in when rate
		// limiting is broken is removing the gate, with no error at all.
		return LoginResult{}, err
	}
	if n > s.local.MaxFailures {
		s.log.WarnContext(ctx, "密码登录尝试过多，已拒绝", slog.String("email", e), slog.Int("attempts", n))
		return LoginResult{}, ErrTooManyAttempts
	}

	acct, err := s.store.GetLocalAccount(ctx, e)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return LoginResult{}, err
		}
		// Run bcrypt once even when the account does not exist: otherwise "no such account" is
		// two orders of magnitude faster than "wrong password", and the response time alone
		// reveals which emails are registered.
		_ = bcrypt.CompareHashAndPassword(s.dummyHash(), []byte(password))
		return LoginResult{}, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(acct.PasswordHash), []byte(password)); err != nil {
		return LoginResult{}, ErrInvalidCredentials
	}

	if err := s.store.ClearLoginFailures(ctx, key); err != nil {
		// Failing to clear only costs this person a few attempts; it should not make a correct
		// login fail.
		s.log.WarnContext(ctx, "清除登录失败计数失败", slog.String("email", e), slog.String("error", err.Error()))
	}
	// The display name comes from the profile (the one entered at sign-up). If not found, fall
	// back to the email prefix; it does not affect login.
	name := ""
	if u, err := s.store.GetUser(ctx, e); err == nil {
		name = u.Name
	}
	return s.finishLogin(ctx, localIdentity(e, name), "local_password")
}

// CreateLocalUser lets a platform admin create a local account for someone. It **does not log
// in** the new account; it only returns its identity.
func (s *Service) CreateLocalUser(ctx context.Context, actor, email, password, name string, admin bool) (Identity, error) {
	if !s.local.Enabled {
		return Identity{}, ErrLocalDisabled
	}
	ok, err := s.IsPlatformAdmin(ctx, actor)
	if err != nil {
		return Identity{}, err
	}
	if !ok {
		return Identity{}, ErrForbidden
	}
	e, err := validateEmail(email)
	if err != nil {
		return Identity{}, err
	}
	if err := validatePassword(password); err != nil {
		return Identity{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.bcryptCost)
	if err != nil {
		return Identity{}, fmt.Errorf("计算密码哈希失败: %w", err)
	}
	now := s.now()
	created, err := s.store.CreateLocalAccount(ctx, LocalAccount{
		Email: e, PasswordHash: string(hash), Admin: admin,
		CreatedBy: normalizeEmail(actor), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return Identity{}, err
	}
	if !created {
		return Identity{}, ErrEmailTaken
	}
	identity := localIdentity(e, name)
	// Create a profile first so the member list can show the name before their first login.
	// A failure is only logged.
	if err := s.store.PutUser(ctx, User{Email: e, Name: identity.Name, CreatedAt: now, UpdatedAt: now}); err != nil {
		s.log.WarnContext(ctx, "保存用户档案失败", slog.String("error", err.Error()))
	}
	s.log.InfoContext(ctx, "平台管理员创建了本地账号",
		slog.String("actor", normalizeEmail(actor)), slog.String("email", e), slog.Bool("admin", admin))
	return identity, nil
}

// localIdentity builds the identity of a local account. When the display name is empty, the
// email prefix is used.
func localIdentity(email, name string) Identity {
	n := strings.TrimSpace(name)
	if n == "" {
		n = emailPrefix(email)
	}
	return Identity{Email: email, Name: n}
}

var (
	dummyOnce sync.Once
	dummyVal  []byte
)

// dummyHash is the hash used for a dummy comparison "when the account does not exist"; its cost
// matches real hashes.
// Cached in the process: it is just a constant; if lost it is computed again.
func (s *Service) dummyHash() []byte {
	dummyOnce.Do(func() {
		h, err := bcrypt.GenerateFromPassword([]byte("ama-dummy-password-for-timing"), s.bcryptCost)
		if err == nil {
			dummyVal = h
		}
	})
	return dummyVal
}
