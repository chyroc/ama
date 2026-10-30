// Expand cards and merged forwards into text the agent can read.
//
// # What the agent sees for these two message types before expansion
//
//	merged forward   →  Merged and Forwarded Message      (one English sentence, zero information)
//	card             →  [卡片]                            (or worse: "please upgrade to the latest client")
//
// What the two have in common is that they **do not look like an error**, so the agent does
// not ask about them or fetch them. It just answers the user with a useless sentence. The part
// that fetches the original content is in fetch.go; this file only handles "how to turn it
// into text once it is fetched".
//
// # Cards have two schemas with different field names
//
// Tested: the same API returns these two shapes (depending on which schema version the card
// was sent with):
//
//	2.0   {"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"body text"}]}}
//	1.0   {"elements":[{"tag":"div","text":{"content":"body text"}}]}
//
// There is also a **degraded form** (what you get without user_card_content), with yet another
// shape:
//
//	{"title":null,"elements":[[{"tag":"text","text":"Please upgrade to the latest client"}]]}
//	                          ^^ note: an array of arrays, rows × segments, same as post
//
// All three must be recognized. The test is not the schema field (the degraded form has none),
// but **what the elements look like**: element is an array → rows × segments (degraded form /
// post style); element is an object → card element (1.0 / 2.0).
//
// # Why there is a length limit
//
// One merged forward can hold hundreds of messages (in testing, a single child message of a
// log alert had a 3KB body, because it carried a base64-encoded log search link). Putting all of
// it into the prompt has two real costs: it takes up the context window, and it is **stored
// forever in RunEvent**, the archive you can scroll through on the page.
// So here we truncate by both count and bytes, and **say in the text that it was truncated**:
// if we do not, the agent thinks it saw everything and draws conclusions from partial data.
package lark

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Two msg_types that need special handling. They are constants because they are checked here
// and also appear in the placeholder table in content.go; with literals, one gets changed and
// the other is missed.
const (
	// MsgTypeInteractive is a card.
	MsgTypeInteractive = "interactive"
	// MsgTypeMergeForward is a merged forward.
	MsgTypeMergeForward = "merge_forward"
	// MsgTypeSystem is a group event message: group renamed, people added, topic group created, etc.
	//
	// **It is not something anyone said**; skip it entirely in history, see ExpandHistory.
	MsgTypeSystem = "system"
)

// Limits used during expansion. All follow "better to give less than overflow the context".
const (
	// maxForwardChildren is the max number of child messages expanded from a merged forward.
	//
	// 20 covers most "forwarding a discussion for you to look at" cases; more than that is usually
	// the log of a whole group, and then the agent should ask the user what matters, not read it all.
	maxForwardChildren = 20

	// maxChildTextLen is the character limit (in runes) for a **single** expanded child message.
	//
	// It guards against "one child message eats the whole budget". In testing, a log alert body
	// was 3KB+, most of it a base64 search link that is worthless to the agent.
	maxChildTextLen = 800

	// maxExpandedTotalLen is the character limit (in runes) for the whole expanded result.
	//
	// Even with count and per-message limits, the total still needs a cap: 20 × 800 chars = 16000
	// chars, already two orders of magnitude longer than most user messages.
	maxExpandedTotalLen = 4000

	// maxHistoryTextLen is the character limit for a **single history message** after expansion.
	//
	// Smaller than maxChildTextLen: history is "extra context", not "this message itself". 50 ×
	// 800 chars would eat the context window, and reading history only needs the gist.
	maxHistoryTextLen = 300

	// maxHistoryTotalLen is the character limit for the whole history.
	//
	// This is the most important of the limits: history is **injected every turn** (unlike cards,
	// which only appear when a card is sent), and it is also **stored forever in RunEvent**. 6000
	// chars is roughly 50 messages of 120 chars each, enough for most "see what was said before".
	maxHistoryTotalLen = 6000
)

// truncatedNotice is the sentence appended after truncation.
//
// **It is required**: if the agent cannot see "there is more", it treats the partial content as
// complete. The sentence also tells it "fetch it yourself if you want everything"; the
// message_id is given in the harness block.
const truncatedNotice = "…（内容过长已截断，完整内容需自行调用飞书接口获取）"

// ExpandMessage renders a set of fetched messages into one readable text.
//
// items is the return value of GetMessage: the first item is the message itself, the rest are
// child messages (only for merged forwards).
// An empty string means "nothing useful could be rendered"; the caller should keep the original
// placeholder. **Never replace `[卡片]` with an empty string**: then the agent would not even know
// "there is something here".
func ExpandMessage(items []FetchedMessage) string {
	if len(items) == 0 {
		return ""
	}
	root := items[0]
	children := items[1:]

	// Merged forward: the body is a sentinel string; all information is in the child messages.
	if strings.EqualFold(strings.TrimSpace(root.MessageType), MsgTypeMergeForward) {
		return expandForward(children)
	}

	// Other types (mainly cards): only look at this message itself.
	return truncateRunes(cardOrPlainText(root), maxChildTextLen)
}

// expandForward joins the child messages of a merged forward into one text.
func expandForward(children []FetchedMessage) string {
	if len(children) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("[合并转发，共 " + strconv.Itoa(len(children)) + " 条]")

	shown := 0
	truncated := false
	for _, child := range children {
		if shown >= maxForwardChildren {
			truncated = true
			break
		}
		// A recalled child message has no content; skipping it is better than printing an empty line.
		if child.Deleted {
			continue
		}
		text := truncateRunes(cardOrPlainText(child), maxChildTextLen)
		if text == "" {
			continue
		}
		// Stop when the total is exceeded; do not cut the last message into half a sentence.
		if utf8Len(b.String())+utf8Len(text) > maxExpandedTotalLen {
			truncated = true
			break
		}
		b.WriteString("\n" + strconv.Itoa(shown+1) + ". " + text)
		shown++
	}

	if shown == 0 {
		// Nothing could be rendered (all recalled, or all of unknown types).
		// Return an empty string so the caller keeps the placeholder.
		return ""
	}
	if truncated || shown < countRenderable(children) {
		b.WriteString("\n" + truncatedNotice)
	}
	return b.String()
}

// countRenderable counts how many child messages could have been rendered (not recalled).
//
// It is used to tell "did we really give less": only then do we add the truncation note.
// Otherwise every forward would carry "truncated", and the agent would think it is always
// incomplete.
func countRenderable(children []FetchedMessage) int {
	n := 0
	for _, c := range children {
		if !c.Deleted {
			n++
		}
	}
	return n
}

// cardOrPlainText gets the readable text of a message: cards go through card parsing, others reuse the existing decodeContent.
func cardOrPlainText(m FetchedMessage) string {
	msgType := strings.TrimSpace(m.MessageType)
	if strings.EqualFold(msgType, MsgTypeInteractive) {
		if text := cardText(m.Content); text != "" {
			return text
		}
		// The card could not be parsed; fall back to the placeholder (with image_key etc.), not an empty string.
		return decodeContent(m.Content, msgType)
	}
	return decodeContent(m.Content, msgType)
}

// cardText decodes card content into plain text. It recognizes all three shapes, see the file header.
func cardText(raw string) string {
	obj := decodeToObject(raw)
	if obj == nil {
		return ""
	}

	var lines []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			lines = append(lines, s)
		}
	}

	// Title: 1.0 has it at header.title.content, the degraded form at top-level title.
	add(stringField(obj, "title"))
	if header, ok := obj["header"].(map[string]any); ok {
		if title, ok := header["title"].(map[string]any); ok {
			add(stringField(title, "content"))
		}
		if sub, ok := header["subtitle"].(map[string]any); ok {
			add(stringField(sub, "content"))
		}
	}

	// Body: 2.0 has it at body.elements, 1.0 and the degraded form at top-level elements.
	elements := obj["elements"]
	if body, ok := obj["body"].(map[string]any); ok {
		if be, ok := body["elements"]; ok {
			elements = be
		}
	}
	add(elementsText(elements))

	return collapseBlankLines(strings.Join(lines, "\n"))
}

// elementsText flattens card elements into text.
//
// Both shapes must be recognized (see the file header): an array element means "rows ×
// segments" (degraded form / post style); an object element means a card element (1.0 / 2.0).
func elementsText(v any) string {
	items, ok := v.([]any)
	if !ok {
		return ""
	}
	var out []string
	for _, item := range items {
		switch el := item.(type) {
		case []any:
			// The segments of one row; reuse the post segment parsing.
			var parts []string
			for _, seg := range el {
				if m, ok := seg.(map[string]any); ok {
					if s := postSegment(m); s != "" {
						parts = append(parts, s)
					}
				}
			}
			if line := strings.TrimSpace(strings.Join(parts, "")); line != "" {
				out = append(out, line)
			}
		case map[string]any:
			if s := cardElementText(el); s != "" {
				out = append(out, s)
			}
		}
	}
	return strings.Join(out, "\n")
}

// cardElementText gets the text of one card element. Unknown elements return an empty string (skip it, do not make up content).
func cardElementText(el map[string]any) string {
	switch strings.TrimSpace(stringField(el, "tag")) {
	case "markdown":
		// 2.0 rich text: the body is directly in content.
		return stringField(el, "content")
	case "div":
		// 1.0 plain text: in text.content. A div may also carry fields (multi-column text).
		var parts []string
		if text, ok := el["text"].(map[string]any); ok {
			parts = append(parts, stringField(text, "content"))
		}
		for _, f := range asSlice(el["fields"]) {
			if fm, ok := f.(map[string]any); ok {
				if t, ok := fm["text"].(map[string]any); ok {
					parts = append(parts, stringField(t, "content"))
				}
			}
		}
		return strings.TrimSpace(strings.Join(nonEmpty(parts), "\n"))
	case "note":
		// Note: elements mix icons and text segments.
		var parts []string
		for _, e := range asSlice(el["elements"]) {
			if em, ok := e.(map[string]any); ok {
				parts = append(parts, firstNonEmpty(
					stringField(em, "content"), stringField(em, "text")))
			}
		}
		return strings.TrimSpace(strings.Join(nonEmpty(parts), " "))
	case "hr", "img":
		// Dividers have no text; images in cards are usually decoration, the body is elsewhere.
		return ""
	case "column_set", "form", "interactive_container", "collapsible_panel":
		// Containers: recurse into them. Column children are in columns, others in elements.
		var parts []string
		for _, key := range []string{"elements", "columns"} {
			if s := elementsText(el[key]); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.TrimSpace(strings.Join(nonEmpty(parts), "\n"))
	case "button":
		// Button text helps with "what is this card about" (e.g. "Approve", "View details").
		if text, ok := el["text"].(map[string]any); ok {
			if s := stringField(text, "content"); s != "" {
				return "[按钮 " + s + "]"
			}
		}
		return ""
	default:
		// Unknown elements: take text **only if they carry their own text/content**; do not make up content.
		if text, ok := el["text"].(map[string]any); ok {
			return stringField(text, "content")
		}
		return firstNonEmpty(stringField(el, "content"), stringField(el, "text"))
	}
}

// decodeToObject decodes "a JSON string that contains JSON" into an object. Returns nil if it
// cannot be decoded.
//
// Same origin as the two-level decoding in decodeContent (see pitfall 1 in content.go), but
// here we want the object itself, not the text, so that function cannot be reused.
func decodeToObject(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	inner := raw
	var unquoted string
	if err := json.Unmarshal([]byte(raw), &unquoted); err == nil {
		inner = unquoted
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(inner), &obj); err != nil {
		return nil
	}
	return obj
}

// truncateRunes truncates by rune and appends a truncation note if the limit is exceeded.
//
// **By rune, not by byte**: a Chinese character is three bytes; cutting by byte splits a
// character in half and leaves garbled text at the end.
func truncateRunes(s string, limit int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return strings.TrimSpace(string(runes[:limit])) + truncatedNotice
}

// utf8Len measures length in runes.
func utf8Len(s string) int { return len([]rune(s)) }

// asSlice reads an any as an array; returns nil if it is not an array.
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// nonEmpty filters out empty strings.
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// History messages
// ---------------------------------------------------------------------------

// ExpandHistory renders a list of history messages into text for the agent to read.
//
// items is the return value of ListMessages. **Render order is always oldest first** (earliest
// at the top), because that is how a chat log is read. If the caller fetched newest first
// (to get the latest N), it must pass newestFirst as true, and this function flips it back.
//
// excludeID is the id of "the current message": it was already given to the agent as the user
// text. Showing it again in history is a duplicate, and makes the model think the user said it
// twice. Pass empty to exclude nothing.
//
// An empty string means there is no readable history; the caller then does not inject this part.
func ExpandHistory(items []FetchedMessage, newestFirst bool, excludeID string) string {
	if len(items) == 0 {
		return ""
	}

	// Items fetched newest first must be flipped to oldest first before rendering. **Do not modify the caller's slice in place**: it is someone else's data.
	ordered := make([]FetchedMessage, len(items))
	copy(ordered, items)
	if newestFirst {
		for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
			ordered[i], ordered[j] = ordered[j], ordered[i]
		}
	}

	excludeID = strings.TrimSpace(excludeID)
	var lines []string
	total := 0
	truncated := false

	for _, m := range ordered {
		if m.Deleted || (excludeID != "" && m.MessageID == excludeID) {
			continue
		}
		// ⚠️ **Skip group events entirely** (group renamed, people added, topic group created).
		//
		// They are not something anyone said and are worth nothing to the agent. If not skipped they
		// render as two lines of `用户：[system]`, and **both parts are wrong**: the content is a
		// placeholder (system content has no text field, only a `{template, from_user, to_chatters}`
		// struct, which decodeContent does not recognize, so it gives a placeholder), and sender_type
		// is an **empty string**, which historySpeaker defaults to "user". So the model sees "some user
		// said [system]": pure noise, and it may think it failed to decode something and ask about it.
		//
		// The test uses msg_type, not "does the body look like a placeholder": the latter is string
		// matching, and a user typing `[system]` themselves would get deleted by mistake.
		if strings.EqualFold(strings.TrimSpace(m.MessageType), MsgTypeSystem) {
			continue
		}
		text := truncateRunes(cardOrPlainText(m), maxHistoryTextLen)
		if text == "" {
			continue
		}
		// History has many turns and many people, so **who said it must be marked**. Otherwise the
		// agent treats everything as said by one person, and may even take the bot's own earlier
		// replies as user requests.
		line := historySpeaker(m) + "：" + text
		if total+utf8Len(line) > maxHistoryTotalLen {
			truncated = true
			break
		}
		lines = append(lines, line)
		total += utf8Len(line)
	}

	if len(lines) == 0 {
		return ""
	}
	out := strings.Join(lines, "\n")
	if truncated {
		// Is it the **earlier** part that got truncated? No: we join from oldest to newest and stop
		// when over the limit, so what gets dropped is the most recent. This must be said clearly,
		// or the agent will think it saw the latest context.
		out += "\n" + truncatedNotice
	}
	return out
}

// historySpeaker marks the speaker of a message in history.
//
// **It can only go as fine as "person" vs "bot"**: in testing, the sender block fetched with
// the app identity has no name (see the FetchedMessage comment in fetch.go), so a real name is
// not available. An open_id is too long and means nothing to the model. The "user" / "bot"
// split is what the agent really needs: it must know which messages it said before.
func historySpeaker(m FetchedMessage) string {
	switch strings.TrimSpace(strings.ToLower(m.SenderType)) {
	case "app", "bot":
		return "机器人"
	case "":
		// ⚠️ **When sender_type is empty, do not default to "user".**
		//
		// That fallback lies: it turns "we do not know who said it" into "a real person said it",
		// and the model has no way to see that it is a guess. In testing, the sender block of group
		// event messages (system) is four empty strings. Those are already skipped above, but **other
		// types may also have an empty sender in the future** (anonymous messages, messages sent by
		// the platform on someone's behalf), and saying "user" then would be making up facts.
		//
		// "Someone" is honest: it reads naturally and does not pretend to know the identity.
		return "某人"
	default:
		return "用户"
	}
}
