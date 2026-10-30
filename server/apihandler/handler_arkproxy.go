// HTTP pass-through layer for the raw Ark Managed Agents API.
//
// # Shape
//
// Routes come in two scopes (for how credentials are resolved, see core/arkproxy.go):
//
//	/api/runs/{runId}/ark/...            resolve the Ark key by session
//	/api/workspaces/{workspaceId}/ark/.. resolve the Ark key by workspace
//
// The path after `ark/` is the part after Ark's /api/v3. The session id is filled in from runId,
// for example GET /api/runs/{runId}/ark/resources maps to
//      GET https://ark.../api/v3/sessions/{runId}/resources.
// The query is forwarded as is (including bracketed params like created_at[gt]). Request body bytes are forwarded as is
// (same for JSON and multipart), and response body bytes are returned as is.
//
// ⚠️ **Failure responses use Ark's error shape, not AMA's {message,code} envelope.**
// This is on purpose: these endpoints are aliases of Ark endpoints, so we return errors exactly as Ark does, and callers handle them
// per the Ark
// docs. Only failures "on AMA's own side" (run does not exist, workspace has no key configured, network down)
// use the standard envelope.

package apihandler

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/chyroc/ama/server/shared/infra/ma"
)

// arkProxyMaxBody is the request body limit for pass-through.
//
// The largest request in the Ark docs is sending a video event (body ≤64 MiB), and file upload is about the same size;
// anything larger gets a 413 while reading the body and never reaches Ark.
const arkProxyMaxBody = 64 << 20

// arkProxyDesc describes one pass-through route.
type arkProxyDesc struct {
	method  string
	pattern string
	// scope decides whether the Ark key is resolved by run or by workspace.
	scope string // "run" | "workspace"
	// suffix is the path template after Ark's /api/v3; placeholders have the same names as PathValue.
	suffix string
	// When stream is true, relay as SSE (do not read the whole response body, flush each chunk).
	stream bool
}

const (
	arkScopeRun       = "run"
	arkScopeWorkspace = "workspace"
)

// arkProxyRoutes is the full table of pass-through routes.
//
// ⚠️ This and authz/matrix.go are two descriptions of the same routes (that one is permission levels, this one is forwarding shape).
// Adding or removing a route means changing both: missing matrix panics on startup, missing here gives a 404.
//
// Ark "updates" are always POST (not PATCH/PUT), so on the same resource path
// three methods coexist: GET = read, POST = update/create, DELETE = delete.
var arkProxyRoutes = []arkProxyDesc{
	// -- Session: upgrade / resources / threads / files (resolved by run) ----------------------

	{http.MethodPost, "/api/runs/{runId}/ark/upgrade", arkScopeRun, "/sessions/{runId}/upgrades", false},

	{http.MethodGet, "/api/runs/{runId}/ark/resources", arkScopeRun, "/sessions/{runId}/resources", false},
	{http.MethodPost, "/api/runs/{runId}/ark/resources", arkScopeRun, "/sessions/{runId}/resources", false},
	{http.MethodGet, "/api/runs/{runId}/ark/resources/{resourceId}", arkScopeRun,
		"/sessions/{runId}/resources/{resourceId}", false},

	{http.MethodGet, "/api/runs/{runId}/ark/threads", arkScopeRun, "/sessions/{runId}/threads", false},
	{http.MethodGet, "/api/runs/{runId}/ark/threads/{threadId}", arkScopeRun,
		"/sessions/{runId}/threads/{threadId}", false},
	{http.MethodGet, "/api/runs/{runId}/ark/threads/{threadId}/events", arkScopeRun,
		"/sessions/{runId}/threads/{threadId}/events", false},
	{http.MethodGet, "/api/runs/{runId}/ark/threads/{threadId}/stream", arkScopeRun,
		"/sessions/{runId}/threads/{threadId}/stream", true},

	// The Files API is under run: files belong to a cloud account, and must be uploaded to the account this session is in
	// (resolved by run); otherwise the mounted resource and the session are in different accounts and the agent cannot read it.
	{http.MethodGet, "/api/runs/{runId}/ark/files", arkScopeRun, "/files", false},
	{http.MethodPost, "/api/runs/{runId}/ark/files", arkScopeRun, "/files", false},
	{http.MethodGet, "/api/runs/{runId}/ark/files/{fileId}", arkScopeRun, "/files/{fileId}", false},
	{http.MethodDelete, "/api/runs/{runId}/ark/files/{fileId}", arkScopeRun, "/files/{fileId}", false},

	// -- Workspace: Vaults and credentials (resolved by workspace) ---------------------------

	{http.MethodGet, "/api/workspaces/{workspaceId}/ark/vaults", arkScopeWorkspace, "/vaults", false},
	{http.MethodPost, "/api/workspaces/{workspaceId}/ark/vaults", arkScopeWorkspace, "/vaults", false},
	{http.MethodGet, "/api/workspaces/{workspaceId}/ark/vaults/{vaultId}", arkScopeWorkspace,
		"/vaults/{vaultId}", false},
	{http.MethodPost, "/api/workspaces/{workspaceId}/ark/vaults/{vaultId}", arkScopeWorkspace,
		"/vaults/{vaultId}", false},
	{http.MethodDelete, "/api/workspaces/{workspaceId}/ark/vaults/{vaultId}", arkScopeWorkspace,
		"/vaults/{vaultId}", false},

	{http.MethodGet, "/api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials",
		arkScopeWorkspace, "/vaults/{vaultId}/credentials", false},
	{http.MethodPost, "/api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials",
		arkScopeWorkspace, "/vaults/{vaultId}/credentials", false},
	{http.MethodGet, "/api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}",
		arkScopeWorkspace, "/vaults/{vaultId}/credentials/{credentialId}", false},
	{http.MethodPost, "/api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}",
		arkScopeWorkspace, "/vaults/{vaultId}/credentials/{credentialId}", false},
	{http.MethodDelete, "/api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}",
		arkScopeWorkspace, "/vaults/{vaultId}/credentials/{credentialId}", false},

	// -- Workspace: Memory Stores and memory entries --------------------------------------

	{http.MethodGet, "/api/workspaces/{workspaceId}/ark/memory-stores",
		arkScopeWorkspace, "/memory_stores", false},
	{http.MethodPost, "/api/workspaces/{workspaceId}/ark/memory-stores",
		arkScopeWorkspace, "/memory_stores", false},
	{http.MethodGet, "/api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}",
		arkScopeWorkspace, "/memory_stores/{memoryStoreId}", false},
	{http.MethodPost, "/api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}",
		arkScopeWorkspace, "/memory_stores/{memoryStoreId}", false},
	{http.MethodDelete, "/api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}",
		arkScopeWorkspace, "/memory_stores/{memoryStoreId}", false},

	{http.MethodGet, "/api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories",
		arkScopeWorkspace, "/memory_stores/{memoryStoreId}/memories", false},
	{http.MethodPost, "/api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories",
		arkScopeWorkspace, "/memory_stores/{memoryStoreId}/memories", false},
	// The Ark path is batch_create (snake case). The AMA path uses a hyphen only to keep URL style consistent;
	// it is changed back to Ark's original when forwarding.
	{http.MethodPost, "/api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/batch-create",
		arkScopeWorkspace, "/memory_stores/{memoryStoreId}/memories/batch_create", false},
	{http.MethodGet, "/api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}",
		arkScopeWorkspace, "/memory_stores/{memoryStoreId}/memories/{memoryId}", false},
	{http.MethodPost, "/api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}",
		arkScopeWorkspace, "/memory_stores/{memoryStoreId}/memories/{memoryId}", false},
	{http.MethodDelete, "/api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}",
		arkScopeWorkspace, "/memory_stores/{memoryStoreId}/memories/{memoryId}", false},
}

func (h *Handler) registerArkProxyRoutes(mux Mux) {
	for _, d := range arkProxyRoutes {
		d := d
		if d.stream {
			mux.HandleFunc(d.method+" "+d.pattern, h.arkProxyStreamHandler(d))
		} else {
			mux.HandleFunc(d.method+" "+d.pattern, h.arkProxyHandler(d))
		}
	}
}

// arkProxyHandler handles normal (non-streaming) pass-through.
func (h *Handler) arkProxyHandler(d arkProxyDesc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := h.buildArkProxyRequest(w, r, d)
		if !ok {
			return
		}
		var resp *ma.RawResponse
		var err error
		if d.scope == arkScopeRun {
			resp, err = h.workspace.ArkProxyDoForRun(r.Context(), r.PathValue("runId"), req)
		} else {
			resp, err = h.workspace.ArkProxyDoForWorkspace(r.Context(), r.PathValue("workspaceId"), req)
		}
		if err != nil {
			// resp is not nil: Ark returned 4xx/5xx; forward Ark's error JSON as is.
			if resp != nil {
				writeArkRawResponse(w, resp)
				return
			}
			h.fail(w, r, err)
			return
		}
		writeArkRawResponse(w, resp)
	}
}

// arkProxyStreamHandler relays an SSE stream.
func (h *Handler) arkProxyStreamHandler(d arkProxyDesc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			h.log.ErrorContext(r.Context(), "ResponseWriter 不支持 Flush，SSE 无法工作")
			h.failWith(w, http.StatusInternalServerError, "服务端不支持流式响应", "internal")
			return
		}
		req, ok := h.buildArkProxyRequest(w, r, d)
		if !ok {
			return
		}
		var stream *ma.RawStream
		var err error
		if d.scope == arkScopeRun {
			stream, err = h.workspace.ArkProxyStreamForRun(r.Context(), r.PathValue("runId"), req)
		} else {
			stream, err = h.workspace.ArkProxyStreamForWorkspace(r.Context(), r.PathValue("workspaceId"), req)
		}
		if err != nil {
			if stream != nil {
				defer stream.Close()
				copyArkStreamError(w, stream)
				return
			}
			h.fail(w, r, err)
			return
		}
		defer stream.Close()

		// Not 2xx (such as a 4xx for a missing thread): headers are not written yet, so return the error body to the client directly.
		if stream.StatusCode >= 300 {
			copyArkStreamError(w, stream)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		buf := make([]byte, 4096)
		for {
			n, readErr := stream.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return // client disconnected
				}
				flusher.Flush()
			}
			if readErr != nil {
				return // EOF, client disconnect and upstream disconnect all end here
			}
		}
	}
}

// buildArkProxyRequest translates the incoming HTTP request into a ma.RawRequest.
// When it returns false, the error response has already been written.
func (h *Handler) buildArkProxyRequest(w http.ResponseWriter, r *http.Request, d arkProxyDesc) (ma.RawRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, arkProxyMaxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// Over 64 MiB, the MaxBytesReader error message is useless to the caller, so give a clear message.
		h.failWith(w, http.StatusRequestEntityTooLarge,
			"请求体超过方舟透传接口的 64 MiB 上限", "request_too_large")
		return ma.RawRequest{}, false
	}
	return ma.RawRequest{
		Method:      d.method,
		Path:        expandArkPath(d.suffix, r),
		Query:       r.URL.Query(),
		Body:        body,
		ContentType: r.Header.Get("Content-Type"),
	}, true
}

// expandArkPath replaces {xxx} in the template with escaped PathValues.
func expandArkPath(tmpl string, r *http.Request) string {
	var b strings.Builder
	rest := tmpl
	for {
		start := strings.IndexByte(rest, '{')
		if start < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:start])
		end := strings.IndexByte(rest[start:], '}')
		name := rest[start+1 : start+end]
		b.WriteString(url.PathEscape(r.PathValue(name)))
		rest = rest[start+end+1:]
	}
}

// writeArkRawResponse writes Ark's response body and status code as is.
//
// ⚠️ **Content-Type is not passed through directly; only JSON types are allowed**. The pass-through endpoints share the origin with AMA Web
// and use the Cookie: if upstream returned text/html (a tampered response, or a future endpoint that returns file content),
// opening that URL directly in a browser would run scripts under the AMA origin. All responses of this batch of Ark endpoints are JSON,
// so any non-JSON type is returned as application/json. Better that the browser does not render it than to let HTML through.
func writeArkRawResponse(w http.ResponseWriter, resp *ma.RawResponse) {
	w.Header().Set("Content-Type", safeArkJSONContentType(resp.Header.Get("Content-Type")))
	// A same-origin endpoint should not be treated as document navigation: download instead of render, one more layer of XSS protection.
	w.Header().Set("Content-Disposition", "attachment")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body)
}

// safeArkJSONContentType only lets JSON Content-Types through; everything else falls back to application/json.
func safeArkJSONContentType(ct string) string {
	media := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	switch media {
	case "application/json", "application/problem+json":
		return media
	default:
		return "application/json"
	}
}

// copyArkStreamError, when Ark returns non-2xx during the SSE handshake, writes the error body with its status code.
// It also does not let non-JSON types through (see the security comment on writeArkRawResponse).
func copyArkStreamError(w http.ResponseWriter, stream *ma.RawStream) {
	body, _ := io.ReadAll(io.LimitReader(stream, arkProxyMaxBody+1))
	w.Header().Set("Content-Type", safeArkJSONContentType(stream.Header.Get("Content-Type")))
	w.Header().Set("Content-Disposition", "attachment")
	w.WriteHeader(stream.StatusCode)
	_, _ = w.Write(body)
}
