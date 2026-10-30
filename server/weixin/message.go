package weixin

import (
	"fmt"
	"strings"

	wx "github.com/chyroc/ama/server/shared/infra/weixin"
)

// messageID is the dedup key of an inbound message: message_id, else client_id, else seq.
func messageID(msg wx.Message) string {
	switch {
	case msg.MessageID != "":
		return string(msg.MessageID)
	case msg.ClientID != "":
		return "client:" + msg.ClientID
	case msg.Seq != 0:
		return fmt.Sprintf("seq:%d", msg.Seq)
	}
	return ""
}

// scopeKey is the session scope of a conversation. A WeChat bot talks with users one to one, so each
// user is one continuous conversation (the user can start over with /new).
func scopeKey(msg wx.Message) string {
	return "user:" + msg.FromUserID
}

// messageText turns a WeChat message into the text the agent reads.
//
// Media cannot be read yet (the CDN download is not ported), so it becomes a placeholder: the agent
// at least knows the user sent something it cannot see, instead of receiving an empty message. A voice
// message with WeChat's own transcript becomes that transcript.
func messageText(msg wx.Message) string {
	var parts []string
	for _, it := range msg.Items {
		if t := itemText(it); t != "" {
			parts = append(parts, t)
		}
		if it.RefMsg != nil {
			if q := quoteText(it.RefMsg); q != "" {
				parts = append([]string{q}, parts...)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func itemText(it wx.Item) string {
	switch it.Type {
	case wx.ItemText:
		if it.Text != nil {
			return strings.TrimSpace(it.Text.Text)
		}
	case wx.ItemVoice:
		if it.Voice != nil && strings.TrimSpace(it.Voice.Text) != "" {
			return strings.TrimSpace(it.Voice.Text)
		}
		return "[语音]"
	case wx.ItemImage:
		return "[图片]"
	case wx.ItemVideo:
		return "[视频]"
	case wx.ItemFile:
		if it.File != nil && it.File.FileName != "" {
			return "[文件] " + it.File.FileName
		}
		return "[文件]"
	}
	return ""
}

// quoteText renders a quoted message as markdown quote lines.
func quoteText(ref *wx.RefMessage) string {
	text := ""
	if ref.Item != nil {
		text = itemText(*ref.Item)
	}
	if text == "" {
		text = strings.TrimSpace(ref.Title)
	}
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// harnessBlock is the platform context sent with each message as a second text block.
//
// It follows the `<harness:xxx>` convention (see core/envprompt.go and feishu/larkprompt.go): the facts
// are `key: value` lines before the first `## ` heading (the page renders them as a table), and the
// guidance for the model comes after. It goes after the user's text so it never becomes the session title.
func harnessBlock(msg wx.Message) string {
	var b strings.Builder
	b.WriteString("<harness:weixin>\n")
	b.WriteString("channel: weixin\n")
	b.WriteString("chat_type: direct\n")
	fmt.Fprintf(&b, "user_id: %s\n", oneLine(msg.FromUserID))
	if id := messageID(msg); id != "" {
		fmt.Fprintf(&b, "message_id: %s\n", oneLine(id))
	}
	b.WriteString("\n## How this conversation works\n")
	b.WriteString("- The user is talking to you in WeChat, one to one. Your reply is sent back to WeChat as a message.\n")
	b.WriteString("- WeChat shows plain text with light markdown; images in markdown are dropped. Keep answers short and direct.\n")
	b.WriteString("- [图片] / [视频] / [文件] / [语音] mark media the user sent that you cannot see yet; ask the user to describe it if needed.\n")
	b.WriteString("- All messages from this user continue one session; the user can send /new to start over.\n")
	b.WriteString("</harness:weixin>")
	return b.String()
}

// oneLine keeps a fact on one line, so a strange id can never break the key: value layout.
func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(strings.TrimSpace(s))
}

// Commands the bot understands without asking the agent.
const (
	cmdNew  = "/new"
	cmdHelp = "/help"
)

// command returns the command in text ("" when it is not one). /reset is an alias of /new.
func command(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	switch t {
	case "/new", "/reset", "/clear":
		return cmdNew
	case "/help":
		return cmdHelp
	}
	return ""
}
