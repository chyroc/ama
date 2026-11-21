package httpd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCUAAuthorizerRunsAfterLogin(t *testing.T) {
	type identityKey struct{}
	authorize := func(ctx context.Context, desktopID string) error {
		if desktopID != "desk_1" {
			t.Fatalf("desktop id = %q", desktopID)
		}
		if user, _ := ctx.Value(identityKey{}).(string); user != "user@example.com" {
			return errors.New("missing identity")
		}
		t.Logf("identity present for %s", desktopID)
		return nil
	}
	var order []string
	login := func(next http.Handler) http.Handler {
		order = append(order, "login-wrapper")
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "login")
			ctx := context.WithValue(r.Context(), identityKey{}, "user@example.com")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "upstream")
		w.WriteHeader(http.StatusNoContent)
	})
	h := login(CUAAuthorizerMiddleware(authorize, upstream))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desktops/desk_1/cua-app/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("已登录成员应可访问: %d", rec.Code)
	}
}

func TestCUAAuthorizerRejectsUnauthorizedDesktop(t *testing.T) {
	h := CUAAuthorizerMiddleware(func(context.Context, string) error { return errors.New("denied") },
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("不应进入上游") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/desktops/desk_2/cua-app/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("无权限桌面必须 403: %d", rec.Code)
	}
}
