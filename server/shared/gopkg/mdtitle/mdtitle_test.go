// Edge cases for automatic title extraction.
//
// # Why this group is written in such detail
//
// Each extraction rule maps to a result that "causes no error but looks bad": the title becomes
// `![](https://...)`, the language tag `go`, half a Chinese character, or just a blank line.
// All of these store fine and return 200; only a person looking at the list notices something is off.
//
// So the assertions are not "the function ran" but **what each kind of malformed input turns into**.

package mdtitle

import (
	"strings"
	"testing"
)

// TestDeriveIssueTitleRejectsBothEmpty locks in "title and body both empty -> reject".
//
// Returning an empty string is a signal to the caller (validateIssueDraft turns it into
// ErrInvalidArgument). **This deliberately does not fall back to "untitled issue"**: both empty is a
// record with nothing in it, which is a request that should be rejected; a fallback would silently
// store an empty record, and the list would gain a row with nothing inside.
func TestDeriveIssueTitleRejectsBothEmpty(t *testing.T) {
	for _, tc := range []struct{ name, title, body string }{
		{"两者都是空串", "", ""},
		{"两者都是空白", "   ", "\n\t \n"},
		{"标题空白正文空串", "  ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Derive(tc.title, tc.body); got != "" {
				t.Errorf("期望返回空串（由调用方拒绝），实际 %q", got)
			}
		})
	}
}

// TestDeriveIssueTitleAcceptsEitherOne locks in "filling in just one is enough".
//
// This is the core of the requirement: title is optional, body is optional, but at least one is needed.
func TestDeriveIssueTitleAcceptsEitherOne(t *testing.T) {
	t.Run("只填标题", func(t *testing.T) {
		if got := Derive("登录后白屏", ""); got != "登录后白屏" {
			t.Errorf("期望原样用标题，实际 %q", got)
		}
	})
	t.Run("只填正文", func(t *testing.T) {
		got := Derive("", "点了登录按钮之后整页白掉")
		if got != "点了登录按钮之后整页白掉" {
			t.Errorf("期望从正文提取，实际 %q", got)
		}
	})
}

// TestDeriveIssueTitlePrefersExplicitTitle locks in "if a title is given, the body is not looked at".
//
// The other way around (a # heading in the body overrides what the user typed) would leave the user
// unable to change the auto-extracted result, without knowing the body is the cause.
func TestDeriveIssueTitlePrefersExplicitTitle(t *testing.T) {
	got := Derive("我自己起的名字", "# 正文里的大标题\n\n内容")
	if got != "我自己起的名字" {
		t.Errorf("期望用显式标题，实际 %q", got)
	}
}

// TestExtractTitleStripsMarkdown is the main table of extraction rules.
//
// Each row is a body shape seen in real use. **The most important ones are those with `!`**:
// posting just an image is the most common usage, and without stripping, the title would be a string
// of `![](https://...)`.
func TestExtractTitleStripsMarkdown(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		// -- block prefixes --
		{"ATX 标题", "# 登录后白屏", "登录后白屏"},
		{"多级标题", "### 三级标题", "三级标题"},
		{"引用", "> 引用的一句话", "引用的一句话"},
		{"嵌套引用", ">> 套了两层", "套了两层"},
		{"无序列表", "- 第一条", "第一条"},
		{"星号列表", "* 第一条", "第一条"},
		{"有序列表", "1. 第一步", "第一步"},
		{"右括号有序列表", "2) 第二步", "第二步"},
		{"引用套列表", "> - 引用里的列表", "引用里的列表"},

		// -- inline emphasis --
		{"粗体", "**很严重**的问题", "很严重的问题"},
		{"下划线粗体", "__很严重__的问题", "很严重的问题"},
		{"斜体", "*轻微*的问题", "轻微的问题"},
		{"行内代码", "`npm run build` 会失败", "npm run build 会失败"},
		{"删除线", "~~已修复~~ 又复现了", "已修复 又复现了"},

		// -- images and links: this group is the focus --
		{"图片取 alt", "![登录页截图](https://x/a.png)", "登录页截图"},
		{"链接取文字", "[看这个 issue](https://x/1)", "看这个 issue"},
		{"裸尖括号链接", "<https://example.com/x>", "https://example.com/x"},
		{"文字后跟图片", "复现步骤见 ![图](a.png)", "复现步骤见 图"},

		// -- skip lines with no content --
		{"跳过前导空行", "\n\n\n正文第一行", "正文第一行"},
		{"跳过纯空白行", "   \n\t\n真正的内容", "真正的内容"},
		{"跳过围栏行", "```go\npackage main\n```", "package main"},
		{"跳过波浪围栏", "~~~\ncode here\n~~~", "code here"},
		{"跳过分隔线", "---\n分隔线下面的话", "分隔线下面的话"},
		{"跳过星号分隔线", "***\n后面的话", "后面的话"},
		{"跳过表格分隔行", "|---|---|\n实际内容", "实际内容"},

		// -- an image without alt moves on to the next line --
		{
			"无 alt 图片顺延",
			"![](https://x/a.png)\n\n然后就崩了",
			"然后就崩了",
		},
		{
			"多张无 alt 图片顺延",
			"![](a.png)\n![](b.png)\n最后一句话",
			"最后一句话",
		},

		// -- whitespace squashing --
		{"压缩连续空格", "有   很多    空格", "有 很多 空格"},
		{"去掉首尾空白", "   两边有空格   ", "两边有空格"},
		{"制表符也压", "制表\t\t符", "制表 符"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Derive("", tc.body)
			if got != tc.want {
				t.Errorf("正文 %q\n期望标题 %q\n实际标题 %q", tc.body, tc.want, got)
			}
		})
	}
}

// TestExtractTitleFallsBackToUntitled locks in "the fallback when nothing is left after stripping".
//
// **It must not return an empty string.** An empty title is an invisible row in the list; the user
// thinks the data is broken, or that the submission failed, and submits again.
func TestExtractTitleFallsBackToUntitled(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"只有一张无 alt 的图", "![](https://x/a.png)"},
		{"只有分隔线", "---"},
		{"只有围栏", "```\n```"},
		{"只有标点", "!!!???"},
		{"只有表格框线", "|---|---|---|"},
		{"图片加空行", "\n\n![](a.png)\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Derive("", tc.body)
			if got != untitledTitle {
				t.Errorf("正文 %q 期望兜底成 %q，实际 %q", tc.body, untitledTitle, got)
			}
			if got == "" {
				t.Error("兜底标题不能是空串——列表里会是一行看不见的东西")
			}
		})
	}
}

// TestTitleTruncatesByRunes locks in "truncate by rune, not byte".
//
// Cutting by byte splits a Chinese character in half (one Chinese character is 3 bytes in UTF-8), and
// the replacement character U+FFFD gets stored. **In other tests that damage just looks like "the
// title is a bit short"**, so this test checks two things explicitly: the rune count is right, and the
// result is still valid UTF-8 (no half characters).
func TestTitleTruncatesByRunes(t *testing.T) {
	long := strings.Repeat("中", 200)
	got := Derive("", long)

	runes := []rune(got)
	if len(runes) != maxTitleRunes {
		t.Errorf("期望截到 %d 个 rune，实际 %d 个", maxTitleRunes, len(runes))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断后应补省略号，实际 %q", got)
	}
	// Half a Chinese character would become U+FFFD.
	if strings.ContainsRune(got, '�') {
		t.Error("结果里有替换字符 U+FFFD——说明是按 byte 截的，把汉字切断了")
	}
	// The first 79 should be kept as-is.
	if want := strings.Repeat("中", maxTitleRunes-1) + "…"; got != want {
		t.Errorf("截断结果不对\n期望 %q\n实际 %q", want, got)
	}
}

// TestExplicitTitleAlsoTruncates locks in "an over-long title the user typed is truncated too".
//
// Truncating only the auto-extracted one would let "pasting a whole paragraph into the title box"
// get around the limit, and that is exactly what happens most often.
func TestExplicitTitleAlsoTruncates(t *testing.T) {
	got := Derive(strings.Repeat("a", 300), "")
	if len([]rune(got)) != maxTitleRunes {
		t.Errorf("显式标题也要截到 %d 个 rune，实际 %d 个",
			maxTitleRunes, len([]rune(got)))
	}
}

// TestTitleIsAlwaysSingleLine locks in "a title is always one line".
//
// Newlines from the body in a title would suddenly double the list row height, which looks like broken CSS.
func TestTitleIsAlwaysSingleLine(t *testing.T) {
	for _, body := range []string{
		"第一行\n第二行",
		"# 标题\n正文",
		"  缩进的第一行\n\t制表符行",
	} {
		got := Derive("", body)
		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("正文 %q 提出的标题带换行：%q", body, got)
		}
	}
}

// TestHashtagIsNotHeading locks in "`#urgent` is not a heading marker".
//
// ATX headings require a space after #. Without that rule, "#urgent" would be stripped to "urgent",
// losing the hash sign the user meant to write.
func TestHashtagIsNotHeading(t *testing.T) {
	if got := Derive("", "#紧急 这个要马上处理"); got != "#紧急 这个要马上处理" {
		t.Errorf("`#紧急` 不该被当成标题前缀剥掉，实际 %q", got)
	}
}
