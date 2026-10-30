// Auth and frame protocol tests for the host daemon WebSocket endpoint.
//
// ⚠️ WS tests must use httptest.NewServer: httptest.ResponseRecorder does not implement
// Hijacker, so Upgrade always fails (already hit in httpd/wsprobe_test.go).
//
// These tests cover the security boundary lost because the WS endpoint bypasses the authz matrix
// gate: host_token auth during the handshake, Origin rejection, first-frame rules, hostId
// overwrite and ack meaning for results, and the close codes for revoke / kicking the old
// connection.
package apihandler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/gorilla/websocket"
)

// dialHostDaemonWS starts the test server and dials it; with wantErr=true it expects the
// handshake to fail and returns the HTTP response so the status code can be checked.
func dialHostDaemonWS(t *testing.T, daemon HostDaemonServices, header http.Header,
) (*websocket.Conn, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(newHostDaemonHandler(t, daemon))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/host-daemon/ws"
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(url, header)
	if err != nil {
		t.Fatalf("WS 拨号失败: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, srv
}

func dialHostDaemonWSExpectReject(t *testing.T, daemon HostDaemonServices, header http.Header,
) int {
	t.Helper()
	srv := httptest.NewServer(newHostDaemonHandler(t, daemon))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/host-daemon/ws"
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	_, resp, err := dialer.Dial(url, header)
	if err == nil {
		t.Fatal("期望握手被拒，实际成功")
	}
	if resp == nil {
		t.Fatalf("握手失败但没有 HTTP 响应: %v", err)
	}
	return resp.StatusCode
}

func authHeader(token string) http.Header {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return h
}

func helloFrame(lastSeq int64) hostproto.WSFrame {
	return hostproto.WSFrame{
		Type:  hostproto.WSFrameHello,
		V:     hostproto.WSProtoVersion,
		Hello: &hostproto.HeartbeatRequest{LastSeq: lastSeq, Name: "test-mac", OS: "darwin", Arch: "arm64"},
	}
}

func writeFrame(t *testing.T, conn *websocket.Conn, frame hostproto.WSFrame) {
	t.Helper()
	if err := conn.WriteJSON(frame); err != nil {
		t.Fatalf("写帧失败: %v", err)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn) hostproto.WSFrame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var frame hostproto.WSFrame
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	return frame
}

// expectClose asserts that the next read event is a close frame with the given application code.
func expectClose(t *testing.T, conn *websocket.Conn, code int) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	ce, ok := err.(*websocket.CloseError)
	if !ok {
		t.Fatalf("期望 close %d，实际读错误: %v", code, err)
	}
	if ce.Code != code {
		t.Fatalf("close code = %d，期望 %d", ce.Code, code)
	}
}

func TestHostDaemonWSHandshakeAuth(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a"}
	if got := dialHostDaemonWSExpectReject(t, daemon, nil); got != http.StatusUnauthorized {
		t.Fatalf("无令牌握手应 401，实际 %d", got)
	}
	if got := dialHostDaemonWSExpectReject(t, daemon, authHeader("nope")); got != http.StatusUnauthorized {
		t.Fatalf("错令牌握手应 401，实际 %d", got)
	}
}

func TestHostDaemonWSRejectsOrigin(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a"}
	h := authHeader("good-token")
	h.Set("Origin", "https://evil.example.com")
	// When CheckOrigin rejects, gorilla returns 403.
	if got := dialHostDaemonWSExpectReject(t, daemon, h); got != http.StatusForbidden {
		t.Fatalf("带 Origin 的握手应 403，实际 %d", got)
	}
}

func TestHostDaemonWSHelloAck(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a"}
	conn, _ := dialHostDaemonWS(t, daemon, authHeader("good-token"))
	writeFrame(t, conn, helloFrame(41))

	ack := readFrame(t, conn)
	if ack.Type != hostproto.WSFrameHelloAck {
		t.Fatalf("首帧应答类型 = %q，期望 hello_ack", ack.Type)
	}
	if len(ack.Calls) != 1 || ack.Calls[0].ID != "htc_ws1" {
		t.Fatalf("hello_ack 内容不对: %+v", ack.Calls)
	}
	if len(daemon.beginCalls) != 1 || daemon.beginCalls[0].LastSeq != 41 {
		t.Fatalf("BeginStream 收到的游标不对: %+v", daemon.beginCalls)
	}
}

func TestHostDaemonWSResultsAckAndHostIDOverride(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a"}
	conn, _ := dialHostDaemonWS(t, daemon, authHeader("good-token"))
	writeFrame(t, conn, helloFrame(0))
	_ = readFrame(t, conn) // hello_ack

	// The body claims a different host; the server must overwrite it with the token's host.
	writeFrame(t, conn, hostproto.WSFrame{
		Type: hostproto.WSFrameResults, V: hostproto.WSProtoVersion, ID: 7,
		Results: &hostproto.ResultsRequest{
			HostID:      "hst_OTHER",
			ExecResults: []hostproto.ExecResultItem{{CallID: "htc_x", Result: hostproto.ExecResult{ExitCode: intPtr(0)}}},
		},
	})
	ack := readFrame(t, conn)
	if ack.Type != hostproto.WSFrameResultsAck || ack.ID != 7 || !ack.OK {
		t.Fatalf("results 应答不对: %+v", ack)
	}
	if len(daemon.results) != 1 || daemon.results[0].CallID != "htc_x" {
		t.Fatalf("应收到 1 条结果，实际 %+v", daemon.results)
	}
}

// When the server fails to handle results it replies ok=false but keeps the connection: the next
// message is still received.
func TestHostDaemonWSResultsErrorKeepsConnection(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a", resultsErr: persistmodel.ErrNotFound}
	conn, _ := dialHostDaemonWS(t, daemon, authHeader("good-token"))
	writeFrame(t, conn, helloFrame(0))
	_ = readFrame(t, conn)

	writeFrame(t, conn, hostproto.WSFrame{
		Type: hostproto.WSFrameResults, V: hostproto.WSProtoVersion, ID: 1,
		Results: &hostproto.ResultsRequest{
			ExecResults: []hostproto.ExecResultItem{{CallID: "htc_y"}},
		},
	})
	ack := readFrame(t, conn)
	if ack.Type != hostproto.WSFrameResultsAck || ack.OK || ack.Error == "" {
		t.Fatalf("失败应答应为 ok=false 且带错误说明: %+v", ack)
	}

	// The connection is still alive: another hello frame still gets a reply.
	writeFrame(t, conn, helloFrame(1))
	ack2 := readFrame(t, conn)
	if ack2.Type != hostproto.WSFrameHelloAck {
		t.Fatalf("失败应答后连接应保持，实际收到: %+v", ack2)
	}
}

func TestHostDaemonWSBadFirstFrame(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a"}
	conn, _ := dialHostDaemonWS(t, daemon, authHeader("good-token"))
	// The first frame is not hello.
	_ = conn.WriteJSON(hostproto.WSFrame{Type: hostproto.WSFrameResults, V: 1})
	expectClose(t, conn, hostproto.WSCloseBadProtocol)
}

// The host was revoked in the window after the handshake and before the first frame: revoked
// frame + close 4001.
func TestHostDaemonWSRevokedOnBegin(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a", beginErr: persistmodel.ErrNotFound}
	conn, _ := dialHostDaemonWS(t, daemon, authHeader("good-token"))
	writeFrame(t, conn, helloFrame(0))
	frame := readFrame(t, conn)
	if frame.Type != hostproto.WSFrameRevoked {
		t.Fatalf("应先收 revoked 帧，实际: %+v", frame)
	}
	expectClose(t, conn, hostproto.WSCloseRevoked)
}

// After a newer connection for the same host is set up, the old connection gets close 4002 right
// after it receives hello_ack.
func TestHostDaemonWSReplacedClose(t *testing.T) {
	daemon := &fakeHostDaemon{authedHostID: "hst_a", replacedStream: true}
	conn, _ := dialHostDaemonWS(t, daemon, authHeader("good-token"))
	writeFrame(t, conn, helloFrame(0))
	if frame := readFrame(t, conn); frame.Type != hostproto.WSFrameHelloAck {
		t.Fatalf("先应收到 hello_ack，实际 %q", frame.Type)
	}
	expectClose(t, conn, hostproto.WSCloseReplaced)
}

func intPtr(i int) *int { return &i }
