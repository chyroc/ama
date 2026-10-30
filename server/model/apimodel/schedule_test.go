// JSON shape tests for the scheduled task model.
//
// Same concern as apimodel_test.go: the frontend depends on **whether fields exist**, and a wrong tag is not a compile error;
// it only shows up on the page as odd, hard-to-debug problems. Schedule has two kinds of things that must be pinned:
//
//   - The three counters (runCount / failCount / consecutiveFailures) **must not have omitempty**,
//     otherwise 0 drops the key, and the frontend adds undefined and gets NaN;
//   - The three payload fields in spec and the optional fields of the delivery address **must have omitempty**,
//     otherwise a cron task emits everyMs:0 / fireAt:"", and the frontend gets the type wrong when it decides
//     by "is this key present".
//
// These two go in opposite directions, which is exactly where they are easiest to get backwards, so there is one case each.

package apimodel_test

import (
	"encoding/json"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// TestScheduleCountsSurviveZero: the keys of the three counters must still be present when they are 0.
//
// The list page shows "ran N times / failed M times"; a missing key renders as undefined or computes NaN.
// This is the same hard rule as the four TokenUsage fields (see hard rule two at the top of apimodel.go).
func TestScheduleCountsSurviveZero(t *testing.T) {
	raw, err := json.Marshal(apimodel.Schedule{})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	for _, key := range []string{"runCount", "failCount", "consecutiveFailures"} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s 在零值时消失了——前端拿它做加法会算出 NaN，这个字段不能带 omitempty", key)
		}
	}
}

// TestScheduleOptionalFieldsOmitted: a cron task must not emit the payload fields of interval / oneshot.
//
// Put the other way: if the frontend renders the form by "does spec have everyMs" and the like,
// an always-present everyMs:0 would make a cron task be treated as interval.
func TestScheduleOptionalFieldsOmitted(t *testing.T) {
	s := apimodel.Schedule{
		ID:     "sch_1",
		Name:   "工作日巡检",
		Prompt: "汇总昨天的告警",
		Spec: apimodel.ScheduleSpec{
			Kind: apimodel.ScheduleCron,
			Cron: "30 9 * * 1-5",
			TZ:   apimodel.DefaultScheduleTZ,
		},
		Delivery: apimodel.DeliveryTarget{Kind: apimodel.DeliveryRunOnly},
		Status:   apimodel.ScheduleActive,
		Origin:   apimodel.OriginUser,
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}

	var got struct {
		Spec     map[string]json.RawMessage `json:"spec"`
		Delivery map[string]json.RawMessage `json:"delivery"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	for _, key := range []string{"everyMs", "fireAt"} {
		if _, ok := got.Spec[key]; ok {
			t.Errorf("cron 任务的 spec 里不该出现 %s——前端按键的有无判断类型，"+
				"恒存在的零值会让它认错", key)
		}
	}
	// The delivery address of run_only must not carry any Feishu fields.
	for _, key := range []string{"feishuAppId", "chatId", "threadId", "chatName"} {
		if _, ok := got.Delivery[key]; ok {
			t.Errorf("run_only 的 delivery 里不该出现 %s", key)
		}
	}
	// But kind is required and must always be present.
	if _, ok := got.Delivery["kind"]; !ok {
		t.Error("delivery.kind 是必填字段，不能消失")
	}
}

// TestScheduleRoundTrip: each of the three trigger kinds does a serialization round trip; no field may be lost.
func TestScheduleRoundTrip(t *testing.T) {
	for _, spec := range []apimodel.ScheduleSpec{
		{Kind: apimodel.ScheduleCron, Cron: "30 9 * * 1-5", TZ: "Asia/Shanghai"},
		{Kind: apimodel.ScheduleInterval, EveryMs: 300_000},
		{Kind: apimodel.ScheduleOneshot, FireAt: "2026-08-27T10:00:00.000Z"},
	} {
		t.Run(string(spec.Kind), func(t *testing.T) {
			raw, err := json.Marshal(spec)
			if err != nil {
				t.Fatalf("序列化失败：%v", err)
			}
			var back apimodel.ScheduleSpec
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("反序列化失败：%v", err)
			}
			if back != spec {
				t.Errorf("往返后不一致：%+v != %+v", back, spec)
			}
		})
	}
}

// TestRunTriggerValues: the literal values of the six trigger sources must match the frontend union type exactly.
//
// RunTrigger in web/src/types/domain.ts on the frontend is a discriminated union, and RUN_TRIGGER_LABELS enumerates it
// exhaustively: change one letter here, and over there the label cannot be found and shows as blank, while the Go side reports no error.
func TestRunTriggerValues(t *testing.T) {
	want := map[apimodel.RunTrigger]string{
		apimodel.TriggerManual:   "manual",
		apimodel.TriggerFeishu:   "feishu",
		apimodel.TriggerAPI:      "api",
		apimodel.TriggerSchedule: "schedule",
		apimodel.TriggerWebhook:  "webhook",
	}
	for got, expect := range want {
		if string(got) != expect {
			t.Errorf("触发来源字面值变了：%q，期望 %q（前端 RunTrigger 联合要逐字一致）", got, expect)
		}
	}
}
