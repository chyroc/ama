// Routes for the session container (Docker): view status, resume, list files.
//
// For the domain notes and "why four states", see the file header of core/sandbox.go.
// Only the HTTP layer lives here: read params, translate errors, write responses.
package apihandler

import (
	"encoding/json"
	"net/http"
	"time"
)

// terminalTicketTTL is how long one terminal ticket is valid. **It must match the constant of the
// same name in core/terminalticket.go**: core signs the ticket; here we only convert it to seconds for the frontend.
//
// ⚠️ It is copied instead of imported from core: this package does not import core (that was the whole point of this move).
// And it was not moved into domain, because the whole sign-and-redeem logic is still in core, and the constant goes with the logic;
// only "how many seconds to tell the frontend" is in the HTTP layer.
//
// If the two sides disagree, **the effect is mild**, which is why copying is OK: the frontend only uses it to decide "should I
// get a new ticket early", and real expiry is decided by the server when it redeems the ticket. If the copy is too large, the frontend
// refetches a bit later
// (that handshake is rejected; just fetch again). If it is too small, we only request a few extra tickets.
// If the TTL ever becomes a config item, the right way is to have the signing endpoint return it together with the ticket, not
// to configure it in two places.
const terminalTicketTTL = 30 * time.Second

// registerSandboxRoutes mounts the container routes.
//
// All of them live under /api/runs/{runId}/sandbox: the container belongs to one run, it is not a separate entity
// (same call as /trace, which is also mounted under run).
//
// ⚠️ Each one must be declared in authz/matrix.go. **Miss one and startup panics.**
func (h *Handler) registerSandboxRoutes(mux Mux) {
	// Current container state. Four states: none / running / paused / gone.
	mux.HandleFunc("GET /api/runs/{runId}/sandbox", h.getRunSandbox)
	// Explicitly resume a paused container.
	//
	// **Its own route, instead of letting the status query resume automatically**: resuming makes the container use
	// the host's CPU and memory again. Letting a read-only "take a look at the status" cause that cost,
	// without the user knowing what they triggered, is wrong. So it takes a click.
	mux.HandleFunc("POST /api/runs/{runId}/sandbox/resume", h.resumeRunSandbox)

	// List files in the container. **Read-only**: this feature offers no delete/overwrite/upload.
	mux.HandleFunc("GET /api/runs/{runId}/sandbox/files", h.listRunSandboxFiles)

	// View a file's content. **Read-only, text only**: binary returns binary:true with no content.
	mux.HandleFunc("GET /api/runs/{runId}/sandbox/file", h.readRunSandboxFile)

	// webshell: run one command in the container. **ActionManage**, see the reasoning in authz/matrix.go
	// (the container holds all of the agent's output; changing it means changing the agent's execution environment).
	mux.HandleFunc("POST /api/runs/{runId}/sandbox/shell", h.runSandboxShell)

	// Terminal: WebSocket, proxied to a real PTY in the container. Also **ActionManage**.
	//
	// Note it is **GET**: a WebSocket handshake is a GET with an Upgrade header;
	// written as POST, the browser could not send it at all.
	mux.HandleFunc("GET /api/runs/{runId}/sandbox/terminal", h.streamSandboxTerminal)
	// Terminal upstream: send keystrokes into the container.
	mux.HandleFunc("POST /api/runs/{runId}/sandbox/terminal/input", h.writeSandboxTerminal)

	// Sign a one-time ticket for opening the terminal WS.
	//
	// **This one is a normal same-origin endpoint** (carries the Cookie, goes through authz for ActionManage),
	// and only it can sign tickets. The real WS endpoint is on another domain, registered on the bare mux,
	// and authenticates by redeeming this ticket. For why this detour is required, see the file header of core/terminalticket.go
	// (in short: the Cookie is host-only and is not sent to the WS-only domain, and WS does not support custom request headers).
	mux.HandleFunc("POST /api/runs/{runId}/sandbox/terminal/ticket", h.issueTerminalTicket)
}

// issueTerminalTicket signs a one-time ticket for opening this run's terminal.
//
// The response on purpose **has only the ticket value and its lifetime**, no WS address: the frontend builds that address from config
// (VITE_TERMINAL_WS_BASE), because it is part of the deployment setup, not a property of this request.
func (h *Handler) issueTerminalTicket(w http.ResponseWriter, r *http.Request) {
	ticket, err := h.sandbox.IssueTerminalTicket(r.Context(), r.PathValue("runId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticket": ticket,
		// Seconds, so the frontend can decide "should I request a new one". **No absolute time**:
		// that would need both clocks to agree, and the frontend clock is the user's machine clock.
		"expiresInSeconds": int(terminalTicketTTL.Seconds()),
	})
}

// streamSandboxTerminal pushes the container terminal's output to the browser over SSE.
//
// **It is SSE, not WebSocket**: the production gateway uses HTTP/2, and WS cannot run over h2
// (the upgrade header is dropped, and the server sees an invalid handshake). For the reason see core/sandboxterm.go.
//
// ⚠️ **Do not call writeJSON here**: once streaming starts, the response body is SSE frames,
// and writing JSON would break the format. The Service side only returns an error when it fails **before the stream opens**.
func (h *Handler) streamSandboxTerminal(w http.ResponseWriter, r *http.Request) {
	if err := h.sandbox.StreamRunSandboxTerminal(w, r, r.PathValue("runId")); err != nil {
		h.fail(w, r, err)
	}
}

// writeSandboxTerminal sends one browser input frame into the container.
//
// The body is the JSON frame to forward to the container **as is** (such as `{"type":"input","data":"ls"}`),
// wrapped in {terminalId, frame}. The frame is not parsed: the protocol is defined by the platform.
func (h *Handler) writeSandboxTerminal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TerminalID string          `json:"terminalId"`
		Frame      json.RawMessage `json:"frame"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	// runId comes from the path (this route already has it). It is passed down to renew "the terminal is still in use":
	// that is tracked per Run, while terminalId is only an in-process handle that other instances do not recognize.
	if err := h.sandbox.WriteRunSandboxTerminal(
		r.Context(), r.PathValue("runId"), body.TerminalID, body.Frame,
	); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readRunSandboxFile reads the content of a file in the container, for the file detail view on the page.
func (h *Handler) readRunSandboxFile(w http.ResponseWriter, r *http.Request) {
	out, err := h.sandbox.ReadRunSandboxFile(
		r.Context(), r.PathValue("runId"), r.URL.Query().Get("path"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// runSandboxShell runs one webshell command in the container.
//
// One command per request (not a long-lived connection): dockertools stores cwd by shellID in a small file **inside the container**,
// so "feels like a terminal" needs no WebSocket, and two adjacent commands landing on different pods is fine.
func (h *Handler) runSandboxShell(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	res, err := h.sandbox.RunSandboxShell(r.Context(), r.PathValue("runId"), body.Command)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// listRunSandboxFiles lists one directory in the container.
//
// path comes from the query and defaults to the root. When the container is not running it returns 200 + empty list + current state,
// **not an error**: that is not a failure, and the frontend shows guidance by status, such as "Paused, click here to resume".
func (h *Handler) listRunSandboxFiles(w http.ResponseWriter, r *http.Request) {
	listing, err := h.sandbox.ListRunSandboxFiles(
		r.Context(), r.PathValue("runId"), r.URL.Query().Get("path"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

// getRunSandbox returns the container's current state.
//
// **The state is looked up live** (one container inspect), not read from a cache. For why, see the core/sandbox.go file header:
// containers pause on their own and get reclaimed on their own, so a stored snapshot will end up saying "running" when it is long gone.
func (h *Handler) getRunSandbox(w http.ResponseWriter, r *http.Request) {
	state, err := h.sandbox.RunSandboxState(r.Context(), r.PathValue("runId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// resumeRunSandbox resumes a paused container and returns the state after resuming.
//
// Resuming has to bring the container back up (**on the order of ten-plus seconds**), so this endpoint is slow by nature. The frontend must show
// waiting feedback,
// so the user does not think the click did nothing and click again.
func (h *Handler) resumeRunSandbox(w http.ResponseWriter, r *http.Request) {
	state, err := h.sandbox.ResumeRunSandbox(r.Context(), r.PathValue("runId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// RegisterTerminalWS registers the terminal WebSocket endpoint on its own.
//
// # Why it is not in registerSandboxRoutes
//
// That function takes a `Mux`, and the caller passes in a mux **wrapped with authentication and authorization**
// (the two decorator layers in httpd). This route must not go through those two layers:
//
//   - It connects to another domain, and that connection carries no Cookie (ama_session is host-only),
//     and browser WebSocket does not support custom request headers, so the auth middleware would always see it as not logged in,
//     the handshake would end at 401, and the browser would only see "connection failed".
//   - Its auth is **redeeming a one-time ticket**, and that ticket already passed authentication + authz when it was issued.
//
// So httpd registers it by calling this method with the **bare mux**. Two functions instead of a parameter:
// "which routes are protected" should be obvious at the wiring site, not hidden in a boolean.
//
// ⚠️ This route is **not in authz/matrix.go** (adding it would trigger the reverse panic), which means
// it bypasses this repo's "missing declaration fails startup" gate. The cases in sandboxtermws_test.go
// make up for that. Read those before touching it.
func (h *Handler) RegisterTerminalWS(mux Mux) {
	mux.HandleFunc("GET /api/runs/{runId}/sandbox/terminal/ws", h.streamSandboxTerminalWS)
}

// streamSandboxTerminalWS is the HTTP entry of the terminal WS endpoint.
//
// ⚠️ **It does not go through h.fail**: that function writes a JSON body, while this path either upgrades to WebSocket
// or returns a bare status code. The Service side writes the response itself (see StreamRunSandboxTerminalWS),
// because only it knows whether it can "still write HTTP" or "has already upgraded".
func (h *Handler) streamSandboxTerminalWS(w http.ResponseWriter, r *http.Request) {
	h.sandbox.StreamRunSandboxTerminalWS(w, r, r.PathValue("runId"))
}
