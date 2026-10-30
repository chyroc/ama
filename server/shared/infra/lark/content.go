// Content parsing for inbound messages: flatten Feishu's content into plain text and strip
// @mentions.
//
// Only text / post are handled; other types get a bracketed placeholder. Extend this when images
// and files really need support.
//
// # Pitfall one: content is "a JSON string that holds JSON"
//
// Feishu's message.content field is itself a string, and the JSON is inside that string:
//
//	"content": "{\"text\":\"hello\"}"
//
// So it must be decoded twice. Decoding once gives the raw text full of backslashes, which looks
// like "there is content" but is not text at all; fed to the agent directly it is a pile of escape
// characters.
//
// # Pitfall two: an @mention in the text is a placeholder, not a name
//
// When the user types "@Helper please look this up", the text in content is:
//
//	"@_user_1 please look this up"
//
// The real name is in the parallel mentions array. To strip it cleanly all three forms must be
// tried: the placeholder `@_user_1`, the expanded `@name`, and `@ name`, where Feishu sometimes
// inserts a space. Missing one leaves the agent with an odd leading @.
//
// # Pitfall three: do not look at the text to decide whether the bot was @mentioned
//
// Looking at the text breaks on renames, and gets fooled by "the user typed an @ with the same name
// by hand". Compare the structured identity (open_id / app_id) in mentions instead.

package lark

import (
	"encoding/json"
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// parseMessageText takes the plain text out of Feishu's content, strips @mentions, and decides
// whether the bot was @mentioned.
func parseMessageText(
	rawContent, messageType string, mentions []*larkim.MentionEvent, bot BotIdentity,
) (text string, mentionedBot bool) {
	return stripMentions(decodeContent(rawContent, messageType), mentions),
		mentionsBot(mentions, bot)
}

// decodeContent decodes content into plain text. See pitfall one: decode twice.
func decodeContent(raw, messageType string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return ""
	}

	// ⚠️ **The content of a merged forward is not JSON**; it is a fixed English sentinel, `Merged
	// and Forwarded Message` (observed, see the header of expand.go). Without a special case, the
	// second decode below fails and that English sentence is handed to the agent as if the user
	// said it. That is exactly the symptom being fixed here: the agent gets an English sentence
	// with no information, and since it does not look like an error, it does not ask.
	//
	// Only a placeholder here; the real expansion needs one more API call and lives in expand.go.
	if strings.EqualFold(strings.TrimSpace(messageType), MsgTypeMergeForward) {
		return "[合并转发]"
	}

	// Layer one: content is usually a JSON string, so first strip the outer quotes and escapes.
	inner := raw
	var unquoted string
	if err := json.Unmarshal([]byte(raw), &unquoted); err == nil {
		inner = unquoted
	}

	// Layer two: the real object is inside.
	var obj map[string]any
	if err := json.Unmarshal([]byte(inner), &obj); err != nil {
		// If it does not decode into an object, it was plain text all along (or a shape we do not
		// know), so use it as is.
		return collapseBlankLines(inner)
	}
	return collapseBlankLines(textFromObject(obj, messageType))
}

// textFromObject takes text out of the content object according to the message type.
func textFromObject(obj map[string]any, messageType string) string {
	switch strings.TrimSpace(messageType) {
	case "", "text":
		return stringField(obj, "text")
	case "post":
		return postText(obj)
	default:
		// Images, files, audio, cards and so on.
		//
		// ⚠️ **The resource key must be carried out; a bare `[image 消息]` placeholder is not
		// enough.**
		// This was a real reported gap: this used to output only the type name, so image_key /
		// file_key were dropped at this layer, and the agent got "there is something but I know
		// nothing about it". **It could not even start fetching it**, because fetching a resource
		// needs the (message_id, file_key) pair (see resourceRef).
		//
		// Now the output has the shape `[图片 img_xxx]`, and together with the
		// `lark-cli im +messages-resources-download --message-id … --file-key …`
		// given in the <harness:lark> block, the bytes can really be downloaded.
		//
		// It still **does not download the bytes**: that would stuff a blob of base64 into the
		// prompt for every message with an image, and it would be stored in RunEvent forever along
		// with the conversation. Fetching on demand is the right granularity.
		if t := stringField(obj, "text"); t != "" {
			return t
		}
		return resourcePlaceholder(obj, strings.TrimSpace(messageType))
	}
}

// resourcePlaceholder builds a placeholder **with the key** for non-text messages.
//
// Example shapes (when the key is not available it falls back to just the type name, same as before
// the change):
//
//	[图片 img_v3_02ad_e19f…]
//	[文件 report.xlsx file_v3_abc…]
//	[语音 file_v3_xyz… 3s]
//
// The Chinese type names are hard-coded in a table instead of using Feishu's msg_type directly
// because a mixed form like `[image 消息]` reads awkwardly, and the agent works in a Chinese context.
// Unknown types keep the original msg_type, which is more honest than making up a Chinese name.
func resourcePlaceholder(obj map[string]any, messageType string) string {
	label := map[string]string{
		"image": "图片", "file": "文件", "audio": "语音",
		"media": "视频", "sticker": "表情", MsgTypeInteractive: "卡片",
		// The content of a merged forward is not JSON at all (it is the fixed sentinel string
		// `Merged and Forwarded Message`), so normally this is never reached: decodeContent returns
		// that English sentence as is when it cannot decode an object. This entry is kept to cover
		// "Feishu one day switches to returning JSON", so it would at least still be a Chinese
		// label. The real expansion is in expand.go.
		MsgTypeMergeForward: "合并转发",
	}[messageType]
	if label == "" {
		label = messageType
	}

	parts := []string{label}
	// The file name goes before the key: it carries more information for both people and the model
	// (`report.xlsx` reads better than file_v3_…).
	if name := stringField(obj, "file_name"); name != "" {
		parts = append(parts, name)
	}
	// image_key and file_key are two field names: images use the former, the rest use the latter.
	// Try both.
	for _, k := range []string{"image_key", "file_key"} {
		if v := stringField(obj, k); v != "" {
			parts = append(parts, v)
			break
		}
	}
	if d := stringField(obj, "duration"); d != "" {
		parts = append(parts, d+"ms")
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// postText flattens rich text (post) into multi-line plain text.
//
// The structure of post is {"title": "...", "content": [[seg, seg], [seg]]}: the outer array is
// lines, the inner array is the segments within one line, and each segment has its own tag (text /
// a / at / img ...).
func postText(obj map[string]any) string {
	var lines []string
	if title := stringField(obj, "title"); title != "" {
		lines = append(lines, title)
	}
	rows, _ := obj["content"].([]any)
	for _, row := range rows {
		segs, ok := row.([]any)
		if !ok {
			continue
		}
		var parts []string
		for _, seg := range segs {
			m, ok := seg.(map[string]any)
			if !ok {
				continue
			}
			if s := postSegment(m); s != "" {
				parts = append(parts, s)
			}
		}
		if line := strings.TrimSpace(strings.Join(parts, "")); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// postSegment takes the readable text of one rich text segment.
func postSegment(seg map[string]any) string {
	switch strings.TrimSpace(stringField(seg, "tag")) {
	case "text", "a":
		return stringField(seg, "text")
	case "at":
		// @ segment: keep the placeholder form and let stripMentions strip it in one place.
		if k := stringField(seg, "user_id"); k != "" {
			return "@" + k
		}
		return stringField(seg, "text")
	case "img":
		// Images embedded in rich text also need the key; otherwise the agent sees "[图片]" with no
		// way to act on it.
		// The image field name in rich text segments is image_key (same as image messages).
		if k := stringField(seg, "image_key"); k != "" {
			return "[图片 " + k + "]"
		}
		return "[图片]"
	default:
		return stringField(seg, "text")
	}
}

// stripMentions removes every @mention from the text. See pitfall two: all three forms must be
// tried.
func stripMentions(text string, mentions []*larkim.MentionEvent) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	for _, m := range mentions {
		if m == nil {
			continue
		}
		for _, candidate := range mentionCandidates(m) {
			text = strings.ReplaceAll(text, candidate, "")
		}
	}
	return collapseBlankLines(text)
}

// mentionCandidates lists the forms an @ may take in the text.
func mentionCandidates(m *larkim.MentionEvent) []string {
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	// The placeholder itself, like @_user_1. This is the most common.
	add(str(m.Key))
	if name := str(m.Name); name != "" {
		add("@" + name)
		// Feishu sometimes inserts a space between the @ and the name.
		add("@ " + name)
	}
	return out
}

// mentionsBot decides whether this message @mentions our bot. See pitfall three: compare the
// structured identity, not the text.
func mentionsBot(mentions []*larkim.MentionEvent, bot BotIdentity) bool {
	openID := strings.TrimSpace(bot.OpenID)
	appID := strings.TrimSpace(bot.AppID)
	if openID == "" && appID == "" {
		return false
	}
	for _, m := range mentions {
		if m == nil {
			continue
		}
		if m.Id != nil && openID != "" && str(m.Id.OpenId) == openID {
			return true
		}
		if appID != "" && str(m.TenantKey) == appID {
			return true
		}
	}
	return false
}

// stringField takes a string field from a map; a wrong type counts as empty.
func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// collapseBlankLines trims each line and removes blank lines, then trims the whole thing.
//
// Stripping the @ often leaves a pile of spaces and blank lines ("@Helper\n\nplease check" becomes
// "\n\nplease check"); without cleanup, the first character the agent gets is a newline.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
