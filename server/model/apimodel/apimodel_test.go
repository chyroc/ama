// Byte-fidelity tests.
//
// The frontend depends not only on field types but also on field **presence** and **format**: optional fields must be missing when they should be,
// nullable fields must be null when they should be, empty arrays must not become null, and the three decimals of timestamps must not be lost.
// Getting these wrong is never a compile error; it only shows up on the page as odd, hard-to-debug problems ("unsaved" lit forever,
// TypeError, NaN, broken sorting).
//
// So here the real seed data is used as a golden file: decode, encode again, and compare with the original file **byte for byte**.
// One assertion pins four kinds of traps at once: omitempty, null vs missing, empty slices, timestamp format.
//
// The seed file is exported mechanically from the frontend's seed.ts by scripts/export-seed/export.sh,
// so it is exactly the shape the frontend really sees, not an approximation we wrote by hand.

package apimodel_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// seedShape matches the top-level structure of seed.json.
//
// Deliberately **without** feishuApps: the Feishu app model belongs to server/feishu (the device-code registration path),
// and this package does not rebuild it, so that part of the seed is not asserted here.
type seedShape struct {
	Workspaces []apimodel.Workspace           `json:"workspaces"`
	Agents     []apimodel.Agent               `json:"agents"`
	Skills     []apimodel.Skill               `json:"skills"`
	Runs       []apimodel.Run                 `json:"runs"`
	Events     map[string][]apimodel.RunEvent `json:"events"`
}

func seedPath(t *testing.T) string {
	t.Helper()
	// The apimodel package is in server/model/apimodel, and the seed is embedded in server/model/persistmodel
	// (it used to be in server/core and moved together with loadSeed; go:embed cannot cross parent directories,
	// so seed.json must be in the same directory as the package that references it).
	return filepath.Join("..", "persistmodel", "seed.json")
}

// TestSeedRoundTrip is the core assertion of this file: each entity is decoded and encoded again separately,
// and compared byte for byte with the matching raw bytes in the seed.
//
// It compares per entity instead of as a whole, because when a whole comparison fails you only see "there is a difference"
// and cannot locate the field; per entity it points straight to which key of which record changed shape.
func TestSeedRoundTrip(t *testing.T) {
	raw, err := os.ReadFile(seedPath(t))
	if err != nil {
		t.Fatalf("读种子失败：%v", err)
	}

	// First decode one level as RawMessage to get the raw bytes of each entity
	var rawTop map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawTop); err != nil {
		t.Fatalf("解析种子顶层失败：%v", err)
	}

	var seed seedShape
	if err := json.Unmarshal(raw, &seed); err != nil {
		t.Fatalf("解析种子失败：%v", err)
	}

	// Compare domain by domain: split the raw byte arrays and compare item by item with what we serialize.
	// feishuApps is not compared; see the comment on seedShape.
	checkList(t, "workspaces", rawTop["workspaces"], seed.Workspaces)
	checkList(t, "agents", rawTop["agents"], seed.Agents)
	checkList(t, "skills", rawTop["skills"], seed.Skills)
	checkList(t, "runs", rawTop["runs"], seed.Runs)
}

// checkList splits rawArray into the raw bytes of each item and compares them with the serialized items.
func checkList[T any](t *testing.T, name string, rawArray json.RawMessage, items []T) {
	t.Helper()
	var rawItems []json.RawMessage
	if err := json.Unmarshal(rawArray, &rawItems); err != nil {
		t.Fatalf("%s: 拆数组失败：%v", name, err)
	}
	if len(rawItems) != len(items) {
		t.Fatalf("%s: 条数不一致，原始 %d 解出 %d", name, len(rawItems), len(items))
	}
	for i, item := range items {
		got, err := marshalNoEscape(item)
		if err != nil {
			t.Fatalf("%s[%d]: 序列化失败：%v", name, i, err)
		}
		want := compact(t, rawItems[i])
		if !bytes.Equal(got, want) {
			t.Errorf("%s[%d] 不是字节保真：\n期望 %s\n实际 %s", name, i, want, got)
		}
	}
}

// marshalNoEscape serializes without HTML escaping: JS's JSON.stringify does not escape < > &,
// while the standard library does by default; without turning it off they would not match.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a newline
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// compact removes extra whitespace from the raw bytes, so the comparison is only about content.
// The seed comes from JSON.stringify and has no whitespace anyway; this step is just defensive.
func compact(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact 失败：%v", err)
	}
	return buf.Bytes()
}

// TestEventsPassthrough confirms events are passed through as is, and building the array introduces no escaping.
func TestEventsPassthrough(t *testing.T) {
	raw, err := os.ReadFile(seedPath(t))
	if err != nil {
		t.Fatalf("读种子失败：%v", err)
	}
	var top struct {
		Events map[string]json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("解析失败：%v", err)
	}

	total := 0
	for runID, rawEvents := range top.Events {
		var events []apimodel.RunEvent
		if err := json.Unmarshal(rawEvents, &events); err != nil {
			t.Fatalf("%s: 解析事件失败：%v", runID, err)
		}
		total += len(events)

		// The built array must equal the raw array byte for byte
		got := apimodel.AppendJSONArray(events)
		want := compact(t, rawEvents)
		if !bytes.Equal(got, want) {
			t.Errorf("%s 的事件数组不是字节保真\n期望 %s\n实际 %s", runID, want, got)
		}

		// The index fields must really be decoded, otherwise filtering and sorting silently stop working
		for i, e := range events {
			if e.Type == "" {
				t.Errorf("%s[%d] 没解出 type", runID, i)
			}
			if e.Sequence == 0 {
				t.Errorf("%s[%d] 没解出 sequence", runID, i)
			}
		}
	}
	// The total number of events **follows seed.ts**; when the seed changes, this number must change too.
	if total != 105 {
		t.Errorf("事件总数应为 105，实际 %d", total)
	}
}

// TestAppendJSONArrayEmpty: an empty event list must build as [], not null:
// listRunEvents returns an array, and the frontend calls .map() directly.
func TestAppendJSONArrayEmpty(t *testing.T) {
	if got := string(apimodel.AppendJSONArray(nil)); got != "[]" {
		t.Errorf("nil 应拼成 []，实际 %s", got)
	}
	if got := string(apimodel.AppendJSONArray([]apimodel.RunEvent{})); got != "[]" {
		t.Errorf("空切片应拼成 []，实际 %s", got)
	}
}

// TestNoHTMLEscape guards the "pass through as is" line: an event containing < > & must not change shape after building.
// The current seed has none of these three characters, but skill bodies are user-editable Markdown and will have them sooner or later.
func TestNoHTMLEscape(t *testing.T) {
	src := `{"id":"sevt-x","type":"agent.message","sequence":1,"session_thread_id":null,"content":[{"type":"text","text":"if a < b && c > d"}]}`
	var e apimodel.RunEvent
	if err := json.Unmarshal([]byte(src), &e); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	got := string(apimodel.AppendJSONArray([]apimodel.RunEvent{e}))
	want := "[" + src + "]"
	if got != want {
		t.Errorf("HTML 字符被转义了：\n期望 %s\n实际 %s", want, got)
	}
}

// TestNowISO: the timestamp format must match JS's toISOString(): exactly three decimals, ending in Z.
// A mismatch would also break listRuns' string sorting ('.' < 'Z').
func TestNowISO(t *testing.T) {
	got := apimodel.NowISO()
	// Like 2026-08-25T10:00:00.000Z, 24 characters in total
	if len(got) != 24 {
		t.Errorf("长度应为 24，实际 %d（%s）", len(got), got)
	}
	if got[len(got)-1] != 'Z' {
		t.Errorf("应以 Z 结尾：%s", got)
	}
	if got[19] != '.' {
		t.Errorf("第 20 位应是小数点：%s", got)
	}
	// It must parse back, and formatting it again keeps the same shape
	var parsed struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal([]byte(`{"t":"`+got+`"}`), &parsed); err != nil {
		t.Fatalf("时间戳不是合法 JSON 字符串：%v", err)
	}
}

// ---------------------------------------------------------------------------
// The default level of ProgressDetail
// ---------------------------------------------------------------------------

// **An empty string must normalize to "show"**; that is the whole reason this field is a string enum and not a bool.
//
// Existing agents' jsonb doc has no such key, so it decodes as an empty string; old frontends do not send
// this key on PATCH either. Both cases must land on "show": landing on "hide" would make the thinking chain of every existing agent
// silently disappear overnight, with no error at all.
//
// A bool would surely hit this trap: Go's bool zero value is false, while the default is "show", exactly the opposite.
func TestNormalizeProgressDetailDefaultsToFull(t *testing.T) {
	for _, in := range []apimodel.ProgressDetail{"", "   ", "full", "unknown", "BRIEF"} {
		if got := apimodel.NormalizeProgressDetail(in); got != apimodel.ProgressDetailFull {
			t.Errorf("%q 该归一成 full（默认展示），得到 %q", in, got)
		}
	}
	// Only an explicit brief means "hide".
	if got := apimodel.NormalizeProgressDetail(apimodel.ProgressDetailBrief); got != apimodel.ProgressDetailBrief {
		t.Errorf("brief 该保持 brief，得到 %q", got)
	}
	if got := apimodel.NormalizeProgressDetail(" brief "); got != apimodel.ProgressDetailBrief {
		t.Errorf("带空白的 brief 也该认出来，得到 %q", got)
	}
}

// Existing agents' doc has no progressDetail key: it must decode as "show".
//
// This pins **the real shape of existing data** (a JSON without that key), not a zero value written by hand in Go:
// the latter cannot prove "the rows in the database read back correctly".
func TestLegacyAgentJSONDefaultsToFullProgress(t *testing.T) {
	var a apimodel.Agent
	if err := json.Unmarshal([]byte(
		`{"id":"agent_x","name":"存量 agent","followUp":""}`), &a); err != nil {
		t.Fatalf("存量 agent 该能解出来: %v", err)
	}
	if a.ProgressDetail != "" {
		t.Fatalf("没有这个键时该解成空串，得到 %q", a.ProgressDetail)
	}
	if apimodel.NormalizeProgressDetail(a.ProgressDetail) != apimodel.ProgressDetailFull {
		t.Error("存量 agent 必须仍然展示思维链，否则全站的思维链会静默消失")
	}
}

// Old frontends do not send this key on PATCH, so it is an empty string in the draft too: the user's setting must not be silently turned off.
func TestLegacyAgentDraftDefaultsToFullProgress(t *testing.T) {
	var d apimodel.AgentDraft
	if err := json.Unmarshal([]byte(`{"name":"老前端存的"}`), &d); err != nil {
		t.Fatalf("老草稿该能解出来: %v", err)
	}
	if apimodel.NormalizeProgressDetail(d.ProgressDetail) != apimodel.ProgressDetailFull {
		t.Error("老前端不发这个键时该按「展示」处理，否则每存一次就把思维链关掉一次")
	}
}
