// Accessors for the nine domain packages that have moved out. **Only for the wiring code
// (cmd/amad).**
//
// # Why these methods are needed
//
// Step 14 of the package split plan changed the orchestrator layer from "holds one
// core.Service" to "holds the domain packages directly" (see the file header of
// orchestrator/orchestrator.go). So cmd/amad has to pass the domain package instances to
// orchestrator.New, but those instances are built inside core.New (half of their
// dependencies are still on the core side: issuing ids, getting the current actor,
// back-filling Ark events, queuing syncs...), so the wiring code cannot reach them.
//
// Two options: move the wiring of all eight domains to cmd/amad, or let core hand over the
// ones it has already built. We chose the latter, because the former would also mean moving
// "the half of the dependencies still in core", which is exactly what later steps do one by
// one. Moving them early to avoid adding 8 methods would cram three or four steps of work
// into this one, and it could not be finished.
//
// # ⚠️ This is not "another back door for the handler"
//
// What the handler can call is still decided only by the ten narrow interfaces of
// orchestrator (the copies on the core side were deleted in step 15 of the split; the only
// definition is in orchestrator/<domain>/service.go).
// The nine methods in this file **return the concrete types of the domain packages**,
// while the handler gets interfaces, so it cannot reach this.
//
// This can be checked by machine: `grep -rn 'svc\.Issues()\|\.Agents()' core/handler*.go`
// should find nothing. Who calls them: only cmd/amad/orchestrator.go.
//
// # ⚠️ This whole file will go away
//
// Each time a domain moves out of core, its method here should be deleted too (by then
// cmd/amad builds that domain package instance itself, without going through core).
// **When the methods reach zero, this refactoring is done.**
// So before adding a method here, first ask "is this domain moving in the wrong direction?"

package core

import (
	agentsvc "github.com/chyroc/ama/server/service/agent"
	arkcredsvc "github.com/chyroc/ama/server/service/arkcred"
	issuesvc "github.com/chyroc/ama/server/service/issue"
	revisionsvc "github.com/chyroc/ama/server/service/revision"
	runsvc "github.com/chyroc/ama/server/service/run"
	schedulesvc "github.com/chyroc/ama/server/service/schedule"
	skillsvc "github.com/chyroc/ama/server/service/skill"
	suggestionsvc "github.com/chyroc/ama/server/service/suggestion"
	workspacesvc "github.com/chyroc/ama/server/service/workspace"
)

// Accessors for the domain packages. Each returns the instance built in New, **never nil**
// (if New cannot build one it returns an error, and never returns a half-built Service).
func (s *Service) Issues() *issuesvc.Service           { return s.issues }
func (s *Service) Suggestions() *suggestionsvc.Service { return s.suggestions }
func (s *Service) Revisions() *revisionsvc.Service     { return s.revisions }
func (s *Service) ArkCreds() *arkcredsvc.Service       { return s.arkCreds }
func (s *Service) Workspaces() *workspacesvc.Service   { return s.workspaces }
func (s *Service) Agents() *agentsvc.Service           { return s.agents }
func (s *Service) Schedules() *schedulesvc.Service     { return s.schedules }
func (s *Service) Runs() *runsvc.Service               { return s.runs }
func (s *Service) Skills() *skillsvc.Service           { return s.skills }
