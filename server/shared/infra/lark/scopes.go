// Constant tables of the permissions, events and callbacks the app needs.
//
// These tables are the "what should be there" half of the readiness inspection: compare them with
// the granted permissions and subscribed events Feishu actually returns, and the difference is what
// the user is prompted to add.

package lark

import (
	"net/url"
	"strings"
)

// The permission for the app to inspect its own config. Without it, the inspection endpoints can
// only read the permission list, not the app details.
const ScopeSelfManage = "application:application:self_manage"

// ScopeGroupMessage is the permission for "receive group messages without @mentioning the bot".
//
// It is separate because it is not part of the base template: whether to request it depends on the
// trigger mode the user picks, so it is passed through BeginSpec.TenantScopes instead of being
// hard-coded in BaseScopes.
//
// ⚠️ **Without it, Feishu never pushes group messages that do not @mention the bot.** It is not
// that we filter them out: the events never reach us, so there is no trace in the inbound logs or
// the dedup table. The "no @ needed in topic groups" trigger mode depends on it completely; without
// it, it fails silently with zero errors.
//
// ⚠️⚠️ **But it only covers messages "sent by users", not by bots**; see the two IncludeBot
// constants below. In an alert group where only bots talk, a group with just this one permission
// receives no events at all.
const ScopeGroupMessage = "im:message.group_msg"

// ScopeGroupMessageIncludeBot / ScopeGroupAtMessageIncludeBot are the permission level for "also
// push me messages sent by **bots**".
//
// # These two constants came directly out of a production incident
//
// In an alert group the agent did not reply at all, and it took two rounds of investigation. The
// real cause: Feishu splits "receive group messages" into two levels, and we only requested the one
// without bots:
//
//	im:message.group_msg                          users only, **no bots**  ← all we had before
//	im:message.group_msg.include_bot:read         users + other bots (still not its own)
//	im:message.group_at_msg:readonly              only users @mentioning me, **no bots**
//	im:message.group_at_msg.include_bot:readonly  users + other bots @mentioning me
//
// Translated from the "permission requirements" section of the official docs
// (open.feishu.cn/document/server-docs/im-v1/message/events/receive):
//
//	To receive messages sent by all users and other bots in a group chat: needs the "get user and
//	bot messages in groups" (im:message.group_msg.include_bot:read) permission; does not include
//	messages sent by the current bot itself. To receive only messages from all users in a group
//	chat, without bot messages: needs the "get all messages in groups" (im:message.group_msg) permission
//
// **Everyone talking in an alert group is a bot** (observed: nearly all messages came from
// third-party alert bots, none from real people), so without this permission level not a single
// event from such a group is delivered, and however correct the `trigger.go` filtering is, it has
// no input. This failure has **zero errors**: the long connection is fine, readiness is all green,
// and the inbound table has no rows.
//
// ⚠️ **The mismatch between the `:read` and `:readonly` suffixes is how the official text is, not a
// typo.** Checked against the official docs on open.feishu.cn, word for word.
//
// ⚠️ **They do not replace ScopeGroupMessage; they are a separate parallel level.** The group
// history path ("fetching group history needs im:message.group_msg" in feishu/README.md) names
// group_msg itself, so both must be requested; include_bot cannot replace it.
//
// ⚠️ **They also stay out of BaseScopes**, for the same reason as ScopeGroupMessage: they are only
// needed conditionally, when no-@ mode is chosen. Putting them in the base template would wrongly
// report missing permissions for every ordinary app.
const (
	ScopeGroupMessageIncludeBot   = "im:message.group_msg.include_bot:read"
	ScopeGroupAtMessageIncludeBot = "im:message.group_at_msg.include_bot:readonly"
)

// The open platform permission request page. The user checks permissions here and then publishes a
// new version themselves.
//
// One per domain: mainland Feishu and international Lark are two separate open platforms, and
// sending an international user to open.feishu.cn would leave them going in circles on a site where
// the app does not exist. Same precedent as the two DefaultOpenURL* constants in client.go.
const (
	scopeApplyURLFeishu = "https://open.feishu.cn/page/scope-apply"
	scopeApplyURLLark   = "https://open.larksuite.com/page/scope-apply"
)

// ScopeApplyURL builds the "go request these permissions on the open platform" URL, with the scopes
// prefilled.
//
// **Built on the server, not the frontend**: appID and region are both on the server, and building
// it on the frontend would mean implementing the domain selection logic again. An empty string
// means it cannot be built (missing appID or no permissions requested), and the caller uses that to
// decide whether to show the entry. An empty string is better than a link that 404s when clicked.
func ScopeApplyURL(region Region, appID string, scopes ...string) string {
	id := strings.TrimSpace(appID)
	// TrimSpace each one first, then dedupe: uniqueNonEmpty only blocks pure empty strings, not "
	// ", and an all-whitespace permission name would produce `scopes=%20%20`: the URL looks fine,
	// but the page cannot check a single item. Clean up here in place, without changing what
	// uniqueNonEmpty does (the registration addons path also uses it).
	trimmed := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if v := strings.TrimSpace(s); v != "" {
			trimmed = append(trimmed, v)
		}
	}
	wanted := uniqueNonEmpty(trimmed)
	if id == "" || len(wanted) == 0 {
		return ""
	}
	base := scopeApplyURLFeishu
	if NormalizeRegion(string(region)) == RegionLark {
		base = scopeApplyURLLark
	}
	query := url.Values{}
	query.Set("clientID", id)
	// Comma-separated, and **must not be escaped to %2C**: the open platform splits this parameter
	// on literal commas. url.Values.Encode() would escape it, so it is built by hand here.
	return base + "?" + query.Encode() + "&scopes=" + strings.Join(wanted, ",")
}

// CallbackCardAction is the card interaction callback; button clicks on cards sent by the bot come
// back through it.
const CallbackCardAction = "card.action.trigger"

// CallbackTypeWebSocket is the value callback_info.callback_type should have in long connection
// mode.
//
// With a long connection, card callbacks are also delivered over that websocket; with the wrong
// type, buttons on cards do nothing when clicked. This is the only readiness item still judged from
// the "app details snapshot": it is the config itself, not a runtime grant.
const CallbackTypeWebSocket = "websocket"

// BaseScopes are the base permissions needed to send and receive messages.
var BaseScopes = []string{
	ScopeSelfManage,
	"im:chat",
	"im:message:readonly",
	"im:message:send_as_bot",
	"im:message.p2p_msg:readonly",
	"im:message.group_at_msg:readonly",
	"im:message.reactions:read",
	"im:message.reactions:write_only",
}

// BaseEvents are the events that must be subscribed.
//
// Besides receiving messages, members joining/leaving a group and group disbanding must also be
// subscribed; otherwise after the bot is removed from a group or the group is disbanded, the
// platform side keeps a stale binding.
var BaseEvents = []string{
	"im.message.receive_v1",
	"im.message.recalled_v1",
	"im.chat.disbanded_v1",
	"im.chat.member.bot.added_v1",
	"im.chat.member.bot.deleted_v1",
	"im.chat.member.user.added_v1",
	"im.chat.member.user.deleted_v1",
	"im.chat.member.user.withdrawn_v1",
}

// BaseCallbacks are the callbacks that must be subscribed.
var BaseCallbacks = []string{CallbackCardAction}

// MissingValues returns the items in required that are not in actual, used to compute the
// difference for permissions/events.
//
// The return value is never nil (an empty slice, not nil), so it serializes to [] in JSON rather
// than null: with null the frontend needs an extra empty check, with [] it can map directly.
func MissingValues(required, actual []string) []string {
	have := make(map[string]struct{}, len(actual))
	for _, v := range actual {
		have[v] = struct{}{}
	}
	missing := make([]string, 0, len(required))
	for _, v := range required {
		if _, ok := have[v]; !ok {
			missing = append(missing, v)
		}
	}
	return missing
}

// ContainsValue reports whether values contains expected.
func ContainsValue(values []string, expected string) bool {
	for _, v := range values {
		if v == expected {
			return true
		}
	}
	return false
}

// uniqueNonEmpty dedupes and removes empty strings, keeping the original order.
func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
