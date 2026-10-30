// Wiring assertions for the WebSocket probe endpoint.
//
// # Two "zero-error" wiring traps are pinned here
//
// The endpoint itself (echo) has little to test; what really goes wrong is whether it is wired into
// routing correctly:
//
//  1. **Not registered on the mux** → the request falls to the SPA branch and gets 200 + an HTML
//     page. On the browser side the symptom is a failed handshake, while curl gets 200, which looks
//     like "a gateway problem".
//  2. **Missing from isBackendPath** → same as above, exactly the same symptom. The two traps are
//     independent; fixing one and missing the other gives exactly the same result as fixing neither.
//
// In production it is harder to track down: the gateway expects 101, gets 200 HTML, and returns
// **500**, so the symptom becomes "implemented but the gateway does not support WebSocket", a
// completely wrong conclusion, and it sends people to the gateway team while the problem is two
// lines of code on our side.
//
// So this does not test the echo content; it tests "does a valid handshake request get 101".

package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestWebSocketProbeUpgrades checks that the probe endpoint really upgrades the protocol and can
// send and receive both ways.
//
// **The check is 101 + one real round trip, not "not 404".** Asserting only "not 404" would be
// satisfied by the SPA branch's 200, which is exactly what happens when isBackendPath is missing,
// so it would test something always true.
//
// ⚠️ **Must use httptest.NewServer, not httptest.NewRecorder.** The recorder does not implement
// http.Hijacker, so Upgrade always fails and returns 500; that red comes from the test fixture and
// has nothing to do with the code under test, and changing production code to follow it would only
// break things. Same as noted in TestStatusRecorderSupportsHijack.
func TestWebSocketProbeUpgrades(t *testing.T) {
	srv := httptest.NewServer(newRBACServer(t))
	defer srv.Close()

	// **Deliberately no credentials.** The probe endpoint must not require authentication:
	// authentication would stop the handshake from happening at all, so a 401 in production could
	// not tell whether the gateway swallowed the upgrade headers or we blocked it ourselves.
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + PathWebSocketProbe
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("握手 %s 失败（HTTP %d）：%v\n"+
			"200 = 请求落到了 SPA（没注册进 mux，或漏加进 isBackendPath）——"+
			"线上这会被 Frontier 变成 500，症状像「网关不支持 WS」但根因在这两行。\n"+
			"401 = 被挂到了 authorized 上，握手根本没发生。",
			PathWebSocketProbe, code, err)
	}
	defer func() { _ = conn.Close() }()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("握手返回 %d，期望 101", resp.StatusCode)
	}

	// The first frame is a greeting sent by the server on its own. **This one proves the downlink
	// works**: a 101 alone only means the handshake succeeded, not that data can flow down (any hop
	// buffering it would go unnoticed).
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, greeting, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("没收到服务端主动发的首帧：%v —— 握手成了但下行不通", err)
	}
	if !strings.Contains(string(greeting), "hello from amad") {
		t.Errorf("首帧内容是 %q，期望含 \"hello from amad\"", greeting)
	}

	// Then check uplink + echo; only then is the whole two-way path working.
	const payload = "ping-12345"
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("上行发送失败：%v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, echoed, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("没收到回显：%v", err)
	}
	if string(echoed) != payload {
		t.Errorf("回显是 %q，期望 %q", echoed, payload)
	}
}

// TestWebSocketProbeIsBackendPath pins the routing split step on its own.
//
// It is **not redundant** with the one above: this one uses a plain GET (no upgrade headers) and
// checks "this path is assigned to the backend"; the one above checks "the backend really
// upgrades". The two can break separately and look the same when broken; merged into one test you
// could not tell which step went wrong.
//
// Without upgrade headers gorilla returns 400 ("Bad Request"), **which is the right answer**: it
// proves the request reached the WS handler. 200 means it fell to the SPA.
//
// This one can use a recorder: it does not need Hijack (it never gets to the upgrade step).
func TestWebSocketProbeIsBackendPath(t *testing.T) {
	if !isBackendPath(PathWebSocketProbe) {
		t.Fatalf("isBackendPath(%q) 是 false —— 请求会落到 SPA 拿到 200 HTML，"+
			"而网关期待 101，于是线上回 500。见 wsprobe.go 的文件头。",
			PathWebSocketProbe)
	}

	srv := newRBACServer(t)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathWebSocketProbe, nil))

	if rec.Code == http.StatusOK {
		t.Fatalf("不带升级头访问 %s 得到 200 —— 说明它落到了 SPA 那一支。"+
			"期望是 400（gorilla 拒绝一个不合法的握手，证明请求到了 WS handler）。",
			PathWebSocketProbe)
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("%s 回了 HTML（Content-Type=%q）—— 落到 SPA 了。", PathWebSocketProbe, ct)
	}
}
