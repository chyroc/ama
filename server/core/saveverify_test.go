// Behavior tests for the three-level online check run before saving an Ark credential.
//
// The classification itself (401/not activated/5xx/200 -> three levels) is pinned in
// arkverify_test.go with a fake Ark. What is pinned here is **what the save path does after it
// gets a level**:
//
//	bad key          hard block: returns ErrPrecondition + fine code ark_key_invalid, **no row in DB**;
//	not activated    blocked the first time (fine code ark_managed_agent_not_open, not saved),
//	                 saved only when resent with ConfirmNotActivated;
//	dependency fault allowed + response carries a transient warning; GET has no warning;
//	OK               allowed, no warning.
//
// All three scopes (workspace / account / agent) share verifyBeforeSave. The workspace scope tests
// all four cases; the other two each pin the most important interaction, "not activated can be
// saved after confirming".

package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/persistmodel"
	arkcredsvc "github.com/chyroc/ama/server/service/arkcred"
)

const testArkKey = "ARK0123456789abcdef0123456789abcd"

// rejectCode extracts the fine code from a save error; returns an empty string when it is not a
// *SaveRejectedError.
func rejectCode(err error) string {
	var rejected *arkcredsvc.SaveRejectedError
	if errors.As(err, &rejected) {
		return rejected.FailCode()
	}
	return ""
}

// Bad key: hard block + not saved.
func TestSaveRejectsInvalidKey(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	fake := &fakeVerifier{result: arkcredsvc.VerifyResult{
		Status: arkcredsvc.VerifyStatusInvalidKey, RequestID: "req-x",
	}}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	_, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing",
	})
	if !errors.Is(err, persistmodel.ErrPrecondition) {
		t.Fatalf("坏 key 应挂 ErrPrecondition（回 422），实际 %v", err)
	}
	if code := rejectCode(err); code != arkcredsvc.RejectCodeInvalidKey {
		t.Fatalf("细码应为 %s，实际 %q", arkcredsvc.RejectCodeInvalidKey, code)
	}
	if !strings.Contains(err.Error(), "request_id：req-x") {
		t.Errorf("坏 key 文案应带 request_id 便于报障，实际 %q", err.Error())
	}
	// **This row must not be in the DB**: the point of a hard block is that nothing is saved.
	got, gerr := svc.GetArkCredential(ctx, seedWS)
	if gerr != nil {
		t.Fatalf("读凭证不该报错：%v", gerr)
	}
	if got.ApiKeyMasked != "" {
		t.Errorf("被拦的坏 key 不该落库，实际掩码 %q", got.ApiKeyMasked)
	}
	if fake.calls != 1 {
		t.Errorf("应只打一次探针，实际 %d", fake.calls)
	}
}

// Not activated: blocked first, allowed after confirming; and the bad-key level ignores the
// confirm flag.
func TestSaveNotActivatedRequiresConfirmation(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	fake := &fakeVerifier{result: arkcredsvc.VerifyResult{
		Status: arkcredsvc.VerifyStatusNotActivated, AccountID: "2101581202",
	}}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	// First time: no confirmation -> blocked.
	_, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing",
	})
	if !errors.Is(err, persistmodel.ErrPrecondition) {
		t.Fatalf("未开通首次应被拦，实际 %v", err)
	}
	if code := rejectCode(err); code != arkcredsvc.RejectCodeNotActivated {
		t.Fatalf("细码应为 %s，实际 %q", arkcredsvc.RejectCodeNotActivated, code)
	}
	if !strings.Contains(err.Error(), "2101581202") || !strings.Contains(err.Error(), "仍然保存") {
		t.Errorf("文案要带账号 id 与「仍然保存」出口，实际 %q", err.Error())
	}
	if masked, _ := svc.GetArkCredential(ctx, seedWS); masked.ApiKeyMasked != "" {
		t.Error("确认前不该落库")
	}

	// Second time: with confirmation -> saved.
	out, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing", ConfirmNotActivated: true,
	})
	if err != nil {
		t.Fatalf("确认后应放行保存，实际 %v", err)
	}
	if out.ApiKeyMasked == "" {
		t.Error("确认后应真的存下来")
	}
}

// The confirm flag has no effect on a bad key: ConfirmNotActivated cannot force-save a bad key.
func TestSaveConfirmationDoesNotBypassInvalidKey(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	fake := &fakeVerifier{result: arkcredsvc.VerifyResult{
		Status: arkcredsvc.VerifyStatusInvalidKey,
	}}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	_, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing", ConfirmNotActivated: true,
	})
	if !errors.Is(err, persistmodel.ErrPrecondition) ||
		rejectCode(err) != arkcredsvc.RejectCodeInvalidKey {
		t.Fatalf("坏 key 即便带确认标记也必须拦，实际 %v", err)
	}
}

// Dependency fault (5xx/timeout/unrecognized error): allowed + transient warning, and the warning
// is not stored.
func TestSaveSkippedOnTransientWarnsButPersists(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	fake := &fakeVerifier{result: arkcredsvc.VerifyResult{
		Status: arkcredsvc.VerifyStatusSkipped, Cause: errors.New("dial tcp: i/o timeout"),
	}}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	out, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing",
	})
	if err != nil {
		t.Fatalf("依赖故障必须放行保存，实际返回错误：%v", err)
	}
	if out.Warning == nil || out.Warning.Kind != "verify_unavailable" {
		t.Fatalf("放行时要带 verify_unavailable 警告，实际 %+v", out.Warning)
	}

	// GET has no warning (transient, not stored).
	got, _ := svc.GetArkCredential(ctx, seedWS)
	if got.Warning != nil {
		t.Errorf("warning 不该入库/随 GET 返回，实际 %+v", got.Warning)
	}
	if got.ApiKeyMasked == "" {
		t.Error("key 应已落库")
	}
}

// Probe says OK: saved normally, no warning.
func TestSaveOKWithoutWarning(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	fake := &fakeVerifier{result: arkcredsvc.VerifyResult{Status: arkcredsvc.VerifyStatusOK}}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	out, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing",
	})
	if err != nil {
		t.Fatalf("OK 时应保存成功：%v", err)
	}
	if out.Warning != nil {
		t.Errorf("OK 时不该有警告，实际 %+v", out.Warning)
	}
}

// No verifier wired (local / some test setups): save behaves exactly as before, no block, no
// warning.
func TestSaveWithoutVerifierUnchanged(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	svc, _ := New(Config{Store: store}) // no ArkVerifier

	out, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing",
	})
	if err != nil {
		t.Fatalf("无 verifier 时应照常保存：%v", err)
	}
	if out.Warning != nil {
		t.Errorf("无 verifier 不该有警告")
	}
}

// Account scope: not activated is blocked the first time and saved after confirming; after
// confirming there should be no warning (the user just made the choice).
func TestSaveGlobalNotActivatedConfirmation(t *testing.T) {
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Email: "me@example.com"})
	store, _ := coremem.NewSeededMemStore()
	fake := &fakeVerifier{result: arkcredsvc.VerifyResult{
		Status: arkcredsvc.VerifyStatusNotActivated, AccountID: "2100583705",
	}}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing",
	}); rejectCode(err) != arkcredsvc.RejectCodeNotActivated {
		t.Fatalf("账号级首次也应被未开通警告拦下，实际 %v", err)
	}

	out, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing", ConfirmNotActivated: true,
	})
	if err != nil {
		t.Fatalf("确认后应放行：%v", err)
	}
	if out.ApiKeyMasked == "" || out.Warning != nil {
		t.Errorf("确认后应静默存下（无 warning），实际 %+v", out)
	}
}

// Agent scope: not activated is blocked the first time and saved after confirming.
func TestSaveAgentNotActivatedConfirmation(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	fake := &fakeVerifier{result: arkcredsvc.VerifyResult{
		Status: arkcredsvc.VerifyStatusNotActivated,
	}}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	if _, err := svc.SaveAgentArkCredential(ctx, "agent_triage", ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing",
	}); rejectCode(err) != arkcredsvc.RejectCodeNotActivated {
		t.Fatalf("agent 级首次也应被未开通警告拦下，实际 %v", err)
	}

	out, err := svc.SaveAgentArkCredential(ctx, "agent_triage", ArkCredentialDraft{
		APIKey: testArkKey, Region: "cn-beijing", ConfirmNotActivated: true,
	})
	if err != nil {
		t.Fatalf("确认后应放行：%v", err)
	}
	if out.ApiKeyMasked == "" {
		t.Error("确认后应真的存下来")
	}
}
