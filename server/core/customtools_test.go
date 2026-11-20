// Tests for the worker-side custom tool wiring on the core side.
//
// Three things can silently break here, and each one fails differently:
//
//   - the declarations do not reach arkSpecFor → the model never sees the tool (zero errors);
//   - the declarations are left out of the fingerprint → an Agent created before the datasource
//     was enabled never catches up (zero errors, permanent);
//   - the per-work implementations are not passed to the runner → Ark sends
//     agent.custom_tool_use, nothing matches the name, and the work hangs on requires_action.
//
// So these tests pin "what the spec / the fingerprint / the runner map contain", not "a function
// was called".

package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// fakeCustomTool is a minimal toolset.Tool.
type fakeCustomTool struct{ name string }

func (t fakeCustomTool) Name() string { return t.name }

func (t fakeCustomTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	return toolset.TextResult(t.name + ":" + string(input))
}

// fakeCustomTools records what core asked it for.
type fakeCustomTools struct {
	decls      []ma.CustomToolSpec
	tools      map[string]toolset.Tool
	declCalls  int
	gotSession string
	gotWork    string
	gotLogger  *slog.Logger
}

func (f *fakeCustomTools) Declarations() []ma.CustomToolSpec {
	f.declCalls++
	return f.decls
}

func (f *fakeCustomTools) ToolsFor(sessionID, workID string, logger *slog.Logger) map[string]toolset.Tool {
	f.gotSession, f.gotWork, f.gotLogger = sessionID, workID, logger
	return f.tools
}

// testCustomToolDecls is what the real datasources.Registry declares (name + schema shape matter,
// not the wording).
func testCustomToolDecls() []ma.CustomToolSpec {
	return []ma.CustomToolSpec{
		{
			Name:        "query_sql",
			Description: "run a read-only SQL",
			InputSchema: map[string]any{
				"sql":      map[string]any{"type": "string", "description": "one statement"},
				"max_rows": map[string]any{"type": "integer", "description": "row cap"},
			},
			Required: []string{"sql"},
		},
		{
			Name:        "read_object",
			Description: "read one text object",
			InputSchema: map[string]any{
				"key":       map[string]any{"type": "string"},
				"max_bytes": map[string]any{"type": "integer"},
			},
			Required: []string{"key"},
		},
	}
}

// TestCustomToolHelpersNilSafe: with no datasource configured the helpers must return nil, so the
// runner gets CustomTools == nil and behaves exactly like before the feature existed.
func TestCustomToolHelpersNilSafe(t *testing.T) {
	svc, _ := newService(t)
	if got := svc.customToolDecls(); got != nil {
		t.Errorf("未配置数据源时 Declarations 应当为 nil，实际 %v", got)
	}
	if got := svc.customToolsForWork("sess", "work", nil); got != nil {
		t.Errorf("未配置数据源时 ToolsFor 应当为 nil，实际 %v", got)
	}
	// A zero-value Service (some tests build one directly) must be safe too.
	if got := (&Service{}).customToolDecls(); got != nil {
		t.Errorf("零值 Service 应当安全返回 nil，实际 %v", got)
	}
}

// TestArkSpecCarriesCustomTools: the declarations reach the Ark Agent spec, and nil stays nil
// (nil means "do not touch tools" on update; an empty slice would clear them).
func TestArkSpecCarriesCustomTools(t *testing.T) {
	agent := apimodel.Agent{
		ID:     "agt_1",
		Name:   "数据分析",
		System: "你是助手",
		Model:  apimodel.ModelConfig{ID: "doubao-pro"},
	}

	spec := arkSpecFor(agent, testCustomToolDecls())
	if len(spec.CustomTools) != 2 {
		t.Fatalf("spec 里应当有 2 个自定义工具声明，实际 %d 个", len(spec.CustomTools))
	}
	if spec.CustomTools[0].Name != "query_sql" || spec.CustomTools[1].Name != "read_object" {
		t.Errorf("声明名不对：%+v", spec.CustomTools)
	}
	if spec.CustomTools[0].Description == "" || len(spec.CustomTools[0].InputSchema) == 0 {
		t.Error("声明必须带 Description 与 InputSchema（否则模型不知道该不该调、怎么调）")
	}

	plain := arkSpecFor(agent, nil)
	if plain.CustomTools != nil {
		t.Errorf("没有自定义工具时 spec.CustomTools 必须是 nil，实际 %v", plain.CustomTools)
	}
	// Everything else must be identical: enabling a datasource may only add the tools.
	if plain.System != spec.System || plain.Model != spec.Model || plain.Name != spec.Name {
		t.Error("加不加自定义工具不该影响 spec 的其它字段")
	}
}

// TestArkSpecFingerprintTracksCustomTools: a change in any part of the declarations must change
// the fingerprint, otherwise an existing Ark Agent never gets the new tool declaration.
func TestArkSpecFingerprintTracksCustomTools(t *testing.T) {
	agent := apimodel.Agent{ID: "agt_1", Name: "a", System: "s", Model: apimodel.ModelConfig{ID: "m"}}

	base := testCustomToolDecls()
	baseFP := ArkSpecFingerprint(arkSpecFor(agent, base))
	if again := ArkSpecFingerprint(arkSpecFor(agent, testCustomToolDecls())); again != baseFP {
		t.Fatalf("同样的声明两次算出不同指纹（map 遍历顺序不稳定？）%q vs %q", baseFP, again)
	}
	if noTools := ArkSpecFingerprint(arkSpecFor(agent, nil)); noTools == baseFP {
		t.Fatal("加了工具声明却指纹不变——已有 Agent 永远不会追平，且零报错")
	}

	t.Run("name change", func(t *testing.T) {
		decls := testCustomToolDecls()
		decls[0].Name = "query_sql_v2"
		if got := ArkSpecFingerprint(arkSpecFor(agent, decls)); got == baseFP {
			t.Error("工具名变了，指纹必须变")
		}
	})
	t.Run("description change", func(t *testing.T) {
		decls := testCustomToolDecls()
		decls[0].Description += "（只读）"
		if got := ArkSpecFingerprint(arkSpecFor(agent, decls)); got == baseFP {
			t.Error("工具说明变了，指纹必须变")
		}
	})
	t.Run("input schema change", func(t *testing.T) {
		decls := testCustomToolDecls()
		decls[0].InputSchema["sql"] = map[string]any{"type": "string", "description": "changed"}
		if got := ArkSpecFingerprint(arkSpecFor(agent, decls)); got == baseFP {
			t.Error("入参 schema 变了，指纹必须变")
		}
	})
	t.Run("required change", func(t *testing.T) {
		decls := testCustomToolDecls()
		decls[0].Required = []string{"sql", "max_rows"}
		if got := ArkSpecFingerprint(arkSpecFor(agent, decls)); got == baseFP {
			t.Error("Required 变了，指纹必须变")
		}
	})
	t.Run("added tool", func(t *testing.T) {
		decls := append(testCustomToolDecls(), ma.CustomToolSpec{
			Name:        "third",
			Description: "third tool",
			InputSchema: map[string]any{"x": map[string]any{"type": "string"}},
		})
		if got := ArkSpecFingerprint(arkSpecFor(agent, decls)); got == baseFP {
			t.Error("多了一个工具，指纹必须变")
		}
	})
}

// TestServiceWiresCustomToolProvider: Config.CustomTools → Service → both helpers.
func TestServiceWiresCustomToolProvider(t *testing.T) {
	_, store := newService(t)
	provider := &fakeCustomTools{
		decls: testCustomToolDecls(),
		tools: map[string]toolset.Tool{
			"query_sql":   fakeCustomTool{name: "query_sql"},
			"read_object": fakeCustomTool{name: "read_object"},
		},
	}
	svc, err := New(Config{Store: store, CustomTools: provider})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}

	decls := svc.customToolDecls()
	if len(decls) != 2 || provider.declCalls != 1 {
		t.Fatalf("声明应当来自 provider：decls=%d calls=%d", len(decls), provider.declCalls)
	}

	tools := svc.customToolsForWork("sess-7", "work-8", nil)
	if len(tools) != 2 {
		t.Fatalf("实现应当有 2 个，实际 %d 个", len(tools))
	}
	if provider.gotSession != "sess-7" || provider.gotWork != "work-8" {
		t.Errorf("session/work id 没传下去：session=%q work=%q", provider.gotSession, provider.gotWork)
	}
	// A nil logger must be replaced by the Service logger, never passed as nil (the tool's audit
	// path would otherwise fall back to slog.Default and lose the request fields).
	if provider.gotLogger == nil {
		t.Error("传 nil logger 时应当补上 Service 的 logger")
	}
	for _, name := range []string{"query_sql", "read_object"} {
		if _, ok := tools[name]; !ok {
			t.Errorf("缺少工具 %q 的实现（声明了却没有实现 = work 卡在 requires_action）", name)
		}
	}
}

// TestCustomToolsDeclarationsMatchImplementations is the core-side half of the anti-drift check:
// what goes into the Ark Agent spec and what goes into the runner must have the same names.
func TestCustomToolsDeclarationsMatchImplementations(t *testing.T) {
	_, store := newService(t)
	provider := &fakeCustomTools{
		decls: testCustomToolDecls(),
		tools: map[string]toolset.Tool{
			"query_sql":   fakeCustomTool{name: "query_sql"},
			"read_object": fakeCustomTool{name: "read_object"},
		},
	}
	svc, err := New(Config{Store: store, CustomTools: provider})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	declared := map[string]bool{}
	for _, d := range svc.customToolDecls() {
		declared[d.Name] = true
	}
	for name, tool := range svc.customToolsForWork("s", "w", nil) {
		if !declared[name] {
			t.Errorf("实现了 %q 但没声明（模型永远不会调，零报错）", name)
		}
		if tool.Name() != name {
			t.Errorf("map key %q 与 Name() %q 不一致", name, tool.Name())
		}
	}
	for name := range declared {
		if _, ok := provider.tools[name]; !ok {
			t.Errorf("声明了 %q 但注册表里没有实现", name)
		}
	}
}
