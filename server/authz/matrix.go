// Route permission table: what level of permission each business route needs, and where the workspace comes from.
//
// # This table is a security boundary; think before changing it
//
// One missing from the table → that route panics at startup (it does not silently allow; see the two-way check in guard.go).
// A level set too low in the table → people without enough permission can do what they should not, **with no error at all**.
//
// So adding a route is not "just add a line"; you must answer: what happens if a viewer does this?
// What happens if a non-member does it?

package authz

// permission is the permission declaration of one route.
type permission struct {
	// action is the action level this route requires.
	action Action
	// When kind is not KindNone, the workspace is resolved from a resource id of this type.
	kind ResourceKind
	// idParam is the name of the resource id parameter in the path (required when kind is not KindNone).
	//
	// **It must be written out entry by entry, not guessed**: two routes at the same level use different parameter names:
	// /api/runs/{id} uses id, while /api/runs/{runId}/events uses runId.
	// The wrong name gives an empty string, then resolution fails and the whole route returns 404, which in tests
	// just looks like "that endpoint is broken".
	idParam string
	// When allowMissing is true, an unresolvable resource is **not blocked**; it goes straight to the handler.
	//
	// Only for GET /api/runs/{runId}/events: it belongs to the "list endpoints never 404" kind
	// (core/handler.go file header); when the run does not exist the handler returns 200 + [] itself.
	// If Guard jumped in with 404 on this one, it would turn the frontend's empty list into a thrown exception.
	allowMissing bool
}

// wsParam is the path parameter name of the workspace id in workspace-level routes; core and feishu both use it.
const wsParam = "workspaceId"

// routePermissions is the full declaration of "method + pattern → permission".
//
// The key must be **exactly the same** as the pattern written in core.Handler.Register / feishu.Handler.Register
// (including the method and space); Guard compares both ways at startup and panics on any mismatch.
//
// ⚠️ The nine /api/auth/ routes and /healthz **are not here**: they are registered on the bare mux (httpd/server.go),
// not through protectedMux, so they are outside Guard's view. Writing them here would trigger the reverse panic "declared but not registered".
var routePermissions = map[string]permission{
	// -- Workspaces ---------------------------------------------------------------
	//
	// List and create are ActionNone: the list is **filtered by membership** instead of rejected (core.ListWorkspacesFor),
	// and anyone can create a workspace (becoming its owner).
	"GET /api/workspaces":  {action: ActionNone},
	"POST /api/workspaces": {action: ActionNone},

	// Look up a workspace by key / check whether a key is taken: the workspace comes from a **query parameter**, which Guard's pattern
	// cannot express, so these two can only be declared ActionNone.
	//
	// So the membership check of getWorkspaceByKey **lives in Service** (core.GetWorkspaceByKey
	// returns ErrNotFound for non-members): the only place in the whole design where "authorization is written in the Service layer".
	// keyTaken deliberately does **no** filtering at all: it must check duplicates globally, otherwise users would create a workspace whose key
	// is already taken by someone else, and then hit the unique constraint on save.
	"GET /api/workspace-by-key":    {action: ActionNone},
	"GET /api/workspace-key-taken": {action: ActionNone},

	"PATCH /api/workspaces/{workspaceId}":  {action: ActionManage},
	"DELETE /api/workspaces/{workspaceId}": {action: ActionManage},

	// -- Members (all Manage: whoever can change members can promote themselves to owner) -------------------
	"POST /api/workspaces/{workspaceId}/members":              {action: ActionManage},
	"PATCH /api/workspaces/{workspaceId}/members/{memberId}":  {action: ActionManage},
	"DELETE /api/workspaces/{workspaceId}/members/{memberId}": {action: ActionManage},

	// -- Workspace-level host binding --
	//
	// GET is lowered to ActionRead: member (ActionWrite) can create/change agents and must pick an executor on the Agent edit page;
	// without reading the host list they could not choose. Writes (bind/unbind) are still Manage: they
	// decide which machines the workspace can use, the same level as members.
	// v1 also adds a check in the Service layer: you can only bind machines **you own**.
	"GET /api/workspaces/{workspaceId}/hosts":             {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/hosts":            {action: ActionManage},
	"DELETE /api/workspaces/{workspaceId}/hosts/{hostId}": {action: ActionManage},

	// -- Agent --------------------------------------------------------------
	"GET /api/workspaces/{workspaceId}/agents":  {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/agents": {action: ActionWrite},
	"GET /api/agents/{id}":                      {action: ActionRead, kind: KindAgent, idParam: "id"},
	"PATCH /api/agents/{id}":                    {action: ActionWrite, kind: KindAgent, idParam: "id"},
	"DELETE /api/agents/{id}":                   {action: ActionWrite, kind: KindAgent, idParam: "id"},

	// Agent history versions (rollback after mistakes).
	//
	// **Reading is Read, rollback is Write**, the same level as PATCH rather than Manage: whatever changes a rollback can make,
	// the user could already make field by field with PATCH; making it Manage would mean
	// "you can change it, but you cannot change it back", which blocks nothing and is counter-intuitive.
	//
	// ⚠️ **viewer must never roll back**, and ActionWrite already guarantees this (viewer < member).
	//
	// ⚠️ Rollback does **not** need an extra level for "changed integrationIds / envAliases":
	// it goes through Service's UpdateAgent, where ensureIntegrationsUnchanged /
	// ensureEnvAliasesUnchanged block non-admins as usual: **they check "did the value change", not
	// "was it sent"**, so rolling back to a version with different authorizations is cleanly rejected,
	// while member can still roll back to a version that only changed the prompt.
	// This is exactly the benefit of designing those two gates as "check for change"; see that part in core/agent.go.
	"GET /api/agents/{id}/revisions":                        {action: ActionRead, kind: KindAgent, idParam: "id"},
	"GET /api/agents/{id}/revisions/{revisionId}":           {action: ActionRead, kind: KindAgent, idParam: "id"},
	"POST /api/agents/{id}/revisions/{revisionId}/rollback": {action: ActionWrite, kind: KindAgent, idParam: "id"},

	// -- Agent authorization injection (handing a real person's credentials to an agent) ----------------------
	//
	// **ActionManage**, the same level as PUT /api/agents/{id}/ark-credential, for an even heavier reason:
	// the Ark credential decides "whose bill it goes on", while authorization injection decides "whose identity the agent acts with":
	// what is injected into the container is a person_account token, representing the real person who owns the workspace,
	// and **the agent can read it with a single `env`**; what it can do equals what that employee can do
	// (see the "security boundary" section of core/credentialinject.go).
	// Letting member check it themselves would let them lend someone else's identity to a program.
	//
	// ⚠️⚠️ **Why the whole `PATCH /api/agents/{id}` is not raised to Manage**:
	// this is the easiest place to get this change wrong, and the next person will most likely agonize over it again:
	//
	//   Renaming, changing the prompt, switching models, binding skills **are things member should be able to do** (ActionWrite).
	//   And integrationIds is just one field in that PATCH request body. Raising the whole route to Manage
	//   would lock all everyday agent editing to admins just to lock one field: a feature regression,
	//   and a very hidden one (member would only find "I cannot change the prompt any more", never suspecting the permission matrix).
	//
	//   **The matrix judges by route; it cannot judge by field.** So to tighten a field, it must first become a route:
	//   exactly the pattern the ark-credential group already used (for the same agent, PATCH is Write,
	//   while PUT .../ark-credential is Manage). This entry copies it.
	//
	// ⚠️ Splitting out the route **is only the first half**. The old opening must be closed at the same time, otherwise member could still change it from
	// the request body of `PATCH /api/agents/{id}`, a bypass with zero errors:
	//   - PATCH: core.UpdateAgent no longer writes this field, and returns 403 directly when the value differs from the database;
	//   - POST (create agent): non-admins sending a non-empty integrationIds get 403 directly.
	// Both are on the core side, out of Guard's reach (see the "Guard cannot judge fields" kind in the README).
	//
	// **No matching GET**: this field is already in the response of `GET /api/agents/{id}`,
	// which is already ActionRead. Opening another read route that returns only a subset would be a duplicate surface,
	// and the two would drift sooner or later (same as the README's "do not declare the same endpoint twice").
	// The asymmetry with ark-credential has a reason: that one is not stored on the agent, so it must be read separately.
	"PUT /api/agents/{id}/integrations": {action: ActionManage, kind: KindAgent, idParam: "id"},

	// "Has the Ark side caught up after saving". See core/agentsync.go.
	//
	// Reading is ActionRead: it only returns a sync status, the same level as GET /api/agents/{id}
	// (whoever can see this agent can see whether it is synced).
	//
	// ⚠️ **Retry is ActionWrite, not Read**: it really changes the Agent on the Ark side
	// (rerunning ensureBinding), and viewer should not be able to trigger it. The same level as PATCH /api/agents/{id}
	// is right: only those who can change this agent can make it resync.
	"GET /api/agents/{id}/sync-task":  {action: ActionRead, kind: KindAgent, idParam: "id"},
	"POST /api/agents/{id}/sync-task": {action: ActionWrite, kind: KindAgent, idParam: "id"},

	// "Which entry is this agent / skill / session in the Ark console"; all three are **ActionRead**.
	//
	// The same level as GET /api/agents/{id} is right: they return resource ids and region, and **whoever can see
	// this agent can already see which workspace it belongs to and which region that workspace is configured with**
	// (GET .../ark-credential is also Read, and the mask it returns carries more information).
	//
	// ⚠️ **No plaintext key, and no credential content at all**: only region, an enum value.
	// What really decides "can you see it after clicking through" is the user's permission on the Volcengine side, not here.
	"GET /api/agents/{id}/ark-console": {action: ActionRead, kind: KindAgent, idParam: "id"},

	// "What is my role in this workspace". **ActionRead**: it asks about your own identity,
	// and viewer should be able to ask too: the UI uses it to decide "draw the authorization block as editable or read-only + explanation".
	//
	// This endpoint uses core.RoleOf, **the same function** Guard uses to judge permissions, so there is no second truth:
	// the frontend should not compute it from workspace.members itself.
	"GET /api/workspaces/{workspaceId}/my-role": {action: ActionRead},

	// -- Skills ---------------------------------------------------------------
	"GET /api/workspaces/{workspaceId}/skills":  {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/skills": {action: ActionWrite},
	// Upload a skill package. **Write, not Manage**: what it creates is fully equivalent to a hand-written skill
	// (same table, same source); why would hand-writing count as Write while uploading counts as Manage?
	// viewer is blocked (read-only), member can upload: member could already create skills.
	//
	// No kind/idParam needed: workspaceId is right in the path, so Guard need not resolve it.
	"POST /api/workspaces/{workspaceId}/skills/upload": {action: ActionWrite},
	// Fork: copy a skill into a custom skill of this workspace. Same as above, Write and no resolution needed.
	//
	// ⚠️ **The source skill id goes in the body instead of the path, deliberately.** Written as
	// `POST /api/skills/{id}/fork`, Guard would resolve the owning workspace by id, and resource.go
	// returns ErrPlatformResource for built-in skills (turned into "reject writes"), so forking a built-in skill
	// would always be 403, and that 403 would not point to "the route shape was chosen wrong" at all.
	"POST /api/workspaces/{workspaceId}/skills/fork": {action: ActionWrite},
	"GET /api/skills/{id}":                           {action: ActionRead, kind: KindSkill, idParam: "id"},
	"PATCH /api/skills/{id}":                         {action: ActionWrite, kind: KindSkill, idParam: "id"},
	"DELETE /api/skills/{id}":                        {action: ActionWrite, kind: KindSkill, idParam: "id"},
	// "Which entry is this skill in the Ark console", Read level; for the reason see the agent entry.
	//
	// Built-in skills likewise go through resource.go's ErrPlatformResource → "allow reads",
	// so this works for them too, while the Service side returns an empty ref (built-in skills belong to no workspace,
	// so no key can be located; see GetSkillArkConsole).
	"GET /api/skills/{id}/ark-console": {action: ActionRead, kind: KindSkill, idParam: "id"},

	// Skill history versions, the same levels as those three of agent (see the part above).
	//
	// ⚠️ Built-in skills go through resource.go's ErrPlatformResource → Guard's "allow reads, reject writes",
	// so GET works (the Service side returns ErrPrecondition saying "built-in skills have no history"),
	// while rollback is rejected right at the Guard layer. Blocking at both layers is deliberate, same as UpdateSkill.
	"GET /api/skills/{id}/revisions":                        {action: ActionRead, kind: KindSkill, idParam: "id"},
	"GET /api/skills/{id}/revisions/{revisionId}":           {action: ActionRead, kind: KindSkill, idParam: "id"},
	"POST /api/skills/{id}/revisions/{revisionId}/rollback": {action: ActionWrite, kind: KindSkill, idParam: "id"},
	// Reads the content of a skill attachment (`?path=` says which file).
	//
	// **Read level**: it only shows files already in the skill package, the same level as `GET /api/skills/{id}`.
	// Built-in skills go through resource.go's ErrPlatformResource → Guard's "allow reads",
	// so the detail page works for built-in skills too (they have zero attachments; the Service side returns 404).
	"GET /api/skills/{id}/file": {action: ActionRead, kind: KindSkill, idParam: "id"},
	// Changes an attachment's content. **Write level, the same as PATCH /api/skills/{id}**:
	// changing an attachment and changing the body are two halves of the same thing; why could member do one but not the other.
	//
	"PUT /api/skills/{id}/file": {action: ActionWrite, kind: KindSkill, idParam: "id"},
	// -- Issues ------------------------------------------------------
	//
	// The whole group is workspace resources, so the levels split along "read / write":
	//
	//	Read   view lists, details, comments, attachments (including the images themselves)
	//	Write  file issues, edit issues, change status and priority, comment, upload attachments, delete
	//
	// **None of them is Manage.** Filing and discussing issues is the most everyday collaboration in a workspace;
	// Manage would leave member unable even to report an issue, and members are the main users of this feature.
	// The same level as "create/edit/delete Agents and skills" is right: both are things member should be able to do.
	//
	// ⚠️ Delete is also Write, not Manage, which is worth explaining: it is the same level as "delete Agent" and "delete skill"
	// (those two are also Write). An issue is not a credential-like resource; deleting one affects only itself.
	"GET /api/workspaces/{workspaceId}/issues":  {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/issues": {action: ActionWrite},
	"GET /api/issues/{id}":                      {action: ActionRead, kind: KindIssue, idParam: "id"},
	"PATCH /api/issues/{id}":                    {action: ActionWrite, kind: KindIssue, idParam: "id"},
	"DELETE /api/issues/{id}":                   {action: ActionWrite, kind: KindIssue, idParam: "id"},

	// Comments. The two under an issue resolve by issue; editing/deleting a single one resolves by the comment itself.
	//
	// ⚠️ The kinds of the two groups **differ** (KindIssue vs KindIssueComment), yet idParam is id in both:
	// exactly the shape this table's comments warn about. Resolving with the wrong type would look up the issue table with a comment id,
	// giving a constant 404, which just looks like "this endpoint is broken".
	"GET /api/issues/{id}/comments":     {action: ActionRead, kind: KindIssue, idParam: "id"},
	"POST /api/issues/{id}/comments":    {action: ActionWrite, kind: KindIssue, idParam: "id"},
	"PATCH /api/issue-comments/{id}":    {action: ActionWrite, kind: KindIssueComment, idParam: "id"},
	"DELETE /api/issue-comments/{id}":   {action: ActionWrite, kind: KindIssueComment, idParam: "id"},
	"GET /api/issues/{id}/attachments":  {action: ActionRead, kind: KindIssue, idParam: "id"},
	"POST /api/issues/{id}/attachments": {action: ActionWrite, kind: KindIssue, idParam: "id"},

	// ⚠️⚠️ **Attachment download: the entry in this table most easily set wrong.**
	//
	// This URL is put into `<img src>` in Markdown bodies and loaded by the browser as an image,
	// so it **looks like a static resource**, and "why authenticate a static resource" is exactly the deadly intuition.
	//
	// The consequence of ActionNone: anyone with the URL (it appears in shared links,
	// browser history, logs) could see images in **someone else's workspace**. And this leak
	// **produces no error, and no UI would ever show that it happened**.
	//
	// So it is ActionRead + resolving the owning workspace by attachment, the same level as "view this workspace's issues":
	// only those who can see the issue can see its attachments, not loosened one bit.
	//
	// Credentials are not a problem: `<img>` requests carry no custom headers, but same-origin automatically sends the HttpOnly Cookie,
	// and among the three channels of auth.credentialFrom there is exactly the Cookie one.
	//
	// httpd/issue_test.go pins "people in workspace A getting workspace B's attachment → 403" with the real wiring.
	"GET /api/issue-attachments/{id}":    {action: ActionRead, kind: KindIssueAttachment, idParam: "id"},
	"DELETE /api/issue-attachments/{id}": {action: ActionWrite, kind: KindIssueAttachment, idParam: "id"},

	// -- Ark credentials -----------------------------------------------------------
	//
	// Writes are Manage, not Write: it is a **paid workspace-level credential**; replacing or deleting it affects
	// the chats of all agents in this workspace, on a different scale from "changing one agent's prompt".
	//
	// GET and verify are Read: GET only returns the mask (core.GetArkCredential never emits plaintext),
	// and verify writes nothing (it calls a read-only endpoint once with the stored key).
	"GET /api/workspaces/{workspaceId}/ark-credential":          {action: ActionRead},
	"PUT /api/workspaces/{workspaceId}/ark-credential":          {action: ActionManage},
	"DELETE /api/workspaces/{workspaceId}/ark-credential":       {action: ActionManage},
	"PATCH /api/workspaces/{workspaceId}/ark-credential/region": {action: ActionManage},
	"POST /api/workspaces/{workspaceId}/ark-credential/verify":  {action: ActionRead},

	// Reference the global credential. **Manage**, the same level as PUT: it replaces the key used by the whole workspace,
	// affecting the chats of all agents in this workspace: the same scale of operation as "pasting a new key",
	// only the source of the key differs. Set lower, member could quietly switch the workspace to someone else's credential.
	"POST /api/workspaces/{workspaceId}/ark-credential/use-global": {action: ActionManage},

	// -- Agent-level Ark credential (the most specific of the three levels) ----------------------------
	//
	// The levels are **exactly the same** as the workspace-level group: read and verify are Read, writes are Manage.
	// The reason is the same too: it is a paid credential; replacing or deleting it changes which key all of this agent's chats use
	// and whose bill they go on, on a different scale from "changing one agent's prompt".
	//
	// ⚠️ **Write would be a real privilege escalation**: member can already change agents (prompt, model,
	// skills are all Write), but none of those involve money. Letting member quietly switch an agent to another key
	// would let them decide whose bill it goes on, and apart from the Agent detail page, nowhere in the UI
	// would show that it happened.
	//
	// Resolution: the workspace is resolved from the **agent id** (kind: KindAgent, idParam: "id"),
	// the same as GET/PATCH /api/agents/{id}.
	//
	// ⚠️ DELETE is "use the workspace setting instead", not "delete a credential": the agent level has no stored inherit state;
	// no row means following (see the top of core/agentark.go). It is still Manage: it also changes
	// which key this agent uses.
	"GET /api/agents/{id}/ark-credential":          {action: ActionRead, kind: KindAgent, idParam: "id"},
	"PUT /api/agents/{id}/ark-credential":          {action: ActionManage, kind: KindAgent, idParam: "id"},
	"DELETE /api/agents/{id}/ark-credential":       {action: ActionManage, kind: KindAgent, idParam: "id"},
	"PATCH /api/agents/{id}/ark-credential/region": {action: ActionManage, kind: KindAgent, idParam: "id"},
	"POST /api/agents/{id}/ark-credential/verify":  {action: ActionRead, kind: KindAgent, idParam: "id"},

	// -- Account-level Ark credential (the "configure once globally" one) --------------------------
	//
	// **All ActionNone**, for exactly the same reason as the integrations / user-onboarding / user-preferences
	// groups: the object is not a workspace resource but "my own Ark key".
	// The pattern has no workspaceId, and should not: the same person sees the same one in every workspace.
	//
	// Privilege escalation cannot be expressed here: the subject comes from context (several methods in core/ark.go all start by calling
	// preferencesActor → auth.ActorFrom), and **no email appears in the path or request body**.
	//
	// ⚠️ ActionRead would be worse: someone who has not joined any workspace could not configure their own global credential,
	// while "configure the credential first, then create a workspace" is exactly the smoothest way to start.
	//
	// ⚠️ **What is opened here is not "reading credentials"**: GET only returns the mask (apimodel.GlobalArkCredential
	// has no plaintext field at all), and DELETE / PUT only touch your own row. Plaintext never leaves the server.
	"GET /api/global-ark-credential":         {action: ActionNone},
	"PUT /api/global-ark-credential":         {action: ActionNone},
	"DELETE /api/global-ark-credential":      {action: ActionNone},
	"POST /api/global-ark-credential/verify": {action: ActionNone},

	// -- Auto-fetch Ark API Keys from the Volcengine account (three read-only routes) --------------------------
	//
	// **Also ActionNone**, with exactly the same argument as the group above: they look up "the Volcengine
	// accounts under **my own** name, and the Ark keys in those accounts". The subject comes from context (the three methods in Service all start by
	// calling preferencesActor → auth.ActorFrom), and **no email appears in the path or request body**.
	// Privilege escalation cannot be expressed: you cannot name someone else.
	//
	// ⚠️ The {accountId} in the path **is a Volcengine account id, not our resource id**, so there is no kind /
	// idParam: it does not need to resolve to a workspace, and cannot. Looking it up in the workspace table would always 404.
	//
	// ⚠️ ActionRead would likewise be worse (people not yet in any workspace need to configure their own credential most),
	// for the reason in the group above.
	//
	// ⚠️ **These three return only masks and ids, no plaintext** (apimodel.ArkKeyOption has no plaintext field).
	// After the user chooses, it goes through the two PUTs above, and the server fetches the plaintext itself; see the section
	// "plaintext never passes through the frontend" at the top of core/arkfetch.go. If someday someone adds "return the plaintext too while at it" here,
	// the level of these three must be rethought.
	"GET /api/ark-fetch/status":                    {action: ActionNone},
	"GET /api/ark-fetch/accounts":                  {action: ActionNone},
	"GET /api/ark-fetch/accounts/{accountId}/keys": {action: ActionNone},

	// -- Authorization management (account-level, unrelated to workspaces; see the comment in core/handler.go) --------------
	//
	// **All ActionNone**, all four of them. This is not "forgot to set a level": the object of these four routes **is not
	// a workspace resource**: they look up "my authorization status on some external platform", start "my own authorization",
	// and revoke "my own". Guard's model is "actor + workspace → role", while here
	// **the pattern has no workspaceId**.
	//
	// Does that loosen permissions? No, because **the subject comes from context, not from request parameters**
	// (every method of core.IntegrationRegistry starts by taking auth.ActorFrom(ctx)).
	// In other words, when logged-in user A calls these four endpoints, what they see and operate on is always A's own authorization,
	// and they can neither get nor change anyone else's: privilege escalation cannot be expressed here, so there is nothing to judge.
	//
	// ⚠️ Conversely, **if someday an ability to "look up someone else's authorization by actor" is added, these must be re-leveled**.
	// Only then does privilege escalation first become possible.
	//
	// ActionRead would be worse, not safer: Read requires workspace membership, so a newcomer who has not joined any
	// workspace could not open "authorization management", while they need to authorize here first more than anyone.
	//
	// ⚠️⚠️ **authorize and authorization now take an optional `?workspaceId=`** (when starting authorization /
	// polling, "which workspace to choose the Feishu app by"; see authorizeIntegration in core/handler.go).
	// So the sentence above, "the pattern has no workspaceId", now only holds for the **path**: the workspace
	// **can now be named, just through the query**.
	//
	// These two are still ActionNone, for exactly the same reason as above (newcomers not yet in any workspace must not be blocked),
	// and **Guard's pattern cannot express query parameters at all**. So the membership check can only live in
	// the Service layer: core.LarkUserProvider.ensureWorkspaceMember first checks
	// "are you a member of this workspace", and if not returns ErrNotFound (**404 instead of 403,
	// so it cannot serve as an oracle for "does this workspace exist / has it connected a Feishu app"**),
	// and **never queries the database with that id**.
	//
	// This is the **third place** where "authorization is written in the Service layer"; the first two are GET /api/workspace-by-key
	// (the workspace also comes from the query) and GET /api/feedback (allowlist). All three have the reason written in the code,
	// and are pinned by tests.
	"GET /api/integrations":                       {action: ActionNone},
	"GET /api/integrations/{provider}":            {action: ActionNone},
	"POST /api/integrations/{provider}/authorize": {action: ActionNone},
	"DELETE /api/integrations/{provider}":         {action: ActionNone},
	// Poll "has the user clicked agree" (used by the device code flow; see core/handler.go).
	// Same as above: the subject comes from context, and what is asked is always your own pending authorization flow.
	"GET /api/integrations/{provider}/authorization": {action: ActionNone},
	// "Which apps can this source choose / switch to another" (only Feishu user authorization has this concept).
	//
	// Also ActionNone, with **exactly the same** rule as the four above: the subject comes from context,
	// what is listed is always "the apps I myself can see", and what is switched is always my own choice.
	//
	// ⚠️ **The PUT one is not "writing a workspace resource"**; do not give it ActionManage just because it is a write:
	// it writes an account-level preference (lark_user_app_prefs, whose primary key is only actor),
	// unrelated to workspaces. Manage would leave "people not yet in any workspace" unable to switch apps,
	// while they may well have only one Feishu app they registered themselves.
	//
	// Escalation surface: the apps that can be chosen come from "the apps connected to the workspaces this person is in", narrowed in the Service layer
	// (core.LarkUserProvider.SelectApp checks that appID is in that list),
	// so filling in an arbitrary cli_xxx cannot be set.
	"GET /api/integrations/{provider}/apps": {action: ActionNone},
	"PUT /api/integrations/{provider}/app":  {action: ActionNone},

	// -- User profiles (account-level: look up display names and avatars by email) ------------------------------
	//
	// **ActionNone**, for a reason of the same shape as the ones above but a different source, so it is explained separately.
	//
	// It looks up display names and avatars in the users table, **which is company directory-level information**:
	// colleagues' names and avatars are already visible everywhere in Feishu, in member lists, and in every run history,
	// so it is no escalation surface. The input is a list of emails given by the caller, and nothing is returned for ones not found,
	// so it also **cannot reveal anything beyond "does this person exist"**.
	//
	// ⚠️ Why not ActionRead: Read requires workspace membership, while the object of this endpoint is not a
	// workspace resource at all (the pattern has no workspaceId, and should not: the same person's avatar in whichever workspace
	// is the same). Read would leave "newcomers not yet in any workspace" unable even to look up their own avatar.
	//
	// ⚠️ But **it is not without a threshold**: httpd's Protect middleware still requires login,
	// and anonymous requests get 401 before reaching this table. ActionNone means
	// "logged in is enough, no extra workspace role required", not "anyone can call it".
	"POST /api/user-profiles": {action: ActionNone},

	// -- Onboarding (account-level, unrelated to workspaces) -------------------------------------
	//
	// **Both are ActionNone**, for **exactly the same** reason as the four authorization management routes above: the object is not a workspace resource,
	// but "has this person seen the onboarding". The pattern has no workspaceId, and should not:
	// the same person sees the same state in every workspace.
	//
	// Privilege escalation cannot be expressed here: the subject comes from context (onboardingActor in core/onboarding.go
	// uses auth.ActorFrom), and **no email appears in the path or request body**. So when logged-in user A calls these two,
	// what they read and change is always A's own record, and they can neither get nor change anyone else's.
	//
	// ⚠️ The difference from the two API Key routes is worth spelling out, so nobody "unifies" them: API Key hangs
	// under {workspaceId} with Read, because it can require the caller to be at least a member of some workspace.
	// **Onboarding cannot do that**: a newcomer logging in for the first time may not belong to any workspace yet, and they are exactly the only
	// person who needs onboarding. ActionRead would give 403 to exactly the people who most need to see the onboarding.
	//
	// ⚠️ Conversely, if someday an ability for "an admin to check whether someone has seen the onboarding" is added, it **must be
	// a new route with a new level**; do not add an optional email parameter to these two: the moment that is added,
	// privilege escalation first becomes possible, and ActionNone for these two no longer holds.
	"GET /api/user-onboarding":  {action: ActionNone},
	"POST /api/user-onboarding": {action: ActionNone},

	// -- User preferences (account-level, unrelated to workspaces) -------------------------------------
	//
	// **Both are ActionNone**, for **exactly the same** reason as the two onboarding routes above: the object is not a workspace resource,
	// but "which language and color scheme this person likes". The pattern has no workspaceId, and should not.
	//
	// Privilege escalation cannot be expressed here: the subject comes from context (preferencesActor in core/preferences.go
	// uses auth.ActorFrom), and **no email appears in the path or request body**.
	//
	// ⚠️ ActionRead would likewise be worse: preferences are read in PrefsProvider, which wraps **outside**
	// AuthGate (the whole tree needs it to provide the language): someone not yet in any workspace would get a 403
	// just by opening the page, and the UI language would fall back to the default. That is not "safer"; it makes the outermost thing
	// depend on the innermost permission.
	"GET /api/user-preferences": {action: ActionNone},
	"PUT /api/user-preferences": {action: ActionNone},

	// -- Problem feedback (cross-workspace; read and write permissions are completely asymmetric) ------------------------------
	//
	// ⚠️ **These two are both ActionNone, but for completely different reasons. Do not see them as a pair.**
	//
	// POST (report): ActionNone means **really no authorization needed**, like the two onboarding routes:
	// the object is not a workspace resource, the subject comes from context (feedbackActor in core/feedback.go),
	// and the request body has no email, so "giving feedback in someone else's name" cannot be expressed.
	// And people who hit problems should be able to give feedback; putting it under a workspace would block exactly the scenario that most needs feedback,
	// "a newcomer not yet in any workspace cannot open the page".
	//
	// GET (read the list): ActionNone **does not mean it is public**. It means "Guard cannot handle this one":
	// feedback is cross-workspace (it also includes errors that happen outside workspaces), and authz's model is "actor + workspace →
	// role", while there is no workspace to judge here at all. Expressing "is this the dev team" with Guard would need a
	// brand new platform-level role concept, the most expensive option described in the README.
	//
	// **Its gate is in Service**: core.ListFeedback starts by checking the FeedbackReaders allowlist
	// (config item feedback.readers, through shared/infra/config),
	// and anyone not on the list gets ErrAccessDenied → 403. Empty means nobody can read it (fail-closed).
	//
	// This is the second place in the whole design where "authorization is written in the Service layer"; the first is
	// GET /api/workspace-by-key (the workspace comes from a query parameter). Both have the reason written in the code,
	// and are pinned by tests. `httpd/feedback_test.go` hits the real route with the real wiring,
	// proving ordinary users (including owners) get 403 and only people on the allowlist get 200.
	//
	// ⚠️ **Why not add an admin flag to API Keys (which looks more "proper")**:
	// keys strictly inherit the creator's permissions, an invariant written in three places (the top of auth/apikey.go,
	// repository/schema/auth_pg.sql, and the leveling reason of the two api-keys routes below). Once a key can carry
	// an admin flag, **a viewer could make an admin key themselves**: a real privilege escalation,
	// and it would immediately break the ActionRead of those two routes below.
	//
	// The email allowlist and the key mechanism are **orthogonal and automatically compatible**: the identity a key resolves to is the creator's email,
	// so dev team members calling this endpoint with their own keys pass naturally, and nobody else passes with any key.
	"POST /api/feedback": {action: ActionNone},
	"GET /api/feedback":  {action: ActionNone},

	// -- Product suggestions (platform-level; all 13 are ActionNone) ----------------------------
	//
	// ⚠️ **This is the third kind of "something is wrong" thing**, not the same as the problem feedback above, nor the issue group.
	// For the full comparison along four axes see core/README-suggestion.md;
	// the point most relevant to this table is: **it has no workspace dimension**: those three tables do not even have a workspace_id column
	// (not "nullable", but no such concept at all).
	//
	// So the whole group can only be ActionNone, for a reason **of the same kind as GET /api/feedback but more thorough**:
	// that one is at least "cross-workspace" (an error happened to occur in some workspace), while here there is not even a "happened to".
	// authz's model is "actor + workspace → role", and there is no workspace to judge here.
	//
	// **ActionNone does not mean public.** The gates are all in Service (core/suggestion.go):
	//
	//	the person who made the suggestion  can read and change **their own** (suggestionForRead compares author)
	//	platform staff                      can read and change **all**, and only they can change status (isSuggestionStaff)
	//
	// "Platform staff" reuses the feedback.readers allowlist; **when not wired up, nobody is platform staff**
	// (fail-closed, the same direction as ListFeedback).
	//
	// This is the **third place** in the whole design where "authorization is written in the Service layer"; the first two are
	// GET /api/workspace-by-key and GET /api/feedback.
	//
	// ⚠️⚠️ **Do not add kind / idParam to these following the issue group.** That would make Guard resolve suggestion ids
	// in the issues table, always failing → every one 404s, with the error saying "resource does not exist":
	// a failure that does not point to the real cause at all. Guard passes ActionNone routes straight through,
	// and kind/idParam written there are silently ignored, so this mistake cannot even be found at startup.
	//
	// ⚠️ The attachment route (GET /api/suggestion-attachments/{id}) needs special care:
	// the issue route with the same name is ActionRead + KindIssueAttachment, relying on Guard resolving the workspace to block
	// cross-workspace access; **this one cannot block it, and can only rely on Service looking up the owning suggestion and comparing author**.
	// If missed, anyone with the URL could see screenshots others posted, with zero errors.
	// The httpd layer has a real-wiring test pinning "someone else's attachment gets 403".
	"GET /api/suggestions":         {action: ActionNone},
	"POST /api/suggestions":        {action: ActionNone},
	"GET /api/suggestions/{id}":    {action: ActionNone},
	"PATCH /api/suggestions/{id}":  {action: ActionNone},
	"DELETE /api/suggestions/{id}": {action: ActionNone},

	"GET /api/suggestions/{id}/replies":   {action: ActionNone},
	"POST /api/suggestions/{id}/replies":  {action: ActionNone},
	"PATCH /api/suggestion-replies/{id}":  {action: ActionNone},
	"DELETE /api/suggestion-replies/{id}": {action: ActionNone},

	"GET /api/suggestions/{id}/attachments":  {action: ActionNone},
	"POST /api/suggestions/{id}/attachments": {action: ActionNone},

	"GET /api/suggestion-attachments/{id}":    {action: ActionNone},
	"DELETE /api/suggestion-attachments/{id}": {action: ActionNone},

	// -- API Keys (account-level, two path shapes) -----------------------------------
	//
	// That table in the database **has no workspace_id column**; Service takes an email, and the SQL is
	// `where id = $1 and email = $2`. So the two groups of routes below get the same keys.
	//
	// # The two workspace-level routes: workspaceId takes no part in filtering
	//
	// It is there only so Guard can resolve the workspace, thus requiring the caller to be at least a member of that workspace.
	// The level is read, not write/manage: **creating a key changes nothing in this workspace**.
	// A key inherits the creator's own permissions, so a key created by a viewer can only read too;
	// manage would instead block viewers from read-only API access, which is a legitimate need.
	//
	// They are kept because these two were already published in openapi.json; deleting them would make agents written against the old spec
	// get 404, and that error would not point to "the endpoint changed path".
	"GET /api/workspaces/{workspaceId}/api-keys":  {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/api-keys": {action: ActionRead},

	// # The two account-level routes: ActionNone, the same pattern as integrations / onboarding
	//
	// There is no workspace to judge here: the pattern has no workspaceId, and should not: the global settings page is
	// outside any workspace (account-level config should not depend on some workspace), so it cannot get a workspaceId.
	//
	// **Does that loosen permissions? No, because privilege escalation cannot be expressed here.** actor only comes from
	// auth.ActorFrom(ctx), and **no email appears in the path or request body**, so when logged-in user A
	// calls these two, what they see and create are always A's own keys. This is not "checked the subject";
	// it is "you cannot name someone else", which is safer than the former, because the former may miss that check.
	//
	// The rule is **exactly the same** as the four /api/integrations routes and the two /api/user-onboarding routes,
	// and DELETE /api/api-keys/{id} had this shape from the start (see the entry below).
	//
	// ⚠️ ActionRead would be worse, not safer: Read requires workspace membership, so someone not yet
	// in any workspace could not manage their own keys, while they need a key to call the API more than anyone.
	// This is the same reason the two onboarding routes cannot be Read.
	//
	// ⚠️ The reasoning for these all rests on "keys strictly inherit the creator's permissions". **If someday scopes are added
	// (like "create an admin key"), these four must be re-leveled**: at that moment a person with zero permissions
	// could make an admin key for themselves.
	"GET /api/api-keys":  {action: ActionNone},
	"POST /api/api-keys": {action: ActionNone},

	// Revocation **does not hang under a workspace**, because it needs none: ownership is decided by the SQL where condition
	// (`delete ... where id = $1 and email = $2`); someone else's key simply does not match there.
	//
	// So this one is ActionNone: there is no workspace to judge, and privilege escalation cannot be expressed here.
	// Same reason as the four /api/integrations routes.
	"DELETE /api/api-keys/{id}": {action: ActionNone},

	// A platform admin creates a local account (email + password) for someone. **Platform-level**, with no workspace to resolve, so it can only be
	// ActionNone (= logged in passes Guard); "is this a platform admin" is decided by the auth handler itself
	// (auth.Service.CreateLocalUser: the auth.admins list in config, or a local account with is_admin),
	// and non-admins get 403 access_denied. The "person being created" in the path and request body does not affect the caller's permissions.
	"POST /api/admin/users": {action: ActionNone},

	// -- Account-level hosts (machines registered by the ama daemon) ------------------------------
	//
	// Same as api-keys: an account-level resource, the pattern has no workspaceId, and escalation is decided by the SQL where
	// owner_email. GET lists your own machines, DELETE revokes your own machine.
	// The daemon's registration POST /api/host-daemon/register is also account-level; see below.
	"GET /api/hosts":                 {action: ActionNone},
	"DELETE /api/hosts/{hostId}":     {action: ActionNone},
	"POST /api/host-daemon/register": {action: ActionNone},

	// -- Account-level personal files (object storage upload/download; all 8 are ActionNone) ----------------
	//
	// The same shape as /api/hosts: the resource belongs to no workspace, the owner is the logged-in user, and Guard's
	// "actor + workspace → role" model has no workspace to judge here. Escalation protection has two layers, neither in
	// Guard:
	//
	//  1. The owner email always comes from auth.ActorFrom(ctx); self-claims in the request body/query are not accepted;
	//  2. every SQL behind service/userfile carries owner_email, and other people's ids always give 404
	//     (not revealing existence).
	//
	// ⚠️ ActionNone still means "must be logged in": requests without credentials get 401 at the outer Protect,
	// and this table only decides whether a workspace role is checked after login.
	"POST /api/files":                        {action: ActionNone},
	"GET /api/files":                         {action: ActionNone},
	"GET /api/files/{id}":                    {action: ActionNone},
	"GET /api/files/{id}/content":            {action: ActionNone},
	"DELETE /api/files/{id}":                 {action: ActionNone},
	"PUT /api/files/{id}/content":            {action: ActionNone},
	"PUT /api/files/{id}/parts/{partNumber}": {action: ActionNone},
	"POST /api/files/{id}/complete":          {action: ActionNone},
	"POST /api/files/{id}/abort":             {action: ActionNone},

	// -- Self-description (no auth) ---------------------------------------------------
	//
	// ⚠️ **These two are not in this table**, for the same reason as the five /api/auth/ routes: they are registered on the bare mux
	// (httpd/server.go), not through protectedMux, so they are outside Guard's view.
	// Writing them here would trigger the reverse panic "declared but not registered".
	//
	// No auth is required: an agent must first understand this platform before it can authenticate. What is opened is only the **structure**
	// (field names, paths, permission levels), with no instance data at all; see the security boundary in openapi/handler.go.

	// -- Run history and usage (read-only) ---------------------------------------------
	"GET /api/workspaces/{workspaceId}/runs":  {action: ActionRead},
	"GET /api/workspaces/{workspaceId}/usage": {action: ActionRead},
	"GET /api/runs/{id}":                      {action: ActionRead, kind: KindRun, idParam: "id"},
	// The only allowMissing: when the run does not exist the handler returns 200 + []; see permission.allowMissing.
	"GET /api/runs/{runId}/events": {action: ActionRead, kind: KindRun, idParam: "runId", allowMissing: true},
	// Observability reference. **No allowMissing**: it is a getter of a single resource,
	// and should 404 when the run does not exist (it is not the "list endpoints never 404" kind).
	// Where this session is in the Ark console, Read level; for the reason see the agent entry.
	"GET /api/runs/{runId}/ark-console": {action: ActionRead, kind: KindRun, idParam: "runId"},

	// -- Session container (Docker executor) --------------------------------------------
	//
	// Viewing state is Read: it only returns "is there a container, alive or paused", nothing inside the container.
	// If viewer can see the run history, they should be able to see whether this run's container is still there.
	"GET /api/runs/{runId}/sandbox": {action: ActionRead, kind: KindRun, idParam: "runId"},

	// Resuming the container is **Write**, not Read.
	//
	// It is not a query: resuming brings the container up again, taking CPU and memory of the host (all sessions share
	// the same Docker daemon). A viewer clicking it repeatedly could slow down the containers of other people's chats,
	// a fault that would not point to "someone is clicking the resume button" at all.
	//
	// It is not Manage because it leaks nothing and changes no data: member can already start chats
	// (which also create containers), and resuming an existing container is no more dangerous than that.
	"POST /api/runs/{runId}/sandbox/resume": {action: ActionWrite, kind: KindRun, idParam: "runId"},

	// Listing files in the container is **Read**, the same level as "view run history, view chat content".
	//
	// The rule is what it exposes: directory and file names, which the agent would already mention in the chat
	// ("I wrote the result to /tmp/out.json"), and viewer can see that message.
	// A higher level would contradict itself: "you can see the chat saying what was written, but not that file".
	//
	// ⚠️ It **only lists, does not read**: there is no endpoint to fetch file content. Content may contain secrets (configs written by the agent,
	// injected credentials landing on disk), which is a problem of another level that would need its own design.
	"GET /api/runs/{runId}/sandbox/files": {action: ActionRead, kind: KindRun, idParam: "runId"},

	// Viewing the content of a single file. **The same level as listing files (Read)**, for the same reason: in the chat the agent
	// would already paste file contents for viewer to see; this is just another place to see the same thing.
	//
	// ⚠️ It exposes one more layer than listing files: file **content** may contain secrets (configs written by the agent).
	// Read accepts that scope: whoever can see this run can already see what the agent said
	// and wrote. If it really needs tightening, what should be tightened is "who can see this run", not this one.
	"GET /api/runs/{runId}/sandbox/file": {action: ActionRead, kind: KindRun, idParam: "runId"},

	// ⚠️⚠️ **webshell: the only one in this group at Manage, and the one that most must not be set lower.**
	//
	// It gives **arbitrary command execution inside the container**, sharing the same file system with the agent. Three reasons:
	//
	//  1. The container holds all the outputs of the agent's work this time (code, intermediate files, data);
	//     getting into the shell means reading and changing them: full read/write over workspace data;
	//  2. if command-line tools the agent used left credential caches on the container disk, the shell can cat them;
	//  3. changing the container means changing the agent's execution environment, which the agent uses in the next turn.
	//
	// Set lower, there is no error at all, and no UI would ever show that it happened.
	//
	// ⚠️ **Clarification**: injected credentials go through `ExecSpec.Envs`, **sent separately for each tool call**,
	// so they are not in the webshell's environment.
	// The claim "one env in the shell gets the owner's credentials" is inaccurate; do not use it as the reason:
	// the three above are the reasons.
	"POST /api/runs/{runId}/sandbox/shell": {action: ActionManage, kind: KindRun, idParam: "runId"},

	// Terminal (WebSocket). **The same level as the one above, for exactly the same reasons**: it gives the same ability,
	// just through another channel (a real PTY; vim/top/Ctrl-C all work).
	//
	// ⚠️ It is a **GET**: a WebSocket handshake is a GET with the Upgrade header.
	// Do not treat it as Read because "GET is usually a read": that is the entry in this table most easily set wrong,
	// and Read would let any viewer run arbitrary commands in the container.
	// Terminal downstream (SSE) and upstream (POST). **The same level for both**: the upstream one is "typing commands" itself,
	// and setting it lower would bypass the threshold of the downstream one.
	"GET /api/runs/{runId}/sandbox/terminal":        {action: ActionManage, kind: KindRun, idParam: "runId"},
	"POST /api/runs/{runId}/sandbox/terminal/input": {action: ActionManage, kind: KindRun, idParam: "runId"},
	// Issues a one-time ticket for opening the terminal WS. **Deliberately one level lower than the two above (ActionRead)**.
	//
	// The terminal WS actually only accepts this ticket (redeemed on the bare mux), so the ticket route is the only entry to the terminal;
	// with the level lowered, permissions must be restored on the issuing side, and only in Service, not here:
	// the matrix can only look up "what role this person has in this workspace" by route, and cannot tell who owns the host
	// in the run snapshot, while under the two-level binding model **the terminal of a personal direct-connection host may only be opened by the machine owner
	// themselves**, who may only be a member/viewer in the workspace. Tiered authorization (container/workspace machine = admin,
	// personal machine = owner) is done fail-closed in core.IssueTerminalTicket, the same kind of row-level check as
	// service/agent's "only agent.CreatedBy can change personal authorizations"
	// (the matrix cannot express "you own the resource").
	//
	// ⚠️ Lowering the level does not affect containers and workspace hosts: those two are still judged by workspace admin in Service,
	// and viewers trying to get a ticket still get 403; only runs with run.HostSource=personal take the owner tier.
	//
	// ⚠️ The real WS endpoint (the one on another domain) **is not in this table**: it is registered on the bare mux
	// and authenticated by redeeming this ticket; writing it here would trigger the reverse panic "declared but not registered".
	// That also means it bypasses this gate, so its authentication is pinned by core/sandboxtermws_test.go.
	"POST /api/runs/{runId}/sandbox/terminal/ticket": {action: ActionRead, kind: KindRun, idParam: "runId"},

	// -- Passthrough of Ark Managed Agents raw endpoints (see apihandler/handler_arkproxy.go) ----
	//
	// These endpoints forward Ark's resource API as is (request/response bytes unchanged, fields the same as Ark).
	// Levels are set by "what this really does on the Ark side":
	//
	// Reads (list resources/threads/files/memories/Vaults) = ActionRead, the same level as viewing run events:
	// whoever can see this session can see which files it has attached and which threads it has.
	//
	// Session-side writes (upgrade the session, attach/delete files) = ActionWrite, the same level as sending messages to the session:
	// they change how this one session runs, and member can already drive this session.
	//
	// ⚠️ **Writes to Vaults and credentials are Manage, not Write.** Those are credential materials
	// (MCP OAuth tokens / bearer tokens), and at runtime the agent accesses third parties with the credential owner's identity:
	// the same scale as the workspace-level ark-credential; letting member write them freely would let them decide
	// whose identity the agent acts with, for exactly the same reason as the ark-credential group.
	// Writes to memory stores/memory entries are still Write: that is session memory data, not identity material.
	//
	// ⚠️ **Reading credentials is also Read, not Manage**: Ark's credential responses **contain no secret**
	// (access_token/token are all write-only); what is read back is only names and mask info.
	"POST /api/runs/{runId}/ark/upgrade":                                                         {action: ActionWrite, kind: KindRun, idParam: "runId"},
	"GET /api/runs/{runId}/ark/resources":                                                        {action: ActionRead, kind: KindRun, idParam: "runId"},
	"POST /api/runs/{runId}/ark/resources":                                                       {action: ActionWrite, kind: KindRun, idParam: "runId"},
	"GET /api/runs/{runId}/ark/resources/{resourceId}":                                           {action: ActionRead, kind: KindRun, idParam: "runId"},
	"GET /api/runs/{runId}/ark/threads":                                                          {action: ActionRead, kind: KindRun, idParam: "runId"},
	"GET /api/runs/{runId}/ark/threads/{threadId}":                                               {action: ActionRead, kind: KindRun, idParam: "runId"},
	"GET /api/runs/{runId}/ark/threads/{threadId}/events":                                        {action: ActionRead, kind: KindRun, idParam: "runId"},
	"GET /api/runs/{runId}/ark/threads/{threadId}/stream":                                        {action: ActionRead, kind: KindRun, idParam: "runId"},
	"GET /api/runs/{runId}/ark/files":                                                            {action: ActionRead, kind: KindRun, idParam: "runId"},
	"POST /api/runs/{runId}/ark/files":                                                           {action: ActionWrite, kind: KindRun, idParam: "runId"},
	"GET /api/runs/{runId}/ark/files/{fileId}":                                                   {action: ActionRead, kind: KindRun, idParam: "runId"},
	"DELETE /api/runs/{runId}/ark/files/{fileId}":                                                {action: ActionWrite, kind: KindRun, idParam: "runId"},
	"GET /api/workspaces/{workspaceId}/ark/vaults":                                               {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/ark/vaults":                                              {action: ActionManage},
	"GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}":                                     {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}":                                    {action: ActionManage},
	"DELETE /api/workspaces/{workspaceId}/ark/vaults/{vaultId}":                                  {action: ActionManage},
	"GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials":                         {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials":                        {action: ActionManage},
	"GET /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}":          {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}":         {action: ActionManage},
	"DELETE /api/workspaces/{workspaceId}/ark/vaults/{vaultId}/credentials/{credentialId}":       {action: ActionManage},
	"GET /api/workspaces/{workspaceId}/ark/memory-stores":                                        {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/ark/memory-stores":                                       {action: ActionWrite},
	"GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}":                        {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}":                       {action: ActionWrite},
	"DELETE /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}":                     {action: ActionWrite},
	"GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories":               {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories":              {action: ActionWrite},
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/batch-create": {action: ActionWrite},
	"GET /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}":    {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}":   {action: ActionWrite},
	"DELETE /api/workspaces/{workspaceId}/ark/memory-stores/{memoryStoreId}/memories/{memoryId}": {action: ActionWrite},

	// -- Chat ---------------------------------------------------------------
	"POST /api/workspaces/{workspaceId}/conversations": {action: ActionWrite},
	"POST /api/runs/{runId}/events":                    {action: ActionWrite, kind: KindRun, idParam: "runId"},
	"GET /api/runs/{runId}/events/stream":              {action: ActionRead, kind: KindRun, idParam: "runId"},
	// Steering (interrupt + send a new message instead) is the same level as sending a message: both are "speaking into this session",
	// and interrupting itself is already allowed through POST .../events, so raising the level separately would be inconsistent.
	"POST /api/runs/{runId}/redirect": {action: ActionWrite, kind: KindRun, idParam: "runId"},

	// Queued messages waiting to be sent. Reading is the same level as sending messages (both are "content of this session"),
	// and queueing and cancelling are Write: they eventually become a user.message.
	//
	// ⚠️ The list route has allowMissing: when the run does not exist the handler returns 200 + [],
	// the same discipline as GET .../events (Guard jumping in with 404 would turn the empty list into a thrown exception).
	"GET /api/runs/{runId}/pending":                {action: ActionRead, kind: KindRun, idParam: "runId", allowMissing: true},
	"POST /api/runs/{runId}/pending":               {action: ActionWrite, kind: KindRun, idParam: "runId"},
	"DELETE /api/runs/{runId}/pending/{messageId}": {action: ActionWrite, kind: KindRun, idParam: "runId"},

	// -- Scheduled tasks -----------------------------------------------------------
	"GET /api/workspaces/{workspaceId}/schedules":  {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/schedules": {action: ActionWrite},
	"GET /api/schedules/{id}":                      {action: ActionRead, kind: KindSchedule, idParam: "id"},
	"PATCH /api/schedules/{id}":                    {action: ActionWrite, kind: KindSchedule, idParam: "id"},
	"PATCH /api/schedules/{id}/status":             {action: ActionWrite, kind: KindSchedule, idParam: "id"},
	"DELETE /api/schedules/{id}":                   {action: ActionWrite, kind: KindSchedule, idParam: "id"},
	"POST /api/schedules/{id}/run-now":             {action: ActionWrite, kind: KindSchedule, idParam: "id"},

	// History versions of scheduled tasks, the same levels as those three of agent (see the part above).
	"GET /api/schedules/{id}/revisions":                        {action: ActionRead, kind: KindSchedule, idParam: "id"},
	"GET /api/schedules/{id}/revisions/{revisionId}":           {action: ActionRead, kind: KindSchedule, idParam: "id"},
	"POST /api/schedules/{id}/revisions/{revisionId}/rollback": {action: ActionWrite, kind: KindSchedule, idParam: "id"},

	// -- Feishu integration -----------------------------------------------------------
	//
	// Writes on registrations and single apps are Manage, giving **the same answer** as the requireManage
	// already inside the feishu Service (onboarding.go / readiness.go / apps.go):
	// if the two layers disagree you get "Guard allows, Service rejects", and the error cannot point to the real cause.
	"GET /api/workspaces/{workspaceId}/feishu/apps":                      {action: ActionRead},
	"GET /api/workspaces/{workspaceId}/feishu/chats":                     {action: ActionRead},
	"POST /api/workspaces/{workspaceId}/feishu/registrations":            {action: ActionManage},
	"GET /api/workspaces/{workspaceId}/feishu/registrations/{sessionId}": {action: ActionManage},
	"GET /api/feishu/apps/{id}":                                          {action: ActionRead, kind: KindFeishuApp, idParam: "id"},
	"POST /api/feishu/apps/{id}/readiness/refresh":                       {action: ActionManage, kind: KindFeishuApp, idParam: "id"},
	"POST /api/feishu/apps/{id}/admin-authorization":                     {action: ActionManage, kind: KindFeishuApp, idParam: "id"},
	// The trigger mode is Manage, consistent with the other single-app writes in this group: it changes "does the bot reply in groups without
	// being @-mentioned", which is this app's external behavior.
	"PATCH /api/feishu/apps/{id}/trigger-mode": {action: ActionManage, kind: KindFeishuApp, idParam: "id"},
	"DELETE /api/feishu/apps/{id}":             {action: ActionManage, kind: KindFeishuApp, idParam: "id"},

	// -- For development (only registered when DevRoutes is on; see optionalRoutes below) ------------
	"POST /api/_debug/reset": {action: ActionNone},
	"POST /api/_debug/clear": {action: ActionNone},

	// -- Fallback ---------------------------------------------------------------
	//
	// Unmatched /api/ paths get a structured envelope from core's notFound. It knows no workspace, and should not
	// be blocked by authorization: if blocked, "wrong route" would show up as "no permission".
	"/api/": {action: ActionNone},
}

// optionalRoutes are routes that **may not be registered**, excluded from the "declared but not registered" direction of the check.
//
// The two _debug routes are only registered when DevRoutes is true (off in production). Without the exemption, production startup would panic for
// "declared but not registered", which would really take the service down.
var optionalRoutes = map[string]bool{
	"POST /api/_debug/reset": true,
	"POST /api/_debug/clear": true,
}
