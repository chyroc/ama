// The write operation of `ama skill`: upload a whole skill package.
//
// Subcommand:
//
//	ama skill upload <local path> [--dry-run] [--workspace <id>]
//
// # Why this command exists instead of going through `ama api`
//
// The backend's POST /api/workspaces/{ws}/skills/upload takes multipart/form-data, while the
// `ama api` escape hatch parses its third argument as JSON (see rawAPI); **it cannot reach this
// API**. So the agent in the container could not create skill packages, with only hand-written curl
// in bash left, while the usage clearly says "do not hand-write curl". This command fills that gap.
//
// Skill packages come in two shapes, decided by the server from the **file name extension** (see
// skillsvc.ParseSkillPackage):
//
//	foo.zip    a zip package, which must contain SKILL.md; the top-level directory is stripped
//	SKILL.md   bare markdown (with frontmatter)
//
// So this only sends the file as is and **does not parse on the client**: there is only one set of
// parsing rules, on the server side; parsing on both sides would sooner or later judge differently,
// and that inconsistency would give no error.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// skillUploadFieldName is the name of the file field in the upload form.
//
// It has the same value "file" as uploadFieldName in apihandler on the server side, and is
// **deliberately defined separately**: the CLI is a separately released binary installed in
// containers and cannot reference the server's packages. Getting it wrong makes the server reply
// "please choose a file to upload", which points at "you did not choose a file" while the real
// problem is a mismatched field name.
const skillUploadFieldName = "file"

// maxSkillUploadBytes matches skillsvc.MaxUploadBytes on the server side (100 MiB), also
// deliberately written separately. It has only one job here: **stop it before sending a hundred-plus
// MiB**, so it is not uploaded for nothing and then rejected by the server. The real boundary is
// still on the amad side.
const maxSkillUploadBytes = 100 << 20

// skillUpload is the implementation of `ama skill upload`.
func (c *client) skillUpload(args []string) error {
	path, rest := shiftPositional(args)
	if strings.TrimSpace(path) == "" {
		return errors.New(
			"要上传的文件路径：ama skill upload <路径>\n" +
				"裸 SKILL.md 与 zip 技能包都行，服务端按扩展名判：.zip 走包解析，其余按 SKILL.md 解析。")
	}

	ws, err := c.wsID(rest)
	if err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打不开要上传的文件 %q：%w", path, err)
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("读不到 %q 的文件信息：%w", path, err)
	}
	if st.IsDir() {
		return fmt.Errorf("%q 是目录。技能上传一次只收一个文件：裸 SKILL.md 或 zip 包；"+
			"整个目录要先打成 zip（zip 里的顶层目录服务端会剥掉）", path)
	}
	if st.Size() > maxSkillUploadBytes {
		return fmt.Errorf("文件太大：%d 字节，上限 %d 字节。技能包里不该有这么大的东西",
			st.Size(), int64(maxSkillUploadBytes))
	}

	// The multipart body is assembled in memory, about the size of the file itself. The 100 MiB
	// limit is the reason "assembling it this way is safe"; anything bigger should switch to
	// streaming, but skill packages should never get there.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(skillUploadFieldName, filepath.Base(path))
	if err != nil {
		return fmt.Errorf("拼上传表单失败：%w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("读取 %q 的内容失败：%w", path, err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("拼上传表单失败：%w", err)
	}

	apiPath := "/api/workspaces/" + url.PathEscape(ws) + "/skills/upload"
	if hasFlag(args, "--dry-run") {
		// dryRun only parses without saving: lets the agent check, before really creating it, that
		// the parsed name and file list are right (especially whether the zip's top-level directory
		// was stripped correctly).
		apiPath += "?dryRun=true"
	}

	raw, err := c.doStream(http.MethodPost, apiPath, mw.FormDataContentType(), &body, int64(body.Len()))
	if err != nil {
		return err
	}
	return printJSON(raw)
}
