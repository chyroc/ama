// `ama files`: account-level personal files (backend /api/files).
//
// Subcommands:
//
//	ama files list [--limit N]                list my completed files
//	ama files upload <local path> [--type CT] upload (≤8MiB directly, larger ones split into parts automatically)
//	ama files get <id>                        file details (including a time-limited download URL)
//	ama files url <id>                        print only the download URL (for shell substitution)
//	ama files download <id> [local path]      download to a file, relayed through AMA
//	ama files delete <id> --yes               delete a file (needs explicit confirmation)
//	ama files abort <id>                      cancel an unfinished upload
//
// # Why large files need their own HTTP client
//
// The client in main.go has a 25-second timeout (it must be shorter than the container bash tool's
// 60s), meant for small JSON APIs. Uploading GiB-scale files, and downloading, can take far longer
// than 25s, so this uses a client **with no overall timeout** (httpLong), relying only on the TCP
// layer and the peer's behavior.
//
// # Downloads are relayed through AMA, not via downloadUrl
//
// downloadUrl is an object storage presigned link, signed for the host s3.publicEndpoint (the
// address for browsers). This command often runs in a Docker session container or a user's own
// machine, where **that address may not be reachable** (compose defaults to the host's
// localhost:19000, which inside a container points to the container itself). The only thing it can
// always reach is AMA itself, so download calls `GET /api/files/{id}/content`, streamed through by
// amad.
//
// `ama files url` still prints downloadUrl: that is for browsers / places that can reach object
// storage directly.
// ⚠️ Do not send an Authorization header to a third-party address like downloadUrl: that would send
// out the AMA credential.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// partUploadRetries is the number of retries after a part upload fails (3 attempts in total,
// including the first).
//
// Re-uploading the same part number is an idempotent overwrite, so retrying needs no extra state;
// the backoff is a fixed short interval, not worth pulling an exponential backoff library into the
// container for.
const partUploadRetries = 3

// filesCmd is the entry of `ama files`.
func (c *client) filesCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("files 需要子命令：list / upload / get / url / download / delete / abort")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "", "list":
		return c.filesList(rest)
	case "upload", "put":
		return c.filesUpload(rest)
	case "get", "show":
		return c.filesGet(rest)
	case "url":
		return c.filesURL(rest)
	case "download", "pull":
		return c.filesDownload(rest)
	case "delete", "rm", "remove":
		return c.filesDelete(rest)
	case "abort":
		return c.filesAbort(rest)
	default:
		return fmt.Errorf("files 没有 %q 子命令，可用：list / upload / get / url / download / delete / abort", sub)
	}
}

// cliUserFile is the file shape returned by /api/files (aligned with apimodel.UserFile; only the
// needed fields are picked here; unknown fields are ignored by the JSON parser).
type cliUserFile struct {
	ID               string `json:"id"`
	Filename         string `json:"filename"`
	ContentType      string `json:"contentType"`
	Size             int64  `json:"size"`
	Status           string `json:"status"`
	UploadMode       string `json:"uploadMode"`
	PartSize         int64  `json:"partSize"`
	CreatedAt        string `json:"createdAt"`
	DownloadURL      string `json:"downloadUrl"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

func (c *client) filesList(args []string) error {
	flags := parseFlags(args)
	q := ""
	if v := strings.TrimSpace(flags["limit"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return errors.New("--limit 要正整数")
		}
		q = "?limit=" + v
	}
	raw, err := c.do(http.MethodGet, "/api/files"+q, nil)
	if err != nil {
		return err
	}
	return printJSON(raw)
}

// filesUpload uploads a local file.
func (c *client) filesUpload(args []string) error {
	local, _ := shiftPositional(args)
	if local == "" {
		return errors.New("要本地文件路径：ama files upload <path>")
	}
	flags := parseFlags(args)

	f, err := os.Open(local)
	if err != nil {
		return fmt.Errorf("打不开 %s：%w", local, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("读文件信息失败：%w", err)
	}
	if st.IsDir() {
		return fmt.Errorf("%s 是目录，files upload 一次只传一个文件", local)
	}
	size := st.Size()
	name := flags["name"]
	if name == "" {
		// filepath.Base strips the directory part the caller passed on Linux; displaying the file name does not need the path.
		name = filepath.Base(local)
	}
	contentType := flags["type"] // optional; the server stores it as is

	fmt.Fprintf(os.Stderr, "上传 %s（%s）…\n", name, humanSize(size))

	// 1) Start the upload.
	raw, err := c.do(http.MethodPost, "/api/files", map[string]any{
		"filename":    name,
		"contentType": contentType,
		"size":        size,
	})
	if err != nil {
		return err
	}
	var uf cliUserFile
	if err := json.Unmarshal(raw, &uf); err != nil {
		return fmt.Errorf("解析发起上传响应失败：%w", err)
	}

	// 2) Send the content by mode. On any transfer failure try to abort, so no half-finished upload is left in storage/the database.
	if uf.UploadMode == "simple" {
		if err := c.filesPutSimple(uf, f, size); err != nil {
			return c.failUpload(uf, err)
		}
	} else {
		if err := c.filesPutParts(uf, f, size); err != nil {
			return c.failUpload(uf, err)
		}
	}

	// 3) Fetch the details once to confirm completion (including the download URL), and print them for the caller.
	return c.filesGetByID(uf.ID)
}

// filesPutSimple PUTs the whole object (≤8MiB).
func (c *client) filesPutSimple(uf cliUserFile, f *os.File, size int64) error {
	_, err := c.doStream(http.MethodPut, "/api/files/"+url.PathEscape(uf.ID)+"/content",
		"application/octet-stream", f, size)
	return err
}

// filesPutParts does a multipart upload: split by the partSize the server gives, PUT part by part, with limited retries on failure.
func (c *client) filesPutParts(uf cliUserFile, f *os.File, total int64) error {
	partSize := uf.PartSize
	if partSize <= 0 {
		// Fallback: a multipart response should always carry partSize; without it the server is
		// broken; use the protocol constant instead of panicking on a division by zero.
		partSize = 8 * 1024 * 1024
	}
	totalParts := int((total + partSize - 1) / partSize)
	if totalParts > 10000 {
		return fmt.Errorf("文件按 %d 字节切出 %d 片，超过 10000 片上限", partSize, totalParts)
	}

	for n := 1; n <= totalParts; n++ {
		offset := int64(n-1) * partSize
		chunk := partSize
		if n == totalParts {
			chunk = total - offset
		}
		if err := c.uploadOnePartWithRetry(uf.ID, n, f, offset, chunk); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "  片 %d/%d 已传（%s）\n", n, totalParts, humanSize(chunk))
	}

	raw, err := c.do(http.MethodPost, "/api/files/"+url.PathEscape(uf.ID)+"/complete", nil)
	if err != nil {
		return fmt.Errorf("合并分片失败：%w", err)
	}
	_ = raw
	fmt.Fprintf(os.Stderr, "合并完成。\n")
	return nil
}

// uploadOnePartWithRetry reads one part from the given offset with io.SectionReader, retrying on failure.
func (c *client) uploadOnePartWithRetry(
	id string, partNumber int, f *os.File, offset, size int64,
) error {
	var lastErr error
	for attempt := 1; attempt <= partUploadRetries; attempt++ {
		// A new SectionReader each round: the previous round may already have consumed it.
		r := io.NewSectionReader(f, offset, size)
		_, err := c.doStream(http.MethodPut,
			fmt.Sprintf("/api/files/%s/parts/%d", url.PathEscape(id), partNumber),
			"application/octet-stream", r, size)
		if err == nil {
			return nil
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "  片 %d 第 %d 次失败：%v\n", partNumber, attempt, err)
		if attempt < partUploadRetries {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	return fmt.Errorf("片 %d 重试 %d 次仍失败：%w", partNumber, partUploadRetries, lastErr)
}

// failUpload reports an upload failure and tries to abort (best-effort; errors only go to stderr and
// do not override the real cause of failure).
func (c *client) failUpload(uf cliUserFile, cause error) error {
	if uf.UploadMode == "multipart" && uf.ID != "" {
		if _, err := c.do(http.MethodPost,
			"/api/files/"+url.PathEscape(uf.ID)+"/abort", nil); err != nil {
			fmt.Fprintf(os.Stderr, "（顺带取消上传也失败了，可稍后手动 ama files abort %s：%v）\n",
				uf.ID, err)
		}
	}
	return cause
}

func (c *client) filesGet(args []string) error {
	id, _ := shiftPositional(args)
	if id == "" {
		return errors.New("要文件 id：ama files get <id>。用 `ama files list` 查")
	}
	return c.filesGetByID(id)
}

func (c *client) filesGetByID(id string) error {
	raw, err := c.do(http.MethodGet, "/api/files/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return printJSON(raw)
}

// filesURL prints only the download URL (no other output), handy for `curl $(ama files url uf_x)`.
func (c *client) filesURL(args []string) error {
	id, _ := shiftPositional(args)
	if id == "" {
		return errors.New("要文件 id：ama files url <id>")
	}
	raw, err := c.do(http.MethodGet, "/api/files/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	var uf cliUserFile
	if err := json.Unmarshal(raw, &uf); err != nil {
		return err
	}
	if uf.DownloadURL == "" {
		return errors.New("服务端没返回下载 URL——文件可能还没传完（status=uploading）")
	}
	// ⚠️ fmt.Print rather than printJSON: the output of this command is a URL meant to be consumed
	// directly by the shell, and any extra text would break `curl $(ama files url ...)`.
	fmt.Println(uf.DownloadURL)
	return nil
}

// filesDownload downloads to a local file relayed through AMA (see "downloads are relayed through AMA" in the file header).
func (c *client) filesDownload(args []string) error {
	id, rest := shiftPositional(args)
	if id == "" {
		return errors.New("要文件 id：ama files download <id> [本地路径]")
	}
	raw, err := c.do(http.MethodGet, "/api/files/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	var uf cliUserFile
	if err := json.Unmarshal(raw, &uf); err != nil {
		return err
	}
	if uf.Status != "" && uf.Status != "completed" {
		return errors.New("文件还没传完（status=" + uf.Status + "），不能下载")
	}

	dest, _ := shiftPositional(rest)
	if dest == "" {
		// The server allows slashes in file names; when saving to disk only take the last segment, so it cannot be written outside the target directory.
		dest = filepath.Base(uf.Filename)
		if dest == "" || dest == "." || dest == string(filepath.Separator) {
			dest = uf.ID
		}
	}

	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/api/files/"+url.PathEscape(id)+"/content", nil)
	if err != nil {
		return err
	}
	// This calls AMA itself, so sending the token is right (unlike a third-party address like downloadUrl).
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpLong.Do(req)
	if err != nil {
		return fmt.Errorf("连不上 AMA（%s）：%w\n这是网络问题，不是权限问题", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return describeHTTPError(resp.StatusCode, b)
	}

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("创建本地文件 %s 失败：%w", dest, err)
	}
	n, err := io.Copy(out, resp.Body)
	if err != nil {
		out.Close()
		os.Remove(dest)
		return fmt.Errorf("写入 %s 失败：%w", dest, err)
	}
	if err := out.Close(); err != nil {
		return err
	}
	// curl leaves an incomplete file and exits 0 when the connection breaks; here it is checked against Content-Length ourselves.
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		os.Remove(dest)
		return fmt.Errorf("下载不完整：收到 %d 字节，应为 %d", n, resp.ContentLength)
	}
	fmt.Fprintf(os.Stderr, "已下载 %s（%s）\n", dest, humanSize(n))
	return nil
}

func (c *client) filesDelete(args []string) error {
	id, _ := shiftPositional(args)
	if id == "" {
		return errors.New("要文件 id：ama files delete <id> --yes")
	}
	if !hasConfirm(args) {
		// The same gate as ama api's write operations: explain it to the user first, then --yes.
		return fmt.Errorf("这会永久删除文件 %s（对象与记录都删，不可恢复）。确认后加 --yes 重跑", id)
	}
	if _, err := c.do(http.MethodDelete, "/api/files/"+url.PathEscape(id), nil); err != nil {
		return err
	}
	fmt.Printf("已删除 %s\n", id)
	return nil
}

func (c *client) filesAbort(args []string) error {
	id, _ := shiftPositional(args)
	if id == "" {
		return errors.New("要文件 id：ama files abort <id>")
	}
	if _, err := c.do(http.MethodPost,
		"/api/files/"+url.PathEscape(id)+"/abort", nil); err != nil {
		return err
	}
	fmt.Printf("已取消上传 %s\n", id)
	return nil
}

// humanSize is for file progress: below 1 KiB it shows bytes (the existing humanBytes always returns
// "0 KiB" for small files, so uploading a 17-byte file would look like uploading an empty file),
// otherwise KiB/MiB/GiB.
func humanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	const unit = 1024
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
