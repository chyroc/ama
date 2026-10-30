// Guard: wraps authorization around each handler, per route.
//
// # Position: after authentication, before business logic, and it must be after routing
//
//	httpd.New:  mux → protectedMux (auth.Protect authentication) → Guard (this package, authorization) → business handler
//
// It wraps **each single handler** instead of the whole mux, for exactly the same reason as auth.Protect:
// the standard library's 405 relies on "match the route first, then check the method". If authorization happened before routing,
// a GET on an endpoint that only accepts POST would get 403 instead of 405; httpd's TestMethodRouting
// fails on the spot, and the error does not point to the real cause at all.
//
// A side benefit: the decorator closure gets **the pattern at registration time**, so permissions can be declared by pattern,
// instead of parsing the URL at request time to guess which route it belongs to.

package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/chyroc/ama/server/auth"
)

// Mux is the route registration ability Guard needs, the same shape as core.Mux / feishu.Mux.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Guard is the authorization decorator.
//
// It implements Mux, so the Register code of business modules does not change by a single line; it also records every registered
// pattern for Verify's two-way completeness check.
type Guard struct {
	inner  Mux
	member Membership
	log    *slog.Logger

	// registered records every pattern registered through this Guard, so Verify can find missing declarations.
	registered map[string]bool
}

// Config is the configuration needed to build a Guard.
type Config struct {
	// Membership is required: looks up roles and resolves resource ownership.
	Membership Membership
	// Logger empty means slog's default logger.
	Logger *slog.Logger
}

// New builds a Guard.
func New(cfg Config) (*Guard, error) {
	if cfg.Membership == nil {
		return nil, errors.New("authz: Membership 不能为 nil")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Guard{member: cfg.Membership, log: log, registered: map[string]bool{}}, nil
}

// Wrap returns a Mux wrapped with authorization, on which business modules register routes.
//
// The same Guard can Wrap several times (once each for core and feishu); registered is shared,
// so Verify sees all routes of both modules.
func (g *Guard) Wrap(inner Mux) Mux {
	return &guardedMux{guard: g, inner: inner}
}

// guardedMux is the decorator layer returned by Wrap.
type guardedMux struct {
	guard *Guard
	inner Mux
}

// HandleFunc looks up the table to set permissions at registration, and runs the check at request time.
//
// **The lookup happens at registration time**: an undeclared pattern panics immediately, instead of being found only when someone requests it.
func (m *guardedMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	g := m.guard
	perm, ok := routePermissions[pattern]
	if !ok {
		// A new route was added without declaring permissions in matrix.go. **This is the most dangerous kind of miss**:
		// without the panic it would silently allow; any logged-in user could call this endpoint, with no error at all.
		panic(fmt.Sprintf("authz: 路由 %q 没有声明权限，请在 authz/matrix.go 的 routePermissions 里加一行", pattern))
	}
	g.registered[pattern] = true

	// ActionNone routes need no workspace authorization; pass through directly, saving one closure layer.
	if perm.action == ActionNone {
		m.inner.HandleFunc(pattern, handler)
		return
	}
	m.inner.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if !g.allow(w, r, perm) {
			return
		}
		handler(w, r)
	})
}

// Verify checks that declarations and registrations match one to one, returning an error if not.
//
// **Both directions must be checked**:
//
//	registered but not declared: already panics in HandleFunc, never gets here;
//	declared but not registered: a typo in the pattern (workspaceld vs workspaceId) is exactly this.
//	                  If only the first direction were checked, that declaration would never match, and the matching real route
//	                  would panic for "not declared", with the error pointing to that real route, making people think
//	                  a declaration was missing, while actually the declaration has a typo.
//
// Called by httpd.New after both modules are wired up.
func (g *Guard) Verify() error {
	var missing []string
	for pattern := range routePermissions {
		if g.registered[pattern] || optionalRoutes[pattern] {
			continue
		}
		missing = append(missing, pattern)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("authz: 这些路由声明了权限但没有被注册（pattern 可能打错字了）：%s",
		strings.Join(missing, "、"))
}

// allow runs one authorization check. Returning false means the response has already been written; the caller just returns.
//
// The order is **resolve first, then check membership, then check level**; each step's failure gives a different response:
//
//	resource cannot be resolved → 404 + code=not_found (not 403! see the comment on ErrResourceNotFound)
//	not a member                → 403 + code=access_denied + the "not a member of this workspace, no access" message
//	level not enough            → 403 + code=access_denied + the "insufficient permission" message
//	database lookup error       → 500 (database jitter should not be called "you have no permission"; that would send users to an admin for permission)
func (g *Guard) allow(w http.ResponseWriter, r *http.Request, perm permission) bool {
	ctx := r.Context()

	// The identity is always there: every route reaching here is behind auth.Protect. Not getting it means the wiring is wrong
	// (Guard was wrapped outside authentication); that is a bug, not the user's problem, so reply 401 to make it visible,
	// instead of replying 403 as a non-member.
	actor := auth.ActorFrom(ctx)
	if actor == "" {
		g.log.ErrorContext(ctx, "授权中间件拿不到身份，检查 Guard 是否包在了 auth.Protect 之外",
			slog.String("path", r.URL.Path))
		writeEnvelope(w, http.StatusUnauthorized, "请先登录", "unauthenticated")
		return false
	}

	workspaceID, ok := g.resolveWorkspace(w, r, perm)
	if !ok {
		return false
	}
	// allowMissing and cannot be resolved: allow and let the handler deal with it (list endpoints return 200 + []).
	if workspaceID == "" {
		return true
	}

	role, isMember, err := g.member.RoleOf(ctx, workspaceID, actor)
	if err != nil {
		if g.clientGone(ctx, r, err, "查空间成员") {
			return false
		}
		g.log.ErrorContext(ctx, "查空间成员失败",
			slog.String("path", r.URL.Path),
			slog.String("workspace", workspaceID),
			slog.String("error", err.Error()))
		writeEnvelope(w, http.StatusInternalServerError, "服务内部错误，请稍后重试", "internal")
		return false
	}
	if !isMember {
		g.logDenied(ctx, r, workspaceID, actor, "非成员")
		writeEnvelope(w, http.StatusForbidden, MsgNotMember, "access_denied")
		return false
	}
	if levelOf(role) < requiredLevel(perm.action) {
		g.logDenied(ctx, r, workspaceID, actor, "角色 "+string(role)+" 权限不足")
		writeEnvelope(w, http.StatusForbidden, MsgForbidden, "access_denied")
		return false
	}
	return true
}

// resolveWorkspace decides which workspace this request acts on.
//
// The second return value false means the response has already been written. Returning an empty string with true means
// "an allowMissing route could not resolve the resource", and the caller should allow it.
func (g *Guard) resolveWorkspace(w http.ResponseWriter, r *http.Request, perm permission) (string, bool) {
	if perm.kind == KindNone {
		// Workspace-level route: the workspace comes straight from the path parameter.
		id := strings.TrimSpace(r.PathValue(wsParam))
		if id == "" {
			// Declared as workspace-level but the parameter is missing, meaning the pattern in matrix.go does not match the real route.
			g.log.ErrorContext(r.Context(), "路由声明为空间维度但路径里没有 workspaceId",
				slog.String("path", r.URL.Path))
			writeEnvelope(w, http.StatusInternalServerError, "服务内部错误，请稍后重试", "internal")
			return "", false
		}
		return id, true
	}

	resourceID := strings.TrimSpace(r.PathValue(perm.idParam))
	workspaceID, err := g.member.WorkspaceOf(r.Context(), perm.kind, resourceID)
	if err != nil {
		// Platform-level resource (built-in skill): belongs to no workspace, so membership cannot be judged.
		// **Allow reads, always reject writes**: the body is maintained by the platform, and nobody can change it.
		//
		// This branch must be checked before ErrResourceNotFound: both are "the workspace cannot be resolved",
		// but they mean opposite things (one is "this thing does not exist", the other is "this thing belongs to everyone").
		if errors.Is(err, ErrPlatformResource) {
			if perm.action == ActionRead {
				return "", true
			}
			g.logDenied(r.Context(), r, "", auth.ActorFrom(r.Context()), "平台内置资源不可修改")
			writeEnvelope(w, http.StatusForbidden, MsgPlatformReadOnly, "access_denied")
			return "", false
		}
		if errors.Is(err, ErrResourceNotFound) {
			if perm.allowMissing {
				return "", true
			}
			// 404 instead of 403; see the comment on ErrResourceNotFound. The message and code must have the same
			// shape as core's fail; the frontend's six nullable getters rely on code to recognize "does not exist".
			writeEnvelope(w, http.StatusNotFound, "资源不存在", "not_found")
			return "", false
		}
		if g.clientGone(r.Context(), r, err, "反解资源归属空间") {
			return "", false
		}
		g.log.ErrorContext(r.Context(), "反解资源归属空间失败",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		writeEnvelope(w, http.StatusInternalServerError, "服务内部错误，请稍后重试", "internal")
		return "", false
	}
	return workspaceID, true
}

// logDenied records one denied access.
//
// Info instead of Warn: a denial is an **expected** normal result (someone clicked a button they should not, or after switching workspaces
// refreshed an old link); Warn would flood the log and drown the real anomalies. But it must be recorded:
// when "a user says they cannot open some page", this log line is the only thing that directly gives the reason.
func (g *Guard) logDenied(ctx context.Context, r *http.Request, workspaceID, actor, reason string) {
	g.log.InfoContext(ctx, "授权拒绝",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("workspace", workspaceID),
		slog.String("actor", actor),
		slog.String("reason", reason))
}

// writeEnvelope writes an error envelope with the same shape as the core / feishu / auth packages.
//
// The shape is a flat {message, code}: message is for users (the frontend shows it directly), code is for programs.
// The three business packages each have a helper with the same name; this is the fourth. It is not shared because sharing one
// writeJSON across packages would lose locality of "what whose errors look like", and the envelope has only two fields.
// clientGone reports whether this failure is "the client left on its own"; if so it logs one Info line and returns true.
//
// The check is **whether the request's ctx has been cancelled**, not whether the error chain contains context.Canceled:
// pgx wraps cancellation in its own error types (sometimes `context canceled`, sometimes wrapped into
// a "query failed (sql=…)" message), and `errors.Is` may not catch it. ctx.Err() is the only
// reliable signal at that layer: net/http sets it when the connection drops, regardless of how the driver wraps things.
//
// ⚠️ **Why it must be checked separately**: the SSE route (/events/stream) drops whenever a user closes the page;
// this is **normal behavior that happens every day**, not a fault. Without the check there are three consequences, all with zero errors:
//
//   - The log fills with ERROR and drowns the real faults; this repo already got burned once by "the authorization management page
//     logs three ERRORs every time it is opened" (see the ErrAppsUnsupported part in handler.go);
//   - The frontend treats it as a 5xx and **auto-reports a problem**, so "a user closed a page" becomes a
//     production issue; one past investigation was misled exactly this way;
//   - Writing the response is pointless: the connection is gone, and WriteHeader would just get another broken pipe.
//
// When returning true it **writes no response body**, for the third reason above. The caller just returns false.
func (g *Guard) clientGone(ctx context.Context, r *http.Request, err error, what string) bool {
	if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		return false
	}
	g.log.InfoContext(ctx, what+"时客户端已断开，跳过（不是故障）",
		slog.String("path", r.URL.Path),
		slog.String("error", err.Error()))
	return true
}

func writeEnvelope(w http.ResponseWriter, status int, message, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"message": message, "code": code})
}
