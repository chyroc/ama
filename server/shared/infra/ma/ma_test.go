package ma

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/model"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/session"
)

// The tests below only cover the parts that need no network: argument validation, option
// handling, translation from convenience inputs to SDK request bodies, and error
// classification. For real API calls see example/main.go.

func TestNewRejectsEmptyAPIKey(t *testing.T) {
	if _, err := New(""); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("空 API Key 应返回 ErrMissingAPIKey，实际: %v", err)
	}
}

func TestNewWiresAllServices(t *testing.T) {
	ins, err := New("dummy-key")
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if ins.Agents() == nil || ins.Environments() == nil || ins.Sessions() == nil ||
		ins.Vaults() == nil || ins.MemoryStores() == nil || ins.Skills() == nil {
		t.Fatal("有子服务未初始化")
	}
	if ins.Client() == nil {
		t.Fatal("Client() 返回 nil")
	}
}

func TestNewWithClientRejectsNil(t *testing.T) {
	if _, err := NewWithClient(nil); err == nil {
		t.Fatal("nil client 应报错")
	}
}

// Unset options must not be passed to the SDK; otherwise a zero value is taken as the user's
// explicit intent and overrides the SDK's own defaults.
func TestConfigOptionsOnlyPassesSetValues(t *testing.T) {
	if got := len(newOptions().configOptions()); got != 0 {
		t.Fatalf("无选项时应下传 0 个，实际 %d", got)
	}

	full := newOptions(
		WithRegion("cn-beijing"),
		WithBaseURL("https://example.invalid"),
		WithTimeout(time.Second),
		WithRetryTimes(3),
		WithHTTPClient(&http.Client{}),
	)
	if got := len(full.configOptions()); got != 5 {
		t.Fatalf("五个选项应全部下传，实际 %d", got)
	}
}

// WithRetryTimes(0) means "explicitly no retry", which is different from "not set"; the two must
// be distinguishable.
func TestWithRetryTimesZeroIsExplicit(t *testing.T) {
	o := newOptions(WithRetryTimes(0))
	if !o.hasRetry {
		t.Fatal("WithRetryTimes(0) 应记为已设置")
	}
	if got := len(o.configOptions()); got != 1 {
		t.Fatalf("应下传 1 个选项，实际 %d", got)
	}
}

func TestAgentSpecToRequest(t *testing.T) {
	req := AgentSpec{
		Name:   "triage",
		Model:  "doubao-seed-2-1-pro-260628",
		Speed:  "fast",
		System: "be brief",
		Tools:  []string{ToolsetDefault, "custom_toolset"},
		// Skills must be set here and asserted.
		//
		// This case used to cover only 6 fields and **neither set Skills nor asserted
		// req.Skills**, so when the core layer failed to pass skills along, this package showed
		// no problem at all. The symptom was "the user configured skills, the model says there
		// are none", with zero errors. The wiring-side case lives in core/arkrun_test.go; this
		// one guards this package's own translation.
		Skills: []SkillRef{
			{SkillID: "skill-1", Version: "2"},
			{SkillID: "skill-2"},
		},
	}.toRequest()

	if req.Name != "triage" {
		t.Errorf("Name = %q", req.Name)
	}
	if req.Model.ID != "doubao-seed-2-1-pro-260628" {
		t.Errorf("Model.ID = %q", req.Model.ID)
	}
	if !req.Model.Speed.Set || string(req.Model.Speed.Value) != "fast" {
		t.Errorf("Speed 未正确设置: %+v", req.Model.Speed)
	}
	if !req.System.Set || req.System.Value != "be brief" {
		t.Errorf("System 未正确设置: %+v", req.System)
	}
	// When Description is not given it should stay "unset", not an explicit empty string.
	if req.Description.Set {
		t.Errorf("Description 未提供却被设置成 %+v", req.Description)
	}
	if len(req.Tools) != 2 || req.Tools[0].Type != ToolsetDefault {
		t.Errorf("Tools = %+v", req.Tools)
	}

	// Skills: count, id and type must all be right.
	//
	// type is asserted on its own because it is a **required** Ark field; missing it gives an
	// immediate 400 InvalidParameter (an English error that does not point here). Callers
	// usually only fill skill_id and rely on toSkillRefs to fill in custom.
	if len(req.Skills) != 2 {
		t.Fatalf("Skills 应有 2 条，实际 %d 条: %+v", len(req.Skills), req.Skills)
	}
	if id, _ := req.Skills[0].SkillID.Get(); id != "skill-1" {
		t.Errorf("Skills[0].SkillID = %q", id)
	}
	if v, _ := req.Skills[0].Version.Get(); v != "2" {
		t.Errorf("Skills[0].Version = %q", v)
	}
	if string(req.Skills[0].Type) != SkillTypeCustom {
		t.Errorf("Skills[0].Type = %q，期望 %s（漏了方舟会 400）",
			req.Skills[0].Type, SkillTypeCustom)
	}
	// The one without Version must stay "unset", not an explicit empty string: on the Ark side
	// an empty string does not mean "use the latest version".
	if req.Skills[1].Version.Set {
		t.Errorf("未提供 Version 却被设置成 %+v", req.Skills[1].Version)
	}
}

// TestAgentSpecOmitsSkillsWhenNone pins down "no skills means no skills key".
//
// On the Ark side "not sent" and "empty array sent" are two different things: the latter
// **clears** them. When creating an Agent both give the same result, but on update the
// difference is big (see the comment on toSkillRefs), so nobody should think it is fine to
// change it to return `[]AgentSkillRef{}`.
func TestAgentSpecOmitsSkillsWhenNone(t *testing.T) {
	req := AgentSpec{Name: "n", Model: "m"}.toRequest()
	if req.Skills != nil {
		t.Errorf("没有技能时 Skills 应当是 nil（SDK 据此不写 skills 键），实际 %+v", req.Skills)
	}
}

func TestAgentSpecOmitsEmptySpeed(t *testing.T) {
	req := AgentSpec{Name: "n", Model: "m"}.toRequest()
	if req.Model.Speed.Set {
		t.Errorf("未指定 Speed 时不应下传，实际 %+v", req.Model.Speed)
	}
}

func TestEnvironmentSpecDefaults(t *testing.T) {
	req := EnvironmentSpec{Name: "env"}.toRequest()
	if !req.Config.Set {
		t.Fatal("Config 应被设置")
	}
	cfg := req.Config.Value
	if string(cfg.Type) != EnvCloud {
		t.Errorf("默认环境类型应为 cloud，实际 %q", cfg.Type)
	}
	if !cfg.Networking.Set || string(cfg.Networking.Value.Type) != NetworkUnrestricted {
		t.Errorf("默认网络策略应为 unrestricted，实际 %+v", cfg.Networking)
	}
}

func TestEnvironmentSpecRespectsExplicitValues(t *testing.T) {
	req := EnvironmentSpec{
		Name:       "env",
		Type:       EnvCloud,
		Networking: NetworkLimited,
	}.toRequest()
	cfg := req.Config.Value
	if string(cfg.Type) != EnvCloud {
		t.Errorf("Type = %q", cfg.Type)
	}
	if string(cfg.Networking.Value.Type) != NetworkLimited {
		t.Errorf("Networking = %q", cfg.Networking.Value.Type)
	}
}

// self_hosted must not carry networking; Ark returns 400
// (config.networking: not allowed when config.type is "self_hosted").
// This only showed up when creating an environment against real Ark; the docs do not mention
// it.
func TestEnvironmentSpecSelfHostedOmitsNetworking(t *testing.T) {
	// Even when the caller explicitly gives Networking, it must not be sent, or the request is
	// sure to be rejected.
	req := EnvironmentSpec{
		Name:       "env",
		Type:       EnvSelfHosted,
		Networking: NetworkLimited,
	}.toRequest()
	cfg := req.Config.Value
	if string(cfg.Type) != EnvSelfHosted {
		t.Errorf("Type = %q", cfg.Type)
	}
	if cfg.Networking.Set {
		t.Errorf("self_hosted 不该带 networking，实际 %+v", cfg.Networking.Value)
	}
}

func TestSessionSpecToRequest(t *testing.T) {
	req := SessionSpec{
		AgentID:       "agent_x",
		EnvironmentID: "env_y",
		Title:         "t",
		VaultIDs:      []string{"vault_z"},
	}.toRequest()

	if !req.EnvironmentID.Set || req.EnvironmentID.Value != "env_y" {
		t.Errorf("EnvironmentID = %+v", req.EnvironmentID)
	}
	if !req.Title.Set || req.Title.Value != "t" {
		t.Errorf("Title = %+v", req.Title)
	}
	if len(req.VaultIds) != 1 || req.VaultIds[0] != "vault_z" {
		t.Errorf("VaultIds = %+v", req.VaultIds)
	}
}

// Required-field validation should be caught locally, without a wasted network round trip.
func TestRequiredFieldValidation(t *testing.T) {
	ins, err := New("dummy-key")
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"Agent 缺 Name", func() error {
			_, err := ins.Agents().Create(ctx, AgentSpec{Model: "m"})
			return err
		}},
		{"Agent 缺 Model", func() error {
			_, err := ins.Agents().Create(ctx, AgentSpec{Name: "n"})
			return err
		}},
		{"AgentUpdate 缺 Version", func() error {
			_, err := ins.Agents().Update(ctx, "agent_x", AgentUpdate{Name: "n"})
			return err
		}},
		{"Environment 缺 Name", func() error {
			_, err := ins.Environments().Create(ctx, EnvironmentSpec{})
			return err
		}},
		{"Session 缺 AgentID", func() error {
			_, err := ins.Sessions().Create(ctx, SessionSpec{EnvironmentID: "e"})
			return err
		}},
		{"Session 缺 EnvironmentID", func() error {
			_, err := ins.Sessions().Create(ctx, SessionSpec{AgentID: "a"})
			return err
		}},
		{"SendText 空文本", func() error {
			_, err := ins.Sessions().SendText(ctx, "sess_x", "")
			return err
		}},
		{"SendEvents 无事件", func() error {
			_, err := ins.Sessions().SendEvents(ctx, "sess_x")
			return err
		}},
		{"Vault 缺 displayName", func() error {
			_, err := ins.Vaults().Create(ctx, "")
			return err
		}},
		{"MemoryStore 缺 name", func() error {
			_, err := ins.MemoryStores().Create(ctx, "", "")
			return err
		}},
		{"CreateMemory 缺 path", func() error {
			_, err := ins.MemoryStores().CreateMemory(ctx, "ms_x", "", "c")
			return err
		}},
		{"Skill 缺 file", func() error {
			_, err := ins.Skills().Create(ctx, nil, "f.zip", "t")
			return err
		}},
		{"Run 空文本", func() error {
			_, err := ins.Sessions().Run(ctx, "sess_x", "")
			return err
		}},
	}

	for _, tc := range cases {
		if err := tc.call(); err == nil {
			t.Errorf("%s: 应返回校验错误，实际为 nil", tc.name)
		}
	}
}

func TestUserTextEventCarriesText(t *testing.T) {
	ev := UserTextEvent("hello")
	// OneOf is a union; get the concrete branch to confirm the text was not lost.
	um, ok := ev.OneOf.GetManagedAgentsUserMessageEventParams()
	if !ok {
		t.Fatal("事件不是 user message")
	}
	if len(um.Content) != 1 {
		t.Fatalf("Content 长度 = %d", len(um.Content))
	}
	tb, ok := um.Content[0].OneOf.GetManagedAgentsTextBlock()
	if !ok {
		t.Fatal("content block 不是 text block")
	}
	if tb.Text != "hello" {
		t.Errorf("Text = %q", tb.Text)
	}
}

func TestOptionalHelpersTreatEmptyAsUnset(t *testing.T) {
	if optAgentString("").Set {
		t.Error("空串应视为未设置")
	}
	if got := optAgentString("x"); !got.Set || got.Value != "x" {
		t.Errorf("非空串应设置，实际 %+v", got)
	}
	if optAgentInt32(0).Set {
		t.Error("0 应视为未设置（走服务端默认）")
	}
	if got := optAgentInt32(5); !got.Set || got.Value != 5 {
		t.Errorf("非零应设置，实际 %+v", got)
	}
}

func TestErrorHelpers(t *testing.T) {
	apiErr := &model.APIError{
		Code:           "InvalidParameter",
		HTTPStatusCode: http.StatusNotFound,
		RequestId:      "req-123",
	}

	got, ok := AsAPIError(apiErr)
	if !ok || got.Code != "InvalidParameter" {
		t.Fatalf("AsAPIError 失败: %+v ok=%t", got, ok)
	}
	if StatusCode(apiErr) != http.StatusNotFound {
		t.Errorf("StatusCode = %d", StatusCode(apiErr))
	}
	if RequestID(apiErr) != "req-123" {
		t.Errorf("RequestID = %q", RequestID(apiErr))
	}
	if !IsNotFound(apiErr) {
		t.Error("IsNotFound 应为 true")
	}
	if IsRateLimited(apiErr) || IsAuthError(apiErr) {
		t.Error("404 不该被判为限流或鉴权错误")
	}

	// Must still be recognized after wrapping (errors.As follows the Unwrap chain).
	wrapped := fmtErrorf("调用失败: %w", apiErr)
	if !IsNotFound(wrapped) {
		t.Error("包装后的错误应仍可识别为 404")
	}

	// Non-Ark errors should degrade safely, without panicking.
	plain := errors.New("boom")
	if _, ok := AsAPIError(plain); ok {
		t.Error("普通错误不该被识别为 APIError")
	}
	if StatusCode(plain) != 0 || RequestID(plain) != "" {
		t.Error("普通错误应返回零值")
	}
	if IsNotFound(nil) || IsAuthError(nil) {
		t.Error("nil 错误不该被判为任何具体错误")
	}
}

func TestIsAuthErrorAndRateLimited(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := &model.APIError{HTTPStatusCode: code}
		if !IsAuthError(err) {
			t.Errorf("%d 应判为鉴权错误", code)
		}
	}
	if !IsRateLimited(&model.APIError{HTTPStatusCode: http.StatusTooManyRequests}) {
		t.Error("429 应判为限流")
	}
}

// AgentUpdate.Tools is replaced as a whole, so translation must not drop items; unset fields
// must not be sent.
func TestAgentUpdateToRequest(t *testing.T) {
	req := AgentUpdate{
		Version: 2,
		Name:    "renamed",
		Model:   "new-model",
		Tools:   []string{"a", "b"},
	}.toRequest()

	if req.Version != 2 {
		t.Errorf("Version = %d", req.Version)
	}
	if !req.Name.Set || req.Name.Value != "renamed" {
		t.Errorf("Name = %+v", req.Name)
	}
	if !req.Model.Set || req.Model.Value.ID != "new-model" {
		t.Errorf("Model = %+v", req.Model)
	}
	if len(req.Tools) != 2 || req.Tools[0].Type != "a" || req.Tools[1].Type != "b" {
		t.Errorf("Tools = %+v", req.Tools)
	}
	// Unset fields stay unset, so the current values are not overwritten with empty ones.
	if req.System.Set || req.Description.Set {
		t.Errorf("未填字段被下传: System=%+v Description=%+v", req.System, req.Description)
	}
}

// Changing only the name must not bring along an empty Model that overwrites the existing model
// config.
func TestAgentUpdateOmitsEmptyModel(t *testing.T) {
	req := AgentUpdate{Version: 1, Name: "n"}.toRequest()
	if req.Model.Set {
		t.Errorf("未指定 Model 时不应下传，实际 %+v", req.Model)
	}
	if req.Tools != nil {
		t.Errorf("未指定 Tools 时应为 nil，实际 %+v", req.Tools)
	}
}

// fmtErrorf avoids pulling in fmt outside the test body just for one %w wrap.
func fmtErrorf(format string, args ...any) error {
	return wrapErr{msg: format, err: args[len(args)-1].(error)}
}

type wrapErr struct {
	msg string
	err error
}

func (w wrapErr) Error() string { return w.msg }
func (w wrapErr) Unwrap() error { return w.err }

func TestEventRawPayloadAndRawEventPayloads(t *testing.T) {
	// 1. Known type
	known := &session.ManagedAgentsSessionStatusRunningEvent{}
	if err := json.Unmarshal([]byte(`{"type":"session.status_running","id":"sevt-1"}`), known); err != nil {
		t.Fatalf("unmarshal 失败: %v", err)
	}
	knownRaw, err := EventRawPayload(known)
	if err != nil {
		t.Fatalf("EventRawPayload known 失败: %v", err)
	}
	if !strings.Contains(string(knownRaw), `"type":"session.status_running"`) {
		t.Errorf("已知类型序列化不对: %s", string(knownRaw))
	}

	// 2. Unknown type fallback (with RawPayload)
	orig := []byte(`{"id":"evt-123","type":"user.tool_result","content":[{"text":"ok"}]}`)
	unknown := &session.ManagedAgentsUnknownSessionEvent{
		RawPayload: orig,
	}
	unknownRaw, err := EventRawPayload(unknown)
	if err != nil {
		t.Fatalf("EventRawPayload unknown 失败: %v", err)
	}
	if string(unknownRaw) != string(orig) {
		t.Errorf("未知类型应保留 RawPayload: 实际 %s vs 期望 %s", string(unknownRaw), string(orig))
	}

	// 3. RawEventPayloads list extraction
	resp := &session.ListSessionEventsResponse{
		Events: []session.ManagedAgentsSessionEvent{known, unknown},
	}
	raws, err := RawEventPayloads(resp)
	if err != nil {
		t.Fatalf("RawEventPayloads 失败: %v", err)
	}
	if len(raws) != 2 {
		t.Fatalf("RawEventPayloads 长度应为 2，实际 %d", len(raws))
	}
}
