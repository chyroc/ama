// HTTP boundary: decode requests, call the orchestration layer, map sentinel errors to status codes and Chinese messages.
//
// # What this package is
//
// Step 15 of the package split plan moved this whole set of files out of `package core`. After the move,
// **core contains no HTTP code**, and the plan's first constraint, "handlers only know the orchestration layer;
// domain packages are not exposed to handlers", went from a convention to a compile-time fact:
//
//	this package's imports have no core: `go list -deps ./apihandler | grep -c /core$` = 0
//
// Before, that constraint was kept only by a **hand-copied copy of the interfaces** in core/handlerservice.go
// (core cannot import the orchestration layer, that would be a cycle, so copying was the only option). The copy and the original were kept
// in sync by two lines of static assertions, which prevented drift, but "what handlers can call" still had two spellings. Now there is only one:
// the ten narrow interfaces in orchestrator/<domain>/service.go. This package references them directly, and the copy has been deleted.
//
// # Step 16: domains now also have compile-time boundaries between each other
//
// The rule above only guards "handler ↔ domain package". **Between domains there was nothing**: Handler had only
// one `svc` field holding the union of nine domains, so writing
// `h.svc.CreateAgent(...)` in handler_issue.go still compiled. The convention was followed at the time (each handler_<domain>.go
// only called its own domain), but it was kept by discipline, not by the compiler.
//
// Now Handler holds ten fields by domain, each the narrow interface of that domain, and cross-domain calls fail to compile right away.
// For the measured result, see the comment on the Handler struct.
//
// ⚠️ **This package imports the orchestration layer, not the other way around.** If the orchestration layer imports this package or core,
// the packages it is meant to hide show up in `go list -deps ./orchestrator`, and this layer becomes pointless.
// For the full reasoning about direction, see the file header of orchestrator/routes.go.
//
// # Same conventions as the feishu package
//
// Error envelope {message, code}, thin handlers, all business decisions in the Service,
// so the two modules behave the same to the outside in the same process.
//
// # The frontend shows message directly to the user
//
// FormModal.tsx / WorkspacesPage.tsx both do `e instanceof Error ? e.message : fallback`,
// so message must be a readable Chinese sentence, and must match the original text in LocalApiClient: e2e asserts
// a few of those original strings. code is a stable identifier for programs to check.
//
// # Five nullable getters and 404
//
// Five frontend methods return `T | null` (getWorkspaceByKey / getAgent / getSkill /
// getFeishuApp / getRun). HttpApiClient turns 404 into null, but **it must check that the envelope's
// code == "not_found"**, not just the status code: when a route is wrong, the standard library mux returns a 404 that is
// text/plain "404 page not found". Checking only the status code would treat "the route is mounted wrong" as
// "resource does not exist", which shows up on the page as a silent empty state and is very hard to debug. So this package registers a fallback
// route on the mux, so that unmatched /api/ paths also return the same envelope.
//
// # List endpoints never return 404
//
// listAgents / listSkills / listRuns / listRunEvents all return arrays, not nullable values,
// so "workspace does not exist" or "run does not exist" always returns 200 + [], not 404. Returning 404 would make the page
// throw instead of showing an empty list.

package apihandler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/orchestrator"
	"github.com/chyroc/ama/server/orchestrator/agent"
	"github.com/chyroc/ama/server/orchestrator/ark"
	"github.com/chyroc/ama/server/orchestrator/issue"
	"github.com/chyroc/ama/server/orchestrator/revision"
	"github.com/chyroc/ama/server/orchestrator/sandbox"
	"github.com/chyroc/ama/server/orchestrator/skill"
	"github.com/chyroc/ama/server/orchestrator/suggestion"
	"github.com/chyroc/ama/server/orchestrator/workspace"
	integrationsvc "github.com/chyroc/ama/server/service/integration"
)

// Handler exposes the orchestration layer as HTTP endpoints.
type Handler struct {
	// One field for each of the ten domains; the type is **the narrow interface of that domain**.
	//
	// ⚠️ This is deliberately **not one `svc orchestrator.Service`** (the union of nine domains). A union lets
	// handlers call all 133 methods, so writing
	// `h.svc.CreateAgent(...)` in handler_issue.go still compiles, and "domains cannot see each other" is only a convention,
	// kept by discipline and ignored by the compiler. Split into ten fields, that line becomes a compile error:
	// `h.issue.CreateAgent undefined (type issue.Service has no field or method CreateAgent)`.
	//
	// So this layer has two compile-time boundaries instead of one:
	//
	//	handler ↔ domain pkg  handlers only see the 133 listed by the orchestration layer (set up in step 15)
	//	domain  ↔ domain      each handler file only sees its own domain (set up in step 16)
	//
	// The field name is the domain name, matching orchestrator/<domain>/ one to one; handler_<domain>.go
	// uses the field with the same name. Step 17 split the nine domains into nine packages, so the field type changed from
	// `orchestrator.IssueService` to `issue.Service`: the package name carries the domain name,
	// instead of a prefix.
	//
	// ⚠️ **That step did not add a new wall here; do not describe it as if it did.** What blocks cross-domain calls has always been
	// "the field type is a narrow interface" (set up in step 16), unrelated to how packages are split: `h.issue`
	// is reachable from any file in package apihandler, and imports do not control field access.
	// Splitting packages only changed the type name in the error. What it really blocks in addition is **between domain interface declarations**:
	// the nine interfaces used to be in the same package, so ark.go could write IssueService directly; now
	// orchestrator/ark cannot see orchestrator/issue unless it imports it.
	//
	// **handler.go itself belongs entirely to the workspace domain**: after agent and skill were split out, all of its remaining
	// 47 call sites were checked to fall within the 47 methods of workspace.Service, none crossing domains
	// (member / run / schedule all live under that domain name; there are now two domains that span several sources:
	// workspace and agent, see composite.go in each package). The 9 agent handlers
	// are in handler_agent.go, and the 16 skill ones in handler_skill.go.
	workspace  workspace.Service
	agent      agent.Service
	skill      skill.Service
	ark        ark.Service
	issue      issue.Service
	suggestion suggestion.Service
	sandbox    sandbox.Service
	revision   revision.Service
	// hosts / hostDaemon are the host executor domain and can be nil (setups without a host store wired in,
	// such as tests that only cover part of the path); when nil, the whole group of related routes is not registered, same policy as integrations.
	hosts      HostServices
	hostDaemon HostDaemonServices
	// userFiles is the account-level personal file domain and can be nil (deployments without object storage); when nil it returns 503,
	// see userFilesOr503 in handler_userfiles.go.
	userFiles UserFileServices

	log *slog.Logger
	// When devRoutes is true, the reset endpoints under _debug are registered, see Register.
	devRoutes bool
	// integrations is the provider registry for "integration management" and can be nil (when no source is registered).
	//
	// When nil, those four routes **are still registered**: the list returns `[]` and a single item returns 404. If they were not registered, the frontend would
	// get 404, and on the frontend 404 means "this backend version does not have the feature", which is different from "the feature exists but no provider is configured";
	// the page could not give actionable text like "go configure such-and-such setting".
	integrations *integrationsvc.IntegrationRegistry
}

// HandlerConfig is the configuration needed to build a Handler.
type HandlerConfig struct {
	// Orchestrator is required; it is the orchestration layer itself.
	//
	// ⚠️ **It takes `*orchestrator.Orchestrator` instead of ten interface fields**, because
	// "wiring should have no chance to be mixed up": ten fields would mean the wiring site writes ten lines of
	// `Issue: orch.Issue()`, and mixing up two lines (`Issue: orch.Suggestion()`)
	// gives **zero errors**: both are interfaces and both are non-nil, and you only find out when that route runs and
	// creating an issue creates a suggestion. Taking the orchestration layer itself means the ten lines in NewHandler
	// do the picking, and the wiring site cannot get it wrong.
	//
	// Handlers still only see the ten narrow interfaces: *Orchestrator does not have those 133 methods;
	// it only has ten accessors, and what they return are interfaces.
	Orchestrator *orchestrator.Orchestrator
	// If Logger is empty, the slog default logger is used.
	Logger *slog.Logger
	// Integrations is the provider registry for "integration management"; empty means no integration source is connected
	// (those four routes are still registered, the list is just empty, see Handler.integrations).
	Integrations *integrationsvc.IntegrationRegistry
	// When DevRoutes is true, POST /api/_debug/reset and /clear are registered.
	//
	// Before each e2e case, window.__amaApi.reset() restores the mock store to seed data
	// (web/e2e/fixtures.ts). After the frontend switched to HTTP that entry point was gone; without an equivalent endpoint
	// 57 cases would run on accumulated shared state and pollute each other. **Only enable it in dev and test environments.**
	DevRoutes bool
	// Hosts / HostDaemon are the host executor domain services (service/host). When empty, the whole group of related routes
	// is not registered (local setups that run only some dependencies).
	Hosts      HostServices
	HostDaemon HostDaemonServices
	// UserFiles is the account-level personal file domain service (service/userfile). When empty, this group of routes returns 503
	// (deployments without object storage).
	UserFiles UserFileServices
}

// NewHandler builds a Handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	// ⚠️ What is checked here is a **concrete pointer type**, so `== nil` is a real check,
	// unlike when taking an interface, where typed-nil can fool it (a `var s *core.Service` put into an interface
	// is `!= nil`; this repo hit that at schedulerLeases).
	// Taking *Orchestrator removes that trap as well.
	if cfg.Orchestrator == nil {
		return nil, errors.New("apihandler: Orchestrator 不能为 nil")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	orch := cfg.Orchestrator
	return &Handler{
		// Each of the ten domains takes its own narrow interface. **Only these ten lines touch the orchestration layer**;
		// after that, each handler file can only see its own domain.
		workspace:  orch.Workspace(),
		agent:      orch.Agent(),
		skill:      orch.Skill(),
		ark:        orch.Ark(),
		issue:      orch.Issue(),
		suggestion: orch.Suggestion(),
		sandbox:    orch.Sandbox(),
		revision:   orch.Revision(),

		log:       log,
		devRoutes: cfg.DevRoutes,
		// cfg.Integrations being nil is valid (no integration source connected); registry() handles it.
		integrations: cfg.Integrations,

		hosts:      cfg.Hosts,
		hostDaemon: cfg.HostDaemon,
		userFiles:  cfg.UserFiles,
	}, nil
}

// Register mounts this module's routes on the mux.
//
// Paths come in two groups, same as the feishu package: those with a workspace are collection operations, and operations on a single resource take only
// its own id. The latter matches the existing shape of the frontend ApiClient (item-level methods pass only one opaque id).
//
// Note that the 7 Feishu routes under /api/feishu/ and /api/workspaces/{id}/feishu/ are
// registered by the feishu package; this package does not touch them.
// Mux is the route registration capability Register needs. It is now an **alias** of the interface with the same name in the orchestration layer.
//
// The full reasoning (why not hard-code *http.ServeMux, why auth must be wrapped per route) moved to
// orchestrator/routes.go, where this interface is defined. Only what users need to know is kept here:
// the mux passed in is a decorator wrapped with authentication and authorization, so the dozens of route registrations in this file
// need no change at all, and every route is protected.
//
// ⚠️ **It must be an alias (with `=`), not a new type.** Go method sets match exactly: if written as
// `type Mux orchestrator.Mux`, then `Register(mux Mux)` no longer satisfies
// `orchestrator.Routes`, and the error is "have Register(apihandler.Mux) want
// Register(orchestrator.Mux)": the two names look the same, and the only difference is that equals sign.
//
// ⚠️ This package importing the orchestration layer is **allowed**; the other way around is not (if the orchestration layer imports this package,
// things it is meant to hide show up in `go list -deps ./orchestrator`, and this layer becomes pointless).
// For direction, see the file header of orchestrator/routes.go.
type Mux = orchestrator.Mux

func (h *Handler) Register(mux Mux) {
	// Workspaces
	mux.HandleFunc("GET /api/workspaces", h.listWorkspaces)
	mux.HandleFunc("POST /api/workspaces", h.createWorkspace)
	// Looking up a workspace by key and checking whether a key is taken both use query params, **not** path segments.
	//
	// It was first written as /api/workspaces/by-key/{key}, and the standard library mux panicked at startup: it and
	// /api/workspaces/{workspaceId}/agents "both match /api/workspaces/by-key/agents
	// but neither is more specific". Go 1.22's mux **refuses to register** such ambiguity instead of falling back to
	// registration order (stricter than Hertz, where the same shapes can coexist).
	// Query params remove this whole class of conflicts at the root, and we also do not have to worry about URL escaping in keys.
	mux.HandleFunc("GET /api/workspace-by-key", h.getWorkspaceByKey)
	mux.HandleFunc("GET /api/workspace-key-taken", h.keyTaken)
	mux.HandleFunc("PATCH /api/workspaces/{workspaceId}", h.updateWorkspace)
	mux.HandleFunc("DELETE /api/workspaces/{workspaceId}", h.deleteWorkspace)

	// Members
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/members", h.addMember)
	mux.HandleFunc("PATCH /api/workspaces/{workspaceId}/members/{memberId}", h.updateMemberRole)
	mux.HandleFunc("DELETE /api/workspaces/{workspaceId}/members/{memberId}", h.removeMember)
	// Agent: 10 routes, all registered in handler_agent.go (the tenth orchestration-layer domain package;
	// the methods were split from workspace.Service into agent.Service).
	h.registerAgentRoutes(mux)

	// "What is my role in this workspace." Used by the frontend to decide "draw the integrations section as editable or read-only".
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/my-role", h.getMyRole)

	// Skills: 16 routes (9 core + 7 market/import), all registered in handler_skill.go.
	// The 7 market routes used to be a separate market domain package (handler_market.go), and were merged into the skill domain
	// together with their methods: they also operate on skills, just "where the skill comes from".
	// The real implementations of all 16 methods are in service/skill (11b moved them out of core, and the arkconsole domain split added
	// the console link), and the whole domain is satisfied by the domain package.
	h.registerSkillRoutes(mux)

	// Issues: 11 routes, all registered in handler_issue.go.
	//
	// It is its own file because one of the routes **returns bytes uploaded by users** (attachment download),
	// whose caveats are completely different from the JSON ones here, and mixed together it is easy to copy as if it were the same kind.
	h.registerIssueRoutes(mux)

	// Product suggestions (Suggestion): 13 routes, all registered in handler_suggestion.go.
	//
	// ⚠️ **This is not the same thing as the Issue group above** (a third kind of "something is wrong", see
	// core/README-suggestion.md). The most important difference: it is **platform-level**; the collection routes
	// are not mounted under a workspace, matrix has only ActionNone, and **all auth is in the Service layer**.
	h.registerSuggestionRoutes(mux)

	// Session container (Docker): view status, resume. Registered in handler_sandbox.go.
	//
	// Its own file for the same reason as the Issue group: these routes hit **live state of an external platform**
	// (they inspect the container every time and do not read the database), so the caveats are completely different from the ones here that read the local database.
	h.registerSandboxRoutes(mux)

	// Pass-through for the raw Ark Managed Agents API (session resources/threads/files/Vaults/memory stores).
	h.registerArkProxyRoutes(mux)

	// Host executor: account-level host list/revoke + workspace binding (authorized mux;
	// for ActionNone/ActionManage see matrix); daemon registration is also on this one.
	// The daemon heartbeat/results are registered with RegisterHostDaemon in the httpd bare mux section.
	h.registerHostsRoutes(mux)
	h.registerHostDaemonRegisterRoute(mux)

	// Account-level personal files (object storage upload/download). All 8 are ActionNone; owner isolation is in
	// the Service/SQL layer (see the file header of handler_userfiles.go).
	h.registerUserFileRoutes(mux)

	// Three levels of Ark credentials (workspace / account / Agent) + getting a key from the cloud account.
	// Registered in handler_ark.go; for why, see the top of that file.
	h.registerArkRoutes(mux)

	// Integration management (multiple integration providers)
	//
	// Deliberately **not mounted under {workspaceId}**: an integration is "this person (or this service) authorizes an external platform",
	// unrelated to the workspace, and the same person sees the same state in any workspace. Mounting it under the workspace would suggest "each workspace
	// needs its own authorization", which is a false mental model.
	//
	// ⚠️ /api/integrations and /api/integrations/{provider} coexisting **does not conflict**: Go 1.22
	// ServeMux only refuses when two patterns are "neither more specific than the other", and here the former is the parent path of the latter,
	// with a different number of segments, so any concrete URL can only match one of them. What would really panic is
	// /api/integrations/{provider} and /api/integrations/authorize, a "literal segment vs
	// wildcard segment" at the same level. That is why authorize is mounted under {provider} instead of at the same level.
	mux.HandleFunc("GET /api/integrations", h.listIntegrations)
	mux.HandleFunc("GET /api/integrations/{provider}", h.getIntegration)
	mux.HandleFunc("POST /api/integrations/{provider}/authorize", h.authorizeIntegration)
	// Poll "has the user clicked approve yet". **The device code flow has no callback**, so we can only ask,
	// see integrationsvc.IntegrationRegistry.PollAuthorization.
	//
	// GET instead of POST: it is read-only (ask for the status once), and the frontend calls it repeatedly at pollInterval,
	// while POST would make "calling repeatedly" look like creating things repeatedly.
	// ⚠️ It does have a side effect (the successful call stores the credential), but it is **idempotent**:
	// once a device_code has been exchanged for a token, the intermediate state is deleted, and asking again only says "no pending authorization".
	mux.HandleFunc("GET /api/integrations/{provider}/authorization", h.pollIntegrationAuthorization)
	// "Which app this source can pick / switch to another". **Only Feishu user authorization has this concept**
	// (one person may have registered several Feishu apps, and a token is tied to an app);
	// the other four sources get 400 on these two routes, see integrationsvc.IntegrationRegistry.ListApps.
	//
	// ⚠️ Why two separate routes instead of adding fields to integrationsvc.IntegrationState:
	// State's Details are name/value pairs and cannot express the structured table "which ones can be picked / is each one
	// authorized"; forcing it in would make the frontend parse strings, which will drift with zero errors.
	// See the comment on apimodel.LarkUserApp.
	//
	// PUT instead of POST: this is "set the current choice to X", idempotent; submitting the same value again has no side effects.
	mux.HandleFunc("GET /api/integrations/{provider}/apps", h.listIntegrationApps)
	mux.HandleFunc("PUT /api/integrations/{provider}/app", h.selectIntegrationApp)
	mux.HandleFunc("DELETE /api/integrations/{provider}", h.revokeIntegration)

	// Look up display names and avatars for a batch of emails (account-level, unrelated to the workspace).
	//
	// # Why POST and not GET
	//
	// The input is a **batch** of emails. In the query, a page of 20 creators would be 20
	// `?email=` params, six or seven hundred characters, and each proxy layer has its own URL length limit;
	// beyond it you get 414 or **silent truncation**, and the latter makes the last few people's avatars go missing for no visible reason.
	// A body has no such limit.
	//
	// It is read-only (POST for a read-only operation looks odd), but "batch query with POST + body"
	// is common practice, and there is no idempotency concern here: querying twice is exactly the same as once.
	mux.HandleFunc("POST /api/user-profiles", h.resolveUserProfiles)

	// Onboarding (account-level)
	//
	// Same as integration management, **not mounted under {workspaceId}**: "have I seen the onboarding" is a property of the person.
	// Also, a newcomer logging in for the first time may not belong to any workspace yet, and they are exactly the one who needs onboarding;
	// mounting it under a workspace would stop the people who most need onboarding from opening this endpoint.
	//
	// Neither takes **any identity param**: the person is taken from auth.ActorFrom(ctx). See the onboarding.go file header.
	mux.HandleFunc("GET /api/user-onboarding", h.getOnboarding)
	mux.HandleFunc("POST /api/user-onboarding", h.markOnboarding)

	// User preferences (language and appearance). Same as above: account-level, not mounted under {workspaceId}.
	//
	// ⚠️ It is **at the same level as /api/user-onboarding and both are literal segments**, which does not conflict: Go 1.22 ServeMux
	// only refuses when two patterns are "neither more specific than the other", and two different literal paths are not ambiguous.
	mux.HandleFunc("GET /api/user-preferences", h.getUserPreferences)
	mux.HandleFunc("PUT /api/user-preferences", h.saveUserPreferences)

	// Problem feedback.
	//
	// Same as the two groups above, **not mounted under {workspaceId}**: the place where something went wrong may not be inside a workspace at all
	// (the workspace list page, preferences, a newcomer who has not joined any workspace). Mounting it under a workspace would mean those places
	// **cannot report at all**, and they are exactly where newcomers get stuck most.
	//
	// ⚠️ The two routes have completely different permissions; do not assume they are symmetric because they look alike:
	//
	//	POST  any logged-in user can write (the person who hit the problem is the one who most needs to report it)
	//	GET   **only the dev team can read**; the check is in the Service (core.ListFeedback)
	//
	// Why the GET permission is not in Guard: the authz model is "actor + workspace → role", while feedback is
	// cross-workspace, with no workspace to check. In matrix it is ActionNone (there is a whole section explaining it there);
	// the real gate is in the Service, and a test pins down that normal users get 403.
	mux.HandleFunc("POST /api/feedback", h.submitFeedback)
	mux.HandleFunc("GET /api/feedback", h.listFeedback)

	// Run history (read-only)
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/runs", h.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", h.getRun)
	mux.HandleFunc("GET /api/runs/{runId}/events", h.listRunEvents)
	// Where this session is in the Ark console. **run.id is the Ark session id**, so this one
	// can always give an id; region comes from the credential actually in effect. See core/arkconsole.go.
	mux.HandleFunc("GET /api/runs/{runId}/ark-console", h.getRunArkConsole)

	// Usage dashboard (read-only aggregation). Mounted under {workspaceId} with the same shape as /runs, so it does not trigger
	// Go 1.22 ServeMux's ambiguity refusal (a literal segment like /api/workspaces/usage would panic at startup;
	// see the note about looking up a workspace by key near the top of this file).
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/usage", h.workspaceUsage)

	// Chat. One chat is one Run with trigger=manual, so it is mounted under runs instead of
	// a separate /conversations/{id} namespace; see the file header of core/chat.go.
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/conversations", h.createConversation)
	mux.HandleFunc("POST /api/runs/{runId}/events", h.appendRunEvents)
	mux.HandleFunc("GET /api/runs/{runId}/events/stream", h.streamRunEvents)
	// Redirect: interrupt this turn + send a new message right away. See RedirectRun in core/chat.go.
	mux.HandleFunc("POST /api/runs/{runId}/redirect", h.redirectRun)

	// Pending messages in the queue. **A server-side queue**, see core/pendingmessage.go.
	//
	// ⚠️ This is not the same as "automatic resend when hitting the tool execution window": that one is a race of tens of milliseconds,
	// retried in place by the server, and does not go through these routes.
	mux.HandleFunc("GET /api/runs/{runId}/pending", h.listPendingMessages)
	mux.HandleFunc("POST /api/runs/{runId}/pending", h.enqueuePendingMessage)
	mux.HandleFunc("DELETE /api/runs/{runId}/pending/{messageId}", h.cancelPendingMessage)

	// Schedules
	//
	// The collection is under workspace and a single resource takes only its own id, same as the domains above.
	// Enable/disable is its own PATCH .../status instead of being merged into PATCH {id}: the frontend toggle is a separate
	// action (one click takes effect, without the other form fields). Going through a full update would mean assembling the full draft first,
	// and the list page does not have it.
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/schedules", h.listSchedules)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/schedules", h.createSchedule)
	mux.HandleFunc("GET /api/schedules/{id}", h.getSchedule)
	mux.HandleFunc("PATCH /api/schedules/{id}", h.updateSchedule)
	mux.HandleFunc("PATCH /api/schedules/{id}/status", h.updateScheduleStatus)
	mux.HandleFunc("DELETE /api/schedules/{id}", h.deleteSchedule)
	mux.HandleFunc("POST /api/schedules/{id}/run-now", h.runScheduleNow)

	// Entity revision history (three routes each for agent / skill / schedule).
	// Mounted under each entity instead of one shared route, for authorization reasons; see the handler_revision.go file header.
	h.registerRevisionRoutes(mux)

	if h.devRoutes {
		mux.HandleFunc("POST /api/_debug/reset", h.debugReset)
		mux.HandleFunc("POST /api/_debug/clear", h.debugClear)
	}

	// Fallback: make unmatched /api/ paths also return a structured envelope instead of the standard library's text/plain.
	// See the note about "404 → null" at the top of this file.
	mux.HandleFunc("/api/", h.notFound)
}

// -- Workspaces -------------------------------------------------------------------

// listWorkspaces returns only the workspaces the current logged-in user is in.
//
// **It uses ListWorkspacesFor, not ListWorkspaces**: the latter does not filter, and is for system paths like the worker pool
// that have no notion of a "current user". Mixing them up would list other people's workspaces to the user,
// and it would look completely normal, just with a few workspaces that should not be there.
func (h *Handler) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	list, err := h.workspace.ListWorkspacesFor(r.Context(), auth.ActorFrom(r.Context()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// getWorkspaceByKey gets a workspace by URL key. The key goes in ?key= instead of a path segment, see the comment on Register.
func (h *Handler) getWorkspaceByKey(w http.ResponseWriter, r *http.Request) {
	ws, err := h.workspace.GetWorkspaceByKey(r.Context(), r.URL.Query().Get("key"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// keyTaken returns {"taken": bool}.
//
// A missing exceptId behaves the same as an empty string (both mean "exclude no workspace"), which matches the frontend
// isWorkspaceKeyTaken(key, exceptId?). An invalid key returns false instead of 400:
// the frontend checks the format itself before submitting, and a 400 here would be caught and shown as a field error message.
func (h *Handler) keyTaken(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	taken, err := h.workspace.IsWorkspaceKeyTaken(r.Context(), q.Get("key"), q.Get("exceptId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"taken": taken})
}

func (h *Handler) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Key         string `json:"key"`
		Description string `json:"description"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	ws, err := h.workspace.CreateWorkspace(r.Context(), apimodel.WorkspaceInput{
		Name: body.Name, Key: body.Key, Description: body.Description,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

// updateWorkspace takes three fields as pointers, to tell "not sent" apart from "sent an empty string".
//
// A missing description key means **clear it**, see the long comment on Service.UpdateWorkspace.
func (h *Handler) updateWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        *string `json:"name"`
		Key         *string `json:"key"`
		Description *string `json:"description"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	ws, err := h.workspace.UpdateWorkspace(r.Context(), r.PathValue("workspaceId"), apimodel.WorkspacePatch{
		Name: body.Name, Key: body.Key, Description: body.Description,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (h *Handler) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := h.workspace.DeleteWorkspace(r.Context(), r.PathValue("workspaceId")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// -- Members -------------------------------------------------------------------

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Role  string `json:"role"`
		// AvatarURL is carried over by the frontend from the people picker's candidate record, and may be empty.
		// It is only a cache, see apimodel.WorkspaceMember.AvatarURL.
		AvatarURL string `json:"avatarUrl"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	m, err := h.workspace.AddMember(r.Context(), r.PathValue("workspaceId"), apimodel.MemberInput{
		Name: body.Name, Email: body.Email, Role: apimodel.WorkspaceRole(body.Role),
		AvatarURL: body.AvatarURL,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role string `json:"role"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	m, err := h.workspace.UpdateMemberRole(r.Context(),
		r.PathValue("workspaceId"), r.PathValue("memberId"), apimodel.WorkspaceRole(body.Role))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	err := h.workspace.RemoveMember(r.Context(), r.PathValue("workspaceId"), r.PathValue("memberId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// getMyRole returns "what role the current logged-in user has in this workspace".
//
// It uses core.RoleOf, **the same function** authz.Guard uses for permission checks, so "can I
// edit" in the UI and "will the server allow it" never have two truths. ⚠️ The frontend **must not** compute it itself from workspace.members:
// there should be only one set of permission rules, and a second computation in the frontend will sooner or later diverge from the server,
// showing up as a read-only UI while the server actually allows it (or the other way around).
func (h *Handler) getMyRole(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	role, ok, err := h.workspace.RoleOf(ctx, r.PathValue("workspaceId"), auth.ActorFrom(ctx))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Anyone who gets here must be a member (Guard's ActionRead already blocked non-members),
	// so ok being false only happens when "a test hits it directly, with no identity". Return an empty string instead of 404:
	// this endpoint means "tell me my role"; no role means empty, not "resource does not exist".
	if !ok {
		role = ""
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"role": string(role),
		// canManage is computed by the server instead of letting the frontend compare strings: when the frontend check disagrees with
		// authz.levelOf there is no error at all (owner and admin being the same level is the easiest thing to miss).
		"canManage": role == apimodel.RoleOwner || role == apimodel.RoleAdmin,
	})
}

// -- Ark credentials ---------------------------------------------------------------

// -- Run history ---------------------------------------------------------------

// listRuns supports five filter params: agentId / status / search / scheduleId / trigger,
// plus limit / offset pagination.
//
// status accepts both **repeated params** (?status=a&status=b, the natural form of URLSearchParams.append)
// and comma-separated values, because a straightforward HttpApiClient would use the former, while hand-written curl
// is more likely to use the latter. Supporting only one would make the other silently take only the first value. Same for trigger.
//
// scheduleId deliberately **reuses this route** instead of adding `GET /api/schedules/{id}/runs`:
// that would need one more declaration in matrix and one more handler, and the permission check would actually be weaker: this route's
// workspaceId is in the path, so Guard checks by workspace directly; looking up the workspace from the schedule is one more hop.
// The filter axis is the same kind as agentId, and putting it in the same query also fits the shape of the frontend RunQuery.
//
// # ⚠️ The default limit is "no limit", the opposite of listFeedback / listSuggestions
//
// For those two, `Limit <= 0` uses the default of 50, because they were paginated from day one. This one cannot:
// it already had 6 callers that expect the full result, and a default of 50 would **silently truncate their results**:
// usage stats would undercount and schedule history would list fewer, while the page still shows a number, just a wrong one.
// So the default stays "everything", and callers that want pages pass limit explicitly. The upper bound is still enforced by the Service.
func (h *Handler) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	statuses := make([]apimodel.RunStatus, 0)
	for _, v := range multiValue(q, "status") {
		statuses = append(statuses, apimodel.RunStatus(v))
	}
	triggers := make([]apimodel.RunTrigger, 0)
	for _, v := range multiValue(q, "trigger") {
		triggers = append(triggers, apimodel.RunTrigger(v))
	}
	query := persistmodel.RunQuery{
		AgentID:    q.Get("agentId"),
		Status:     statuses,
		Search:     q.Get("search"),
		ScheduleID: q.Get("scheduleId"),
		Trigger:    triggers,
	}
	// A number that cannot be parsed is treated as not passed (the Service clamps it), with no error: one mistyped limit should not
	// fail the whole list. Same as in listFeedback.
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil {
		query.Limit = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("offset"))); err == nil {
		query.Offset = n
	}

	list, err := h.workspace.ListRuns(r.Context(), r.PathValue("workspaceId"), query)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.workspace.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// getRunArkConsole returns where this entity is in the Ark console (id + region), so the frontend can
// build a deep link. The agent and skill versions have the same shape, in handler_agent.go / handler_skill.go.
//
// **When the Ark side has not been created yet it returns an empty ref, not 404**: that is the normal lazy-creation state (the agent
// never had a chat, the skill was never used by any agent), and the frontend then does not render the link. Returning 404 would mix up "normal"
// and "resource does not exist". For the full reason see the file header of core/arkconsole.go.
func (h *Handler) getRunArkConsole(w http.ResponseWriter, r *http.Request) {
	ref, err := h.workspace.GetRunArkConsole(r.Context(), r.PathValue("runId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

// workspaceUsage returns a workspace's usage summary, and supports two time window params, since / until
// (fixed-length ISO strings, start inclusive, end exclusive).
//
// **A workspace that does not exist also returns 200 + an all-zero summary**, not 404: the frontend turns not_found into null,
// and the page would become a silent empty state instead of "this workspace has no runs yet". Same as listRuns.
func (h *Handler) workspaceUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sum, err := h.workspace.WorkspaceUsage(r.Context(), r.PathValue("workspaceId"), persistmodel.UsageQuery{
		Since: q.Get("since"),
		Until: q.Get("until"),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// listRunEvents lists events.
//
// The response body is **raw bytes assembled by hand**, not json.Marshal: the standard library escapes < > & into
// < and so on, while JS JSON.stringify does not. Events embed skill bodies and tool inputs
// (user-editable Markdown / JSON strings), and escaping them would make them differ from Ark's raw data.
//
// When the run does not exist it returns 200 + [], not 404: this frontend method returns an array, not a nullable value.
func (h *Handler) listRunEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	events, err := h.workspace.ListRunEvents(r.Context(), r.PathValue("runId"), persistmodel.EventQuery{
		Types:    multiValue(q, "types"),
		ThreadID: q.Get("threadId"),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(apimodel.AppendJSONArray(events))
}

// -- Chat -------------------------------------------------------------------

// createConversation starts a new chat and returns a Run.
func (h *Handler) createConversation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID string `json:"agentId"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	run, err := h.workspace.CreateConversation(r.Context(), r.PathValue("workspaceId"), body.AgentID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The frontend createConversation returns a Run and does not distinguish 200 / 201.
	writeJSON(w, http.StatusOK, run)
}

// appendRunEvents forwards events sent up by the client to Ark, and returns Ark's "accepted events list".
//
// The request body looks like `{"events":[{"type":"user.message","content":[...]}]}`, and the response is
// a **bare array** (not {data:[...]}): the frontend sendMessage uses `accepted[0]` directly.
//
// The response is also hand-assembled raw bytes, for the same reason as listRunEvents: the receipt carries the text the user just typed,
// and json.Marshal would escape < > &, while the same message the frontend later receives from the event stream is not escaped,
// so comparing them gives two different messages (appendLocal dedupes by id; if it cannot dedupe, it draws twice).
func (h *Handler) appendRunEvents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events []json.RawMessage `json:"events"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	accepted, err := h.workspace.AppendRunEvents(r.Context(), r.PathValue("runId"), body.Events)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rawJSONArray(accepted))
}

// listPendingMessages lists the queued messages on this session that have not been sent yet.
//
// **List endpoints never return 404** (see this file's header): when the run does not exist it returns 200 + [];
// returning 404 would make the page throw instead of showing an empty list.
func (h *Handler) listPendingMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := h.workspace.ListPendingMessages(r.Context(), r.PathValue("runId"))
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			writeJSON(w, http.StatusOK, []apimodel.PendingMessage{})
			return
		}
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// enqueuePendingMessage adds a message to the end of the queue and returns the whole queue afterwards.
//
// It returns the whole queue instead of just the new one: the frontend shows "item 2/3",
// and with only one item returned it would have to fetch again.
func (h *Handler) enqueuePendingMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	msgs, err := h.workspace.EnqueuePendingMessage(r.Context(), r.PathValue("runId"), body.Text)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// cancelPendingMessage cancels a queued message.
//
// **Idempotent**: not found also returns 204. Users clicking cancel twice is common, and a "does not exist" error on the second click
// would only make them think something went wrong.
func (h *Handler) cancelPendingMessage(w http.ResponseWriter, r *http.Request) {
	err := h.workspace.CancelPendingMessage(
		r.Context(), r.PathValue("runId"), r.PathValue("messageId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// redirectRun interrupts this turn and sends a new message right away ("redirect").
//
// It is split from appendRunEvents into two routes instead of adding an `?interrupt=1` param: their permission levels
// are the same but their meaning differs, and the authz table is declared by "method + path". **Mixed into one route,
// there would be no way to give redirect its own level later** (same precedent as splitting the sandbox terminal up/downstream into two routes).
//
// The response shape matches appendRunEvents (bare array), so the frontend can share one parser for both.
func (h *Handler) redirectRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	accepted, err := h.workspace.RedirectRun(r.Context(), r.PathValue("runId"), body.Text)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rawJSONArray(accepted))
}

// streamRunEvents relays Ark's event stream to the browser.
//
// # Three things that must be right
//
// **One: Flush after every frame.** Without flush, net/http waits until the buffer fills before sending,
// so the typewriter effect becomes "hold for a while, then dump a big chunk", and long replies may even wait until the stream ends.
//
// **Two: there must be a heartbeat.** Proxies in between cut connections that send no bytes for a long time. Ark itself sends
// `: heartbeat` comment lines, and we do the same. The SSE protocol says lines starting with `:` are comments;
// EventSource ignores them automatically, so they never become a junk frame.
//
// **Three: identity goes through the Cookie, not the query.** EventSource cannot send custom request headers, but for same-origin requests
// it sends the Cookie automatically, and the login credential is exactly an HttpOnly Cookie. So this path gets auth for free,
// through the same auth.Protect as other endpoints, with no special handling.
// **Do not move identity into the query just because "EventSource cannot send headers"** (?actor= and the like): that means writing the credential
// into the URL, which ends up in browser history and server access logs, and the query is something the caller can change freely.
//
// # Four: a missing run must be stopped before writing headers, with a real 404
//
// This one was forced by production logs. **Read it all before changing it.** The old approach was: no pre-check; wait for
// StreamRunEvents to return persistmodel.ErrNotFound, then tell the frontend via "one session.error frame + [DONE]".
// The reasoning at the time was "really blocking it needs a pre-check before the handler writes headers; not worth it". **That reasoning was wrong.**
//
// Why: a missing run is a **permanent** error, and retrying will never succeed. But the shape 200 + in-stream error
// cannot say "stop coming back": at the HTTP layer it looks exactly like "connected, then dropped midway". So on the frontend,
// every re-mount (coming back to the tab, a React effect re-running, the user clicking a dead session in history)
// comes back again, and the server logs an ERROR each time. Measured: the same dead runId was subscribed 3 times within 6 seconds,
// each costing under 1ms (seen in the local amad log).
//
// So now we pre-check with RunExists first: if the run is not there, go through h.fail and return 404 + code:"not_found",
// the same shape as other endpoints, so the frontend request() throws ApiError instead of treating it as an in-stream event.
// Using RunExists instead of GetRun is intentional; for why, see the comment in chat.go.
//
// ⚠️ The pre-check **does not block everything**: the narrow window where the run is deleted after the pre-check passes but before the stream opens still reaches
// the in-stream error branch below. That branch is kept for this reason; it is not redundant.
func (h *Handler) streamRunEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Reaching here means a middleware wrapped a ResponseWriter that does not pass Flush through.
		// This is not something the user can fix, so honestly return 500 instead of pretending to succeed. If it silently degraded,
		// the page would spin forever, and nothing in the symptoms would point to the cause.
		h.log.ErrorContext(r.Context(), "ResponseWriter 不支持 Flush，SSE 无法工作")
		h.failWith(w, http.StatusInternalServerError, "服务端不支持流式响应", "internal")
		return
	}

	// Existence pre-check. **It must happen before WriteHeader below**: once headers are sent, the status code can no longer change.
	if err := h.workspace.RunExists(r.Context(), r.PathValue("runId")); err != nil {
		// A client subscribing with a dead URL is an expected user-input problem, not a server failure,
		// so it is WARN, not ERROR: any browser that kept an old link can trigger this path,
		// and ERROR would flood the alerts, burying the real stream interruptions.
		h.log.WarnContext(r.Context(), "订阅了不存在的运行",
			slog.String("run", r.PathValue("runId")))
		h.fail(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Turn off buffering in reverse proxies like nginx, or they would collect the whole stream before sending.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// Frame writes must be serialized: the heartbeat comes from another goroutine and competes with event frames for the same ResponseWriter.
	var mu sync.Mutex
	write := func(payload []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if _, err := w.Write(payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// Heartbeat. The interval is 15 seconds: much shorter than the common 60-second proxy timeout, without flooding the logs.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-heartbeat.C:
				if err := write([]byte(": heartbeat\n\n")); err != nil {
					return
				}
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	err := h.workspace.StreamRunEvents(ctx, r.PathValue("runId"), func(raw []byte) error {
		for _, frame := range clientFrames(raw) {
			if err := write(frame); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil && ctx.Err() == nil {
		// Headers are already sent and the status code cannot change, so tell the frontend the error as a frame in the stream.
		// Frames the frontend does not recognize are ignored as events, so we use the session.error shape it knows.
		//
		// Levels: reaching here with persistmodel.ErrNotFound can only be the narrow window "the run was deleted after the pre-check passed",
		// the same cause as the pre-check above: the client is holding a dead id, so log WARN.
		// **Everything else stays ERROR**: Ark dropping the stream or a database write blowing up are real failures, and downgrading would hide them.
		if errors.Is(err, persistmodel.ErrNotFound) {
			h.log.WarnContext(ctx, "订阅期间运行已消失",
				slog.String("run", r.PathValue("runId")), slog.String("error", err.Error()))
		} else {
			h.log.ErrorContext(ctx, "对话流中断",
				slog.String("run", r.PathValue("runId")), slog.String("error", err.Error()))
		}
		_ = write(sseData(errorFrame(userMessage(err))))
	}
	// Normal finish: tell the frontend this turn is over. When the frontend gets [DONE] it closes the EventSource;
	// without it, the frontend would think the connection dropped and reconnect, re-subscribing to a session that already finished.
	_ = write(sseData([]byte("[DONE]")))
}

// frameType returns the top-level type of a frame. Returns an empty string if it cannot.
//
// ⚠️ **core/chat.go has a function with the same name and body**; this is a copy, because this package does not
// import core (that was the whole point of this move). It was not made a shared function because the two places use it
// to decide **different things**: core decides "is this a delta frame" (whether to store it),
// and here it decides "which kind of frontend frame to turn it into". It is only one json.Unmarshal of a top-level field,
// with no business logic that could drift; the real knowledge about shapes is in the comment on clientFrames below.
func frameType(raw []byte) string {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	return env.Type
}

// clientFrames translates one Ark frame into frames the frontend understands, 0 or 1 frames.
//
// # Ark's frame shape is **different** from what the frontend wants, so it must be translated here
//
// This was measured (raw output captured with curl against the real Ark), not inferred from the docs. Ark sends:
//
//	{"type":"event_start","event":{"id":"sevt-x","type":"agent.message"}}
//	{"type":"event_delta","event_id":"sevt-x","delta":{"type":"content_delta","index":0,
//	                                                   "content":{"type":"text","text":"Hi"}}}
//
// while the frontend parseFrame (http.ts:210) expects a shape with **kind at the top level**:
//
//	{"kind":"event_start","id":"sevt-x","type":"agent.message","session_thread_id":null}
//	{"kind":"event_delta","id":"sevt-x","text":"Hi"}
//
// Three differences: the discriminator field is kind, not type; id is at the top level, not nested in event; the text is at the top level, not
// buried in delta.content.text. **If Ark's raw output were passed through directly**, parseFrame would reach its last
// fallback `return { kind: "event", event: raw }`, so every delta frame would be treated as a full event
// and pushed into the timeline: the UI would show dozens of empty messages, while the real reply would not show at all.
//
// Final events (agent.message and so on) have no kind, and taking parseFrame's fallback branch is exactly right; pass them through as is.
func clientFrames(raw []byte) [][]byte {
	switch frameType(raw) {
	case "event_start":
		var f struct {
			Event struct {
				ID              string  `json:"id"`
				Type            string  `json:"type"`
				SessionThreadID *string `json:"session_thread_id"`
			} `json:"event"`
		}
		if err := json.Unmarshal(raw, &f); err != nil || f.Event.ID == "" {
			return nil
		}
		out, err := json.Marshal(map[string]any{
			"kind":              "event_start",
			"id":                f.Event.ID,
			"type":              f.Event.Type,
			"session_thread_id": f.Event.SessionThreadID,
		})
		if err != nil {
			return nil
		}
		return [][]byte{sseData(out)}

	case "event_delta":
		var f struct {
			EventID string `json:"event_id"`
			Delta   struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(raw, &f); err != nil || f.EventID == "" {
			return nil
		}
		// Empty text frames are still sent: the frontend accumulates by id, and an empty string is a harmless no-op,
		// while picking and choosing here might miss delta shapes added in the future.
		out, err := json.Marshal(map[string]any{
			"kind": "event_delta",
			"id":   f.EventID,
			"text": f.Delta.Content.Text,
		})
		if err != nil {
			return nil
		}
		return [][]byte{sseData(out)}

	default:
		// Final event: **pass through as is**, not a single byte changed.
		//
		// These bytes must be byte-for-byte identical to what listRunEvents replays. The frontend draft mechanism is
		// "replace the draft with the final event by id", and if the two paths give different bytes, it shows as two messages.
		// Marshaling again would reorder keys and escape < > &, which is exactly this trap.
		return [][]byte{sseData(raw)}
	}
}

// sseData wraps a payload into one SSE frame.
//
// Each newline in the payload must get a `data: ` prefix per line: a bare newline is taken by the SSE parser as the end of the frame,
// and the second half becomes the start of the next frame. Ark's event JSON is single-line today, but we cannot bet on that.
func sseData(payload []byte) []byte {
	var buf bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimRight(payload, "\n"), []byte("\n")) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

// errorFrame builds a session.error event, used to tell the frontend why the stream was interrupted.
//
// The shape matches SessionErrorEvent (types/events.ts): the frontend renders it as
// an error in the timeline. sequence is 0 and id has a local prefix, to tell it apart from real Ark events.
func errorFrame(message string) []byte {
	out, err := json.Marshal(map[string]any{
		"id":                fmt.Sprintf("sevt-local-err-%d", time.Now().UnixNano()),
		"type":              "session.error",
		"sequence":          0,
		"processed_at":      apimodel.NowISO(),
		"session_thread_id": nil,
		"error": map[string]any{
			"type":    "unknown_error",
			"message": message,
		},
	})
	if err != nil {
		return []byte(`{"type":"session.error","error":{"type":"unknown_error","message":"对话流中断"}}`)
	}
	return out
}

// rawJSONArray joins a set of raw JSON values into array bytes.
//
// Same reason as apimodel.AppendJSONArray: avoid the standard library's HTML escaping.
func rawJSONArray(items []json.RawMessage) []byte {
	buf := make([]byte, 0, 64)
	buf = append(buf, '[')
	for i, item := range items {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, item...)
	}
	return append(buf, ']')
}

// -- Schedules ---------------------------------------------------------------

func (h *Handler) listSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := h.workspace.ListSchedules(r.Context(), r.PathValue("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getSchedule(w http.ResponseWriter, r *http.Request) {
	sch, err := h.workspace.GetSchedule(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sch)
}

func (h *Handler) createSchedule(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.ScheduleDraft
	if !h.decode(w, r, &draft) {
		return
	}
	sch, err := h.workspace.CreateSchedule(r.Context(), r.PathValue("workspaceId"), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sch)
}

func (h *Handler) updateSchedule(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.ScheduleDraft
	if !h.decode(w, r, &draft) {
		return
	}
	sch, err := h.workspace.UpdateSchedule(r.Context(), r.PathValue("id"), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sch)
}

// updateScheduleStatus enables or disables a schedule.
//
// Its own route instead of being merged into updateSchedule: the toggle on the list page takes effect with one click, and there is no full
// draft at hand. The body has only one status field.
func (h *Handler) updateScheduleStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status apimodel.ScheduleStatus `json:"status"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	sch, err := h.workspace.SetScheduleStatus(r.Context(), r.PathValue("id"), body.Status)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sch)
}

func (h *Handler) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := h.workspace.DeleteSchedule(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// runScheduleNow does a trial run right away.
//
// **It returns 200 + the whole Schedule instead of 202**: after the click the frontend needs to update that row's
// nextRunAt in the list right away (showing "about to run"), and returning the whole entity saves a lookup.
// The real execution is picked up by the scheduler on its next tick, see the comment on Service.RunScheduleNow.
func (h *Handler) runScheduleNow(w http.ResponseWriter, r *http.Request) {
	sch, err := h.workspace.RunScheduleNow(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sch)
}

// -- dev only ---------------------------------------------------------------

func (h *Handler) debugReset(w http.ResponseWriter, r *http.Request) {
	if err := h.workspace.Reset(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

func (h *Handler) debugClear(w http.ResponseWriter, r *http.Request) {
	if err := h.workspace.Clear(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// notFound makes unmatched /api/ paths also return a structured envelope.
//
// Without it, the standard library returns text/plain "404 page not found", and HttpApiClient's
// "404 → null" rule would treat it as "resource does not exist", so a wrongly mounted route shows as a silent empty state.
// A dedicated code is given here, to tell it apart from a real not_found.
func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.log.WarnContext(r.Context(), "请求了不存在的接口",
		slog.String("method", r.Method), slog.String("path", r.URL.Path))
	writeJSON(w, http.StatusNotFound, map[string]any{
		"message": "接口不存在",
		"code":    "route_not_found",
	})
}

// -- Common -------------------------------------------------------------------

// fail maps business errors to HTTP responses.
//
// message is a Chinese sentence for the user (the frontend shows it directly), and code is a stable identifier for programs to check.
// Errors returned by the Service already carry the Chinese message, so we pass it straight through, but only for
// **known sentinel errors**. Unknown errors only go to the log, so internal details are not exposed to the user.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	// Business errors with a stable detailed code (Ark credential save checks: bad key / account not activated) are answered by that code first:
	// they also wrap ErrPrecondition (422), but the frontend relies on code to tell a "hard error" from
	// "can be saved after confirming", and a single precondition_failed could not tell them apart.
	var codeCarrier interface{ FailCode() string }
	if errors.As(err, &codeCarrier) {
		h.failWith(w, http.StatusUnprocessableEntity, userMessage(err), codeCarrier.FailCode())
		return
	}
	switch {
	case errors.Is(err, persistmodel.ErrInvalidArgument):
		h.failWith(w, http.StatusBadRequest, userMessage(err), "invalid_argument")
	case errors.Is(err, integrationsvc.ErrPollUnsupported):
		// 400 instead of 500: this is "you asked the wrong thing" (asking a source with no device code flow
		// "has the user clicked approve yet"), a caller problem, not a server failure.
		// Falling into 500 would make the frontend show "internal server error" and report it, when nothing is actually broken.
		h.failWith(w, http.StatusBadRequest, userMessage(err), "poll_unsupported")
	case errors.Is(err, integrationsvc.ErrAppsUnsupported):
		// Same as above, also "you asked the wrong thing" (asking a source with only one identity "which apps can be picked").
		//
		// ⚠️ What happens if this one is missing: ListApps/SelectApp have long returned this sentinel,
		// and the comment in integration.go also says "→ 400", but it was not in this switch,
		// so it fell into default: **500 + an ERROR log**. Every source that does not support picking an app behaves like that,
		// so each time the integration management page opens it logs a few errors,
		// while **nothing on the page looks wrong**, so nobody notices; by the time a real failure happens, that real error
		// is already buried under this noise.
		h.failWith(w, http.StatusBadRequest, userMessage(err), "apps_unsupported")
	case errors.Is(err, integrationsvc.ErrWorkspaceScopeUnsupported):
		// Same as above, again "you asked the wrong thing": naming a workspace for a source that does not split apps by workspace.
		//
		// ⚠️ **It must be in this switch**, or it falls into default → 500 + ERROR log.
		// The whole point of this sentinel is "no silent fallback"; turning it into 500 would disguise a
		// clear caller error as a server failure, and the frontend would report it too.
		h.failWith(w, http.StatusBadRequest, userMessage(err), "workspace_scope_unsupported")
	case errors.Is(err, persistmodel.ErrNotFound):
		h.failWith(w, http.StatusNotFound, userMessage(err), "not_found")
	case errors.Is(err, persistmodel.ErrConflict):
		h.failWith(w, http.StatusConflict, userMessage(err), "conflict")
	case errors.Is(err, persistmodel.ErrPrecondition):
		h.failWith(w, http.StatusUnprocessableEntity, userMessage(err), "precondition_failed")
	case errors.Is(err, persistmodel.ErrAccessDenied):
		// 403 instead of 401: the identity is valid, they are just not allowed to do this. Returning 401 would make the frontend
		// jump to the login page, and the user would log in again and hit the same wall.
		h.failWith(w, http.StatusForbidden, userMessage(err), "access_denied")
	default:
		h.log.ErrorContext(r.Context(), "接口处理失败",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		h.failWith(w, http.StatusInternalServerError, "服务内部错误，请稍后重试", "internal")
	}
}

func (h *Handler) failWith(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, map[string]any{"message": message, "code": code})
}

// userMessage extracts the part of the error meant for the user.
//
// Errors in the Service look like `fmt.Errorf("%w: <Chinese message>", persistmodel.ErrNotFound)`, and the wrapping prefix is
// for programs, like "core: ...". Here the prefix is cut off, keeping only the Chinese sentence after the colon.
// If nothing can be extracted, a fallback message is used. Never show something like "core: <resource not found>: xxx" to the user as is.
func userMessage(err error) string {
	msg := err.Error()
	// Looks like "core: <resource not found>: <workspace not found>"; take the last part
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		if tail := strings.TrimSpace(msg[i+2:]); tail != "" {
			return tail
		}
	}
	return "请求无法完成"
}

// decode decodes the JSON request body. If it cannot, it replies 400 and returns false, and the caller returns right away.
func (h *Handler) decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.Body == nil {
		h.failWith(w, http.StatusBadRequest, "请求体不能为空", "invalid_payload")
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		h.failWith(w, http.StatusBadRequest, "请求格式不正确", "invalid_payload")
		return false
	}
	return true
}

// maxProfileEmails is the limit on how many emails can be looked up at once.
//
// There is a limit because any logged-in user can call this endpoint (ActionNone), and the input length is decided by the caller;
// without a cap, one request could make the server run `= any($1)` with tens of thousands of emails.
//
// 200 was chosen: one page of the agent list has at most a few dozen distinct creators, and it is enough for a workspace with hundreds of members.
// If it really is exceeded, **one more call is better than truncation**: truncation gives zero errors, and the last few people's avatars would
// go missing for no visible reason, which looks like "these people never logged in". So when exceeded, return 400 and say so clearly.
const maxProfileEmails = 200

// resolveUserProfiles looks up display names and avatars for a batch of emails.
func (h *Handler) resolveUserProfiles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Emails []string `json:"emails"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if len(body.Emails) > maxProfileEmails {
		h.failWith(w, http.StatusBadRequest, "一次最多查 200 个用户", "too_many_emails")
		return
	}
	profiles, err := h.workspace.ResolveUserProfiles(r.Context(), body.Emails)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Return a map (email → profile) instead of an array: the caller gets a lookup table and does not need to convert it.
	// **Emails that are not found are not in the map**. That is normal (that person never logged in),
	// and the frontend then falls back to initials, see apimodel.UserProfile.
	//
	// A nil map encodes as null while the response contract requires {}, so handle it once: on the frontend,
	// `profiles[email]` on null throws a TypeError right away.
	if profiles == nil {
		profiles = map[string]apimodel.UserProfile{}
	}
	writeJSON(w, http.StatusOK, profiles)
}

// writeJSON writes a JSON response.
//
// HTML escaping is turned off: JS JSON.stringify does not turn < > & into <, while the standard library does by default.
// Skill bodies are user-editable Markdown, and escaping would make them differ from the frontend localStorage version of the data.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

// writeNoContent writes a 204.
//
// A successful DELETE returns 204 **without a body**. On the HttpApiClient side, do not call res.json() on 204
// unconditionally: an empty body throws, turning every successful delete into an error popup.
func writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// multiValue reads a query param that may be repeated or comma-separated.
//
// Both forms are supported: ?types=a&types=b and ?types=a,b. The former is the natural result of URLSearchParams.append
// (HttpApiClient sends it that way), and the latter is handier when writing URLs by hand.
func multiValue(q map[string][]string, key string) []string {
	out := make([]string, 0)
	for _, raw := range q[key] {
		for _, part := range strings.Split(raw, ",") {
			if v := strings.TrimSpace(part); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// ---- Integration management (provider registry) ----
//
// When the registry is empty, the four routes **still work**, returning an empty list / 404 instead of 500. For why, see
// the comment on Handler.integrations.

// listIntegrations lists the status of all integration sources.
//
// # ⚠️ Optional `?workspaceId=`: it decides "which app the Feishu card checks status against"
//
// Not passed = account-level view (the integration management page path; behavior is exactly the same as before).
// Passed = computed for that workspace. **The agent form must pass it**, because the "Start authorization"
// button on the same page also authorizes per workspace. If the two views disagree, the user falls into a dead end: authorization succeeds,
// the token really is stored, but the card checks against another app and always shows "not authorized",
// so they authorize again and again, succeeding every time, and every time the card says not authorized (**HTTP 200 on the whole path**).
// See core.LarkUserProvider.StateForWorkspace.
func (h *Handler) listIntegrations(w http.ResponseWriter, r *http.Request) {
	// List endpoints never return 404: with no provider registered, return 200 + [], and the frontend shows an empty state.
	// Returning 404 would make the page throw instead of showing "no integration source available yet".
	states, err := h.registry().List(r.Context(), r.URL.Query().Get("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// A nil slice would encode as null while the response contract requires [], so handle it once. If the frontend gets null it would
	// crash at .map, showing a blank screen for the whole tab.
	if states == nil {
		states = []integrationsvc.IntegrationState{}
	}
	writeJSON(w, http.StatusOK, states)
}

// getIntegration returns the status of one integration source.
//
// `?workspaceId=` same as listIntegrations. **After authorization succeeds, the frontend only refetches this one**
// (onAuthorized in useIntegrationAuthorize), so if it is not passed here, "right after authorizing"
// would query again by the account default, and the card would jump back to "not authorized" on the spot.
func (h *Handler) getIntegration(w http.ResponseWriter, r *http.Request) {
	state, err := h.registry().State(
		r.Context(), r.PathValue("provider"), r.URL.Query().Get("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// authorizeIntegration starts an authorization and returns the URL to open in a new tab.
//
// # ⚠️ Optional `?workspaceId=`: it decides "which Feishu app is being authorized"
//
// Not passed = account-level view (the integration management page path; behavior is exactly the same as before).
// Passed = pick the app by that workspace. **The in-place "Start authorization" button on the agent form must pass it**,
// because the agent belongs to a workspace, the group messages it reads are received by that workspace's Feishu app,
// and a token is tied to an app. Without it, the account-level default app would be authorized,
// and then at runtime it reports "user authorization has not been done for Feishu app cli_xxx". **That message does not point to
// "you authorized the wrong one"** (which is exactly the bug this change fixed).
//
// ⚠️ **It goes in the query, not the request body**, for two reasons:
//  1. The polling one (pollIntegrationAuthorization) is GET with no request body, and the two **must
//     resolve the same app, or the whole flow dies**. Expressing it in the same shape makes it obvious whether both are wired.
//  2. h.decode returns 400 when there is no request body, and the existing frontend call **sends no request body at all**.
//
// ⚠️ The workspace comes from the query, and **authz patterns cannot express it**, so Guard cannot check it:
// the member check is in the Service layer (core.LarkUserProvider.ensureWorkspaceMember),
// same as `GET /api/workspace-by-key`. See those two comments in authz/matrix.go.
func (h *Handler) authorizeIntegration(w http.ResponseWriter, r *http.Request) {
	url, err := h.registry().AuthorizeURL(
		r.Context(), r.PathValue("provider"), r.URL.Query().Get("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Return only the URL, no 302: the authorization page must open in a **new tab**, so after clicking the user comes back to this page to see the status.
	// A backend redirect would replace the current page, and the user would have to find their way back.
	writeJSON(w, http.StatusOK, map[string]any{"authorizationUrl": url})
}

// pollIntegrationAuthorization checks once "has the user clicked approve yet" (for the device code flow).
//
// **"Still waiting" returns 200 + pending:true, not an error code.** The frontend keeps polling based on it;
// a 4xx would make it stop, and then after the user clicks approve the page never refreshes,
// and that failure looks like "authorization did not succeed", pointing in a completely wrong direction.
//
// ⚠️⚠️ `?workspaceId=` **must be exactly the same as the one passed when starting the authorization**. Starting records device_code on
// `(actor, app_id)`, and polling must use the same app_id to find that record again. If the two disagree,
// GetLarkAuthSession finds nothing → returns "this authorization flow has expired", and its pending is false,
// so the frontend treats it as **final** and stops polling on the first tick. The user then sees "expired" within 3 seconds of
// clicking authorize, **even though starting succeeded**. See core.LarkUserProvider.resolveAppScoped.
func (h *Handler) pollIntegrationAuthorization(w http.ResponseWriter, r *http.Request) {
	status, err := h.registry().PollAuthorization(
		r.Context(), r.PathValue("provider"), r.URL.Query().Get("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// listIntegrationApps lists the apps this source can pick, marking each one's authorization status and which one is current.
//
// ⚠️ **The response has no credentials at all**: it returns apimodel.LarkUserApp, a type that simply has no
// secret / token fields (see the hard rule in the core/integration.go file header).
// app_id and the app name are not secrets: the former is visible to everyone in a Feishu group.
//
// The optional `?workspaceId=` changes the meaning of `current` from "the account-level default" to
// "which one would be authorized when starting authorization **in this workspace**". The agent form uses it to show the target app next to the button;
// without it the user cannot confirm whether they authorized the wrong one, and the error after a wrong authorization does not point to the real cause.
func (h *Handler) listIntegrationApps(w http.ResponseWriter, r *http.Request) {
	apps, err := h.registry().ListApps(
		r.Context(), r.PathValue("provider"), r.URL.Query().Get("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// A nil slice would encode as null while the response contract requires [], same as listIntegrations.
	// If the frontend gets null it would crash at .map, and the whole picker would disappear.
	if apps == nil {
		apps = []apimodel.LarkUserApp{}
	}
	writeJSON(w, http.StatusOK, apps)
}

// selectIntegrationApp switches to another app.
//
// It returns the **full state** after switching instead of 204: switching apps also changes the authorization state
// (the new app may never have been authorized → the card goes from ok to error + a "Start authorization" button appears),
// and the frontend needs this new state to redraw. With 204 it would have to send another GET,
// and the gap between those two requests would briefly show the old state on the card.
func (h *Handler) selectIntegrationApp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AppID string `json:"appId"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	provider := r.PathValue("provider")
	if err := h.registry().SelectApp(r.Context(), provider, body.AppID); err != nil {
		h.fail(w, r, err)
		return
	}
	// ⚠️ **The state is deliberately returned in the account-level view (empty workspaceId)**: this route itself is account-level
	// (SelectApp stores lark_user_app_prefs, "which app I use by default"),
	// only the integration management page calls it, and that page has no workspace context.
	// This is not a missing param: passing a workspace would actually make "the app just selected" and the returned state not be the same one.
	state, err := h.registry().State(r.Context(), provider, "")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *Handler) revokeIntegration(w http.ResponseWriter, r *http.Request) {
	// It may be bound to fail (the platform has no such endpoint / it does not exist as a concept); the failure message says "where to revoke it".
	// See integrationsvc.ErrRevokeUnsupported.
	if err := h.registry().Revoke(r.Context(), r.PathValue("provider")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// registry returns the registry, creating an empty one on the spot when it is nil.
//
// Reason: those four routes must
// **still be registered and still respond**. If they were not registered, the frontend would get 404, and on the frontend 404 means "this backend version
// does not have the feature", which is different from "the feature exists but no provider is configured".
func (h *Handler) registry() *integrationsvc.IntegrationRegistry {
	if h.integrations == nil {
		return integrationsvc.NewIntegrationRegistry(h.log)
	}
	return h.integrations
}

// ---- Onboarding (account-level) ----

// getOnboarding reads the current logged-in user's onboarding state.
//
// **With no record it returns 200 + an object with version 0, not 404.** The Service has already turned
// "this person has no record yet" into a zero value (see the comment on GetOnboarding); here we only write it out.
//
// What 404 would do: the frontend's nullable getter turns it into null, and the caller uses null to mean "still loading",
// so new users would stay in the loading state forever and onboarding would never appear: a broken feature with zero errors.
func (h *Handler) getOnboarding(w http.ResponseWriter, r *http.Request) {
	state, err := h.workspace.GetOnboarding(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// markOnboarding records "finished" or "skipped".
//
// The request body has only two fields, action and version, and **deliberately no email**: the person comes from the session.
// Adding an email field would open a way to "change someone else's onboarding state", and that kind of overreach gives no error at all.
func (h *Handler) markOnboarding(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action  string `json:"action"`
		Version int    `json:"version"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	state, err := h.workspace.MarkOnboarding(r.Context(), apimodel.OnboardingAction(body.Action), body.Version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// 200 instead of 201: this is not "created a resource", it is "set my state to seen",
	// and repeated calls are idempotent (upsert). The frontend uses the return value to refresh its own copy of the state.
	writeJSON(w, http.StatusOK, state)
}

// ---- Account-level Ark credential ----

// ---- Agent-level Ark credential ----

// ---- Get the Ark API Key from the cloud account automatically (three read-only routes) ----

// ---- User preferences ----

// getUserPreferences reads the current logged-in user's language and appearance.
//
// **When never set it returns the default + 200, not 404** (handled by the Service): the frontend's nullable getter
// turns 404 into null, and null there means "still loading", so new users would stay in the
// loading state forever, with no error. Same as getOnboarding / getArkCredential.
func (h *Handler) getUserPreferences(w http.ResponseWriter, r *http.Request) {
	prefs, err := h.workspace.GetUserPreferences(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// saveUserPreferences stores the current logged-in user's preferences and returns the stored values.
//
// The request body has only language and appearance, and **deliberately no email**: the person comes from the session.
// Adding an email field would open a way to "change someone else's preferences", and that kind of overreach gives no error at all.
//
// **PUT instead of PATCH**: the request body is a full set of preferences (the frontend holds the whole object,
// and changing one field still submits the whole thing), so it means a whole replacement. PATCH would suggest "you can send just one field",
// and then whether a missing field means "unchanged" or "set to default" is a question we do not need to introduce.
func (h *Handler) saveUserPreferences(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Language   string `json:"language"`
		Appearance string `json:"appearance"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	prefs, err := h.workspace.SaveUserPreferences(r.Context(), apimodel.UserPreferences{
		Language:   body.Language,
		Appearance: body.Appearance,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Return 200 + the stored values (instead of 204): the Service normalizes unrecognized values to the default,
	// and the frontend needs this result to correct its in-memory state and localStorage cache.
	// With 204 the frontend would never learn that "I saved xx, but zh-CN is what took effect".
	writeJSON(w, http.StatusOK, prefs)
}

// ---- Problem feedback ----

// submitFeedback receives one piece of feedback (both levels share this route).
//
// # Return 204 instead of 200 + body
//
// There is nothing to return for a report: level one is silent (the frontend ignores the return value, see lib/feedback.ts),
// and level two only needs to know "it worked". **The record id is deliberately not returned**: returning it would hand out a
// handle to "this feedback", while the user has no follow-up action on it (cannot edit, delete or query it).
// Returning an id nobody can use would only make people think there is an endpoint to query it.
//
// # No actor in the request body
//
// The person is taken from auth.ActorFrom(ctx), same as the onboarding and integration management routes.
// Adding an email field would open a way to "submit feedback in someone else's name"; that kind of forgery
// gives no error at all, and feedback is used to drive improvements, so a wrong signature is worse than none.
func (h *Handler) submitFeedback(w http.ResponseWriter, r *http.Request) {
	// ⚠️ Decoding into apimodel.FeedbackInput **is the allowlist filter**: fields it does not list
	// (token / cookie / authorization / response body / message text) are dropped right here by
	// encoding/json, and later code never gets them.
	// See "filtering is allowlist-based" at the top of core/feedback.go.
	var in apimodel.FeedbackInput
	if !h.decode(w, r, &in) {
		return
	}
	if err := h.workspace.SubmitFeedback(r.Context(), in); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// listFeedback lists feedback, **only for the dev team**.
//
// The permission check is in the Service (core.ListFeedback), not here and not in Guard. For why, see the comment in
// Register and the docs of ListFeedback. Anyone not on the dev team gets persistmodel.ErrAccessDenied → 403.
//
// **Pagination params are clamped by the Service** (limit upper bound, default, negative numbers); here we only convert strings to int.
// It lives in the Service because those bounds are business rules, and if the handler gets another protocol (say a
// gRPC entry later) they should not have to be rewritten.
func (h *Handler) listFeedback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := persistmodel.FeedbackQuery{
		Level:     persistmodel.FeedbackLevel(strings.TrimSpace(q.Get("level"))),
		ErrorCode: strings.TrimSpace(q.Get("errorCode")),
		Since:     strings.TrimSpace(q.Get("since")),
		Until:     strings.TrimSpace(q.Get("until")),
	}
	// A number that cannot be parsed is treated as not passed (uses the Service default), with no error: one mistyped limit
	// should not fail a whole data-pulling script, and the Service has a clear meaning for 0.
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil {
		query.Limit = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("offset"))); err == nil {
		query.Offset = n
	}

	page, err := h.workspace.ListFeedback(r.Context(), query)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
