// Tests for the AMA integration provider; see integration_ama.go.
//
// This group watches the two places in the four-state mapping that are easiest to get wrong:
//
//	still ok after a revoke              making it error/warning would make the user think it is
//	                                     broken, while nothing needs to be done
//	no secret configured is missing,     missing needs an admin to configure it; clicking a button
//	not error                            does nothing for the user
package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// fakeManagedKeys is a fake implementation of ManagedAPIKeys.
//
// ⚠️ **It deliberately has no Revoke**: the interface no longer has that method (this source does not
// support removing authorization), and the fake narrows with it, so "the provider quietly deletes the
// row" does not even compile here.
type fakeManagedKeys struct {
	enabled bool
	rec     auth.UserAPIKey
	// When recErr is non-nil, Record returns it, to build the two branches "row missing" and
	// "database hiccup".
	recErr error
}

func (f *fakeManagedKeys) Record(_ context.Context, _ string) (auth.UserAPIKey, error) {
	if f.recErr != nil {
		return auth.UserAPIKey{}, f.recErr
	}
	return f.rec, nil
}

func (f *fakeManagedKeys) Enabled() bool { return f.enabled }

const amaTestActor = "alice@example.com"

// readyKeys builds a fake that is "configured, with the row in the database".
func readyKeys() *fakeManagedKeys {
	return &fakeManagedKeys{
		enabled: true,
		rec: auth.UserAPIKey{
			ID:        "ak_abc123",
			Name:      auth.ManagedKeyName,
			Prefix:    "amak_7f3a1b",
			CreatedAt: "2026-08-30T02:00:00.000Z",
		},
	}
}

// TestAMAProviderReadyIsOKAndNotRevocable pins the new contract: once configured the status is ok,
// and **neither action button is offered**.
//
// ⚠️ `CanRevoke == false` is **the contract after the requirement change**, not a missing
// implementation. This once asserted true (the requirement then was "revocable, created again
// automatically after a revoke"); now the requirement says clearly the AMA platform does not support
// removing authorization. Asserting false is meant to turn red right away if "someone adds the
// button back".
func TestAMAProviderReadyIsOKAndNotRevocable(t *testing.T) {
	p := NewAMAProvider(readyKeys(), discardLogger())
	st := p.State(context.Background(), amaTestActor)

	if st.Status != IntegrationOK {
		t.Fatalf("配好之后该是 ok，得到 %q（summary=%q）", st.Status, st.Summary)
	}
	if st.CanAuthorize {
		t.Fatal("这个源没有「发起授权」这个动作，CanAuthorize 必须是 false——" +
			"给了按钮用户点下去只会拿到一个 422")
	}
	if st.CanRevoke {
		t.Fatal("需求：AMA 平台**不支持解除授权**，CanRevoke 必须是 false。" +
			"给了按钮点下去只会拿到 422（Revoke 一律拒绝）")
	}
	if st.ID != AMAIntegrationID {
		t.Fatalf("id 该是 %q，得到 %q", AMAIntegrationID, st.ID)
	}
	// ⚠️ **The ok state deliberately has no Summary / FixHint any more** (trimmed down later).
	//
	// This used to assert "all four states must give a Summary" + "FixHint must point to the API Key
	// page". The reason for the former was "stating the status without the situation says nothing",
	// but for this source the ok sentence "Ready, the agent in the container can operate AMA as you"
	// is **the same sentence** as the green "Ready" chip on the card; saying it again adds nothing.
	//
	// The latter guarded "do not silently block the way out", and that concern still holds, **but the
	// way out is not lost**: it is on the "API Key" page of global settings (only there is there a
	// revoke button, and only there can the prefix and creation time be seen). The rule is "does the
	// user have a decision to make **on this card**": no, this source is automatic all the way.
	//
	// So this assertion is reversed: the ok state **should not** have a Summary, so nobody "adds a
	// sentence on the way" and brings back the one that repeats the chip. The non-ok branches must
	// still explain themselves, each guarded by the cases below.
	if st.Summary != "" {
		t.Errorf("ok 态不该再给 Summary（它与绿色「已就绪」Chip 是同一句话），得到 %q", st.Summary)
	}
	if st.FixHint != "" {
		t.Errorf("ok 态不该再给 FixHint（这个源全程自动，用户没有要做的决定；"+
			"处置凭据的入口在「API Key」页面），得到 %q", st.FixHint)
	}
}

// TestAMAProviderNeverLeaksPlaintextKey pins that **no field of State contains the plain-text key**.
//
// # Why scan every field
//
// The plain text **can be computed** (derivation is deterministic), so "accidentally putting it into
// Summary or FixHint" is a real risk for this provider: those two are free text, and the compiler
// will not stop it. And State goes into the HTTP response, the browser and screenshots unchanged.
//
// ⚠️ **The half "must show prefix / creation time" was deleted** (trimmed down later): the ok state
// now gives no detail at all (those three lines of metadata are on the "API Key" page, the place for
// managing credentials, which also has the revoke button). **The leak check half must stay all the
// more**: it scans all fields regardless of whether details are given, and the risk of the two
// free-text fields Summary/FixHint has not changed at all.
func TestAMAProviderNeverLeaksPlaintextKey(t *testing.T) {
	keys := readyKeys()
	p := NewAMAProvider(keys, discardLogger())
	st := p.State(context.Background(), amaTestActor)

	// Nothing shaped like the full plain text is allowed. Use a probe string that "looks like a whole
	// key": the prefix is amak_ + 6 characters, a whole key is amak_ + 43 characters.
	fullLooking := auth.KeyPrefix + strings.Repeat("x", 43)
	blob := st.Summary + "\n" + st.FixHint + "\n" + st.Description
	for _, d := range st.Details {
		blob += "\n" + d.Label + "\n" + d.Value
	}
	if strings.Contains(blob, fullLooking) {
		t.Fatal("State 里出现了整把 key 形状的串")
	}
	// A more practical check: no detail value should be long enough to hold a complete key.
	for _, d := range st.Details {
		if len(d.Value) >= len(auth.KeyPrefix)+43 {
			t.Fatalf("detail「%s」的值长达 %d 字符，疑似把整把 key 显示出来了",
				d.Label, len(d.Value))
		}
	}
}

// TestAMAProviderMissingRowStaysOK pins the easiest one to get wrong: **when the row is not in the
// database, it is still ok**.
//
// The row can be missing for two reasons (still true after the button on the card was removed): no
// agent has ever used it (lazy creation), or the user revoked it themselves on the API Key page. The
// next injection derives the same key again and creates the row, so at this point:
//
//   - the status is still ok (not error and not warning): the user needs to do nothing;
//   - the message must say "it will be created automatically next time", otherwise the user thinks
//     they broke the feature.
//
// Making it error would leave the user looking for "how to restore it" on a red card, when there is
// nothing to restore.
func TestAMAProviderMissingRowStaysOK(t *testing.T) {
	keys := readyKeys()
	keys.recErr = auth.ErrNotFound
	p := NewAMAProvider(keys, discardLogger())
	st := p.State(context.Background(), amaTestActor)

	if st.Status != IntegrationOK {
		t.Fatalf("行不在时仍该是 ok（下次自动重建），得到 %q——"+
			"显示成 %q 会让用户以为坏了，而他什么都不用做", st.Status, st.Status)
	}
	if st.CanRevoke {
		t.Fatal("这个源不支持解除授权，任何分支都不该给撤销按钮")
	}
	if st.FixHint == "" {
		t.Fatal("要说清「这是正常状态、下次会自动创建」，否则用户以为功能坏了")
	}
	// With the row missing, the previous key's prefix/creation time must not be shown: that row no
	// longer exists.
	if len(st.Details) != 0 {
		t.Fatalf("行不在时不该再显示凭据信息，得到 %+v", st.Details)
	}
}

// TestAMAProviderMissingWhenUnconfigured pins "no derivation secret configured is missing, not error".
//
// This distinction is the whole reason the four-state model exists: missing needs **an admin** to
// configure it, and clicking the authorize button a hundred times does not help. So missing offers no
// action buttons, only "what must be configured".
func TestAMAProviderMissingWhenUnconfigured(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys ManagedAPIKeys
	}{
		{"依赖没装配", nil},
		{"装配了但没配密钥", &fakeManagedKeys{enabled: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewAMAProvider(tc.keys, discardLogger())
			st := p.State(context.Background(), amaTestActor)

			if st.Status != IntegrationMissing {
				t.Fatalf("该是 missing（要管理员去配），得到 %q", st.Status)
			}
			if st.CanAuthorize || st.CanRevoke {
				t.Fatalf("missing 时两个按钮都不该给（用户点了没有任何用）"+
					"CanAuthorize=%v CanRevoke=%v", st.CanAuthorize, st.CanRevoke)
			}
			if !strings.Contains(st.FixHint, "managedKeySecret") {
				t.Fatalf("FixHint 要点名要配哪个配置项，得到 %q", st.FixHint)
			}
		})
	}
}

// TestAMAProviderTransientErrorIsWarning pins "a failed database query degrades to warning, not error".
//
// Exactly the same reason as the passage on isTransient: showing one failed query as "unavailable"
// would make the user revoke and rebuild, while their credential is fine, a completely wasted
// operation. And ama in the container is not affected by this query at all (the credential is
// fetched at execution time), so saying "unavailable" would be **false**.
func TestAMAProviderTransientErrorIsWarning(t *testing.T) {
	keys := readyKeys()
	keys.recErr = errors.New("dial tcp: connection refused")
	p := NewAMAProvider(keys, discardLogger())
	st := p.State(context.Background(), amaTestActor)

	if st.Status != IntegrationWarning {
		t.Fatalf("查库失败该降级成 warning，得到 %q——"+
			"显示成 error 会让用户白撤销重建一遍", st.Status)
	}
	if st.CanRevoke {
		t.Fatal("不知道有没有行时不该给撤销按钮")
	}
	// ⚠️ The underlying error may contain things like connection strings; it should not be shown to
	// the user as is.
	if strings.Contains(st.Summary+st.FixHint, "connection refused") {
		t.Fatal("底层错误原文不该进 State（它会原样显示给用户）")
	}
}

// TestAMAProviderAuthorizeUnsupported pins "there is no authorize action".
func TestAMAProviderAuthorizeUnsupported(t *testing.T) {
	p := NewAMAProvider(readyKeys(), discardLogger())
	if _, err := p.AuthorizeURL(context.Background(), amaTestActor); !errors.Is(err, ErrAuthorizeUnsupported) {
		t.Fatalf("该回 ErrAuthorizeUnsupported，得到 %v", err)
	}
}

// TestAMAProviderRevokeAlwaysRejected pins "calling the DELETE endpoint is rejected".
//
// # ⚠️ Why this cannot be skipped
//
// `CanRevoke = false` only makes **the frontend not render the button**, while
// `DELETE /api/integrations/{provider}` is a generic route (one of those four in matrix.go) that
// anyone can call directly. So "hide the button" and "cannot revoke" are two different things;
// testing only the former tests nothing: the credential underneath could still be deleted, a hidden
// hole with no UI that nobody would think to look at.
//
// Each of the four status branches is hit once: **not configured, ready, row missing, database
// hiccup; none may let it through**. Testing only the ready one would still miss "some branch still
// has a path that can delete".
func TestAMAProviderRevokeAlwaysRejected(t *testing.T) {
	notFound := readyKeys()
	notFound.recErr = auth.ErrNotFound
	flaky := readyKeys()
	flaky.recErr = errors.New("dial tcp: connection refused")

	for _, tc := range []struct {
		name string
		keys ManagedAPIKeys
	}{
		{"依赖没装配", nil},
		{"装配了但没配密钥", &fakeManagedKeys{enabled: false}},
		{"就绪", readyKeys()},
		{"行不在", notFound},
		{"查库失败", flaky},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := NewAMAProvider(tc.keys, discardLogger()).
				Revoke(context.Background(), amaTestActor)

			// persistmodel.ErrPrecondition → 422 (the fail mapping in handler.go).
			if !errors.Is(err, persistmodel.ErrPrecondition) {
				t.Fatalf("该回 persistmodel.ErrPrecondition（→422），得到 %v", err)
			}
			// It must also carry ErrRevokeUnsupported: **this is not "not this time" but "this
			// capability does not exist at all"**. The frontend shows the sentence as is, and only
			// both sentinels together say "do not try again".
			if !errors.Is(err, ErrRevokeUnsupported) {
				t.Fatalf("该带 ErrRevokeUnsupported（表示平台侧确实没有这个能力），得到 %v", err)
			}
			// ⚠️ The error message must give a way out. The frontend's catch deliberately does not
			// hard-code "unsupported" and shows the backend's sentence as is, so without directions
			// the user only sees "no".
			if !strings.Contains(err.Error(), "API Key") {
				t.Fatalf("拒绝信息要指出「去 API Key 页面处置」，得到 %q", err.Error())
			}
		})
	}
}

// TestAMAProviderRevokeDoesNotPromiseInvalidation pins that the rejection message **must not hint
// "revoking makes it invalid"**.
//
// That would be false: revoking only deletes the row, and the next injection derives **the same**
// plain text ("Revoking means reset, not rotate" in auth/managedkey.go, pinned byte for byte as
// after == before by TestManagedKeyRevokeThenReinjectRevives).
//
// And the cost of that lie is a security one: the user thinks they invalidated a leaked credential,
// so they stop reporting it and do not ask an admin to replace managedKeySecret, **while that token
// comes back to life at the next injection**.
func TestAMAProviderRevokeDoesNotPromiseInvalidation(t *testing.T) {
	err := NewAMAProvider(readyKeys(), discardLogger()).
		Revoke(context.Background(), amaTestActor)
	if err == nil {
		t.Fatal("该拒绝")
	}
	msg := err.Error()

	// It must make clear that path is a "reset", not a rotation.
	if !strings.Contains(msg, "重置") {
		t.Fatalf("要点明吊销的语义是重置（下次会重新生成同一把），得到 %q", msg)
	}
	// The only way to really replace a leaked credential is replacing the server secret; name it.
	if !strings.Contains(msg, "managedKeySecret") {
		t.Fatalf("要点名「真要换凭据得管理员换 auth.managedKeySecret」，得到 %q", msg)
	}
}

// TestAMAProviderSatisfiesInterface confirms at compile time that it satisfies IntegrationProvider.
//
// **This line cannot be skipped**: on a signature mismatch the error would be delayed to the
// Register line in main.go, and that error would not point to this file.
var _ IntegrationProvider = (*AMAProvider)(nil)

// TestAMAProviderIDIsStable pins that the id is the same constant the injection path uses.
//
// The injection side decides by AMAIntegrationID "has this agent ticked the AMA integration"; a
// mismatch between the two shows up as "ticked but not injected", with no error.
func TestAMAProviderIDIsStable(t *testing.T) {
	if got := NewAMAProvider(readyKeys(), discardLogger()).ID(); got != AMAIntegrationID {
		t.Fatalf("ID() 与常量不一致：%q vs %q", got, AMAIntegrationID)
	}
	if AMAIntegrationID != "ama" {
		t.Fatalf("id 变了（%q）——它进 URL、进前端 testid、进 agent 的 integrationIds，"+
			"改它需要数据迁移", AMAIntegrationID)
	}
}
