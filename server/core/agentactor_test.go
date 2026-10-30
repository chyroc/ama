package core

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
)

// testModel is the model config required to create an agent (validateAgentDraft requires it).
var testModel = apimodel.ModelConfig{ID: "doubao-seed-2-1-pro-260628"}

// TestCreateAgentStampsActor: creating an agent must record who created it.
func TestCreateAgentStampsActor(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	a, err := svc.CreateAgent(ctx, seedWS, apimodel.AgentDraft{
		Name:   "记账的",
		System: "你负责记账。",
		Model:  testModel,
	})
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	if a.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应为 alice@example.com，实际 %q", a.CreatedBy)
	}
	if a.UpdatedBy != "alice@example.com" {
		t.Errorf("updatedBy 应为 alice@example.com，实际 %q", a.UpdatedBy)
	}
}

// TestUpdateAgentKeepsCreatedBy: after someone else edits it, "who created it" must not be
// overwritten.
//
// ⚠️ This is the easiest place to get this change wrong: UpdateAgent changes fields on **the
// copy it read back**, and also assigning CreatedBy there is a natural slip. After that,
// "who created it" turns into "who edited it last": two fields collapse into one, and
// **no error is reported**. The list page's "created by me" filter would then also include
// "edited by me".
func TestUpdateAgentKeepsCreatedByButChangesUpdatedBy(t *testing.T) {
	svc, _ := newService(t)

	created, err := svc.CreateAgent(asActor("alice@example.com"), seedWS, apimodel.AgentDraft{
		Name:   "记账的",
		System: "你负责记账。",
		Model:  testModel,
	})
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}

	updated, err := svc.UpdateAgent(asActor("bob@example.com"), created.ID, apimodel.AgentDraft{
		Name:   "记账的",
		System: "你负责记账，并且每周汇总。",
		Model:  testModel,
	})
	if err != nil {
		t.Fatalf("改 agent 失败：%v", err)
	}
	if updated.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应仍是 alice（建的人），实际 %q", updated.CreatedBy)
	}
	if updated.UpdatedBy != "bob@example.com" {
		t.Errorf("updatedBy 应变成 bob（改的人），实际 %q", updated.UpdatedBy)
	}
}

// TestUnbindFeishuAppKeepsUpdatedBy: a cascade cleanup is not a user edit and must not touch
// the audit fields.
//
// UnbindFeishuApp is triggered by the feishu module after an app is deleted, and **that path
// has no identity at all** (auth.ActorFrom is always an empty string). Writing UpdatedBy
// there would wipe the existing record to empty: "who edited this agent" is lost for no
// reason, with no error.
//
// This guards the same thing as "do not bump version, do not touch updatedAt"; see that
// method's comment.
func TestUnbindFeishuAppKeepsUpdatedBy(t *testing.T) {
	svc, _ := newService(t)

	created, err := svc.CreateAgent(asActor("alice@example.com"), seedWS, apimodel.AgentDraft{
		Name:         "接飞书的",
		System:       "你接飞书消息。",
		Model:        testModel,
		FeishuAppIDs: []string{"cli_test_unbind"},
	})
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}

	// Simulate the cascade cleanup after the feishu module deletes an app: **a context with no
	// identity**.
	if err := svc.agents.UnbindFeishuApp(context.Background(), "cli_test_unbind"); err != nil {
		t.Fatalf("解绑飞书应用失败：%v", err)
	}

	after, err := svc.GetAgent(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("读回 agent 失败：%v", err)
	}
	if after.UpdatedBy != "alice@example.com" {
		t.Errorf("级联清理不该动 updatedBy，应仍是 alice，实际 %q", after.UpdatedBy)
	}
	if after.CreatedBy != "alice@example.com" {
		t.Errorf("级联清理不该动 createdBy，应仍是 alice，实际 %q", after.CreatedBy)
	}
}

// TestCreateAgentWithoutIdentityLeavesActorEmpty: with no identity, leave it empty instead of
// filling in a fake value.
//
// An empty string with omitempty removes the key entirely, so the frontend reads "unknown
// creator", shown the same way as existing agents. **That is exactly what we want**: a
// guessed owner would be read as fact.
func TestCreateAgentWithoutIdentityLeavesActorEmpty(t *testing.T) {
	svc, _ := newService(t)

	a, err := svc.CreateAgent(context.Background(), seedWS, apimodel.AgentDraft{
		Name:   "没人认领的",
		System: "……",
		Model:  testModel,
	})
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	if a.CreatedBy != "" {
		t.Errorf("无身份时 createdBy 应为空串，实际 %q", a.CreatedBy)
	}
}

// TestResolveUserProfilesDegradesWithoutLookup: without a profile lookup wired in, it must
// degrade, not return an error.
//
// Avatars are purely decorative; if not found, fall back to initials. Letting this break the
// whole list page is not acceptable.
func TestResolveUserProfilesDegradesWithoutLookup(t *testing.T) {
	svc, _ := newService(t)

	got, err := svc.ResolveUserProfiles(context.Background(), []string{"alice@example.com"})
	if err != nil {
		t.Fatalf("未装配时应降级而不是报错，实际：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("未装配时应回空 map，实际 %v", got)
	}
	if got == nil {
		t.Error("应回空 map 而不是 nil——调用方不该再判一次空")
	}
}

// TestResolveUserProfilesLowercasesEmails: email case mismatch must not silently miss.
//
// The users table keys are lowercase (auth.normalizeEmail), while the doc stores CreatedBy as
// is. Without normalizing, "profile not found" would show up with zero errors as "this
// person never logged in", and the avatar would always fall back to initials, which looks
// completely normal.
func TestResolveUserProfilesLowercasesEmails(t *testing.T) {
	svc, _ := newService(t)
	svc.SetUserProfileLookup(stubProfiles{
		byEmail: map[string]apimodel.UserProfile{
			"alice@example.com": {Email: "alice@example.com", Name: "爱丽丝"},
		},
	})

	got, err := svc.ResolveUserProfiles(context.Background(), []string{"Alice@Example.com  "})
	if err != nil {
		t.Fatalf("查档案失败：%v", err)
	}
	if p, ok := got["alice@example.com"]; !ok || p.Name != "爱丽丝" {
		t.Errorf("大小写与空格应被归一化后命中，实际 %v", got)
	}
}

// TestResolveUserProfilesDegradesOnError: an upstream lookup failure only means no avatar.
func TestResolveUserProfilesDegradesOnError(t *testing.T) {
	svc, _ := newService(t)
	svc.SetUserProfileLookup(stubProfiles{err: context.DeadlineExceeded})

	got, err := svc.ResolveUserProfiles(context.Background(), []string{"alice@example.com"})
	if err != nil {
		t.Fatalf("查失败时应降级，实际报错：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("查失败时应回空 map，实际 %v", got)
	}
}

// stubProfiles is a test double for UserProfileLookup.
type stubProfiles struct {
	byEmail map[string]apimodel.UserProfile
	err     error
	// gotEmails records the actual inputs received, to assert "normalization happens before
	// the call".
	gotEmails *[]string
}

func (s stubProfiles) ListUsers(
	_ context.Context, emails []string,
) (map[string]apimodel.UserProfile, error) {
	if s.gotEmails != nil {
		*s.gotEmails = emails
	}
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]apimodel.UserProfile)
	for _, e := range emails {
		if p, ok := s.byEmail[e]; ok {
			out[e] = p
		}
	}
	return out, nil
}

// Let the compiler check interface conformance: if the stub and the real implementation
// signatures drift, it fails right away.
var _ UserProfileLookup = stubProfiles{}

// Reference the auth package to avoid "imported and not used"; asActor is in another file.
var _ = auth.ActorFrom
