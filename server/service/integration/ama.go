// This file implements "AMA itself" as the fourth IntegrationProvider.
//
// It answers a question nobody answered before: **what lets the agent in the container operate on
// the AMA platform's own data.** An agent with the built-in `ama` skill ticked runs the `ama` command
// in the container, and that command needs an AMA API Key (environment variable `AMA_TOKEN`). This
// card is the switch and status of that key.
//
// # ⚠️ It fixes a bug of "claimed to exist but never implemented"
//
// This card is not a purely new feature; it is the fix for a bug. Before it:
//
//   - the body of the built-in ama skill (core/skillbuiltin.go) told the model "ama is installed in
//     this container with credentials configured (injected by the platform through environment
//     variables); just use it, no login, no API Key needed";
//   - the CLI, when missing the token, also said "this variable is injected automatically by the AMA
//     platform when running commands";
//   - cli/README.md marked AMA_TOKEN as "required / injected automatically by the platform in the
//     container".
//
// **And nothing anywhere in the repository had ever injected AMA_TOKEN** (grepped exhaustively:
// across go/ts/tsx/sh excluding cmd/ama-cli itself, the only hit was a line of doc text in
// skillbuiltin.go). So before, an agent with the ama skill ticked would always fail running `ama` in
// the container, and the failure message would make the model turn around and tell the user "the
// platform has not configured injection", a sentence the user cannot act on at all.
//
// # ⚠️⚠️ Why this provider has a Store while providers of external platforms deliberately do not
//
// The file header of integration.go says "status is always queried live, never stored", because
// "authorization is granted by the user **on the platform side**, and the platform is the only
// authority; storing a copy would disagree with the platform, and the disagreement shows up as 'the
// page says authorized, but tool calls still fail'".
//
// **That rule is about the status of external platforms, while this provider's source of truth is in
// our own database** (the `auth_api_keys` table). Querying the database **is** querying the
// authority, not caching a copy of platform status. So having a data dependency does not break the
// rule.
//
// Conversely, if someday someone adds a Store to an external platform's provider to "cache the
// authorization status", that does break the rule. **Do not take this file having a dependency as
// permission.** The rule is one question: **who holds the authority over this status?** Our database
// → query the database; an external platform → you must ask the platform live.
//
// # The status is almost always ok
//
// The requirement says "this one is always true". In the four-state model that means: **as long as
// the server has the derivation secret configured, the status is ok**, because this key needs no
// authorization action from the user; the platform can make it by itself (see the deterministic
// derivation in auth/managedkey.go). So:
//
//	CanAuthorize = false   there is no "authorize" action; clicking it would mean nothing
//	CanRevoke    = false   this source does not support removing authorization; see below
//
// There is only one non-ok: **the server has no derivation secret configured** → missing. Then ama in
// the container cannot work, and an admin must configure it; nothing the user clicks helps. That is
// exactly why missing and error are separate.
//
// # ⚠️ This source does not support removing authorization (a requirement change, not a gap)
//
// **This used to be `CanRevoke = true`, based on the requirement at the time: "revocable; after a
// revoke it is created again automatically at the next injection". That is no longer valid**: the
// requirement now says clearly the AMA platform **does not support removing authorization**, so the
// card has no such button, and `Revoke` rejects it explicitly (returns ErrRevokeUnsupported). Do not
// treat it as a bug and "fix it back".
//
// Removing it also removed a button that did not do what it said: **that button was never "remove
// authorization"**. Revoking only deleted the row in the database, and the next time the container
// ran a command it **derived the same key again** and created the row ("Revoking means reset, not
// rotate" in auth/managedkey.go, pinned byte for byte as `after == before` by
// TestManagedKeyRevokeThenReinjectRevives). So it could neither remove the authorization nor replace
// a leaked token; clicking it only made "credential prefix / creation time" vanish from the card for
// a while.
//
// # How to rotate that key: point to the API Key page; do not block the way out
//
// With the button gone, users must still have a way to act (e.g. if they suspect the token leaked),
// otherwise it is a security problem. **That path already exists and is more proper**: this key is an
// ordinary row in `auth_api_keys` (named auth.ManagedKeyName), and the "API Key" page of global
// settings **can list it and revoke it**: the only filter of `GET /api/api-keys` is `email = $1`, and
// `DELETE /api/api-keys/{id}` only deletes by id + email (authdao/store_pg.go); neither checks
// whether it is managed. So the ok branch's FixHint points there.
//
// ⚠️ But that path's meaning is **also reset, not rotate** (after a revoke the next injection
// derives the same key again). To really replace a leaked token you can only replace the server's
// auth.managedKeySecret (affects everyone) or add a version field to the stable id; both need an
// admin, described in "Rotating the secret" in auth/managedkey.go. **So the text must not say "revoke
// means invalid"**; that would be a lie.
//
// # Still showing ok after a revoke is right
//
// Users can still revoke that row from the API Key page, so the "row is gone" state still happens.
// Then the next container command derives it again and creates the row, so the card is still ok;
// only the two lines in Details (credential prefix, creation time) disappear, and Summary says "it
// will be created automatically the next time an agent uses it".
//
// **Do not make the post-revoke state error or warning.** That would hint to the user "it is broken,
// go fix it", while nothing needs to be done. This is exactly the line between warning/error in the
// four-state model: does the user need to act now.
package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ManagedAPIKeys is the read/write capability of "platform-managed API Keys", implemented by
// auth.Service.
//
// It is an interface instead of taking *auth.Service directly, for two reasons:
//
//  1. **It can be tested offline**: neither method needs a real database;
//  2. **Narrower surface**: auth.Service has dozens of methods (login, sessions, SSO), and this
//     provider should only touch these two. Giving it the whole Service would make "what else does
//     it touch here" something you can only learn by reading the code.
//
// ⚠️ **There is deliberately no Revoke here.** This source does not support removing authorization
// (see the file header), and with the revoke capability taken off the interface, this provider **has
// no way at the type level to delete that row**, harder than just returning an error in Revoke: if
// someone carelessly calls it in some branch in the future, it does not compile. The revoke path
// still exists, but on the API Key page side (through auth.Service's own RevokeAPIKey), not here.
type ManagedAPIKeys interface {
	// Record looks up this person's managed key. If there is none (revoked on the API Key page or
	// never injected) it returns auth.ErrNotFound.
	Record(ctx context.Context, email string) (auth.UserAPIKey, error)

	// Enabled reports whether the server has the derivation secret configured. When false this
	// source is entirely unavailable.
	Enabled() bool
}

// AMAProvider is the integration source "the AMA platform itself".
type AMAProvider struct {
	keys ManagedAPIKeys
	log  *slog.Logger
}

// NewAMAProvider builds a provider. keys may be nil (meaning not wired).
func NewAMAProvider(keys ManagedAPIKeys, logger *slog.Logger) *AMAProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &AMAProvider{keys: keys, log: logger}
}

// AMAIntegrationID is the stable identifier of this source.
//
// It is exported because **the injection path decides by it "has this agent ticked the AMA
// integration"** (see core/credentialinject.go). With the literal "ama" scattered in two places, a
// rename missing one gives no error; it only shows up as "ticked but not injected".
const AMAIntegrationID = "ama"

func (p *AMAProvider) ID() string   { return AMAIntegrationID }
func (p *AMAProvider) Name() string { return "AMA 平台" }
func (p *AMAProvider) Description() string {
	return "让容器里的 agent 能用 ama 命令操作 AMA 平台自己的数据（空间、agent、技能、运行历史）。凭据由平台自动创建，无需你授权"
}

// configured reports whether the server has wired this source.
func (p *AMAProvider) configured() bool {
	return p != nil && p.keys != nil && p.keys.Enabled()
}

// State reports the current state of this person's AMA credential.
//
// There are only two results: configured → ok (whether or not the row is in the database), not
// configured → missing. See "The status is almost always ok" in the file header for why.
func (p *AMAProvider) State(ctx context.Context, actor string) IntegrationState {
	st := IntegrationState{
		ID:          p.ID(),
		Name:        p.Name(),
		Description: p.Description(),
		CheckedAt:   nowISO(),
		// **There is no "authorize" action**: the platform makes this key itself, and the user needs
		// to click nothing.
		CanAuthorize: false,
		// **This source does not support removing authorization** (a requirement; see the file
		// header). None of the four branches offers the button, so it is set once here instead of in
		// each branch: then adding a branch and missing one would grow a button that only gives a
		// 422 when clicked.
		CanRevoke: false,
		// Injecting: AMA_TOKEN / AMA_BASE_URL / AMA_WORKSPACE_ID in the container come from it.
		//
		// ⚠️ **It must be true even when the status is missing**: then it should still appear in the
		// agent's integration checkbox area (checkbox disabled + a line "an admin must configure
		// it"), instead of disappearing entirely; if it disappeared the user would not know it
		// exists, and would never ask anyone to configure it.
		Injectable: true,
	}

	if !p.configured() {
		st.Status = IntegrationMissing
		st.Summary = "服务端还没配好，容器里的 ama 命令不可用"
		// Name what must be configured and by whom. **Do not write "please authorize"**: the user
		// cannot click their way to this config.
		st.FixHint = "请管理员配上 auth.managedKeySecret（生成一把：openssl rand -base64 32）。" +
			"没有它，勾了「ama」内置技能的 agent 在容器里跑 ama 会失败。"
		st.CanRevoke = false
		return st
	}

	// Configured means ok. **Revoked is also ok**; see the file header.
	st.Status = IntegrationOK

	// ⚠️ **This call cannot be skipped, even though its return value is no longer used.** It is
	// **the very check** of this card: whether the row can be found decides which branch below runs:
	// ok / "not created yet" / warning. The return value is discarded only because the ok state no
	// longer shows those three lines of metadata (see below), not because the query is useless.
	_, err := p.keys.Record(ctx, actor)
	switch {
	case err == nil:
		// ⚠️ The ok state **deliberately has no Summary / Details / FixHint** (trimmed down later).
		//
		// In its normal state this card used to be the noisiest on the whole page: an "Ready..."
		// sentence repeating the green chip, three lines of metadata (credential prefix / creation
		// time / last used), plus a long paragraph explaining "revoke = reset, not rotate". And this
		// source is **created and renewed automatically by the platform**: the user has no decision
		// to make on this card, and none of those words could change what they do.
		//
		// ⚠️ **The way out of "how to deal with this credential" is not lost, just no longer always
		// shown**: it is on the "API Key" page of global settings (that key appears there as
		// auth.ManagedKeyName and can be revoked), which is the place for managing credentials
		// anyway. The prefix/creation time can also be found there, along with something that does
		// not fit here (the revoke button). **Do not move this passage back**: an information block
		// that leads to no different decision costs pushing down the cards that really need action.
		st.Status = IntegrationOK

	case errors.Is(err, auth.ErrNotFound):
		// The row is not there. Three possible origins: no agent has ever really used it (the
		// credential is **created lazily**: the row is only created at the first injection; see the
		// EnsureManagedKey call in core/sessionenv.go), the user revoked it themselves on the API
		// Key page, or someone once clicked the remove button that has since been taken away.
		//
		// ⚠️ **This message is not affected by "removing the remove-authorization button" and still
		// holds**: lazy creation did not change at all, and the revoke path on the API Key page is
		// still there, so "the row is gone" still happens.
		st.Summary = "凭据尚未创建，下次 agent 用到时会自动创建"
		st.FixHint = "这是正常状态，不需要你做任何事。" +
			"勾了「AMA」授权的 agent 第一次执行命令时，平台会自动创建凭据。"

	default:
		// The database query failed. **Degrade to warning, not error**: a database hiccup does not
		// mean the credential is broken, and showing it as "unavailable" would make the user revoke
		// and rebuild it, a completely wasted operation; exactly the same reason as the passage on
		// isTransient.
		p.log.WarnContext(ctx, "查 AMA 托管凭据失败，卡片降级显示",
			slog.String("actor", actor), slog.Any("err", err))
		st.Status = IntegrationWarning
		st.Summary = "这次没查到凭据状态，但这不代表它失效了"
		st.FixHint = "稍后点「重新检查」。容器里的 ama 命令不受这次查询失败影响——" +
			"它的凭据是执行时现取的。"
	}
	return st
}

// AuthorizeURL: this source needs no user authorization.
//
// It returns ErrAuthorizeUnsupported instead of making up an address: CanAuthorize is already
// false, the frontend does not render that button at all, so this path is normally never reached.
// If it is, the frontend has an old list, and failing explicitly is the right signal.
func (p *AMAProvider) AuthorizeURL(_ context.Context, _ string) (string, error) {
	return "", ErrAuthorizeUnsupported
}

// Revoke: this source does not support removing authorization; always rejected.
//
// # Why keep this method instead of deleting it
//
// Two reasons; the second is the key one:
//
//  1. `Revoke` is a method of the IntegrationProvider interface (integration.go) and **must be
//     implemented**;
//  2. **`DELETE /api/integrations/{provider}` is a generic route** (one of those four in
//     matrix.go) that anyone can call directly; `canRevoke=false` only makes the frontend not
//     render the button. So "hide the button but still allow deleting underneath" would leave a
//     hidden hole **with no UI that nobody would think to look at**: the row really gets deleted,
//     while the user thinks the capability does not exist.
//
// It wraps persistmodel.ErrPrecondition (→ 422) + ErrRevokeUnsupported, and the frontend shows this
// sentence as is (the catch in IntegrationsTab deliberately does not hard-code "unsupported"). So
// the sentence must say **where to go to deal with it**, not just "no".
//
// ⚠️ **Both sentinels are wrapped with `%w`** (Go 1.20+ supports several %w). If the second were
// `%s`, `errors.Is(err, ErrRevokeUnsupported)` would be **false**: that sentinel would be reduced to
// literal text, the check would degrade to `strings.Contains`, and it would break as soon as the
// text changes. Wrapped with %w, "this capability does not exist at all" is a fact a program can
// check.
//
// ⚠️ The wording does not say "revoking makes it invalid": that path's meaning is also reset (the
// next injection derives the same key), for exactly the same reason as the FixHint in State.
func (p *AMAProvider) Revoke(_ context.Context, _ string) error {
	return fmt.Errorf("%w: %w。AMA 平台的凭据由平台托管，不支持解除授权——"+
		"它由平台自动创建、也由平台自动续上，解除了下次 agent 用到时又会重新生成。"+
		"想处置这把凭据请到全局设置的「API Key」页面，它在那里显示为「%s」，可以吊销；"+
		"但那同样只是重置（下次会重新生成同一把），真要换掉一把已泄漏的凭据"+
		"需要管理员换掉服务端的 auth.managedKeySecret",
		persistmodel.ErrPrecondition, ErrRevokeUnsupported, auth.ManagedKeyName)
}
