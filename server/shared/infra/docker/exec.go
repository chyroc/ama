package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ExecConfig is the input of exec create.
type ExecConfig struct {
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
	User         string   `json:"User,omitempty"`
	Tty          bool     `json:"Tty,omitempty"`
	AttachStdin  bool     `json:"AttachStdin,omitempty"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
}

// ExecCreate registers a command to run in the container and returns the exec id.
//
// The daemon answers 409 while the container is paused (container is paused), and 404 if it does not exist.
func (c *Client) ExecCreate(ctx context.Context, container string, cfg ExecConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.doJSON(ctx, "建 exec", http.MethodPost,
		"/containers/"+url.PathEscape(container)+"/exec", nil, cfg, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ExecInspectResult holds the fields of exec inspect that we use.
type ExecInspectResult struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
	Pid      int  `json:"Pid"`
}

// ExecInspect looks up an exec's state and exit code.
func (c *Client) ExecInspect(ctx context.Context, id string) (*ExecInspectResult, error) {
	var out ExecInspectResult
	if err := c.doJSON(ctx, "查 exec", http.MethodGet,
		"/exec/"+url.PathEscape(id)+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExecResize changes the TTY window size (only meaningful for exec with Tty=true).
func (c *Client) ExecResize(ctx context.Context, id string, rows, cols int) error {
	q := url.Values{"h": {strconv.Itoa(rows)}, "w": {strconv.Itoa(cols)}}
	return c.doJSON(ctx, "调整终端大小", http.MethodPost,
		"/exec/"+url.PathEscape(id)+"/resize", q, nil, nil)
}

// ExecStartAttached starts an exec and gets a raw two-way connection (hijack).
//
// With tty=false, reads are multiplexed frames (split them with Demux); with tty=true, raw bytes.
func (c *Client) ExecStartAttached(ctx context.Context, id string, tty bool) (*HijackedConn, error) {
	return c.hijack(ctx, "启动 exec", "/exec/"+url.PathEscape(id)+"/start",
		map[string]any{"Detach": false, "Tty": tty})
}

// execStartStream starts an exec that needs no stdin and returns the output stream (multiplexed frames).
//
// It does not use hijack: for a request without the Upgrade header the daemon still writes the
// output into the response body,
// so a plain HTTP client reading to EOF is enough, and we save one self-built connection.
func (c *Client) execStartStream(ctx context.Context, id string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, "启动 exec", http.MethodPost, "/exec/"+url.PathEscape(id)+"/start",
		nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ---------------------------------------------------------------------------
// Multiplexed frames
// ---------------------------------------------------------------------------

// Non-TTY exec / attach output is a series of frames:
//
//	[1 byte stream type][3 bytes 0][4 bytes big-endian length][length bytes of payload]
//
// Stream type 0=stdin (never appears), 1=stdout, 2=stderr. The daemon interleaves both outputs on
// the same stream, so it must be split frame by frame, not read as one block of text.
const (
	streamStdin  = 0
	streamStdout = 1
	streamStderr = 2
)

// ErrOutputTooLarge means one of the outputs went over the Demux limit; the excess was dropped.
var ErrOutputTooLarge = errors.New("docker: 命令输出超过上限，超出部分已丢弃")

// Demux splits a multiplexed stream into stdout / stderr, keeping at most limit bytes of each (<=0 means no limit).
//
// When over the limit it still reads the stream to the end (otherwise the daemon side gets stuck
// on back pressure), returns ErrOutputTooLarge,
// and the kept part is written to stdout / stderr as usual.
func Demux(r io.Reader, stdout, stderr io.Writer, limit int64) error {
	var hdr [8]byte
	var written [3]int64
	truncated := false
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("docker: 输出流在帧头中途断开: %w", err)
			}
			return err
		}
		kind := hdr[0]
		size := int64(binary.BigEndian.Uint32(hdr[4:8]))
		var dst io.Writer
		switch kind {
		case streamStdout:
			dst = stdout
		case streamStderr:
			dst = stderr
		case streamStdin:
			dst = io.Discard
		default:
			return fmt.Errorf("docker: 输出流里出现未知的流类型 %d（是不是把 TTY 输出当成了多路复用帧）", kind)
		}
		if dst == nil {
			dst = io.Discard
		}
		keep := size
		if limit > 0 && kind <= streamStderr {
			left := limit - written[kind]
			if left < 0 {
				left = 0
			}
			if keep > left {
				keep = left
				truncated = true
			}
		}
		if keep > 0 {
			n, err := io.CopyN(dst, r, keep)
			written[kind] += n
			if err != nil {
				return fmt.Errorf("docker: 读输出帧失败: %w", err)
			}
		}
		if rest := size - keep; rest > 0 {
			if _, err := io.CopyN(io.Discard, r, rest); err != nil {
				return fmt.Errorf("docker: 读输出帧失败: %w", err)
			}
		}
	}
	if truncated {
		return ErrOutputTooLarge
	}
	return nil
}

// ---------------------------------------------------------------------------
// One-shot execution
// ---------------------------------------------------------------------------

// RunOptions is the input of Run.
type RunOptions struct {
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
	// Stdin, when non-nil, is written to the process and closed after writing (the process sees EOF).
	Stdin []byte
	// OutputLimit is the max bytes kept for each of stdout / stderr; <=0 means no limit.
	OutputLimit int64
}

// RunResult is the result of Run.
type RunResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	// Truncated means the output went over OutputLimit and was cut.
	Truncated bool
}

// Run runs a command in the container, waits for it to finish, and returns the exit code and output.
//
// Timeouts are fully up to ctx: when ctx ends the connection is closed and this returns right
// away, but **the process in the container is not
// killed** by that (the Engine API has no "kill an exec" endpoint). To make the process die on
// time, the caller should
// wrap it with a command like `timeout`; see exec in dockertools.
func (c *Client) Run(ctx context.Context, container string, opt RunOptions) (*RunResult, error) {
	id, err := c.ExecCreate(ctx, container, ExecConfig{
		Cmd:          opt.Cmd,
		Env:          opt.Env,
		WorkingDir:   opt.WorkingDir,
		User:         opt.User,
		AttachStdin:  opt.Stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, err
	}

	var stream io.ReadCloser
	if opt.Stdin != nil {
		conn, err := c.ExecStartAttached(ctx, id, false)
		if err != nil {
			return nil, err
		}
		stream = conn
		// Write stdin in another goroutine: the process may read and write at the same time, and a
		// synchronous write that fills the buffer would deadlock both sides.
		go func() {
			_, _ = io.Copy(conn, bytes.NewReader(opt.Stdin))
			_ = conn.CloseWrite()
		}()
	} else {
		stream, err = c.execStartStream(ctx, id)
		if err != nil {
			return nil, err
		}
	}

	// When ctx is canceled, close the stream ourselves so Demux returns right away instead of waiting for the daemon.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = stream.Close()
		case <-done:
		}
	}()

	var stdout, stderr bytes.Buffer
	derr := Demux(stream, &stdout, &stderr, opt.OutputLimit)
	close(done)
	_ = stream.Close()

	res := &RunResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if derr != nil {
		if errors.Is(derr, ErrOutputTooLarge) {
			res.Truncated = true
		} else {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("docker: 等待命令结束时被中断: %w", ctx.Err())
			}
			return nil, derr
		}
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("docker: 等待命令结束时被中断: %w", ctx.Err())
	}

	// When the output stream hits EOF the process has exited, but there is a small window between
	// the daemon recording the exit code and closing the stream,
	// during which inspect still returns Running=true, ExitCode=0. Using that directly would report
	// a failed command as success.
	// So retry a few times while Running.
	for attempt := 0; ; attempt++ {
		info, err := c.ExecInspect(ctx, id)
		if err != nil {
			return nil, err
		}
		if !info.Running || attempt >= 20 {
			res.ExitCode = info.ExitCode
			return res, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("docker: 等待命令结束时被中断: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
