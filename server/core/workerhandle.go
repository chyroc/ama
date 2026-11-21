// Our own main flow for handling a work item, replacing the SDK's EnvironmentWorker.HandleItem.
//
// # Why not use the SDK's HandleItem directly
//
// Inside, the SDK's HandleItem hard-codes the tool result ledger as "a local file under the
// working directory" (selfhosted.NewFileToolResultStore(workdir)), and HandleItem always
// computes workdir with useWorkdirAsSession=true, so all concurrent work shares one root
// directory /tmp/ama-ss and the ledger is shared globally too. In production this once caused
// "poison pill cross-talk": a tool_result that failed to be sent back stayed in the shared
// ledger, and for **every other session** that pod claimed afterwards, Recover() at runner
// start POSTed it first → Ark answered 400 InvalidPayload for "this session has no such call" →
// the send gate judged the whole work dead. See repository/schema/selfhost_tool_results.sql.
//
// In "full worker Run" mode the SDK actually uses a per-session directory
// (handleItem(..., false)), but that path is private, and Run brings its own WorkPoller (which
// we do not use; see the pollLoop comment for why). So here we copy the handleItem flow and
// **change only one thing**: the tool result ledger uses PG (a *pgToolResultStore bound to the
// session). It works across instances, is isolated per session, survives pod rebuilds, and
// never sends session A's results to session B. The working directory is also split per
// session (used by envinit to download skills).
//
// The rest (GetSession, envinit.Setup, the tool set, heartbeat, StopWork cleanup, running
// SessionToolRunner) matches SDK handleItem line by line and only uses exported SDK APIs; when
// upgrading the SDK, just keep it in line with upstream lib/environments/worker.go.
//
// # Custom tools are wired here, not through EnvironmentWorker
//
// The SDK's EnvironmentWorkerOptions also has a CustomTools field, but it is a **process-level
// map**: one entry shared by every session on the pod, so a tool implementation cannot know which
// session it is serving. That is why the datasource audit lines would have no session id. We build
// the SessionToolRunner directly (below), which lets us pass a **fresh map per work item** whose
// closures carry sessionID + workID (see customToolsForWork). The declaration half of the same
// tools is in arkSpecFor.
package core

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted/envinit"
	"github.com/volcengine/ark-runtime-go/arkruntime/tools/agenttoolset"
)

const (
	workerHeartbeatTTL      = DefaultWorkerHeartbeatTTL
	workerHeartbeatInterval = DefaultWorkerHeartbeatInterval
)

// workProcessor is the function signature for "run one claimed work item". pollLoop calls it
// through p.processWork; production wires in handleWorkItem, pollLoop tests wire in a fake.
type workProcessor func(
	ctx context.Context,
	item selfhosted.WorkItem,
	api selfhosted.API, // the API for this work (wrapped by failover/sendGuard)
	sessionID string,
	// The logger of the caller's (pollLoop's) environment, carrying this environment's
	// workspace/environment/worker_id. **It must be passed per item** and not bound when the
	// closure is built: there is only one closure for the whole pool, so binding it means all
	// environments share one logger (see the comment on p.processWork in Run).
	logger *slog.Logger,
) error

// Heartbeat cause strings, matching the SDK's heartbeatStopCause.
const (
	heartbeatCauseLeaseLost        = HeartbeatCauseLeaseLost
	heartbeatCauseHeartbeatLost    = HeartbeatCauseHeartbeatLost
	heartbeatCausePermanentFailure = HeartbeatCausePermanentFailure
	heartbeatCauseStopRequested    = HeartbeatCauseStopRequested
)

// handleWorkItem handles one claimed work item. rootWorkdir/newTools/ledger/loggers are bound
// via a closure when runOne builds the WorkerPool (see how p.processWork is wired in worker.go).
func (p *WorkerPool) handleWorkItem(
	ctx context.Context,
	item selfhosted.WorkItem,
	api selfhosted.API,
	sessionID, rootWorkdir string,
	newTools func(env *agenttoolset.AgentToolContext) (*agenttoolset.Set, error),
	ledger ToolResultLedger,
	logger *slog.Logger,
	sdkLogger *log.Logger,
) (retErr error) {
	if item.ID == "" {
		return errors.New("work item id must not be empty")
	}
	if sessionID == "" {
		return errors.New("work item does not contain session id")
	}

	// Per-session working directory: the ledger already uses PG, so this is only for envinit
	// to download skills, split per session.
	workdir := filepath.Join(rootWorkdir, sessionID)

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var heartbeatCause atomic.Value
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		p.workerHeartbeat(workCtx, item, api, cancel, func(cause string) {
			heartbeatCause.Store(cause)
		})
	}()

	defer func() {
		cancel()
		<-heartbeatDone
		causeStr, _ := heartbeatCause.Load().(string)
		// Same as SDK shouldStopItem: settle the work item on normal completion / idle exit,
		// and skip StopWork only on lost lock / expired heartbeat / permanent failure;
		// shutdownSafeAPI and the failover wrapper swallow StopWork when letting go for re-dispatch.
		if causeStr != heartbeatCauseLeaseLost && causeStr != heartbeatCauseHeartbeatLost &&
			causeStr != heartbeatCausePermanentFailure {
			_ = stopWorkForItem(context.Background(), api, item)
		}
		if causeStr == heartbeatCauseStopRequested && errors.Is(retErr, context.Canceled) {
			retErr = nil
		}
	}()

	session, err := api.GetSession(workCtx, selfhosted.GetSessionRequest{SessionID: sessionID})
	if err != nil {
		return err
	}
	if session == nil {
		return errors.New("session response is empty")
	}
	if session.ID == "" {
		session.ID = sessionID
	}
	var svc *Service
	if p != nil {
		svc = p.svc
		refs := session.SkillRefs()
		skillIDs := make([]string, 0, len(refs))
		for _, ref := range refs {
			if ref.SkillID != "" {
				skillIDs = append(skillIDs, ref.SkillID)
			}
		}
		p.ensureSessionRunForWork(workCtx, item.EnvironmentID, sessionID, skillIDs, logger)
	}

	// Skills are downloaded to the worker host (envinit). Warn-only, same as the SDK: failure is
	// not fatal (the copy inside the container is what the tools actually read). Workdir uses
	// the per-session directory.
	toolEnv := &agenttoolset.AgentToolContext{Workdir: workdir}
	initOpts := toolEnv.InitOptions()
	initOpts.Workdir = workdir
	initOpts.Logger = sdkLogger
	if err := envinit.New(api, initOpts).Setup(workCtx, session); err != nil {
		// Same as the SDK: a Setup failure only logs a warning (each step inside envinit is
		// warn-only anyway) and does not block, so a failed skill download does not stop the whole work.
		logger.Warn("worker 会话环境准备失败（技能本机落盘，不阻断）",
			slog.String("work_id", item.ID), slog.Any("err", err))
	}

	toolSet, err := newTools(toolEnv)
	if err != nil {
		return err
	}
	defer func() { _ = toolSet.Close() }()

	// Tool result ledger: PG, bound to the current session (the key point: not shared, not
	// relying on local files).
	var resultStore selfhosted.ToolResultStore
	if ledger != nil {
		resultStore = newPGToolResultStore(ledger, sessionID, workCtx, logger)
	}

	runner := selfhosted.NewSessionToolRunner(workCtx, api, sessionID, selfhosted.SessionToolRunnerOptions{
		WorkID:      item.ID,
		Tools:       toolSet,
		ResultStore: resultStore,
		Logger:      sdkLogger,
		// Use 1000-event pages and a 1.5s poll fallback interval so long CUA/multi-step sessions
		// (hundreds of events) do not trigger a multi-page ListEvents storm every 500ms and hit
		// Ark's account RPM rate limit (HTTP 429).
		EventLimit:        toolAuditListLimit,
		EventPollInterval: 1500 * time.Millisecond,
		// Worker-side custom tools (query_sql / read_object). **A new map per work item**, not one
		// built when the pool starts: the tools' closures carry this work's session/work id, so
		// every audit line can say who queried what. customToolsForWork is nil-safe; with no
		// datasource configured this is nil, i.e. exactly the previous behavior.
		CustomTools: svc.customToolsForWork(sessionID, item.ID, logger),
	})
	defer func() { _ = runner.Close() }()
	for runner.Next() {
		// The SDK logs "tool event handled" here; the runner already POSTed the event
		// internally, so nothing else is needed.
	}
	retErr = runner.Err()
	if errors.Is(retErr, selfhosted.ErrIdleTimeout) || errors.Is(retErr, selfhosted.ErrSessionTerminated) {
		return nil
	}
	return retErr
}

// workerHeartbeat copies the SDK's heartbeatLoop: sends a heartbeat every TTL/2, and on 412
// (lost lock) / fatal 4xx / state=stopping it cancels and reports the cause.
func (p *WorkerPool) workerHeartbeat(
	ctx context.Context, item selfhosted.WorkItem, api selfhosted.API,
	cancel context.CancelFunc, markCause func(string),
) {
	RunWorkerHeartbeat(ctx, item, api, cancel, markCause)
}

// stopWorkForItem sends StopWork (force) during cleanup. 409/412 mean someone else already
// settled it; ignore those.
// The api passed in may be the failover wrapper; when it lets go it swallows StopWork.
func stopWorkForItem(ctx context.Context, api selfhosted.API, item selfhosted.WorkItem) error {
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := api.StopWork(stopCtx, selfhosted.StopWorkRequest{
		EnvironmentID: item.EnvironmentID,
		WorkID:        item.ID,
		Force:         true,
	})
	if err != nil && (selfhosted.IsStatus(err, 409) || selfhosted.IsStatus(err, 412)) {
		return nil
	}
	return err
}

// isFatalWorker4xx is the same as SDK isFatal4xx: 4xx and not 408/409/429/412.
func isFatalWorker4xx(err error) bool { return IsFatalWorker4xx(err) }
