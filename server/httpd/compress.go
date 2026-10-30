// gzip compression of static assets.
//
// # Why it is needed
//
// The production load balancer defaults to `gzip_max_length 1m`: **responses larger than 1 MB are
// not compressed at all**. Our first-screen entry is 2.3 MB, just over the line, so the whole
// bundle went uncompressed. Measured: js+css in `web/dist` total 6858 KB, 2141 KB after gzip,
// **4.7 MB transferred for nothing, paid on every page load**. See docs/guides/cdn-static.md.
//
// # Scope: only frontend static assets, not /api/ and /healthz
//
// It is mounted on **the frontend branch** of routeFrontend, not around the whole Handler. This is
// deliberate, with three reasons; the first is about safety, the other two are "no gain from
// compressing":
//
//  1. **SSE and WebSocket are all under /api/**, on the backend branch. Mounting the middleware on
//     the frontend branch means **the routing structure** guarantees they never touch compression,
//     instead of relying on "the filter should let them through". This difference was measured,
//     not a matter of taste; see "Why not rely on content-type filtering" below.
//  2. **The load balancer already compresses the API's JSON responses.** In the prediction table
//     in cdn-static.md (6/6 hits), `/api/openapi.json` is "compressed → compressed": it is on the
//     load balancer's gzip_types list and smaller than 1 MB. Compressing again at the origin only
//     moves the same work from the gateway to our pods and saves no traffic.
//     What really went uncompressed are exactly the **static bundles over 1 MB**, which is what this
//     covers.
//  3. **/healthz is hit by the probe every 10 seconds**; its body is only 15 bytes (far below
//     gzip's 1024-byte minimum, so it would not really be compressed), yet each hit would go
//     through the middleware's checks. It is on the backend branch and naturally unaffected.
//
// A known small gap: `/llms.txt` is `text/markdown`, **not** in the load balancer's gzip_types, so
// it really is uncompressed in production; but it is on the backend branch
// (openapi.PathLLMsTxt), out of this middleware's reach. It is 9 KB and only fetched by external
// agents, so **the middleware is deliberately not moved onto /api/ for it**: that would hand SSE's
// safety back to the filter, and trading reason 1 for 9 KB is not worth it.
//
// # ⚠️ Why not rely on content-type filtering to protect SSE
//
// cdn-static.md originally said "gzhttp decides by content-type by default whether to compress";
// **measured, that does not hold for SSE, at two levels**:
//
//  1. `gzhttp.DefaultContentTypeFilter` only excludes the prefixes video/ audio/ image/jp and
//     archive-like keywords (compress/zip/zstd/...). **`text/event-stream` is not on the exclusion
//     list**, nor are `font/woff2` and `image/png`. Measured with the default config, the SSE
//     response really did get `content-encoding: gzip`.
//  2. The more hidden level: **even if the filter correctly decides "do not compress", SSE still
//     breaks.** `GzipResponseWriter.WriteHeader` only stores the status code without sending it,
//     and its `Flush()` returns right away when the buffer is empty, **without even calling the
//     underlying Flush**. The SSE handlers in this repository do "WriteHeader + Flush to send the
//     headers first, then wait for events", so the headers are held in the middleware until the
//     first frame is really written, and then sent together.
//
// Point 2 was measured, and both filter styles fall into it:
//
//	bare handler (baseline)      headers arrive after=0s   content-encoding=""
//	allow list (SSE not on it)   headers arrive after=3s   content-encoding=""
//	ExceptContentTypes           headers arrive after=3s   content-encoding=""
//
// **Note that content-encoding is empty in the last two rows.** In other words the check "there is
// no content-encoding in the response headers" **cannot detect this failure**: the filter is right,
// and the stream still hangs. On the frontend, `EventSource`'s onopen fires late (in the worst case
// at the heartbeat 15 seconds later), and the chat page looks like "I sent a message and nothing
// happens", **with no error at all**.
//
// So no filter is used as a safety net here; SSE simply takes another path that does not go
// through compression. Pinned by TestSSEResponseIsNeverCompressed.
package httpd

import (
	"net/http"

	"github.com/klauspost/compress/gzhttp"
)

// compressibleContentTypes is the allow list of content-types that may be compressed.
//
// **An allow list, not a deny list**: besides js/css, `web/dist` has woff2 / woff / ttf / png /
// ico, which **are already compressed formats**; compressing them again only burns CPU and
// sometimes makes them larger. gzhttp's default filter **does not exclude** them (it only blocks
// video/ audio/ image/jp and archive types; `font/woff2` and `image/png` are let through), so we
// must list them ourselves.
//
// Conversely, an allow list missing a type only means "saving a bit less traffic", while a deny
// list missing one means "burning CPU for nothing"; the costs of the two failure directions are not
// symmetric, so pick the milder one.
//
// The entries are media types without parameters: when the config has no parameters, gzhttp
// ignores the parameters of the actual response, so `text/javascript` matches the
// `text/javascript; charset=utf-8` Go really sends.
var compressibleContentTypes = []string{
	"text/html",        // index.html and the fallback for SPA deep links
	"text/css",         // 573 KB
	"text/javascript",  // the bulk; the entry is 2.3 MB
	"application/json", // manifest.json
	"image/svg+xml",    // favicon.svg; **not in the load balancer's gzip_types**, so only the origin can compress it
	"text/plain",       // fallback: an occasional .txt output
}

// compressStatic wraps frontend static assets with gzip.
//
// It should only be used on **the frontend branch** of routeFrontend; see the file header for why.
func compressStatic(next http.Handler) http.Handler {
	wrapper, err := gzhttp.NewWrapper(
		gzhttp.ContentTypes(compressibleContentTypes),
		// Keep gzhttp's default minimum of 1024 bytes: for smaller responses the gzip header itself
		// eats the gain, and the result may even be larger.
		gzhttp.MinSize(gzhttp.DefaultMinSize),
	)
	if err != nil {
		// The parameters are all constants in this file; an error can only be a coding mistake, and
		// failing to start is better than silently not compressing.
		panic("httpd: 装配 gzip 中间件失败: " + err.Error())
	}
	return wrapper(next)
}
