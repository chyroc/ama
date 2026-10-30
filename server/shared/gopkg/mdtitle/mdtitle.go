// Package mdtitle extracts titles automatically: when the user leaves the title empty, it takes one
// from the body.
//
// # Why it is needed
//
// People reporting a problem often just want to say what they have to say, not name it first.
// Forcing a title makes people type something like "problem" or "bug", which is worse than
// automatic extraction, because the list fills up with rows of the same name.
//
// But **the list page must have a readable title**, so "not filled in" cannot mean "empty". This
// file turns the first sentence of the body into the title.
//
// # Every rule here guards against the same thing: a title that is a string of symbols
//
// The most typical failure is a user posting just an image (the body is `![](https://xxx/a.png)`),
// so the title becomes that string of Markdown. No error, it saves fine, and the list shows a line of
// garbage; the user just thinks the feature is bad, without realizing that title extraction did not
// strip Markdown. So stripping markup is not a nice-to-have.
//
// Pure functions with no side effects; all edge cases have tests (mdtitle_test.go).
//
// # Why a separate package (this is the whole reason it exists)
//
// It was originally `core/issuetitle.go`, but it is **not owned by issue alone**: the suggestion
// domain uses the same extraction rules (`core/suggestion.go` calls it once on create and once on
// update). core is being split into packages by resource domain; if this were not pulled out first,
// suggestion would stop compiling as soon as issue moved out.
// This is the same kind of move as shared/infra/filebytes and shared/gopkg/humanize.
//
// ⚠️ The package is named mdtitle, not title: it only handles one kind of input, **a Markdown body**.
// The name fixes the scope, so the next person does not stuff things like "naming an agent" in here.
package mdtitle

import (
	"regexp"
	"strings"
	"unicode"
)

// maxTitleRunes is the title length limit, **in runes, not bytes**.
//
// Cutting by byte splits a Chinese character in half (one Chinese character is 3 bytes in UTF-8),
// and what gets stored is a string of replacement characters, which in tests just looks like "the
// title is a bit short".
//
// 80 is set by how much one row of the list page can show: anything longer gets cut off by CSS in
// the UI anyway, and storing it only adds content to the database that nobody sees.
const maxTitleRunes = 80

// untitledTitle is the fallback title when nothing at all can be extracted.
//
// **It must not be an empty string.** An empty title is an invisible row in the list; the user
// thinks the data is broken, or that the submission failed, and submits again. A clear placeholder
// at least tells them "the item is here, it just has no name", and clicking in shows the body.
const untitledTitle = "未命名问题"

var (
	// mdHeading matches ATX heading prefixes: `# ` to `###### `.
	//
	// A space is required after #: `#hashtag` is not a heading, and stripping # would turn "#urgent"
	// into "urgent", losing what the user meant.
	mdHeading = regexp.MustCompile(`^#{1,6}\s+`)

	// mdQuote matches the quote prefix `> `, nesting allowed (`>> `).
	mdQuote = regexp.MustCompile(`^(?:\s*>)+\s*`)

	// mdBullet matches unordered list prefixes `- ` / `* ` / `+ `.
	//
	// A space is also required after it: a line starting with `*emphasis*` is not a list item, and
	// stripping it as a list would eat the first asterisk, so "*important* thing" becomes
	// "important* thing", with a lone extra asterisk.
	mdBullet = regexp.MustCompile(`^\s*[-*+]\s+`)

	// mdOrdered matches ordered list prefixes `1. ` / `2) `.
	mdOrdered = regexp.MustCompile(`^\s*\d+[.)]\s+`)

	// mdFence matches code fence lines (``` or ~~~, optionally with a language tag).
	//
	// **Fence lines are skipped whole, not prefix-stripped**: after stripping ``` what is left is the
	// language name ("go", "bash"), which is a syntax tag, not a title. Skipping it gets the real
	// first line inside the code block.
	mdFence = regexp.MustCompile("^\\s*(?:```|~~~)")

	// mdThematicBreak matches horizontal rules (--- / *** / ___, three or more).
	//
	// It is listed separately because it looks like a list item but has no content; stripped, it is empty.
	mdThematicBreak = regexp.MustCompile(`^\s*(?:-{3,}|\*{3,}|_{3,})\s*$`)

	// mdImage matches images `![alt](url)`, **capturing alt**.
	//
	// ⚠️ It must be handled before links: image syntax is link syntax with a `!` in front, and
	// matching it as a link first turns `![alt](url)` into `!alt`, with an extra exclamation mark.
	mdImage = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)

	// mdLink matches links `[text](url)`, capturing the text.
	mdLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

	// mdBold matches `**bold**` / `__bold__`. It must run before single asterisks, or `**x**` leaves `*x*`.
	mdBold = regexp.MustCompile(`(\*\*|__)(.+?)(\*\*|__)`)

	// mdEmphasis matches `*italic*` / `_italic_`.
	mdEmphasis = regexp.MustCompile(`(\*|_)([^*_]+?)(\*|_)`)

	// mdCode matches inline code `` `code` ``.
	mdCode = regexp.MustCompile("`([^`]*)`")

	// mdStrike matches strikethrough `~~deleted~~`.
	mdStrike = regexp.MustCompile(`~~(.+?)~~`)

	// mdAutolink matches bare angle-bracket links `<https://x>`, taking the content inside.
	mdAutolink = regexp.MustCompile(`<([^>]*)>`)

	// spaceRun matches runs of whitespace (including tabs and full-width spaces), used to squash the
	// title into one line.
	spaceRun = regexp.MustCompile(`\s+`)
)

// Derive decides the final title of an issue.
//
// Three cases:
//
//	title not empty         ->  use it as-is (trim + truncate)
//	title empty, body not   ->  extract from body (see extractTitleFromBody)
//	both empty              ->  return an empty string, **the caller rejects it**
//
// **Returning empty in the last case instead of falling back to "untitled issue" is deliberate.**
// Both empty means a record with no content at all; that is a request that should be rejected, not
// the fixable problem of "no title given". A fallback here would silently store an empty record, and
// the list would gain a row with nothing inside.
//
// The caller is validateIssueDraft in core/issue.go, which turns the empty string into ErrInvalidArgument.
func Derive(title, body string) string {
	if t := strings.TrimSpace(title); t != "" {
		return truncateRunes(collapseSpaces(t), maxTitleRunes)
	}
	if strings.TrimSpace(body) == "" {
		// Both empty: leave it to the caller to reject; no fallback here.
		return ""
	}
	return extractTitleFromBody(body)
}

// extractTitleFromBody takes the first sentence with content from a Markdown body as the title.
//
// body is already known to be non-empty (Derive checked), so this **must return a non-empty
// string**; if it is still empty at the end, fall back to untitledTitle.
//
// It scans line by line instead of only looking at the first line: the first line may well be
// empty, a fence, or an image without alt, and looking only at it gives an empty title while the
// body clearly has text.
func extractTitleFromBody(body string) string {
	for _, line := range strings.Split(body, "\n") {
		// Skip fence lines whole (stripping ``` only leaves the language name).
		if mdFence.MatchString(line) {
			continue
		}
		// Skip horizontal rules: stripped, they are empty.
		if mdThematicBreak.MatchString(line) {
			continue
		}
		if cleaned := stripMarkdown(line); cleaned != "" {
			return truncateRunes(cleaned, maxTitleRunes)
		}
	}
	// The whole body is empty lines / fences / horizontal rules / images without alt.
	return untitledTitle
}

// stripMarkdown strips one line of Markdown into plain text; returns an empty string when no
// content is left.
//
// The order matters; swapping a few steps breaks it:
//
//   - **block prefixes first** (heading/quote/list), they only mean something at line start;
//   - **images before links**: image syntax is a link with `!` in front; the other way leaves `!alt`;
//   - **bold before italic**: `**x**` matched as single asterisks leaves `*x*`.
func stripMarkdown(line string) string {
	s := line

	// Block prefixes. A quote may wrap a list (`> - item`), so after stripping the quote try the list again.
	s = mdQuote.ReplaceAllString(s, "")
	s = mdHeading.ReplaceAllString(s, "")
	s = mdOrdered.ReplaceAllString(s, "")
	s = mdBullet.ReplaceAllString(s, "")

	// Inline structure. Images must come before links.
	s = mdImage.ReplaceAllString(s, "$1")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdAutolink.ReplaceAllString(s, "$1")
	s = mdCode.ReplaceAllString(s, "$1")
	s = mdStrike.ReplaceAllString(s, "$1")
	s = mdBold.ReplaceAllString(s, "$2")
	s = mdEmphasis.ReplaceAllString(s, "$2")

	s = collapseSpaces(s)

	// A line with only punctuation left after stripping also counts as no content.
	//
	// This blocks "table separator rows" (`|---|---|`) and "a line of just a few symbols": they cannot
	// be stripped (they match none of the rules above) but are not titles either; making them a title
	// gives a line of `|---|---|`, as ugly as not stripping Markdown at all.
	if !hasReadableRune(s) {
		return ""
	}
	return s
}

// hasReadableRune reports whether some text has "readable characters": letters, digits or ideographs.
//
// It uses unicode categories instead of listing ASCII: symbols other than Chinese, Japanese and
// emoji must all be classified correctly, and "does this line have real content" must give the same
// answer for Chinese and English users.
func hasReadableRune(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// collapseSpaces squashes runs of whitespace into a single space and trims both ends.
//
// A title is **single-line**: indentation, tabs and trailing spaces from the body only add odd gaps
// in the UI, which look like broken CSS.
func collapseSpaces(s string) string {
	return strings.TrimSpace(spaceRun.ReplaceAllString(s, " "))
}

// truncateRunes truncates by rune, adding an ellipsis when too long.
//
// **By rune, not byte**: one Chinese character is 3 UTF-8 bytes, and cutting by byte splits a
// character in half, storing the replacement character U+FFFD. That damage just looks like "the
// title is a bit short" in tests, and is hard to notice.
//
// The ellipsis is the single character `…`, not three dots: it takes one display width while `...`
// takes three, which more easily overflows the line for a title already at the limit.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	// Cut to limit-1 and then add the ellipsis, so the total length is still limit; otherwise "limit
	// 80" would actually produce an 81-character title, and the database side has no length
	// constraint, so no error.
	return strings.TrimRight(string(runes[:limit-1]), " ") + "…"
}
