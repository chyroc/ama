// Tests for the long-connection supervisor. For now they only cover botIdentity: **this package had no tests at all before**,
// and botIdentity happens to be the riskiest part of this round of changes.
//
// # Why test it separately
//
// `app.BotOpenID` is **the only test for preventing self-triggering** (both the self filter in `shared/infra/lark` and
// isSelfMessage in `trigger.go` compare against it). Storing it wrong gives no error and no symptom at all;
// it only makes that defense never match, and in no-@ mode that means the bot talks to itself in an endless loop.
//
// In production it really was stored wrong: the store had `ou_aaaa…` for `cli_example_main`,
// while Feishu actually had `ou_bbbb…`. The implementation before the change "used the store value directly if non-empty",
// so that wrong value **would never heal itself**.
//
// The three tests below pin the fixed behavior: Feishu wins, write back, and "fall back to the store value when it cannot be read".

package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// fakeLarkBot starts a fake Feishu that only implements two APIs: token exchange and reading bot info.
//
// An empty botOpenID means "identity cannot be read" (the API returns 500), used to check the fallback path.
func fakeLarkBot(t *testing.T, botOpenID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/v3/tenant_access_token/internal",
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "t-token", "expire": 7200,
			})
		})
	mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, _ *http.Request) {
		if botOpenID == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "ok",
			"bot": map[string]any{"open_id": botOpenID, "app_name": "测试机器人"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// supervisorFixture builds a Supervisor just enough to run botIdentity, and pre-fills an app.
func supervisorFixture(t *testing.T, storedOpenID, remoteOpenID string) (*Supervisor, Store, *App) {
	t.Helper()
	store := NewMemStore()
	app := &App{
		ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1",
		Name: "测试应用", BotOpenID: storedOpenID, Region: string(lark.RegionFeishu),
	}
	if _, err := store.CreateApp(context.Background(), app, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	srv := fakeLarkBot(t, remoteOpenID)
	log, _ := captureLogs()
	sup, err := NewSupervisor(SupervisorConfig{
		Store:   store,
		Inbound: &Inbound{}, // botIdentity does not touch it; any non-nil value passes the constructor check
		Leases:  stubLeases{},
		Clients: func(lark.Region) (*lark.Client, error) {
			return lark.New(lark.Config{OpenURL: srv.URL, AccountsURL: srv.URL})
		},
		Logger: log,
	})
	if err != nil {
		t.Fatalf("建监管者失败: %v", err)
	}
	return sup, store, app
}

// stubLeases only exists to pass the non-nil check of NewSupervisor; botIdentity does not use it.
type stubLeases struct{}

func (stubLeases) Acquire(context.Context, string, time.Duration) (lark.Lease, bool, error) {
	return nil, false, nil
}

// ⭐ **If the store has it wrong, Feishu wins, and the value is written back.**
//
// This is the state that really existed in production (stored ou_aaaa…, actually ou_bbbb…).
// Before the change it "used the store value directly if non-empty", so the wrong value never healed, and the self-trigger defense was useless all along.
//
// Mutation check: add `if bot.OpenID != "" { return bot }` back into botIdentity, and this test turns red.
func TestBotIdentityPrefersFeishuOverStaleStoredValue(t *testing.T) {
	const stale, actual = "ou_stale_00000000000000", "ou_actual_0000000000000"
	sup, store, app := supervisorFixture(t, stale, actual)

	got := sup.botIdentity(context.Background(), app, "secret")

	if got.OpenID != actual {
		t.Fatalf("要以飞书返回的为准，期望 %q，实际 %q——用错值等于防自触发失效", actual, got.OpenID)
	}
	// It must also write back, or the next process has to look it up again, and the wrong value stays shown in the UI.
	fresh, err := store.GetAppByAppID(context.Background(), "cli_1")
	if err != nil {
		t.Fatalf("回读应用失败: %v", err)
	}
	if fresh.BotOpenID != actual {
		t.Errorf("纠正后要回写库，库里仍是 %q", fresh.BotOpenID)
	}
}

// ⭐ No write back when they match (avoid writing to the store on every connect).
func TestBotIdentityNoWriteWhenAlreadyCorrect(t *testing.T) {
	const same = "ou_same_000000000000000"
	sup, _, app := supervisorFixture(t, same, same)

	if got := sup.botIdentity(context.Background(), app, "secret"); got.OpenID != same {
		t.Fatalf("一致时应原样返回，实际 %q", got.OpenID)
	}
}

// ⭐ **When the identity cannot be read, fall back to the store value; do not clear it.**
//
// A failing Feishu API should not stop the long connection from starting, let alone erase a correct open_id into an empty string:
// an empty string makes isSelfMessage fall back to "block all bots", blocking third-party alert bots as collateral,
// which is exactly what this round fixes.
//
// Mutation check: when BotInfo fails, `return lark.BotIdentity{AppID: app.AppID}` (dropping the store value),
// and this test turns red.
func TestBotIdentityFallsBackToStoredWhenRemoteFails(t *testing.T) {
	const stored = "ou_stored_0000000000000"
	sup, _, app := supervisorFixture(t, stored, "") // remote 500

	got := sup.botIdentity(context.Background(), app, "secret")

	if got.OpenID != stored {
		t.Fatalf("远端读不到时要退回库里的值，期望 %q，实际 %q", stored, got.OpenID)
	}
	if got.AppID != "cli_1" {
		t.Errorf("AppID 不该丢，实际 %q", got.AppID)
	}
}

// ---------------------------------------------------------------------------
// Dedicated alert for "missing the include_bot permission"
// ---------------------------------------------------------------------------

// ⭐ **When no-@ mode is on but the "receive bot messages" permission is missing, it must be reported explicitly.**
//
// # Why this defense can only live at connect time
//
// Intuitively it belongs with the inbound "received but did not trigger" drop logs, but there it **would never
// fire**: without the permission Feishu does not push the events at all, and Handle is never called. A log that can never be printed
// is a Schrödinger defense; the test is "search for its dedicated log, 0 hits means it never took effect"
// (docs/guides/backend.md). So the check must be placed at a moment that does not depend on messages arriving.
//
// # What this log is worth
//
// In the 2026-09-02 investigation, this app looked **word for word identical** in the logs to a perfectly normal app:
// "long connection started" + "ready", then 20 hours of nothing. And the alert chat had no real people,
// so even the most natural check, "send a message and see", could not be done.
func TestWarnsWhenBotMessagesUnreachable(t *testing.T) {
	const wantScope = lark.ScopeGroupMessageIncludeBot

	cases := []struct {
		name      string
		mode      TriggerMode
		readiness *Readiness
		wantWarn  bool
	}{
		{
			// The state being fixed: the group message permission is there, the one that includes bots is not.
			name: "开了免 @ 且缺含机器人那档：必须报",
			mode: TriggerTopicAndMention,
			readiness: &Readiness{
				GroupMessageScopeGranted: true,
				BotMessageScopeGranted:   false,
			},
			wantWarn: true,
		},
		{
			name: "两档都齐：不该报",
			mode: TriggerTopicAndMention,
			readiness: &Readiness{
				GroupMessageScopeGranted: true,
				BotMessageScopeGranted:   true,
			},
			wantWarn: false,
		},
		{
			// The default mode does not need this permission; reporting it would be noise, and noise drowns real incidents.
			name: "只 @ 才触发：与这个权限无关，不该报",
			mode: TriggerMentionOnly,
			readiness: &Readiness{
				GroupMessageScopeGranted: true,
				BotMessageScopeGranted:   false,
			},
			wantWarn: false,
		},
		{
			// ⚠️ "Never checked" is not "not authorized". Alerting on it would be a groundless accusation,
			// and whoever reads the log would go apply for a permission that may have been there all along.
			name:      "从没巡检过：闭嘴，不能凭空指控",
			mode:      TriggerTopicAndMention,
			readiness: nil,
			wantWarn:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLogs()
			sup := &Supervisor{log: log}
			sup.warnIfBotMessagesUnreachable(&App{
				AppID: "cli_1", WorkspaceID: "ws_1",
				TriggerMode: tc.mode, Readiness: tc.readiness,
			})

			got := buf.String()
			hasWarn := strings.Contains(got, "收其他机器人消息")
			if hasWarn != tc.wantWarn {
				t.Fatalf("期望报警=%v，实际=%v\n日志：%s", tc.wantWarn, hasWarn, got)
			}
			if !tc.wantWarn {
				return
			}
			// The level must be right: this is "the feature cannot work under the current config", not a normal state.
			if !strings.Contains(got, "level=WARN") {
				t.Errorf("该用 Warn 级别，实际：%s", got)
			}
			// It must include the permission name, so whoever reads the log can apply for it directly without digging through docs.
			if !strings.Contains(got, wantScope) {
				t.Errorf("日志要带上权限名 %q，实际：%s", wantScope, got)
			}
			// It must state the **consequence**, not just "permission missing", following the rule in messages.ts.
			if !strings.Contains(got, "一条都不会推下来") {
				t.Errorf("日志要说清后果，否则看的人不知道该不该管，实际：%s", got)
			}
			// Location info: which app, which workspace.
			if !strings.Contains(got, "cli_1") || !strings.Contains(got, "ws_1") {
				t.Errorf("要能定位到具体应用与空间，实际：%s", got)
			}
		})
	}
}
