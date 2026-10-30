// Service of the Ark Credential domain: what it needs to do its work.
//
// Step 8 of the package-split plan. The shape copies the previous five domains (issue / suggestion / revision / schedule /
// workspace), and the check is the same: `go list -deps ./service/arkcred` contains no core.
//
// # This package covers "which key is in effect", not "use the key to do work"
//
// The boundary is not file names but this sentence: **this domain only handles storing, reading and resolving the credential itself** (where it is stored, whether it was verified,
// which of the three levels finally takes effect). Using the resolved key to create Ark Agents, run conversations, upload skills,
// belongs to the run/chat path, **not this package** (core/arkrun.go, core/chat.go).
//
// So the 882 lines of core/ark.go were cut along this line, not moved as a whole file: the first half (credentials) is here,
// the second half (run history ListRuns / GetRun / backfill) stays in core.
//
// # The three credential levels and their fallback order (this order must not change)
//
//	agent level       agent_ark_secrets    ← most specific, highest priority
//	workspace level   ark_secrets
//	account level     global_ark_secrets   ← configured once globally, referenced by workspaces
//
// **More specific wins**, with two entry points:
//
//	ResolveSecret(ws)              the settings page asks "which key does this workspace use by default"
//	ResolveSecretForAgent(ws, id)  conversations and the worker ask "which key does this agent use"
//
// ⚠️ **A lower level never stores a copy of an upper level.** This is how "changing the upper level links through at once" works, and it is the invariant
// in this domain most easily broken with good intentions: casually copying the upper level's key into the lower row ("faster to read") makes changing the upper level
// never take effect, while the UI still shows "from global settings / following workspace settings". **Zero errors, looks right
// at the time, found out days later.** The warnings for each of the three write paths are on the methods; read them before changing anything.
//
// # Five dependencies
//
//	Store        the three tables ark_secrets / global_ark_secrets / agent_ark_secrets,
//	             plus two read-only tables from other domains (workspaces / agents); see store.go
//	Verifier     calls an Ark read-only endpoint for real with a key, **may be nil**
//	KeyFetcher   lists/fetches keys from a Volcano Engine account, **may be nil**
//	ActorEmail   "the person doing this", used to write UpdatedBy. Required
//	OwnerEmailOf "who is the first owner of this workspace", used to write UpdatedBy. Required
//	Logger       optional
//
// The last two are func fields instead of interfaces; see their comments on Config for why: they must be
// **the same function** as on the core side, and injecting a function says that more clearly than declaring an interface.
package arkcred

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf16"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// VerifyStatus classifies the conclusion of "calling an Ark read-only endpoint for real with a key".
//
// Saving a credential splits into three outcomes by it:
//
//	VerifyStatusInvalidKey   hard error, **saving is blocked**: the key is invalid/deleted, and saving it makes the first conversation fail for sure;
//	VerifyStatusNotActivated warning, **saving is allowed** after the user confirms: the key itself is right,
//	                         they may want to set it up first and activate in the Ark console later;
//	VerifyStatusSkipped      **allowed right away**: timeout/5xx/any non-business error means our dependency
//	                         has a problem, and that must not stop the user from saving config.
type VerifyStatus string

const (
	// VerifyStatusOK: Ark accepts this key, and the account has activated Managed Agents.
	VerifyStatusOK VerifyStatus = "ok"
	// VerifyStatusInvalidKey: 401/403, authentication failed (key invalid or deleted).
	VerifyStatusInvalidKey VerifyStatus = "invalid_key"
	// VerifyStatusNotActivated: the key is valid, but Ark returns 404 + code=ManagedAgentNotOpen;
	// this account has not activated the Managed Agents product yet.
	VerifyStatusNotActivated VerifyStatus = "not_activated"
	// VerifyStatusSkipped: the check did not complete this time (timeout, 5xx, 429, any unrecognized error).
	// Deliberately not called unknown: its handling is definite (allow), it just got no conclusion.
	VerifyStatusSkipped VerifyStatus = "skipped"
)

// VerifyResult is the structured conclusion of one read-only probe.
//
// It classifies **only by structured status codes/error codes** (implementation in core/arkverify.go, checks in
// shared/infra/ma/errors.go), never by matching Ark's Chinese message text: text changes, the code is the contract.
type VerifyResult struct {
	Status VerifyStatus
	// AccountID is set only for NotActivated: the account id extracted from Ark's raw error.
	// In the AMA UI the user cannot tell which account a key belongs to, so activation guidance must include it.
	AccountID string
	// RequestID is the request id from Ark, evidence for bug reports; may be present for InvalidKey / NotActivated.
	RequestID string
	// Cause is the technical reason for Skipped, **only for logs**, never put into user-facing text.
	Cause error
}

// Verifier calls an Ark read-only endpoint for real with a key and returns a structured conclusion.
//
// **The interface is declared on the consumer side**: the implementation is LiveArkVerifier in core (it imports shared/infra/ma
// and hits real Ark), wired by cmd/amad. It is an interface so this domain's tests need no network.
//
// The implementation's classification rules (the boundaries of the three outcomes are this function's contract; changing it changes save blocking):
//   - only 401/403 counts as InvalidKey;
//   - only 404 + code=ManagedAgentNotOpen counts as NotActivated (the same 404 also means
//     the resource does not exist, so the status code alone is not enough);
//   - everything else (including failing to build the client, timeout, 5xx, 429, unrecognized errors) is Skipped.
//
// The last rule is the fallback against false positives: when the probe cannot detect NotActivated, the worst case is just "one missing
// not-activated warning", never wrongly blocking a valid but not-activated key as a bad key.
type Verifier interface {
	Verify(ctx context.Context, apiKey string, region apimodel.ArkRegion) VerifyResult
}

// ErrArkKeyInvalid means Ark explicitly rejected this key (401/403), as opposed to a network failure.
//
// **The real one is here; the core side has an alias of the same name** (the same variable, so errors.Is is equivalent).
//
// The save path does not use it (it uses the structured three outcomes of VerifyResult.Status); it is kept because core's
// alias and older call sites reference this sentinel. The history of the frontend verifyArkCredential having no try/catch
// is in the comment on VerifyArkCredential.
var ErrArkKeyInvalid = errors.New("core: 方舟拒绝了这把 API Key")

// When saving is blocked by the online check, the response carries a stable detail code the frontend can branch on programmatically (the HTTP status is still 422).
//
// The frontend cannot branch on Chinese sentences, and the handler encodes every ErrPrecondition as
// precondition_failed by default; these two codes let it tell "bad key, dead end" from "not activated, can save after confirming".
const (
	// RejectCodeInvalidKey: bad key, no "save anyway" way out.
	RejectCodeInvalidKey = "ark_key_invalid"
	// RejectCodeNotActivated: not activated; can be saved after the user confirms (draft.ConfirmNotActivated).
	RejectCodeNotActivated = "ark_managed_agent_not_open"
)

// SaveRejectedError is the business error "saving was blocked by the online check".
//
// It wraps ErrPrecondition (the handler returns 422) and carries a stable detail code FailCode();
// the handler's fail() prefers the detail code, and the frontend uses it to decide between "hard error" and "save after confirming".
type SaveRejectedError struct {
	code string
	msg  string
}

// NewSaveRejectedError builds a save-blocked error. code may only be one of the two detail codes above.
func NewSaveRejectedError(code, msg string) *SaveRejectedError {
	return &SaveRejectedError{code: code, msg: msg}
}

func (e *SaveRejectedError) Error() string { return e.msg }

// Unwrap makes errors.Is(err, persistmodel.ErrPrecondition) hold → the handler returns 422.
func (e *SaveRejectedError) Unwrap() error { return persistmodel.ErrPrecondition }

// FailCode returns the stable detail code for the handler to write into the code field of the error envelope.
func (e *SaveRejectedError) FailCode() string { return e.code }

// maskSecret masks a secret.
//
// **The real one is persistmodel.MaskSecret** (both this package and repository/dao use it to build/assert
// masks, and it is a zero-dependency pure string function). It behaves byte for byte the same as maskSecret in the frontend api.ts;
// the rules are there.
var maskSecret = persistmodel.MaskSecret

// validRegions is the two regions supported by the frontend's ARK_REGIONS.
//
// Unrecognized regions are always rejected; **do not silently fall back to cn-beijing**: that would make the user think they picked
// Singapore, while the key is actually sent to the Beijing endpoint and gets 401 (see the file header of core/arkverify.go).
var validRegions = map[apimodel.ArkRegion]bool{
	apimodel.RegionCNBeijing:    true,
	apimodel.RegionAPSoutheast1: true,
}

// Config is what is needed to build a Service.
type Config struct {
	// Store is this domain's persistence. Required.
	Store Store

	// Verifier checks whether a key works.
	//
	// **May be nil** (tests that do not touch Ark and local runs need no network); then the three Verify methods return
	// a precondition error "the server has no Ark verifier configured", and everything else works as usual.
	Verifier Verifier

	// KeyFetcher supports "automatically get an Ark API Key from a Volcano Engine account".
	//
	// **May be nil** (same meaning as Verifier, not a shutdown): then the related endpoints return a clear
	// precondition error, while **pasting a key by hand still works**. See fetch.go.
	KeyFetcher KeyFetcher

	// ActorEmail gets the email of "the person doing this", written into UpdatedBy of agent-level credentials. Required.
	//
	// # Why an injected function, instead of this package reading ctx itself
	//
	// It must follow **the same rule** as writing the owner when creating a workspace (on the core side that is ownerFrom →
	// workspacesvc.ActingOwner, falling back to a default owner when there is no login identity). If this package
	// implemented it again, "who changed it" would give different answers on different endpoints, with zero errors.
	//
	// ⚠️ It is **deliberately not the same thing** as this package's actorFrom: that one is for account-level credentials (the primary key is the email,
	// must be lowercase, errors when missing), this one is for audit trail (has a fallback when missing). In core the two
	// have always been two functions; merging them would change behavior.
	ActorEmail func(ctx context.Context) string

	// OwnerEmailOf gets the email of a workspace's "first owner", written into UpdatedBy of workspace-level credentials. Required.
	//
	// It is also an injected function: it **must have the same source** as WorkspaceOwnerOf (the real one is
	// workspacesvc.FirstOwnerEmail). That method tells the user on the authorization card "whose authorization
	// is actually in effect", and a mismatch with the person recorded here gives zero errors.
	//
	// ⚠️ A function is injected instead of workspacesvc because **zero imports between domain packages** is a hard rule.
	OwnerEmailOf func(members []apimodel.WorkspaceMember) string

	// Logger is optional; empty uses slog.Default().
	//
	// ⚠️ This domain has three **fail-open** degradations (probing the Volcano Engine account fails, getting the authorization page URL fails,
	// arkEffective cannot be computed), and the log is the only trace of those failures. Pass in a real logger.
	Logger *slog.Logger
}

// Service is the business logic of the Ark credential domain.
type Service struct {
	store    Store
	verifier Verifier
	fetcher  KeyFetcher
	logger   *slog.Logger

	actorEmail   func(ctx context.Context) string
	ownerEmailOf func(members []apimodel.WorkspaceMember) string
}

// New builds a Service. Store / ActorEmail / OwnerEmailOf are all required; the rest are optional.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("arkcred: Store 不能为 nil")
	}
	if cfg.ActorEmail == nil {
		return nil, errors.New("arkcred: ActorEmail 不能为 nil")
	}
	if cfg.OwnerEmailOf == nil {
		return nil, errors.New("arkcred: OwnerEmailOf 不能为 nil")
	}
	return &Service{
		store:        cfg.Store,
		verifier:     cfg.Verifier,
		fetcher:      cfg.KeyFetcher,
		logger:       cfg.Logger,
		actorEmail:   cfg.ActorEmail,
		ownerEmailOf: cfg.OwnerEmailOf,
	}, nil
}

func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// actorFrom gets the current logged-in user; returns ErrInvalidArgument if not logged in.
//
// **Returns 400 instead of 401/403**, exactly like core's preferencesActor: the account-level credential
// routes are behind auth.Protect anyway, so reaching here without an identity means the caller did not come through HTTP (a test or
// an internal call), which is an argument problem. The real "not logged in" gets 401 from the middleware further out.
//
// Lowercasing is **defense in depth**: the account-level credential's primary key is this email, and inconsistent case would give the same person
// two records, showing up as "the global key I configured disappears now and then".
func actorFrom(ctx context.Context) (string, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return "", fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	return strings.ToLower(actor), nil
}

// ValidateAPIKey validates an Ark API Key, the same as validateArkApiKey in the frontend domain.ts.
//
// It returns user-readable Chinese text; e2e 08-ark-credential asserts the exact string.
//
// The prefix is not fixed (console-generated keys look like ARKxxxx, and plain UUIDs have been seen too), so only minimum length and character set
// are checked, without guessing the prefix: guessing wrong would lock valid keys out.
func ValidateAPIKey(key string) error {
	k := strings.TrimSpace(key)
	if k == "" {
		return fmt.Errorf("%w: API Key 不能为空", persistmodel.ErrInvalidArgument)
	}
	// Matches JS .length, counting UTF-16 code units
	if len(utf16.Encode([]rune(k))) < 16 {
		return fmt.Errorf("%w: API Key 长度不足，请确认是否复制完整", persistmodel.ErrInvalidArgument)
	}
	// JS \s accepts U+FEFF in addition to what Go's unicode.IsSpace accepts, so include it explicitly here
	if strings.ContainsFunc(k, isJSSpace) {
		return fmt.Errorf("%w: API Key 不应包含空格", persistmodel.ErrInvalidArgument)
	}
	return nil
}

// isJSSpace reports whether a character matches \s in a JS regex.
//
// JS \s = [\f\n\r\t\v    -     　U+FEFF].
// It differs from Go's unicode.IsSpace in U+FEFF (BOM): Go does not count it as whitespace,
// and copy-pasting a key is exactly when a BOM tends to sneak in.
//
// ⚠️ A literal BOM cannot be written in a comment; Go reports illegal byte order mark, so only U+FEFF can be written.
func isJSSpace(r rune) bool {
	switch r {
	case '\f', '\n', '\r', '\t', '\v', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// resolveDraftKey turns a draft into one plaintext key.
//
// Two sources: typed by the user (APIKey), or fetched from a Volcano Engine account (Fetch). **They are mutually exclusive**:
// with both given it is unclear which to use, and silently picking one would quietly disable the other (the user thinks they picked
// some key, while what gets stored is the typed one).
func (s *Service) resolveDraftKey(ctx context.Context, draft apimodel.ArkCredentialDraft) (string, error) {
	hasManual := strings.TrimSpace(draft.APIKey) != ""
	hasFetch := draft.Fetch != nil

	switch {
	case hasManual && hasFetch:
		return "", fmt.Errorf("%w: 不能同时手填 API Key 和从火山账号获取", persistmodel.ErrInvalidArgument)
	case hasFetch:
		return s.fetchPlainKey(ctx, draft.Fetch.AccountID, draft.Fetch.KeyID)
	default:
		// The typed path (including an empty string; ValidateAPIKey reports that error,
		// and its text is meant for users).
		return draft.APIKey, nil
	}
}
