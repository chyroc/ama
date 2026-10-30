// WebSocket probe endpoint: checks "can the browser reach amad's WebSocket directly through the
// deployment's ingress gateway".
//
// # Why it is needed
//
// If the webshell terminal uplink is "one POST per keystroke", it is slow and may arrive out of
// order; the natural fix is to switch to WebSocket. But whether WS works depends on **the ingress
// gateway**, not amad itself:
//
//   - If the gateway and the browser negotiate **HTTP/2**, the `Connection: Upgrade` mechanism does
//     not exist (RFC 7540 forbids hop-by-hop headers); the gateway drops the upgrade headers, and
//     what reaches amad is no longer a valid handshake;
//   - So the usual approach is to give WS its own ingress that only uses HTTP/1.1 (a separate
//     domain or a separate routing rule) and forward traffic with the `/websocket` prefix to this
//     service.
//
// ⚠️ **Many gateways disconnect after ~30 seconds idle.** amad's own `terminalIdleTimeout` is 30
// **minutes**; the gateway's limit is the real cap. To run the terminal over this path, **the
// frontend must send its own heartbeat at <28s**; otherwise a user staring at the screen without
// typing for half a minute gets disconnected, silently with an EOF and no error.
//
// # ⚠️ When debugging, the check must use a path inside the prefix
//
// Hitting the WS ingress with paths **outside the prefix** like `/healthz` or `/api/openapi.json`
// gives 404s that mean "the gateway has no such route", which says nothing about whether the
// backend is reachable.
//
// A valid check is `/websocket/x`: it is inside the prefix, while this package's mux registers the
// exact `GET /websocket` and isBackendPath also matches exactly, so it falls to the SPA fallback →
// **200 + an HTML page + a logid** (the withLogID middleware stamps it unconditionally; "no logid"
// = the request never entered amad), a fingerprint nothing else can fake.
//
// # Endpoint behavior: echo
//
// The probe must answer "does the handshake succeed, does sending and receiving work both ways", so
// echo is the simplest with a clear check: it sends back whatever it receives, and if the client
// can compare the content, the whole path works. **It touches no business data**, so it needs no
// authentication either (see the comment at Register below).
package httpd

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// PathWebSocketProbe is the path of the probe endpoint.
//
// ⚠️ **It must match the gateway's forwarding rule** (traffic starting with `/websocket` on the WS
// ingress is forwarded to this service). This literal is also needed in isBackendPath, so it is
// defined as a constant instead of written in two places: with one wrong letter the request falls
// to the SPA branch and gets 200 HTML, and the gateway may turn that into another error code, a
// symptom identical to "never implemented".
const PathWebSocketProbe = "/websocket"

// wsProbeUpgrader upgrades the HTTP connection to WebSocket.
//
// ⚠️ **CheckOrigin is open**: this is a pure echo probe that touches no business data and knows no
// identity, and it must be reachable cross-origin: when the page is on the main domain and the WS
// connects to a separate WS domain, the Origin is bound to differ. gorilla's default CheckOrigin
// requires same-origin; keeping the default would make our own side block the handshake, which
// then looks like "the gateway does not support WS". **That is exactly the conclusion this is
// meant to test, faked by our own default.**
//
// This opening only holds for this endpoint. To really move the terminal to WS, that endpoint must
// require authentication and check the Origin.
var wsProbeUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// wsProbeIdleTimeout is the idle limit of a probe connection.
//
// 2 minutes: enough to connect by hand and send a few messages, without letting a forgotten page
// keep the connection all day. A probe endpoint should not have the 30-minute patience of a
// production endpoint.
const wsProbeIdleTimeout = 2 * time.Minute

// registerWebSocketProbe mounts the probe endpoint.
//
// ⚠️ **Registered on the bare mux, not through authorized**. Two reasons:
//
//  1. The probe must answer "does the gateway let the WS handshake through"; an authentication
//     failure returns 401 and **the handshake never happens**, so a 401 cannot tell "the gateway
//     swallowed the upgrade headers" from "we did not let it through", and the test would prove
//     nothing. Same reason the two openapi routes use the bare mux: you must be able to ask before
//     you can get an answer.
//  2. It has no Store, knows no workspace, and only sends back the bytes it receives; there is
//     nothing to leak.
//
// For the same reason it **must not** be written into authz/matrix.go: that would trigger the
// reverse panic "declared but not registered".
func registerWebSocketProbe(mux *http.ServeMux, log *slog.Logger) {
	mux.HandleFunc("GET "+PathWebSocketProbe, func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsProbeUpgrader.Upgrade(w, r, nil)
		if err != nil {
			// When Upgrade fails, gorilla has already written the response; only log here.
			// **This log line is half of the probe**: if production gets no 101, we must be able to
			// tell whether the gateway did not deliver it (nothing in the log) or delivered it but
			// the handshake was invalid (this line is in the log).
			log.Warn("WebSocket 探路握手失败",
				slog.String("err", err.Error()),
				slog.String("connection", r.Header.Get("Connection")),
				slog.String("upgrade", r.Header.Get("Upgrade")),
				slog.String("proto", r.Proto))
			return
		}
		defer func() { _ = conn.Close() }()

		log.Info("WebSocket 探路已连接",
			slog.String("proto", r.Proto),
			slog.String("origin", r.Header.Get("Origin")),
			slog.String("remote", r.RemoteAddr))

		// Say something first right after connecting, **without waiting for the client to send**.
		//
		// That way "handshake succeeded" and "the server can send data down" each have their own
		// evidence: seeing onopen in the browser only shows the 101 arrived; any hop buffering the
		// downlink would go unnoticed. Receiving this hello proves the downlink works.
		greeting := "hello from amad, proto=" + r.Proto
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(greeting)); err != nil {
			log.Warn("WebSocket 探路首帧发送失败", slog.String("err", err.Error()))
			return
		}

		for {
			_ = conn.SetReadDeadline(time.Now().Add(wsProbeIdleTimeout))
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				// Normal close and abnormal disconnect both land here. **Not told apart**: the probe
				// only cares "how long can it send and receive once connected", and that is already
				// in the time difference between the Info line above and this one.
				log.Info("WebSocket 探路连接结束", slog.String("err", err.Error()))
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(mt, msg); err != nil {
				log.Warn("WebSocket 探路回显失败", slog.String("err", err.Error()))
				return
			}
		}
	})
}
