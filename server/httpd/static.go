// Serving the frontend static assets.
//
// # Why the frontend is packed into the backend binary
//
// Single-process deployment: the binary from `go build ./cmd/amad` carries the whole frontend, and
// running it gives the complete AMA. No separate vite, no nginx forwarding, no CORS to configure
// (it is same-origin). For one-command start/stop locally, see dev.sh at the repository root.
//
// # What is in static/ is build output, not source
//
// dev.sh copies `web/dist` in, and it is **gitignored**. The repository only keeps a placeholder
// index.html so a clone that never ran dev.sh can still `go build` (otherwise go:embed cannot find
// the directory and compilation fails, and that error gives no hint that "you should build the
// frontend first").
//
// The placeholder page says plainly "the frontend has not been built", which is easier to track
// down than a blank page or a 404.
//
// # go:embed cannot reach outside the package directory
//
// Tried `//go:embed ../../web/dist`: invalid pattern syntax. embed only accepts the current package
// directory and its subdirectories; this is a language-level limit with no switch. So the output
// has to be copied in, and copying became dev.sh's job.
package httpd

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/chyroc/ama/server/openapi"
)

// staticFS is the frontend build output.
//
// Note the `all:` prefix: without it embed **skips files starting with `_` or `.`**. The Vite
// output has no such files today, but adding a `.well-known` or a chunk starting with an
// underscore later would silently drop a file; in production that shows up as occasional 404s,
// which are hard to track down.
//
//go:embed all:static
var staticFS embed.FS

// staticRoot lifts the embedded static/ subdirectory to the root, so URL paths map directly to file paths.
func staticRoot() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// The embedded directory structure is fixed at compile time; an error here means the source
		// was broken, so panic right away.
		panic("httpd: static 目录缺失，go:embed 应当保证它存在: " + err.Error())
	}
	return sub
}

// spaHandler serves frontend assets and provides the fallback for client-side routing.
//
// The frontend is a react-router SPA: paths like `/w/growth/chat` **have no matching file on
// disk**, so index.html must be returned and the frontend parses the route itself. With a plain
// http.FileServer, refreshing any deep link would give 404, while the home page opens fine, so this
// bug easily slips into production unnoticed.
//
// The fallback only applies to requests that "look like pages": paths with an extension (.js /
// .css / .png) that are not found honestly get 404, not index.html. Otherwise a misspelled js path
// would get an HTML page, and the browser reports "Unexpected token '<'", pointing far away from
// the real cause.
func spaHandler(root fs.FS) http.Handler {
	files := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Backend paths are normally kept out by routeFrontend and never get here. This check stays
		// in case someone mounts spaHandler directly on "/" in the future: then /api requests would
		// get HTML, the frontend fetch blows up at JSON.parse, and the error does not point to
		// routing at all.
		if isBackendPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}

		upath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		// The root path must map to index.html, but **upath must not be rewritten in place**: after
		// that it has the .html extension, the "extension → 404" rule below would block it, and
		// visiting / without a frontend build would give a bare 404 instead of the placeholder page.
		isRoot := upath == ""

		if !isRoot {
			if f, err := root.Open(upath); err == nil {
				_ = f.Close()
				setCacheHeaders(w, upath)
				files.ServeHTTP(w, r)
				return
			}

			// An extension means a specific asset was requested; if it is missing, it is missing.
			if ext := path.Ext(upath); ext != "" {
				http.NotFound(w, r)
				return
			}
		}

		// The root path and all paths that "look like pages" go to frontend routing.
		serveIndex(w, r, root)
	})
}

// routeFrontend sends a request to the backend mux or the frontend SPA by path.
//
// The split is done **outside** the mux instead of registering "/" in it, to keep 405: the
// standard library only returns 405 when "no pattern matches the path"; once the mux has a "/"
// catch-all, a request with the wrong method hits the catch-all instead of getting 405 (measured:
// it went from 405 to 200 + HTML).
func routeFrontend(backend, frontend http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isBackendPath(r.URL.Path) {
			backend.ServeHTTP(w, r)
			return
		}
		frontend.ServeHTTP(w, r)
	})
}

// isBackendPath tells whether a path belongs to the backend and should not go to the frontend SPA.
//
// Only list the prefixes the backend really uses; do not do the opposite and "allow-list frontend
// paths": frontend routes keep being added, and each one missed is a deep link that 404s after a
// refresh.
//
// ⚠️ **/llms.txt must be here**; it is the only backend path in the whole table not under /api/.
// Without it the request would fall to the SPA: the path has a .txt extension, and spaHandler's
// "extension → 404" rule would turn it into a 404, **although the backend did register this
// route**. That symptom is very hard to track down (curl reports 404, and the handler can be found
// in the code), hence this comment.
//
// ⚠️ **/websocket must be here too**, a variant of the same reason as /llms.txt. Without it the
// handshake request would fall to the SPA branch and get 200 + an HTML page, while **the gateway
// expects 101**, so the gateway turns it into a 500. The symptom looks exactly like "this endpoint
// was never implemented", although the handler can be found in the code. See the file header of
// wsprobe.go.
//
// Neither path **repeats the literal here**; each refers to the constant that defines it
// (`/llms.txt` in the openapi package, `/websocket` in wsprobe.go of this package): one wrong
// letter gives the 404/500 above, and both places would "look right".
func isBackendPath(p string) bool {
	return p == "/healthz" || p == "/api" || strings.HasPrefix(p, "/api/") ||
		p == openapi.PathLLMsTxt || p == PathWebSocketProbe
}

// serveIndex returns index.html with status 200, not 404.
//
// To the server an SPA deep link "does not exist", but to the user it is a normal page. Returning
// 404 would make browsers and crawlers think the page is broken, and make frontend error reporting
// record normal visits as failures.
//
// A missing index.html means dev.sh was never run (static/ only has the committed
// placeholder.html). Then return the placeholder page instead of 500: a 500 only sends people to
// the backend logs, while the problem is really "the frontend was not built".
func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	data, err := fs.ReadFile(root, "index.html")
	if err != nil {
		if fallback, ferr := fs.ReadFile(root, "placeholder.html"); ferr == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			// 200 instead of 503: this page is itself "an info page rendered successfully", and a
			// non-200 might make the browser not show the body.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(fallback)
			return
		}
		http.Error(w, "前端资源缺失，请先构建（见仓库根 dev.sh）", http.StatusInternalServerError)
		return
	}
	setCacheHeaders(w, "index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	_ = r
}

// setCacheHeaders picks the caching policy by "does the file name contain a content hash".
//
// Vite adds a content hash to output names under assets/ (index-BLxJQUjL.css); any change means a
// new name, so long caching is safe. index.html is the opposite and must not be cached: it is the
// only entry point to those hashed names, and caching it keeps users loading old js, and **after a
// new version is deployed nothing can correct it**; it shows up as "refresh does not update, only
// clearing the cache helps".
func setCacheHeaders(w http.ResponseWriter, upath string) {
	if strings.HasPrefix(upath, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}
