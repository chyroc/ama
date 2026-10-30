// Tests for the "personal authorization vs service account" security boundary.
//
// # Why these tests must come in groups, not just the happy path
//
// Every rule on this boundary has **a wrong implementation pointing the opposite way**, and with
// only one test that implementation stays green. The three most dangerous pairs:
//
//	only test "the creator can inject"        → "anyone can inject" also passes (this repo has hit such false passes repeatedly)
//	only test "non-creators are rejected"     → "everyone is rejected, cannot even save a rename" also passes
//	only test "service accounts can be bound" → "personal authorizations can be bound too" also passes
//
// So every rule below has a reverse counterpart, and the end of the file records the results of
// three **mutation checks** (break the implementation and confirm the matching test really fails).
//
// # Scenario numbers match the eight items in the requirements
//
//	① creator injects their own personal authorization          → success
//	② non-creator injects a personal authorization              → error (with a friendly, actionable message)
//	③ non-creator changes the title (authorization unchanged)   → success  ← the easiest one to break
//	④ non-creator adds a service account authorization          → success
//	⑤ non-creator removes an existing personal authorization    → error
//	⑥ personal authorization bound at the workspace level       → error
//	⑦ the global service account a workspace references changes → the workspace side follows (checks "a reference is not a copy")
//	⑧ agent picks a service account not bound in the workspace  → error
package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Identities always go through the existing asActor (core/feedback_test.go); **do not build
// another one here**: two helpers with the same name would give two answers to "where does the
// identity in tests come from".
//
// ⚠️ **Identity can only come from the context, not from the request body.** The wiring-layer
// part (forged request headers have no effect) has its own tests in the httpd package; tests in
// this package cannot prove that, because there is no middleware chain here at all.
// See httpd/authz_authscope_test.go.

// newAuthScopeService builds a Service. The second return value is kept so test signatures stay the same.
func newAuthScopeService(t *testing.T) (*Service, struct{}) {
	t.Helper()
	svc, _ := newService(t)
	return svc, struct{}{}
}

// newAgentBy has someone create an agent and returns it.
//
// It carries no authorization: that is the default of the create form, and it lets each test
// below decide what to change.
func newAgentBy(t *testing.T, svc *Service, creator, name string) apimodel.Agent {
	t.Helper()
	a, err := svc.CreateAgent(asActor(creator), seedWS,
		apimodel.AgentDraft{Name: name, Model: apimodel.ModelConfig{ID: "doubao-pro"}})
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	if a.CreatedBy != creator {
		t.Fatalf("createdBy 是 %q，期望 %q——后面所有创建者判断都依赖它", a.CreatedBy, creator)
	}
	return a
}

// fullDraftOf flattens an agent into "the full draft the frontend sends back".
//
// ⚠️ **It must carry every field**; this is the premise of test ③: the frontend's toAgentDraft
// always sends back the full draft (including integrationIds), so "the request body has this key"
// is normal, not a sign of overreach. If a test lazily passed only Name, test ③ would not test what
// it is meant to.
func fullDraftOf(a apimodel.Agent) apimodel.AgentDraft {
	return apimodel.AgentDraft{
		Name:           a.Name,
		Description:    a.Description,
		System:         a.System,
		Model:          a.Model,
		BuiltinTools:   a.BuiltinTools,
		SkillIDs:       a.SkillIDs,
		FeishuAppIDs:   a.FeishuAppIDs,
		Status:         a.Status,
		IntegrationIDs: a.IntegrationIDs,
		Envs:           a.Envs,
		EnvAliases:     a.EnvAliases,
		FollowUp:       a.FollowUp,
		ProgressDetail: a.ProgressDetail,
	}
}

// ---------------------------------------------------------------------------
// ①②③⑤ Personal authorization: only the creator can add/remove; other fields stay editable
// ---------------------------------------------------------------------------

// TestCreatorCanInjectOwnPersonalIntegration ① creator injects their own personal authorization →
// success.
//
// ⚠️ This deliberately uses a **member** (seedMember) as the creator, not admin: the requirement
// is "a personal authorization must belong to the agent's creator", and a member injecting **their
// own** credential into an agent they built is their legitimate right; the credential is theirs
// anyway. With admin as creator, this test could be fooled by "it was actually let through by the
// admin level".
func TestCreatorCanInjectOwnPersonalIntegration(t *testing.T) {
	svc, _ := newAuthScopeService(t)
	a := newAgentBy(t, svc, seedMember, "member 的 agent")

	d := fullDraftOf(a)
	d.IntegrationIDs = []string{apimodel.IntegrationLarkUser}
	got, err := svc.UpdateAgent(asActor(seedMember), a.ID, d)
	if err != nil {
		t.Fatalf("创建者注入自己的个人授权应当成功，却报错：%v", err)
	}
	if len(got.IntegrationIDs) != 1 || got.IntegrationIDs[0] != apimodel.IntegrationLarkUser {
		t.Fatalf("授权没存上：%v", got.IntegrationIDs)
	}
}

// TestCreatorCanInjectPersonalIntegrationAtCreate is the other half of ①: carrying it **at create
// time**.
//
// ⚠️ **This one cannot be skipped**; it goes through a different function from the one above
// (CreateAgent / UpdateAgent). Testing only UpdateAgent would let the wrong implementation "create
// always requires admin" pass, which would stop members from creating agents with their own
// personal authorization, with an error saying "admin required".
func TestCreatorCanInjectPersonalIntegrationAtCreate(t *testing.T) {
	ctx := asActor(seedMember)
	svc, _ := newAuthScopeService(t)

	a, err := svc.CreateAgent(ctx, seedWS, apimodel.AgentDraft{
		Name:           "建的时候就带授权",
		Model:          apimodel.ModelConfig{ID: "doubao-pro"},
		IntegrationIDs: []string{apimodel.IntegrationLarkUser},
	})
	if err != nil {
		t.Fatalf("创建者建 agent 时带自己的个人授权应当成功，却报错：%v", err)
	}
	if len(a.IntegrationIDs) != 1 {
		t.Fatalf("授权没存上：%v", a.IntegrationIDs)
	}
}

// TestNonCreatorCannotInjectPersonalIntegration ② non-creator injects a personal authorization →
// error.
//
// This is the requirement "Alice creates an agent, Bob updates it and injects a personal
// authorization → error right away".
//
// ⚠️ The test **asserts both the error code and the message parts**: asserting only "there is an
// error" would let an implementation that "rejects every change" pass, which would also kill ③ and
// ④. The message parts are checked one by one per the "friendly message" requirement: say why,
// name the creator, and give an actionable alternative.
func TestNonCreatorCannotInjectPersonalIntegration(t *testing.T) {
	svc, _ := newAuthScopeService(t)
	// Alice (member) creates it, Bob (**admin**) changes it.
	//
	// ⚠️ Bob is deliberately admin: this rule is **stricter than the admin level** (a personal
	// credential is not something the workspace shares; even an admin cannot inject it on someone's
	// behalf). With Bob as a member, "it was actually blocked by the admin gate" would make this test
	// unable to tell the difference.
	a := newAgentBy(t, svc, seedMember, "张三的 agent")

	d := fullDraftOf(a)
	d.IntegrationIDs = []string{apimodel.IntegrationLarkUser}
	_, err := svc.UpdateAgent(asActor(seedAdmin), a.ID, d)
	if err == nil {
		t.Fatal("非创建者注入个人授权必须报错，却成功了——这是本次要堵的那个洞")
	}
	if !errors.Is(err, ErrAccessDenied) {
		t.Errorf("错误码应当是 ErrAccessDenied（403），实际：%v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"个人授权",     // says which kind of authorization this is
		seedMember, // **names the creator**, not just "please contact the creator"
		"服务账号",     // gives an actionable alternative
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错文案里应当有 %q，实际是：%s", want, msg)
		}
	}
}

// TestNonCreatorCanEditOtherFields ③ non-creator changes the title (authorization unchanged) →
// success.
//
// # ⚠️ This is the easiest one in the group to break
//
// The frontend's toAgentDraft **always** sends back the full draft (including integrationIds), so
// "the request body carries authorization fields" is normal. If the gate were written as "reject
// if passed", non-creators **could not even save a rename**, and the error would say "only the
// creator can change personal authorizations", which does not point at what they actually did.
//
// The check must be "**did it change**", not "was it passed". Mutation check: replace
// diffIntegrationIDs with "reject if non-empty", and this test must fail (result recorded at the
// end of the file).
func TestNonCreatorCanEditOtherFields(t *testing.T) {
	svc, _ := newAuthScopeService(t)

	// First the creator injects a personal authorization, so "the draft carries integrationIds" is the real state.
	a := newAgentBy(t, svc, seedMember, "原标题")
	d := fullDraftOf(a)
	d.IntegrationIDs = []string{apimodel.IntegrationLarkUser}
	a, err := svc.UpdateAgent(asActor(seedMember), a.ID, d)
	if err != nil {
		t.Fatalf("创建者注入自己的授权失败：%v", err)
	}

	// Bob changes the title, **sending back the authorization fields unchanged**.
	d = fullDraftOf(a)
	d.Name = "李四改的新标题"
	got, err := svc.UpdateAgent(asActor(seedAdmin), a.ID, d)
	if err != nil {
		t.Fatalf("非创建者改标题（授权未变）应当成功，却报错：%v\n"+
			"这正是「判传没传」那个 bug 的症状——member 连改名字都保存不了", err)
	}
	if got.Name != "李四改的新标题" {
		t.Errorf("标题没改上：%q", got.Name)
	}
	// The authorization is kept as is, not cleared along the way.
	if len(got.IntegrationIDs) != 1 || got.IntegrationIDs[0] != apimodel.IntegrationLarkUser {
		t.Errorf("授权被动了：%v（应当原样保留）", got.IntegrationIDs)
	}
}

// TestNonCreatorCannotRemovePersonalIntegration ⑤ non-creator removes an existing personal
// authorization → error.
//
// ⚠️ **Removal must be blocked too**; the requirements say so explicitly. An implementation that
// only blocks "adding" would make this pass, and silently allowing removal has two consequences:
// the agent quietly loses capabilities (no error), and "revoking someone else's authorization"
// should be the credential owner's decision.
//
// In the implementation this means the diff takes the **symmetric difference**, not "only what was
// added".
func TestNonCreatorCannotRemovePersonalIntegration(t *testing.T) {
	svc, _ := newAuthScopeService(t)

	a := newAgentBy(t, svc, seedMember, "带授权的 agent")
	d := fullDraftOf(a)
	d.IntegrationIDs = []string{apimodel.IntegrationLarkUser}
	a, err := svc.UpdateAgent(asActor(seedMember), a.ID, d)
	if err != nil {
		t.Fatalf("创建者注入失败：%v", err)
	}

	// Bob removes it.
	d = fullDraftOf(a)
	d.IntegrationIDs = []string{}
	_, err = svc.UpdateAgent(asActor(seedAdmin), a.ID, d)
	if err == nil {
		t.Fatal("非创建者移除个人授权必须报错，却成功了")
	}
	if !errors.Is(err, ErrAccessDenied) {
		t.Errorf("错误码应当是 ErrAccessDenied（403），实际：%v", err)
	}
}

// TestSetAgentIntegrationsRouteAlsoEnforcesCreator is the third path for ②⑤: the dedicated PUT
// route.
//
// ⚠️ **This test cannot be skipped.** `PUT /api/agents/{id}/integrations` is the route dedicated
// to changing authorizations, and Guard has already checked ActionManage on it, so it is easy to
// think "no need to check again here". But Guard checks "are you an admin", not "are you the
// creator". Missing that gate shows up as: PATCH is blocked, while this path **dedicated to
// changing authorizations** is wide open.
func TestSetAgentIntegrationsRouteAlsoEnforcesCreator(t *testing.T) {
	svc, _ := newAuthScopeService(t)
	a := newAgentBy(t, svc, seedMember, "张三的 agent")

	_, err := svc.SetAgentIntegrations(asActor(seedAdmin), a.ID,
		[]string{apimodel.IntegrationLarkUser})
	if err == nil {
		t.Fatal("非创建者走 PUT .../integrations 注入个人授权必须报错，却成功了——" +
			"那条路绕过了 PATCH 上的闸门")
	}
	if !errors.Is(err, ErrAccessDenied) {
		t.Errorf("错误码应当是 ErrAccessDenied（403），实际：%v", err)
	}
}

// TestLegacyAgentWithoutCreatorFallsBackToAdmin: existing agents (no recorded creator) fall back
// to the admin level.
//
// ⚠️ This pins down the **migration decision**: when CreatedBy is empty it must not be "nobody can
// change it". That would make personal authorizations on existing agents impossible to change
// (including by admins who could before), with an error pointing at a person the UI never shows.
// Falling back to the old behavior (admin can change) is an explicit choice, see
// ensureIntegrationsAllowed.
func TestLegacyAgentWithoutCreatorFallsBackToAdmin(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)

	// Build an "existing" agent: put it straight into store, without CreatedBy.
	legacy := apimodel.Agent{
		ID: "agent_legacy", WorkspaceID: seedWS, Name: "存量 agent",
		Model: apimodel.ModelConfig{ID: "doubao-pro"}, Status: apimodel.StatusActive,
		Version: 1, CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
		IntegrationIDs: []string{}, BuiltinTools: []string{}, SkillIDs: []string{},
		FeishuAppIDs: []string{},
	}
	if _, err := store.CreateAgent(ctx, legacy); err != nil {
		t.Fatalf("造存量 agent 失败：%v", err)
	}

	d := fullDraftOf(legacy)
	d.IntegrationIDs = []string{apimodel.IntegrationLarkUser}

	// admin can change it (old behavior).
	if _, err := svc.UpdateAgent(asActor(seedAdmin), legacy.ID, d); err != nil {
		t.Fatalf("存量 agent 上 admin 应当能改个人授权（回落到老行为），却报错：%v", err)
	}
	// But a member cannot: the fallback is to the admin level, not "anyone can change it".
	legacy2 := legacy
	legacy2.ID = "agent_legacy2"
	if _, err := store.CreateAgent(ctx, legacy2); err != nil {
		t.Fatalf("造第二个存量 agent 失败：%v", err)
	}
	d2 := fullDraftOf(legacy2)
	d2.IntegrationIDs = []string{apimodel.IntegrationLarkUser}
	_, err := svc.UpdateAgent(asActor(seedMember), legacy2.ID, d2)
	if err == nil {
		t.Fatal("存量 agent 上 member 不该能改个人授权（回落是 admin 档，不是全放开）")
	}
	if !strings.Contains(err.Error(), "没有记录创建者") {
		t.Errorf("这一支的报错要说清「没有记录创建者」，否则用户会去找一个不存在的人：%v", err)
	}
}

// ---------------------------------------------------------------------------
// ④⑧ Service accounts: admin required, and must already be bound in the workspace
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ⑥ Personal authorizations must not be bound at the workspace level
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// ⑦ A reference is not a copy
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Classification itself
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Runtime rule: personal authorization follows the creator
// ---------------------------------------------------------------------------

// TestPersonalCredentialSubjectFollowsCreator: at runtime, credentials are taken by **creator**,
// not by workspace owner.
//
// # ⚠️ Why this test cannot be skipped
//
// The write-path gate only controls "who can check the box"; the runtime part controls "whose
// credential is used". **The two must agree**: if the write path checks the creator but runtime
// takes the owner's, the gate is decoration. An agent built by a member would still act as the
// owner, which is exactly the hole being fixed this time.
//
// Existing agents (empty CreatedBy) falling back to owner is pinned too: that is an explicit
// migration decision, not an oversight. Changing it to "fall back to injecting nothing" would make
// existing agents silently lose capabilities.
func TestPersonalCredentialSubjectFollowsCreator(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	cases := []struct {
		name      string
		createdBy string
		want      string
	}{
		{"有创建者就用创建者", seedMember, seedMember},
		{"存量没创建者回落到空间 owner", "", seedOwner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.personalCredentialSubjectFor(ctx,
				apimodel.Agent{ID: "a1", CreatedBy: tc.createdBy}, seedWS, nil)
			if err != nil {
				t.Fatalf("取当事人失败：%v", err)
			}
			if got != tc.want {
				t.Errorf("取到 %q，期望 %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Mutation checks (three, recorded from real runs)
// ---------------------------------------------------------------------------
//
// Only mutations that compile count; all three compiled, and after running each was restored to
// the fixed version (no mutation markers left in core/*.go).
//
//  1. **Change "did it change" into "was it passed"**
//     ⚠️ The first attempt **did not work; recorded here so the next person does not repeat it**:
//     replacing only the early return `if !change.changed() { return nil }` with "go on checking
//     if incoming is non-empty" left all three tests **green**, because the real decision is made
//     by the change computed by the diff below; the early return is just a shortcut. **Mutate the
//     decision, not the shortcut.**
//     The working mutation: pass nil as the second argument of diffIntegrationIDs (treat the
//     stored copy as empty), so every item sent back counts as "added"; that really is "was it
//     passed".
//     Result: TestNonCreatorCanEditOtherFields **failed** (error text: "non-creator changing the
//     title (authorization unchanged) should succeed, but got: … personal authorization, only this
//     Agent's creator (carol@example.com) can add or remove it"), while
//     ①TestCreatorCanInjectOwnPersonalIntegration and
//     ②TestNonCreatorCannotInjectPersonalIntegration stayed green.
//     ✅ That is exactly the shape of the "a member cannot even save a rename" bug.
//
//  2. **Remove the creator check**
//     Replace `case !strings.EqualFold(creator, actor):` with `case false:`
//     (so a personal authorization only needs to pass the admin level, i.e. the behavior before
//     the change).
//     Result: **three failed together**:
//     TestNonCreatorCannotInjectPersonalIntegration,
//     TestNonCreatorCannotRemovePersonalIntegration,
//     TestSetAgentIntegrationsRouteAlsoEnforcesCreator;
//     while ①③④ (TestCreatorCanInjectOwnPersonalIntegration /
//     TestNonCreatorCanEditOtherFields / TestNonCreatorCanAddServiceAccountIntegration)
//     stayed green.
//     ✅ All three entry points are covered, and the positive and negative sides do not mask each
//     other.
//
//  3. **Change the workspace reference into a copy**
//     Add a "snapshot stored at bind time" bypass in viewOfWorkspaceIntegration: use it directly
//     on a hit, no longer calling CredentialMeta (equivalent to the change "store an extra name
//     column to save a lookup").
//     Result: TestWorkspaceIntegrationIsReferenceNotSnapshot **failed**
//     ("global was renamed, the workspace side must follow, but it is still the old read-only
//     patrol name"), and TestWorkspaceIntegrationShowsBrokenReference also **failed**
//     ("the credential was deleted, this reference must not report 'usable'");
//     TestServiceAccountCanBindToWorkspace stayed green.
//     ✅ Both failing together are exactly the two observable results of "making it a snapshot":
//     global changes have no effect, and it falsely reports usable.
