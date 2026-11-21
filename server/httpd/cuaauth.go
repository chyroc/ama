package httpd

import (
	"context"
	"net/http"
)

// CUADesktopAuthorizer decides whether the authenticated request may use a desktop.
// The proxy itself only knows the desktop id; membership and desktop ownership belong to the host.
type CUADesktopAuthorizer func(context.Context, string) error

// CUAAuthorizerMiddleware applies the host's authorization after its login middleware.
// A nil authorizer is rejected at registration time by the caller that owns CUAConfig.
func CUAAuthorizerMiddleware(authorize CUADesktopAuthorizer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := extractCUADesktopID(r.URL.Path); id != "" {
			if err := authorize(r.Context(), id); err != nil {
				http.Error(w, "无权访问该云桌面", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
