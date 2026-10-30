// Orchestration layer: how the mixed-domain Service is satisfied by "three domain packages + a core
// fallback" together.
//
// # Why a separate file
//
// Service has the most mixed sources of the ten narrow interfaces: of its 47 methods, 25 have
// moved into service/{workspace,schedule,run} (the run group includes GetRunArkConsole, moved
// when arkconsole was split out), and 22 are still in core (chat / queued messages / preferences /
// feedback / usage). There are now two domains with mixed sources; the other is agent.Service (7
// from service/agent, 2 falling back to core). It has only two sources and the embedded names do
// not clash, so its composite needs no forwarding at all; see orchestrator/agent/composite.go.
//
// It did not go into service.go because that file is the **interface list** (the handler's view),
// while this file is the **wiring detail** (who satisfies it). The two change for different
// reasons: adding a method the handler can call changes the former; moving a domain out of core
// changes the latter.
//
// # ⚠️ Why the wiring code is in this package, not in the top-level orchestrator
//
// After the split into subdirectories, composite is an **unexported struct of this package**, and
// the top-level orchestrator cannot build it. Two options: export it (together with Fallback and
// the anonymous embedded field name), or have this package provide a New. We chose the latter:
// exporting the struct would spread pure wiring details like "who embeds whom, which field is
// anonymous" outside the package, and that is exactly what this file wants to keep in. Outside
// code only needs to say "these domain packages + this fallback", not how they are put together.
//
// # ⚠️ Those 22 lines were not copied by hand, and should not be
//
// The signatures in Fallback match the corresponding 22 in service.go **word for word**; they were
// filtered mechanically from the latter (removing the 25 already provided by domain packages).
//
// The problem with copying by hand is not the effort, it is that **a wrong copy does not always
// cause an error**: get one parameter type wrong and the compiler only says "*core.Service does not
// implement Fallback", pointing at the wiring site, not at the line you copied wrong. So when moving
// a domain out of core, the right way is to **delete the matching lines from this interface**, not
// to rewrite it.
//
// # ⚠️ The composite picks implementations by embedding depth, not by forwarding
//
// The key rule: **same name at the same depth is ambiguous**, and Go reports "ambiguous selector"
// at the **call site**, not at the struct definition. Methods contributed by embedded fields are
// all at depth 1, so two embedded fields providing the same method name clash.
//
// So Fallback must drop all 25 that the domain packages already provide (when skill / agent were
// split out and when GetRunArkConsole moved into run, each time the methods were **deleted from both
// Service and Fallback**, also guarded by that static assertion). Miss one:
// the method appears twice at depth 1 -> the composite does not satisfy Service -> the static
// assertion at the bottom goes red. Remove one too many: nobody provides it -> also red. **Both
// mistakes are compile errors**, never a silent use of the wrong implementation, which is exactly
// why embedding was chosen over hand-written forwarding.
//
// ⚠️ **`*workspacesvc.Service` is embedded anonymously** (its implicit field name is `Service`)**,
// the other two are named fields**.
// Only one of the three domain packages can be embedded anonymously: their type names are all
// `Service`, and embedding two or more gives `Service redeclared`. So the other two are named
// fields + explicit forwarding, and those 10 methods are the only forwarding code in this file.

package workspace

import (
	"context"
	"encoding/json"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	runsvc "github.com/chyroc/ama/server/service/run"
	schedulesvc "github.com/chyroc/ama/server/service/schedule"
	workspacesvc "github.com/chyroc/ama/server/service/workspace"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// Fallback is the 22 methods in Service that **still only core.Service can satisfy**
// (the real GetRunArkConsole has moved into service/run with the arkconsole split).
// The signatures match orchestrator/workspace/service.go word for word; see the file header for why.
//
// ⚠️ **Exporting it is required, not incidental**: the top-level orchestrator.Fallback embeds it
// (that interface's real implementation is *core.Service), and you cannot embed an unexported name
// across packages.
//
// It shrinks as later steps move domains out of core, and will eventually disappear.
type Fallback interface {
	AppendRunEvents(ctx context.Context, runID string, raw []json.RawMessage) ([]json.RawMessage, error)
	ArkProxyDoForRun(ctx context.Context, runID string, req ma.RawRequest) (*ma.RawResponse, error)
	ArkProxyDoForWorkspace(ctx context.Context, workspaceID string, req ma.RawRequest) (*ma.RawResponse, error)
	ArkProxyStreamForRun(ctx context.Context, runID string, req ma.RawRequest) (*ma.RawStream, error)
	ArkProxyStreamForWorkspace(ctx context.Context, workspaceID string, req ma.RawRequest) (*ma.RawStream, error)
	CancelPendingMessage(ctx context.Context, runID, id string) error
	Clear(ctx context.Context) error
	CreateConversation(ctx context.Context, workspaceID, agentID string) (apimodel.Run, error)
	EnqueuePendingMessage(
		ctx context.Context, runID, text string,
	) ([]apimodel.PendingMessage, error)
	GetOnboarding(ctx context.Context) (apimodel.OnboardingState, error)
	GetUserPreferences(ctx context.Context) (apimodel.UserPreferences, error)
	ListFeedback(ctx context.Context, q persistmodel.FeedbackQuery) (apimodel.FeedbackPage, error)
	ListPendingMessages(
		ctx context.Context, runID string,
	) ([]apimodel.PendingMessage, error)
	MarkOnboarding(ctx context.Context, action apimodel.OnboardingAction, version int) (apimodel.OnboardingState, error)
	RedirectRun(
		ctx context.Context, runID, text string,
	) ([]json.RawMessage, error)
	Reset(ctx context.Context) error
	ResolveUserProfiles(
		ctx context.Context, emails []string,
	) (map[string]apimodel.UserProfile, error)
	RunExists(ctx context.Context, runID string) error
	SaveUserPreferences(ctx context.Context, prefs apimodel.UserPreferences) (apimodel.UserPreferences, error)
	StreamRunEvents(
		ctx context.Context, runID string, onFrame func(raw []byte) error,
	) error
	SubmitFeedback(ctx context.Context, in apimodel.FeedbackInput) error
	WorkspaceUsage(ctx context.Context, workspaceID string, q persistmodel.UsageQuery) (apimodel.UsageSummary, error)
}

// Config is what is needed to build this domain's Service: three domain packages + a fallback for
// "the ones out of reach".
//
// ⚠️ The three fields are deliberately the concrete `*xxxsvc.Service`, not interfaces, for the same
// reason as the top-level orchestrator.Config: this layer's job is to know what it orchestrates.
type Config struct {
	Workspace *workspacesvc.Service
	Schedule  *schedulesvc.Service
	Run       *runsvc.Service
	Fallback  Fallback
}

// New combines the three domain packages and the core fallback into this domain's Service.
//
// ⚠️ **No nil checks here**. The top-level orchestrator.New already checks each one (it is the
// single wiring entry point, and its error can point at "which domain package was not passed");
// checking again here would only give the same error two reports with different wording.
func New(cfg Config) Service {
	return &composite{
		Service:  cfg.Workspace,
		Fallback: cfg.Fallback,
		schedule: cfg.Schedule,
		run:      cfg.Run,
	}
}

// composite makes Service satisfied by "three domain packages + a core fallback" together.
//
// What the four members do:
//
//	*workspacesvc.Service   embedded anonymously, directly contributes 14 methods (workspaces / members)
//	schedule / run          named fields, contribute via the 11 forwarding methods below
//	Fallback       embedded anonymously, contributes the 22 still in core
//
// 14 + 11 + 22 = 47, exactly the number of methods in Service.
//
// ⚠️ **Only one domain package can be embedded anonymously**: all three packages name their type
// Service, and embedding two or more anonymously gives `Service redeclared`. The workspace one is
// anonymous because it contributes the most methods (14), so it saves the most forwarding code.
type composite struct {
	*workspacesvc.Service
	Fallback

	schedule *schedulesvc.Service
	run      *runsvc.Service
}

// The 11 below are the only forwarding code in this file; see the comment on composite for why
// (the type names of two domain packages clash, so they cannot be embedded anonymously). The
// signatures match service.go word for word.

func (c *composite) CreateSchedule(ctx context.Context, workspaceID string, draft apimodel.ScheduleDraft) (apimodel.Schedule, error) {
	return c.schedule.CreateSchedule(ctx, workspaceID, draft)
}

func (c *composite) DeleteSchedule(ctx context.Context, id string) error {
	return c.schedule.DeleteSchedule(ctx, id)
}

func (c *composite) GetSchedule(ctx context.Context, id string) (apimodel.Schedule, error) {
	return c.schedule.GetSchedule(ctx, id)
}

func (c *composite) ListSchedules(ctx context.Context, workspaceID string) ([]apimodel.Schedule, error) {
	return c.schedule.ListSchedules(ctx, workspaceID)
}

func (c *composite) RunScheduleNow(ctx context.Context, id string) (apimodel.Schedule, error) {
	return c.schedule.RunScheduleNow(ctx, id)
}

func (c *composite) SetScheduleStatus(ctx context.Context, id string, status apimodel.ScheduleStatus) (apimodel.Schedule, error) {
	return c.schedule.SetScheduleStatus(ctx, id, status)
}

func (c *composite) UpdateSchedule(ctx context.Context, id string, draft apimodel.ScheduleDraft) (apimodel.Schedule, error) {
	return c.schedule.UpdateSchedule(ctx, id, draft)
}

func (c *composite) GetRun(ctx context.Context, id string) (apimodel.Run, error) {
	return c.run.GetRun(ctx, id)
}

// The real GetRunArkConsole is in service/run (moved from core in the arkconsole split); like the
// three above, it is forwarded through a named field.
func (c *composite) GetRunArkConsole(ctx context.Context, runID string) (apimodel.ArkConsoleRef, error) {
	return c.run.GetRunArkConsole(ctx, runID)
}

func (c *composite) ListRunEvents(ctx context.Context, runID string, q persistmodel.EventQuery) ([]apimodel.RunEvent, error) {
	return c.run.ListRunEvents(ctx, runID, q)
}

func (c *composite) ListRuns(ctx context.Context, workspaceID string, q persistmodel.RunQuery) ([]apimodel.Run, error) {
	return c.run.ListRuns(ctx, workspaceID, q)
}

// *composite must fully satisfy Service.
//
// ⚠️ This line is the gatekeeper of this file. It goes red for only two reasons, both in Fallback:
// a method already provided by a domain package was not removed (same name at same depth ->
// ambiguous), or one too many was removed (nobody provides it). The error names the method.
var _ Service = (*composite)(nil)
