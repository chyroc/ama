// The selection rules for "which Feishu app should this person use".
//
// # Why these tests deserve to exist
//
// This fixes a **zero-error mismatch**: injection did not pick by workspace, so "an agent running in workspace C
// got the token of workspace A's app". The token is bound to the Feishu app, and a mismatch shows up as
// `99991668 token invalid`, or by luck it is valid but cannot read that chat; **neither points to
// "the wrong app was picked"**, so nobody in production would report this bug; they would just feel "Feishu reading works sometimes".
//
// Since runtime errors cannot catch it, only these tests can.
//
// # ⚠️ The fixture must be "three workspaces, three apps, all app_ids different"
//
// This is the precondition for this group to work, not for show. With values from the same source **a wrong implementation also passes**:
// with only one app, "pick by workspace" and "take the first" return the same value,
// so that assertion is always green, as good as not written (a fixture must differ on every axis).
//
// Mutation check (each one done; see each test's comment): delete the part in ResolveApp that "pins to this workspace when WorkspaceID
// is non-empty", falling back to walking all workspaces and taking the first;
// TestResolveAppPinsToRequestedWorkspace and TestResolveAppDoesNotFallBack
// must both turn red.

package feishu

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// fakeWorkspaces answers "which workspaces is this person in" and "what is a workspace called".
//
// The order is **deliberately fixed**: without a workspace context ResolveApp takes the first usable one in this order,
// and "the first" is exactly the wrong implementation we want to tell apart from "pick by workspace".
type fakeWorkspaces struct {
	ids   []string
	names map[string]string
	// When err is non-nil, WorkspaceIDsFor returns an error.
	err error
}

func (f fakeWorkspaces) WorkspaceIDsFor(_ context.Context, _ string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.ids, nil
}

func (f fakeWorkspaces) WorkspaceNameOf(_ context.Context, id string) (string, error) {
	name, ok := f.names[id]
	if !ok {
		return "", ErrAppNotFound
	}
	return name, nil
}

// Three workspaces, three apps, **app_id and secret all different**.
//
// The names are **deliberately all the same, `"AMA 助手"`**: in practice that user's three apps were exactly like this
// (all built following the same doc). This is not filler: it pins the precondition of the UI constraint
// "names alone cannot tell them apart, so app_id must be given too".
const (
	rsWSDemo = "ws_demo"
	rsWSMid  = "ws_demo_a"
	rsWSLast = "ws_demo_b"

	rsAppDemo = "cli_example_a"
	rsAppMid  = "cli_example_b"
	rsAppLast = "cli_example_c"
)

// newResolverFixture builds a "three workspaces, three apps" resolver.
//
// The returned resolver already has Service and the workspace list; the workspace order is demo → mid → last,
// so the wrong implementation "take the first" returns rsAppDemo, and the three tests rely on exactly this to reveal it.
func newResolverFixture(t *testing.T) *UserAppResolver {
	t.Helper()
	store := NewMemStore()
	ctx := context.Background()

	for _, seed := range []struct {
		id, ws, appID, secret string
	}{
		{"fsa_demo", rsWSDemo, rsAppDemo, "secret-demo"},
		{"fsa_mid", rsWSMid, rsAppMid, "secret-mid"},
		{"fsa_last", rsWSLast, rsAppLast, "secret-last"},
	} {
		app := &App{
			ID:          seed.id,
			WorkspaceID: seed.ws,
			AppID:       seed.appID,
			// All three have the same name; see the part above.
			Name:   "AMA 助手",
			Status: StatusConnected,
		}
		if _, err := store.CreateApp(ctx, app, seed.secret); err != nil {
			t.Fatalf("预置应用 %s 失败: %v", seed.appID, err)
		}
	}

	svc, err := New(Config{Store: store, Authorizer: allowAuthorizer{}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	r := NewUserAppResolver(svc, fakeWorkspaces{
		ids: []string{rsWSDemo, rsWSMid, rsWSLast},
		names: map[string]string{
			rsWSDemo: "演示空间",
			rsWSMid:  "中间空间",
			rsWSLast: "最后那个空间",
		},
	})
	if r == nil {
		t.Fatal("NewUserAppResolver 返回 nil")
	}
	return r
}

// ⭐⭐ This is the core of this group: **pick by the requested workspace, not the first one**.
//
// Each of the three workspaces is asserted once. Asserting only one workspace is not enough: if only ws_demo were tested,
// the wrong "take the first" implementation would still pass (demo happens to be first).
//
// Mutation check: delete the whole `if wsID := ...; wsID != ""` part in ResolveApp
// (falling back to walking all workspaces and taking the first), and the mid and last subtests turn red:
//
//	workspace ws_demo_a should pick cli_bb…, actually picked cli_aa…
//	workspace ws_demo_b should pick cli_cc…, actually picked cli_aa…
func TestResolveAppPinsToRequestedWorkspace(t *testing.T) {
	r := newResolverFixture(t)

	for _, tc := range []struct {
		name       string
		workspace  string
		wantApp    string
		wantSecret string
	}{
		{"第一个空间", rsWSDemo, rsAppDemo, "secret-demo"},
		// ⭐ These two are what can reveal "take the first": they are not first in the list.
		{"中间那个空间", rsWSMid, rsAppMid, "secret-mid"},
		{"最后那个空间", rsWSLast, rsAppLast, "secret-last"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appID, secret, err := r.ResolveApp(context.Background(), apimodel.LarkAppQuery{
				Actor:       "alice@example.com",
				WorkspaceID: tc.workspace,
			})
			if err != nil {
				t.Fatalf("ResolveApp 失败: %v", err)
			}
			if appID != tc.wantApp {
				t.Errorf("空间 %s 应当选 %s，实际选了 %s\n"+
					"→ 这正是「跨空间错配」：token 与应用绑定，选错会拿到 "+
					"99991668 token invalid 或读不到那个群，而报错不指向选错了应用",
					tc.workspace, tc.wantApp, appID)
			}
			// The secret must be asserted too: comparing only app_id would let a half-wrong implementation "return the right id with the wrong secret"
			// pass, failing only when exchanging the token.
			if secret != tc.wantSecret {
				t.Errorf("空间 %s 的密钥应当是 %s，实际是 %s", tc.workspace, tc.wantSecret, secret)
			}
		})
	}
}

// ⭐ When the given workspace has no usable app it **must fail, and must not fall back to another workspace**.
//
// Falling back is the most tempting and most wrong move here: it turns "this workspace has not connected a Feishu app yet" into a silent
// mismatch, and the symptoms of a mismatch (401 / cannot read the chat) send people to check Feishu permissions.
//
// Mutation check: change that return in ResolveAppForWorkspace to "keep walking all workspaces",
// and this turns red (it gets cli_aa… instead of an error).
func TestResolveAppDoesNotFallBackToAnotherWorkspace(t *testing.T) {
	r := newResolverFixture(t)

	_, _, err := r.ResolveApp(context.Background(), apimodel.LarkAppQuery{
		Actor:       "alice@example.com",
		WorkspaceID: "ws_没有飞书应用的空间",
	})
	if err == nil {
		t.Fatal("这个空间没有飞书应用，应当明确失败——回落到别的空间会造成静默错配")
	}
	if !errors.Is(err, ErrAppNotFound) {
		t.Errorf("应当是 ErrAppNotFound（→ 卡片上的 missing 状态），实际 %v", err)
	}
	// The error message must include the workspace id: the user has three workspaces, and without saying which one they do not know where to connect.
	if !strings.Contains(err.Error(), "ws_没有飞书应用的空间") {
		t.Errorf("错误信息里应当带上是哪个空间，实际 %q", err.Error())
	}
}

// Without a workspace context (authorize, revoke authorization, card display) it is account-level: take the first usable one.
//
// This pairs with the one above: it pins "an empty WorkspaceID is not an error, it is another rule";
// if it were changed to an error too, users could not start authorization at all.
func TestResolveAppWithoutWorkspaceTakesFirstAvailable(t *testing.T) {
	r := newResolverFixture(t)

	appID, _, err := r.ResolveApp(context.Background(), apimodel.LarkAppQuery{
		Actor: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("账号级解析失败: %v", err)
	}
	if appID != rsAppDemo {
		t.Errorf("没有空间上下文时应当取第一个空间的应用 %s，实际 %s", rsAppDemo, appID)
	}
}

// ⭐ The app the user explicitly picked comes first: this is where "switch to another bot" lands on the resolution side.
//
// Mutation check: delete the `if want != ""` part in ResolveApp, and this turns red
// (it gets cli_aa… instead of the user's choice cli_cc…).
func TestResolveAppPrefersExplicitChoice(t *testing.T) {
	r := newResolverFixture(t)

	appID, secret, err := r.ResolveApp(context.Background(), apimodel.LarkAppQuery{
		Actor: "alice@example.com",
		// The user picked **the last one** in settings, which is not first in the default order.
		PreferAppID: rsAppLast,
	})
	if err != nil {
		t.Fatalf("ResolveApp 失败: %v", err)
	}
	if appID != rsAppLast {
		t.Errorf("用户选了 %s，应当用它，实际用了 %s", rsAppLast, appID)
	}
	if secret != "secret-last" {
		t.Errorf("密钥应当跟着选中的应用走，实际 %s", secret)
	}
}

// ⭐⭐ The preference is a **soft** constraint, the workspace a **hard** one: when they conflict the workspace wins.
//
// This guards against an implementation that is easy to get backwards: "if the user picked one, always use it". Then after the user
// picked an app of workspace A in settings, every agent in workspace B would get A's token,
// a cross-workspace mismatch again, only this time "picked by the user's own hand", even harder to suspect.
//
// Mutation check: replace appInWorkspace under the workspace branch in ResolveApp with a lookup not limited to the workspace,
// and this turns red.
func TestWorkspacePinBeatsPreference(t *testing.T) {
	r := newResolverFixture(t)

	appID, _, err := r.ResolveApp(context.Background(), apimodel.LarkAppQuery{
		Actor: "alice@example.com",
		// Running in workspace mid, but the user's default choice is the app of last.
		WorkspaceID: rsWSMid,
		PreferAppID: rsAppLast,
	})
	if err != nil {
		t.Fatalf("ResolveApp 失败: %v", err)
	}
	if appID != rsAppMid {
		t.Errorf("在空间 %s 里跑就必须用它的应用 %s，实际用了 %s\n"+
			"→ 偏好不许越过空间：越过就是把跨空间错配换个来源重新引入一次",
			rsWSMid, rsAppMid, appID)
	}
}

// When the preference points to an app that "does not exist in this workspace", **quietly fall back to the first in this workspace**,
// rather than failing the whole query.
//
// If it failed, after the user picked an app of workspace A, every agent in workspace B would stop working,
// and the failure would say "no usable Feishu app", pointing at connecting, not at their choice.
func TestPreferenceOutsideWorkspaceIsIgnored(t *testing.T) {
	r := newResolverFixture(t)

	appID, _, err := r.ResolveApp(context.Background(), apimodel.LarkAppQuery{
		Actor:       "alice@example.com",
		WorkspaceID: rsWSMid,
		PreferAppID: "cli_这个应用压根不存在",
	})
	if err != nil {
		t.Fatalf("偏好用不了时应当退回本空间的应用，而不是失败: %v", err)
	}
	if appID != rsAppMid {
		t.Errorf("应当退回本空间的 %s，实际 %s", rsAppMid, appID)
	}
}

// ListUserApps must list all three apps, **with workspace names**.
//
// The workspace name is the only thing in this UI that tells the three same-named apps apart (they are all called `"AMA 助手"`);
// without it users can only guess among three identical options.
func TestListUserAppsCoversEveryWorkspace(t *testing.T) {
	r := newResolverFixture(t)

	apps, err := r.ListUserApps(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("ListUserApps 失败: %v", err)
	}
	if len(apps) != 3 {
		t.Fatalf("三个空间各一个应用，应当列出 3 个，实际 %d 个", len(apps))
	}
	wantWS := map[string]string{
		rsAppDemo: "演示空间",
		rsAppMid:  "中间空间",
		rsAppLast: "最后那个空间",
	}
	for _, app := range apps {
		want, ok := wantWS[app.AppID]
		if !ok {
			t.Errorf("列出了预料之外的应用 %s", app.AppID)
			continue
		}
		if app.WorkspaceName != want {
			t.Errorf("应用 %s 的空间名应当是 %q，实际 %q\n"+
				"→ 三个应用同名（都叫「AMA 助手」），空间名是用户唯一分得清的依据",
				app.AppID, want, app.WorkspaceName)
		}
		if app.Name != "AMA 助手" {
			t.Errorf("应用 %s 的名字应当是「AMA 助手」，实际 %q", app.AppID, app.Name)
		}
	}
}

// ⚠️ **Apps without a secret must not appear in the selectable list**.
//
// If listed, users who pick it get a baffling failure (exchanging a token needs a secret),
// and that failure points at the Feishu API, not at "this app has no secret configured".
//
// The test matches firstUsableApp word for word; a mismatch between the two shows up as "can be picked but does nothing".
func TestListUserAppsSkipsAppsWithoutSecret(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	// One with a secret, one without.
	if _, err := store.CreateApp(ctx, &App{
		ID: "fsa_ok", WorkspaceID: rsWSDemo, AppID: rsAppDemo, Name: "有密钥", Status: StatusConnected,
	}, "secret-demo"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	// Empty-string secret: NULL and the empty string are deliberately not equivalent in the store, but neither can exchange a token.
	if _, err := store.CreateApp(ctx, &App{
		ID: "fsa_nosecret", WorkspaceID: rsWSDemo, AppID: rsAppMid, Name: "没密钥", Status: StatusConnected,
	}, ""); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	svc, err := New(Config{Store: store, Authorizer: allowAuthorizer{}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	r := NewUserAppResolver(svc, fakeWorkspaces{
		ids:   []string{rsWSDemo},
		names: map[string]string{rsWSDemo: "演示空间"},
	})

	apps, err := r.ListUserApps(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("ListUserApps 失败: %v", err)
	}
	if len(apps) != 1 || apps[0].AppID != rsAppDemo {
		t.Fatalf("只有那个配了密钥的应用该被列出，实际 %+v", apps)
	}
}

// When there are no apps at all, ListUserApps returns **a non-nil empty slice**.
//
// nil would be encoded as null, and the frontend would crash at .map on null,
// showing up as the whole picker disappearing (iron rule one of JSON tags in apimodel.go).
func TestListUserAppsReturnsEmptySliceNotNil(t *testing.T) {
	store := NewMemStore()
	svc, err := New(Config{Store: store, Authorizer: allowAuthorizer{}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	r := NewUserAppResolver(svc, fakeWorkspaces{ids: []string{}, names: map[string]string{}})

	apps, err := r.ListUserApps(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("ListUserApps 失败: %v", err)
	}
	if apps == nil {
		t.Fatal("应当返回非 nil 空切片：nil 会编码成 null，前端 .map 会炸")
	}
	if len(apps) != 0 {
		t.Fatalf("没有任何空间时应当是空列表，实际 %d 个", len(apps))
	}
}
