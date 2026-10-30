package weixin

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxReplyRunes is the longest text sent in one WeChat message; longer answers are split. The reference
// client uses the same limit (textChunkLimit 4000).
const maxReplyRunes = 4000

var (
	mdImage    = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	mdSmallH   = regexp.MustCompile(`(?m)^#{5,6}[ \t]+`)
	blankLines = regexp.MustCompile(`\n{3,}`)
)

// cleanReply removes the markdown WeChat cannot show, following the reference client's filter: images are
// dropped (they would show as a raw link) and level 5/6 heading markers are removed (the text is kept).
// Code, tables, bold and other headings pass through.
func cleanReply(text string) string {
	text = mdImage.ReplaceAllString(text, "")
	text = mdSmallH.ReplaceAllString(text, "")
	text = blankLines.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// splitReply cuts text into pieces of at most limit runes, preferring paragraph breaks, then line breaks.
func splitReply(text string, limit int) []string {
	if limit <= 0 {
		limit = maxReplyRunes
	}
	var out []string
	for utf8.RuneCountInString(text) > limit {
		runes := []rune(text)
		head := string(runes[:limit])
		cut := strings.LastIndex(head, "\n\n")
		if cut < len(head)/2 {
			cut = strings.LastIndex(head, "\n")
		}
		if cut < len(head)/2 {
			cut = len(head)
		}
		out = append(out, strings.TrimRight(head[:cut], "\n"))
		text = strings.TrimLeft(text[cut:], "\n")
	}
	if strings.TrimSpace(text) != "" {
		out = append(out, text)
	}
	return out
}

// Fixed replies. They are sent to the WeChat user, so they are user-facing copy.
const (
	replyNotBound = "这个微信机器人还没有绑定 agent。到 AMA 的「微信」页面给它选一个 agent 就能用了。"
	replyDisabled = "这个 agent 已停用，暂时不会回答。到 AMA 的 agent 详情页把状态改回「已启用」就能继续用了。"
	replyNewChat  = "好的，已开始新的对话。"
	replyHelp     = "直接发消息就会由 agent 回答，同一个对话会记住上下文。\n" +
		"/new  开始新的对话（忘掉之前的上下文）\n" +
		"/help  查看这条说明"
	replyEmpty        = "这一轮没有产生回复，可能是执行中出了问题。可以换个说法再问一次。"
	replyRateLimited  = "这一轮没能完成：模型侧连续失败、重试次数已用尽（多半是被限流了）。稍等一会儿再发一次同样的消息即可。"
	replySendFailed   = "消息没能交给 agent 处理，请稍后再试。"
	replyUnsupported  = "暂时只能处理文字消息。"
	replyInterruptRun = "服务正在重启，这条消息没处理完，请稍后再发一次。"
)
