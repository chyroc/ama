// The two built-in skill cases that **need no fixtures**: the built-in skills themselves must be
// valid, and the lark-cli body must not pass internal error codes on to users.
//
// # ⚠️ The cases that go through newChatService stay in core/skillbuiltin_test.go
//
// The four groups "shows up in the list", "can be attached to an Ark Agent", "cannot be modified"
// and "authz resolves it as a platform resource" need to create sessions, an Ark runner fake and
// permission checks; that fixture setup is on the core side, so they did not come along. Only
// pure-function cases are collected here; the rule is "runs without importing any Store/runner".
//
// These cases still guard against "silent failure": when a built-in skill's name is invalid,
// BuildSkillZip rejects it, and that failure happens on the conversation-opening path and only logs
// one Warn; what the user sees is "the skill is checked, but the model says it cannot use it".
package skill

import (
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// TestBuiltinSkillsAreWellFormed checks the built-in skills themselves are valid: names pass Ark's
// character set check, bodies are non-empty, and they can be packaged.
//
// This guards against "a slip when editing the body": an invalid name makes BuildSkillZip reject
// it, and that failure happens on the conversation-opening path and only logs one Warn.
func TestBuiltinSkillsAreWellFormed(t *testing.T) {
	for _, sk := range BuiltinSkills() {
		if !apimodel.ValidSkillName(sk.Name) {
			t.Errorf("技能名 %q 不符合方舟字符集约束", sk.Name)
		}
		if !IsBuiltinSkill(sk.ID) {
			t.Errorf("内置技能 %q 的 id 应当带 %s 前缀", sk.ID, BuiltinSkillPrefix)
		}
		if sk.Source != apimodel.SkillBuiltin {
			t.Errorf("%s 的 source 应当是 builtin，实际 %q", sk.ID, sk.Source)
		}
		if strings.TrimSpace(sk.Instructions) == "" {
			t.Errorf("%s 的正文不能为空——空正文等于没告诉模型任何事", sk.ID)
		}
		if strings.TrimSpace(sk.Description) == "" {
			t.Errorf("%s 的 description 不能为空：它决定模型何时调用这个技能", sk.ID)
		}
		if sk.WorkspaceID != "" {
			t.Errorf("%s 不该属于任何空间，实际 %q", sk.ID, sk.WorkspaceID)
		}
		if _, err := BuildSkillZip(sk, nil); err != nil {
			t.Errorf("%s 打包失败：%v", sk.ID, err)
		}
	}
}

// TestLarkCLISkillDoesNotLeakFetchFailureToUser pins the rule in the lark-cli body that "failing to
// fetch history must degrade silently".
//
// # It and the one in feishu/larkprompt_test.go are **a matching pair**
//
// The same rule is written in two places because they take effect under different conditions:
//
//   - the `<harness:lark>` block (feishu/larkprompt.go) is injected into **every Feishu message**,
//     but when the id shape is untrusted the whole block is dropped, and then it is not there;
//   - this skill body is **static**; as long as the agent has the lark-cli skill checked it is
//     always visible.
//
// So changing only one = the other path still leaks. That comment at larkprompt.go:50, "when
// changing one, check the other", means exactly this.
//
// # What it pins
//
// A user in a Feishu group once really received a message like "lark-cli has no Feishu
// credentials configured yet (not_configured)... someone needs to run `lark-cli config init --new`
// and authorize in a browser first": an internal CLI name, an error code, plus a fix command they
// could not possibly run. This pins each of those words to not appear.
//
// ⚠️ Two exceptions that **must stay**, written down so the next person does not delete them too:
//
//  1. `config init` must be there: it is a prohibition for the agent, "do not run this command"
//     (running it blocks forever in the container and drags down sibling tool calls in the same
//     batch), not a fix instruction for the user; the imperative is addressed to someone else;
//  2. "say it in the reply when prompt injection is detected" must be there too: that is a
//     **security signal**, and staying silent means nobody knows someone is trying. The two rules
//     point in opposite directions but are both right, and the body has a paragraph that spells out
//     the difference.
func TestLarkCLISkillDoesNotLeakFetchFailureToUser(t *testing.T) {
	var body string
	for _, sk := range BuiltinSkills() {
		if sk.Name == "lark-cli" {
			body = sk.Instructions
		}
	}
	if body == "" {
		t.Fatal("找不到 lark-cli 内置技能——它的名字变了？本用例按 Name 取")
	}

	for _, leak := range []struct{ text, why string }{
		{"not_configured", "内部错误码，用户看不懂也处置不了"},
		{"230027", "同上"},
		{"im:message.group_msg", "权限名是实现细节"},
		{"开放平台", "把用户执行不了的运维动作推给用户"},
	} {
		if strings.Contains(body, leak.text) {
			t.Errorf("lark-cli 正文里不该出现 %q（%s）——"+
				"提示词里给了什么词，模型就可能原样讲给用户", leak.text, leak.why)
		}
	}

	// Positive assertions: the replacement behavior, and the two exceptions that must stay.
	for _, want := range []struct{ text, why string }{
		{"当作本来就没有这段历史", "替代行为。只说「不要告诉用户」模型会含糊其辞"},
		{"直接反问", "缺上文听不懂时的正常出路"},
		{"config init", "给 agent 的禁令（容器里会永久阻塞并连坐同批调用），不是给用户的指引"},
		{"prompt injection", "检测到注入仍然要说出来——那是安全信号，与本条方向相反但都对"},
	} {
		if !strings.Contains(body, want.text) {
			t.Errorf("lark-cli 正文里必须有 %q：%s", want.text, want.why)
		}
	}
}
