// Business logic for "automatically get an Ark API Key from a Volcano Engine account".
//
// # It is a shortcut, not the main path
//
// Pasting a key by hand is always kept. This path only saves the steps "go to the Volcano Engine console, find the API Key management page,
// copy, paste back", and it depends on a **reverse-engineered** frontend signature and a set of undocumented console
// internal endpoints, which may break whenever their frontend changes. So any failure in this file should only show up as
// "automatic fetch unavailable, please fill it in by hand", and must never block the user on this path.
//
// # The plaintext never goes through the frontend
//
// The three read-only endpoints (auth status / account list / key list) **return only masks and ids**. After the user picks one,
// the frontend sends back `{accountId, keyId}`, and the server **fetches the plaintext, computes the mask, and stores it encrypted**.
//
// The reverse (the frontend gets the plaintext and PUTs it back) looks simpler, since it reuses the existing save endpoint,
// but it sends a long-lived Ark key through the browser, into the devtools network panel, possibly readable by extensions.
// The plaintext should only flow along "server memory → database".
//
// # There is no create endpoint, so it is "list existing ones for the user to pick"
//
// Volcano Engine offers no OpenAPI for creating an Ark API Key (grep CreateApiKey across the whole repo: zero hits).
// That conveniently avoids the risk of "one new key per click, piling up unused keys".
//
// # Why this goes with the credential domain instead of its own package
//
// It has **only one consumer**: the three save paths of the three credential levels (resolveDraftKey). And what it produces
// is a plaintext key, exactly the core asset of this domain. For the rule "plaintext stays on the server" to hold,
// fetching and storing the plaintext must be visible in the same package. The line on fetchPlainKey,
// "the return value must never go into an HTTP response", would be just a comment across packages.
package arkcred

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// KeyFetcher abstracts "list Ark API Keys from a Volcano Engine account" into an interface.
//
// **The interface is declared on the consumer side**, like Verifier (this package does not import the concrete implementation).
//
// **It may be nil, and cmd/amad currently passes nil**: the user pastes the key by hand. Then the whole automatic fetch feature
// is unavailable; the endpoints return a clear precondition error, and pasting by hand works as usual. The interface is kept as an extension point:
// to support "list keys from the user's Volcano Engine account on their behalf" later, implement it and wire it in cmd/amad.
type KeyFetcher interface {
	// AuthorizeURL gives an authorization page URL for the user to authorize once.
	AuthorizeURL(ctx context.Context) (string, error)
	// ListAccounts lists the Volcano Engine accounts this person has authorized.
	ListAccounts(ctx context.Context, subject string) ([]apimodel.VolcAccount, error)
	// ListKeys lists the Ark API Keys under a Volcano Engine account (only metadata and masks).
	ListKeys(ctx context.Context, subject, accountID string) ([]apimodel.ArkKeyOption, error)
	// Reveal fetches a key's plaintext. **The return value must never go into an HTTP response.**
	Reveal(ctx context.Context, subject, accountID, keyID string) (string, error)
	// IsNotAuthorized checks whether an error means "this person has not authorized yet".
	//
	// It is provided by the implementation instead of this package matching error strings: that check decides whether the UI guides to "go authorize"
	// or shows "fetch failed", and getting it wrong leaves the user staring at a problem they could fix with one click.
	IsNotAuthorized(err error) bool
}

// GetArkFetchStatus checks whether automatic fetch is currently available and whether the current user has authorized.
//
// **The three states must stay separate** (unavailable / not authorized / available), because what the user must do is completely different:
// unavailable → nothing to do, fill it in by hand; not authorized → just click the authorization page; available → pick directly.
// Merging them into two states would show the "one click fixes it" case as "feature unavailable".
func (s *Service) GetArkFetchStatus(ctx context.Context) (apimodel.ArkFetchStatus, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.ArkFetchStatus{}, err
	}
	if s.fetcher == nil {
		return apimodel.ArkFetchStatus{
			Available: false,
			Reason:    "服务端未开启「从火山账号获取」，请手动填写 API Key",
		}, nil
	}

	// Use the account list as the probe: it is the lightest hop on the whole path, and **whether authorized can only be known by calling once**
	// (upstream has no "has this person authorized" endpoint).
	if _, err := s.fetcher.ListAccounts(ctx, actor); err != nil {
		if s.fetcher.IsNotAuthorized(err) {
			url, uerr := s.fetcher.AuthorizeURL(ctx)
			if uerr != nil {
				// Without an authorization page URL the only option is to degrade to "unavailable", but say clearly that this step failed.
				s.log().Warn("取方舟凭证授权页地址失败", "error", uerr.Error())
				return apimodel.ArkFetchStatus{
					Available: false,
					Reason:    "无法生成授权链接，请手动填写 API Key",
				}, nil
			}
			return apimodel.ArkFetchStatus{Available: true, Authorized: false, AuthorizeURL: url}, nil
		}
		// Other failures (network, platform changes) **must not be reported as "not authorized"**: that would send the user to authorize again and again,
		// while the problem is not on their side.
		s.log().Warn("探测火山账号列表失败", "actor", actor, "error", err.Error())
		return apimodel.ArkFetchStatus{
			Available: false,
			Reason:    "暂时无法连接火山账号服务，请稍后重试或手动填写 API Key",
		}, nil
	}
	return apimodel.ArkFetchStatus{Available: true, Authorized: true}, nil
}

// ListVolcAccounts lists the Volcano Engine accounts the current user has authorized, for them to pick one.
//
// A person often has several Volcano Engine accounts (one case had 10), so they must pick. If we guessed one,
// a wrong guess would show as "the listed keys are not the ones I want", and they could not tell why.
func (s *Service) ListVolcAccounts(ctx context.Context) ([]apimodel.VolcAccount, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if s.fetcher == nil {
		return nil, fmt.Errorf("%w: 服务端未开启「从火山账号获取」", persistmodel.ErrPrecondition)
	}
	accounts, err := s.fetcher.ListAccounts(ctx, actor)
	if err != nil {
		return nil, s.arkFetchError("列火山账号失败", err)
	}
	return accounts, nil
}

// ListVolcArkKeys lists the Ark API Keys under a Volcano Engine account. **Only masks and ids, no plaintext.**
func (s *Service) ListVolcArkKeys(ctx context.Context, accountID string) ([]apimodel.ArkKeyOption, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, fmt.Errorf("%w: 缺少火山账号 id", persistmodel.ErrInvalidArgument)
	}
	if s.fetcher == nil {
		return nil, fmt.Errorf("%w: 服务端未开启「从火山账号获取」", persistmodel.ErrPrecondition)
	}
	keys, err := s.fetcher.ListKeys(ctx, actor, accountID)
	if err != nil {
		return nil, s.arkFetchError("列方舟 API Key 失败", err)
	}
	return keys, nil
}

// fetchPlainKey turns {accountID, keyID} into a plaintext key.
//
// Used inside the save path only. **Its return value must never go into an HTTP response.**
func (s *Service) fetchPlainKey(ctx context.Context, accountID, keyID string) (string, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return "", err
	}
	if s.fetcher == nil {
		return "", fmt.Errorf("%w: 服务端未开启「从火山账号获取」", persistmodel.ErrPrecondition)
	}
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(keyID) == "" {
		return "", fmt.Errorf("%w: 缺少火山账号 id 或 key id", persistmodel.ErrInvalidArgument)
	}
	key, err := s.fetcher.Reveal(ctx, actor, accountID, keyID)
	if err != nil {
		return "", s.arkFetchError("获取方舟 API Key 失败", err)
	}
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("%w: 火山返回的 key 是空的", persistmodel.ErrPrecondition)
	}
	return key, nil
}

// arkFetchError translates fetcher errors into Chinese that can be shown to the user.
//
// **Not authorized must be recognized separately**: that is something the user fixes with one click; showing "fetch failed" would make them
// think the feature is broken. Everything else is "automatic fetch unavailable, please fill it in by hand", since this path is
// only a shortcut anyway and blocking here makes no sense.
//
// ⚠️ The error message does not carry the upstream text: it may contain things like the account owner's email (the upstream response
// may come with a long string). The cause goes to the log; the user gets one sentence they can act on.
func (s *Service) arkFetchError(what string, err error) error {
	if s.fetcher != nil && s.fetcher.IsNotAuthorized(err) {
		return fmt.Errorf("%w: 你还没有授权 AMA 代表你访问火山账号，请先完成授权",
			persistmodel.ErrPrecondition)
	}
	s.log().Warn(what, "error", err.Error())
	// The upstream message is useful to the user (for example "which account lacks which permission"), but confirm it is
	// Chinese we built ourselves and not the platform's raw text.
	return fmt.Errorf("%w: %s：%s", persistmodel.ErrPrecondition, what, userFacingFetchReason(err))
}

// userFacingFetchReason picks the part of an upstream error that can be shown to the user.
//
// Only the few kinds this project builds itself are let through (their wording is written for users); anything unrecognized gets a generic sentence,
// so the platform's English errors and internal identifiers are not spread across the UI.
func userFacingFetchReason(err error) string {
	if err == nil {
		return "未知原因"
	}
	msg := err.Error()
	// The rejection errors built by the KeyFetcher implementation explain "which Volcano Engine account lacks which permission", exactly what the user needs to know.
	if strings.Contains(msg, "未授予") || strings.Contains(msg, "尚未授权") {
		return msg
	}
	return "请稍后重试，或改为手动填写 API Key"
}
