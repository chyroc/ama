// Security boundary tests for attachment type detection and response headers.
//
// # This group is security tests; every case has been mutation-verified
//
// "A security check without mutation verification is not verified": that is the rule set in
// core/README-feedback.md. The mutation table is in core/README-issue.md; commenting out any check
// turns a matching case red.
//
// The point of the assertions is not "valid files can be uploaded" but **which layer stops each
// attack shape**: content and extension conflict, arbitrary bytes renamed to .png, newlines in the
// file name, SVG.

package attachment

import (
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// pngBytes is the first few dozen bytes of a minimal valid PNG.
//
// As long as the magic number is right, http.DetectContentType recognizes it, which is exactly the
// detection path we want to test.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR" +
	"\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

// gifBytes / jpegBytes / pdfBytes work the same way, each with its own magic number.
var (
	gifBytes  = []byte("GIF89a\x01\x00\x01\x00\x00\xff\x00,")
	jpegBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
	pdfBytes  = []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n")
	zipBytes  = []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00" + strings.Repeat("\x00", 20))
)

// TestDetectAttachmentTypeAcceptsAllowedKinds is the positive coverage of the allowlist.
//
// It ensures the "reject" cases below do not pass just because "everything is rejected".
func TestDetectAttachmentTypeAcceptsAllowedKinds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		filename string
		content  []byte
		want     string
	}{
		{"png", "shot.png", pngBytes, "image/png"},
		{"jpeg", "photo.jpg", jpegBytes, "image/jpeg"},
		{"jpeg 另一个扩展名", "photo.jpeg", jpegBytes, "image/jpeg"},
		{"gif", "anim.gif", gifBytes, "image/gif"},
		{"pdf", "report.pdf", pdfBytes, "application/pdf"},
		{"zip", "pack.zip", zipBytes, "application/zip"},
		{"txt", "log.txt", []byte("一段纯文本"), "text/plain"},
		// .md / .json both sniff as text/plain (sniffing does not parse structure),
		// so they take the relaxed resolveByExtension branch.
		{"markdown", "notes.md", []byte("# 标题\n正文"), "text/plain"},
		{"json", "data.json", []byte(`{"a":1}`), "application/json"},
		{"大写扩展名", "SHOT.PNG", pngBytes, "image/png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectType(tc.filename, tc.content)
			if err != nil {
				t.Fatalf("合法文件被拒了：%v", err)
			}
			if got != tc.want {
				t.Errorf("期望类型 %q，实际 %q", tc.want, got)
			}
		})
	}
}

// TestDetectAttachmentTypeRejectsHTMLDisguisedAsImage is the most important case in this group.
//
// **Attack shape**: rename an HTML file to .png and upload it. If we trusted the Content-Type the
// client declared, or the extension, this file would be served back to the browser as image/png +
// inline; the browser (especially without nosniff) would find it is really HTML and **render it as
// a page**, and the <script> inside would run in our origin.
//
// It is stopped by the rule "the sniffing result must match the extension".
func TestDetectAttachmentTypeRejectsHTMLDisguisedAsImage(t *testing.T) {
	html := []byte("<!DOCTYPE html><html><body><script>alert(document.cookie)</script></body></html>")

	for _, filename := range []string{"evil.png", "evil.jpg", "evil.gif", "evil.webp"} {
		t.Run(filename, func(t *testing.T) {
			got, err := DetectType(filename, html)
			if err == nil {
				t.Fatalf("HTML 伪装成 %s 竟然通过了，判成 %q——这是一个 XSS", filename, got)
			}
			if !errors.Is(err, persistmodel.ErrInvalidArgument) {
				t.Errorf("应该是 ErrInvalidArgument（→400），实际 %v", err)
			}
		})
	}
}

// TestDetectAttachmentTypeRejectsSVG locks in "SVG is not accepted".
//
// SVG is executable: it can embed <script> and stuff HTML in with <foreignObject>,
// and an image/svg+xml response is rendered by the browser as a vector image even with nosniff,
// with scripts still running.
//
// # Both forms must be tested, because they take different branches
//
// In practice http.DetectContentType gives different answers for these two kinds of SVG:
//
//	bare <svg ...>          ->  text/plain (sniffing "has no opinion")
//	<?xml ...?><svg ...>    ->  text/xml (sniffing has an opinion)
//
// So the former is stopped by the rule in resolveByExtension "no image types when sniffing has no
// opinion", and the latter by "text/xml is not on the allowlist": **two different lines of defense**.
// If only one is tested, the test stays green when the other defense is removed.
//
// ⚠️ If someday someone wants to "just add svg support", this case stops them and forces them to
// first read the explanation at the top of attachment.go in this package.
func TestDetectAttachmentTypeRejectsSVG(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{
			"裸 svg（嗅探回 text/plain）",
			`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		},
		{
			"带 xml 声明的 svg（嗅探回 text/xml）",
			`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg">` +
				`<script>alert(1)</script></svg>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectType("logo.svg", []byte(tc.content))
			if err == nil {
				t.Fatalf("SVG 竟然通过了，判成 %q——它能执行脚本，见 attachment.go 顶部", got)
			}
		})
	}
}

// TestSniffWithoutOpinionNeverYieldsImageType locks in that "second line of defense" itself.
//
// resolveByExtension has a rule that is easy to mistake for redundant and delete: **when sniffing
// has no opinion, the result may not resolve to any image/ type**.
//
// It blocks "rename arbitrary bytes to .png": such a file sniffs as octet-stream, and if it were
// resolved to image/png by extension, it would be sent inline to <img> to render,
// while we know nothing about its content.
//
// It is tested separately because the cases above are **not enough** to lock it in: TestRejectsSVG
// already passes via "not on the allowlist" when svg is not on the allowlist, so you cannot tell
// whether this rule is in effect.
func TestSniffWithoutOpinionNeverYieldsImageType(t *testing.T) {
	for _, tc := range []struct{ name, filename, content string }{
		{"未知二进制改名 png", "x.png", "\x00\x01\xde\xad\xbe\xef"},
		{"纯文本改名 png", "x.png", "这其实是一段文字"},
		{"未知二进制改名 gif", "x.gif", "\x00\x01\xde\xad\xbe\xef"},
		{"未知二进制改名 webp", "x.webp", "\x00\x01\xde\xad\xbe\xef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectType(tc.filename, []byte(tc.content))
			if err == nil {
				t.Fatalf("嗅探认不出的内容被判成了 %q——它会被 inline 渲染", got)
			}
		})
	}
}

// TestDetectAttachmentTypeRejectsArbitraryBytesRenamedToPNG blocks the second way around.
//
// **Attack shape**: upload binary data we do not recognize at all, renamed to .png.
// Sniffing returns application/octet-stream for it ("no opinion"). If the relaxed branch let "no
// opinion + extension is .png" through as image/png, these unknown bytes would be sent inline to <img>.
//
// It is stopped by the rule in resolveByExtension "only non-image types when sniffing has no opinion".
func TestDetectAttachmentTypeRejectsArbitraryBytesRenamedToPNG(t *testing.T) {
	junk := []byte("\x00\x01\x02\x03\xde\xad\xbe\xef" + strings.Repeat("\xff", 100))
	if got, err := DetectType("fake.png", junk); err == nil {
		t.Fatalf("任意字节改名成 .png 竟然通过了，判成 %q", got)
	}
}

// TestDetectAttachmentTypeRejectsMismatchedExtension locks in "reject when the two signals conflict".
//
// The content is a real PNG, the name says .pdf. Picking either is a guess, and one wrong guess is
// a vulnerability, so reject outright.
func TestDetectAttachmentTypeRejectsMismatchedExtension(t *testing.T) {
	if _, err := DetectType("actually-a-png.pdf", pngBytes); err == nil {
		t.Fatal("PNG 内容配 .pdf 扩展名竟然通过了")
	}
}

// TestDetectAttachmentTypeRejectsUnsupportedAndEmpty covers the other two rejection paths.
func TestDetectAttachmentTypeRejectsUnsupportedAndEmpty(t *testing.T) {
	t.Run("空文件单独报错", func(t *testing.T) {
		_, err := DetectType("empty.png", nil)
		if !errors.Is(err, ErrEmpty) {
			// "Unsupported type" would send the user to check the file format, when the real cause is that the file is empty.
			t.Errorf("空文件应报 ErrEmpty，实际 %v", err)
		}
	})
	t.Run("没有扩展名", func(t *testing.T) {
		if _, err := DetectType("README", []byte("内容")); err == nil {
			t.Error("没有扩展名应该被拒（判不出类型）")
		}
	})
	t.Run("可执行文件", func(t *testing.T) {
		elf := []byte("\x7fELF\x02\x01\x01\x00" + strings.Repeat("\x00", 60))
		if _, err := DetectType("payload.exe", elf); err == nil {
			t.Error("可执行文件应该被拒")
		}
	})
}

// TestAttachmentDispositionInlineOnlyForImages locks in "only bitmaps are inline".
//
// Both directions must be tested:
//   - images must be inline, or they will not show in <img src> (the browser treats them as
//     downloads and the image is broken);
//   - everything else must be attachment; that is the last line of defense if type detection is bypassed.
func TestAttachmentDispositionInlineOnlyForImages(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		wantInline  bool
	}{
		{"image/png", true},
		{"image/jpeg", true},
		{"image/gif", true},
		{"image/webp", true},
		{"application/pdf", false},
		{"text/plain", false},
		{"application/json", false},
		{"application/zip", false},
		// Even if an svg/html somehow sneaks in, it must still be attachment.
		{"image/svg+xml", false},
		{"text/html", false},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			got := Disposition(tc.contentType, "file.bin")
			isInline := strings.HasPrefix(got, "inline;")
			if isInline != tc.wantInline {
				t.Errorf("%s 期望 inline=%v，实际头是 %q",
					tc.contentType, tc.wantInline, got)
			}
			if !tc.wantInline && !strings.HasPrefix(got, "attachment;") {
				t.Errorf("非图片必须是 attachment，实际 %q", got)
			}
		})
	}
}

// TestAttachmentDispositionResistsHeaderInjection blocks response header injection.
//
// **Attack shape**: put `\r\n` in the file name followed by a made-up header. The file name is user
// input, and it is put into an HTTP response header; without cleaning, an attacker can inject any
// response header (such as `Set-Cookie`, or a `Content-Type: text/html` overriding the one we set).
//
// Go's net/http does reject header values with newlines at write time, but relying on it means the
// attack shows up as a 500 rather than "handled correctly", and the protection is gone if you switch
// HTTP libraries.
func TestAttachmentDispositionResistsHeaderInjection(t *testing.T) {
	for _, name := range []string{
		"a.png\r\nSet-Cookie: evil=1",
		"a.png\nX-Injected: yes",
		`a".png`,
		`a\".png`,
		"../../etc/passwd",
		"..\\..\\windows\\system32",
		"a\x00b.png",
	} {
		t.Run(name, func(t *testing.T) {
			got := Disposition("image/png", name)
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("文件名 %q 让响应头带上了换行：%q", name, got)
			}
			// Any quote count in the header other than the expected one means filename="..." was closed early.
			// The correct shape is `inline; filename="xxx"; filename*=UTF-8''xxx`,
			// so the quote count is always 2.
			if n := strings.Count(got, `"`); n != 2 {
				t.Errorf("文件名 %q 破坏了引号配对（%d 个引号）：%q", name, n, got)
			}
			if strings.Contains(got, "/") || strings.Contains(got, `\`) {
				t.Errorf("文件名 %q 把路径分隔符带进了头：%q", name, got)
			}
		})
	}
}

// TestAttachmentDispositionEncodesUnicodeFilename locks in that Chinese file names are not garbled.
//
// The filename= field only accepts ASCII; Chinese written into it directly turns into garbage when
// downloaded. So filename*=UTF-8”<pct-encoded> must also be given, and modern browsers prefer it.
func TestAttachmentDispositionEncodesUnicodeFilename(t *testing.T) {
	got := Disposition("image/png", "登录页截图.png")

	if !strings.Contains(got, "filename*=UTF-8''") {
		t.Errorf("缺少 RFC 5987 的 filename*，中文名会乱码：%q", got)
	}
	// After percent-encoding, the UTF-8 byte sequence of U+767B (the first character of the test
	// file name) should be found.
	if !strings.Contains(got, "%E7%99%BB") {
		t.Errorf("中文没有被百分号编码：%q", got)
	}
	// The ASCII fallback copy must not be empty (an empty filename= makes some browsers ignore the
	// whole header, and then the attachment half is lost too).
	if strings.Contains(got, `filename=""`) {
		t.Errorf("ASCII 兜底文件名不能为空：%q", got)
	}
}

// TestAttachmentLimitsAreSane locks in the relationship between the three limits.
//
// The request body limit must be **greater than** the per-file limit: if they were equal, a file of
// exactly 5 MiB would be stopped by the request body gate because of multipart overhead, with an
// error saying "over 5 MiB"; the user sees that the file is exactly 5 MiB and can only trim content
// bit by bit.
func TestAttachmentLimitsAreSane(t *testing.T) {
	if MaxUploadBytes <= MaxBytes {
		t.Errorf("请求体上限（%d）必须大于单文件上限（%d），要给 multipart 封装留余量",
			MaxUploadBytes, MaxBytes)
	}
	if MaxCount <= 0 {
		t.Error("附件数必须有上限，否则一个 issue 能挂无数附件")
	}
}
