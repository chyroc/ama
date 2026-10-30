// Summarizes the permission levels of the 87 routes into "feature domain → lowest role", for the frontend to render a human-readable permission table.
//
// # Why the frontend does not hand-write this table
//
// A hand-written permission description **always drifts**: the next time someone adds a route, that table silently goes out of date. And an outdated
// permission description is **worse than none**: users follow it and think they can do something, get a 403, then go ask
// an admin for permission, and the admin, looking at that table, also thinks they should be able to. Both are fooled by the same piece of paper.
//
// This repo already has an answer for this kind of problem: the OpenAPI spec is generated from this table (openapi/routes.go file header).
// This copies that approach exactly, even the reconciliation shape is the same (generated output checked in + a byte-for-byte comparison test).
//
// # Two derivation steps, with only one hand-written layer in between
//
//	step one  domainOf   route → feature domain (pure path checks, no fallback: unrecognized makes the test red)
//	step two  pairs      (feature domain, action level) → one line of description (**the only hand-written layer**)
//	          roles      computed from levelOf + requiredLevel, not copied
//
// The hand-written layer is pinned from both directions (capability_test.go):
//
//	appears in matrix but not in pairs  → a new route fell into a combination nobody described, red
//	in pairs but not in matrix          → that description no longer has a matching route, red
//
// So "adding a route" forces someone to answer "who should do this, and how is it described in the table", instead of relying on someone remembering.
//
// # roles are computed, so owner and admin are always the same
//
// ⚠️ **owner and admin have exactly the same permissions** (both are LevelAdmin in levelOf; see the comment there
// for why). This is the easiest place to get this table wrong: intuition gives owner a few more checks, and that is wrong,
// misleading users.
//
// Instead of restating that fact here, **we let it be computed**: roles asks levelOf for each role whether the level is enough.
// If one day owner really is split into a higher level, the generated output changes with it, the golden test turns red, and someone is forced to look.
// The only extra thing owner has is "cannot be removed" (countOwners in core/workspace.go),
// which is not an action level, so it is not in this table; the frontend explains it separately below the table.

package authz

import (
	"fmt"
	"sort"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Feature domains. Constants instead of literals: domainOf and the pairs of capabilityDecls must reference the same word,
// and one wrong letter would put that route "into a domain nobody describes"; the test would turn red, but the error says
// "no description" instead of "you made a typo", one more detour.
const (
	domainWorkspace    = "workspace"
	domainMember       = "member"
	domainAgent        = "agent"
	domainSkill        = "skill"
	domainRun          = "run"
	domainConversation = "conversation"
	domainSchedule     = "schedule"
	domainIssue        = "issue"
	domainArk          = "ark"
	domainIntegration  = "integration"
	domainFeishu       = "feishu"
	domainSandbox      = "sandbox"
	domainAPIKey       = "apiKey"
	// domainHost is "the host executor": workspace-level bindings are managed in workspace settings.
	domainHost = "host"
	// domainSelf is queries "about the caller themselves"; currently only "what is my role in this workspace".
	//
	// A separate domain instead of merging into workspace or members: those two describe "what is in the workspace",
	// while this asks **who you are yourself**, and even viewer must be able to ask it.
	domainSelf = "self"
	// domainArkProxy is the passthrough of Ark Managed Agents' raw endpoints (/ark/...):
	// session resources/threads/files, Vaults/credentials, memory stores. It is not "configuring Ark credentials"
	// (that is domainArk), but using an already configured key to operate Ark-side resources.
	domainArkProxy = "arkProxy"
)

// domainOf decides which feature domain a route belongs to by its path. Unrecognized returns an empty string (the test turns red on it).
//
// # Order-sensitive, and a separate set of checks from openapi's tagFor
//
// The check order is "more specific subpaths first": `/api/agents/{id}/ark-credential` contains both agents
// and ark-credential, and must go to "Ark credentials" rather than "Agent": swapping that key changes
// whose bill it goes on, which is not the same thing as changing the prompt.
//
// ⚠️ **openapi/routes.go's tagFor is deliberately not reused.** That one is the endpoint grouping for agents reading the spec
// (grouped by "where this set of endpoints lives"); this is the feature domain for humans (grouped by "what capability this is");
// the two only happen to look alike. Merged into one function, the day the spec wants to adjust grouping would break the permission table along the way,
// and that **reports no error**: the table still looks complete; only the ownership of some row quietly changed.
//
// ⚠️ **There is no default branch returning a fallback domain.** A fallback would turn "a new capability nobody described"
// into "silently filed under workspace settings", exactly the drift this table is meant to prevent.
func domainOf(path string) string {
	switch {
	// The Ark passthrough /ark/ segment must be checked first: it lives under /runs/{id} and /workspaces/{id},
	// and checked late it would be swallowed by the run domain; and it is different from "Ark credential configuration" (domainArk):
	// that configures AMA's own key, while this uses the key to operate Ark-side resources.
	case contains(path, "/ark/"):
		return domainArkProxy
	// The session container must be checked before runs: all its paths live under /api/runs/{runId}/sandbox.
	// Filed under run, "container terminal" (manage) would be squeezed into the same domain as "view run history" (read),
	// the two levels furthest apart in this table.
	case contains(path, "/sandbox"):
		return domainSandbox
	// Ark credentials have two sets of paths (workspace-level /api/workspaces/{}/ark-credential and
	// agent-level /api/agents/{id}/ark-credential), both must be checked before agents.
	case contains(path, "/ark-credential"):
		return domainArk
	case contains(path, "/feishu"):
		return domainFeishu
	// Agent authorization injection (handing a real person's credentials to an agent). Must be checked before agents:
	// it lives on /api/agents/{id}/integrations, but its meaning is far from "change the agent":
	// the injected token represents a real person, and what the agent can do equals what that employee can do.
	case contains(path, "/integrations"):
		return domainIntegration
	// "What is my role in this workspace". It lives under /api/workspaces/{} and must be checked before the members domain,
	// otherwise it would slip past the "/member" case and fall into unclassified.
	//
	// ⚠️ Filed under domainSelf instead of merged into "view workspace content": that row describes business data in the workspace,
	// while this asks about **the caller's own identity**, which even viewer must be able to ask (the UI uses it to decide
	// whether to draw editable or read-only). Mixing it in would make that row's description vague.
	case contains(path, "/my-role"):
		return domainSelf
	case contains(path, "/members"):
		return domainMember
	case contains(path, "/api-keys"):
		return domainAPIKey
	case contains(path, "/conversations"):
		return domainConversation
	// Check "/issue" instead of "/issues": the latter does not match /api/issue-comments and
	// /api/issue-attachments, and those two groups would fall out into "unclassified".
	case contains(path, "/issue"):
		return domainIssue
	case contains(path, "/schedules"):
		return domainSchedule
	case contains(path, "/skills"):
		return domainSkill
	case contains(path, "/agents"):
		return domainAgent
	// runs together with usage: usage is just an aggregated reading of run history.
	//
	// ⚠️ This domain has **both reads and writes**: POST /api/runs/{runId}/events is "send a message" (write),
	// and GET .../events is "read events" (read), same path, different methods. So a domain is not a level;
	// the levels are carried by the (domain, level) combinations below.
	case contains(path, "/runs"), contains(path, "/usage"):
		return domainRun
	// Workspace-level host binding (ActionManage, workspace settings). Must come before the check below for exactly
	// workspace: it lives under workspaces/{}, but its meaning is "decide whose machines this
	// workspace's agents can run on", the same level as members/authorization binding, not renaming the workspace itself.
	case contains(path, "/hosts"):
		return domainHost
	// The workspace itself only has rename and delete (list and create workspace are ActionNone and never enter this table).
	case path == "/api/workspaces/{workspaceId}":
		return domainWorkspace
	}
	return ""
}

// contains is an alias of strings.Contains, only to keep the cases above short.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// capabilityDecl is the hand-written part of one row.
//
// **It does not say "who can do it"**: that is computed from the levels in pairs (see Capabilities). Hand-writing levels
// would create a second source of truth, and when two sources of truth disagree there is no error at all.
type capabilityDecl struct {
	// id is a stable identifier, used by e2e and as the frontend key. Changing it breaks e2e, so treat it as a contract.
	id string
	// label is the feature domain's name, shown directly on screen.
	label string
	// detail is extra explanation, may be empty. Write things you "cannot guess from the label alone".
	detail string
	// pairs are the (feature domain, action level) combinations this row covers, like "agent:read".
	//
	// One row can cover several combinations ("view workspace content" spans agent / skills / run history…),
	// but **their levels must be the same**, otherwise what "lowest role" this row has cannot be stated.
	// That is pinned by TestEveryCapabilityHasOneAction.
	pairs []string
}

// capabilityDecls is all rows of that table, **in display order** (read-only → write → manage).
//
// # Why sorted by role ascending instead of grouped by feature
//
// Users usually look at this table to answer one specific question: "can my role do X". Sorted by role ascending,
// once you find your own level, everything above is what you can do and everything below is what you cannot, done at a glance; grouped by feature,
// rows of the same level are scattered all over, and you would have to check the marks row by row.
//
// ⚠️ **The table has no "non-member" column.** Non-members always get 403, and the workspace list does not even return workspaces
// they are not in: that is not "less permission", it is "cannot see this workspace"; a column full of crosses would make people think they can get in.
// The frontend explains it in one sentence below the table.
var capabilityDecls = []capabilityDecl{
	// -- Read-only (viewer and up) --------------------------------------------------
	{
		id:    "view-content",
		label: "查看空间内容",
		detail: "Agent、技能、运行历史与对话记录、定时任务、问题、飞书接入状态，" +
			"以及空间可用的宿主机列表（Agent 编辑页选执行端要用）",
		pairs: []string{
			domainAgent + ":read",
			domainSkill + ":read",
			domainRun + ":read",
			domainSchedule + ":read",
			domainIssue + ":read",
			domainFeishu + ":read",
			domainHost + ":read",
		},
	},
	{
		id:    "view-sandbox",
		label: "查看会话容器/宿主机状态，并换取网页终端入场票",
		// The terminal ticket route is at the read level, but a ticket is not a terminal: the real authorization is tiered by run at issue time:
		// terminals of containers and workspace hosts still require an admin (see manage-sandbox-shell), and the terminal of a personal direct-connection
		// host can only be opened by the machine owner. Without saying this clearly, members would think they can
		// open every terminal.
		detail: "查看是只读的；终端票能换到的终端仍按运行分权限：容器/空间宿主机" +
			"要管理员，个人直连宿主机只有机器属主本人",
		pairs: []string{domainSandbox + ":read"},
	},
	{
		id:     "view-ark",
		label:  "查看方舟凭证",
		detail: "只有掩码，明文永远不出服务端；也能点「校验」",
		pairs:  []string{domainArk + ":read"},
	},
	{
		id:    "view-ark-proxy",
		label: "读取方舟侧资源",
		detail: "列出/查询会话挂载的文件与线程、Files、保管库 Vault、记忆库与记忆条目；" +
			"这些是方舟 Managed Agents 的原始接口，字段与方舟一致",
		pairs: []string{domainArkProxy + ":read"},
	},
	{
		id:    "create-api-key",
		label: "创建 API Key",
		// ⚠️ This row is the easiest to misread as "a read-only role can escalate privileges", so detail must point out the inheritance.
		detail: "key 严格继承你本人的权限，不是一套独立的作用域",
		pairs:  []string{domainAPIKey + ":read"},
	},
	{
		id:     "view-own-role",
		label:  "查看自己在本空间的角色",
		detail: "任何成员都能查，界面靠它决定哪些操作画成只读",
		pairs:  []string{domainSelf + ":read"},
	},

	// -- Write (member and up) --------------------------------------------------
	{
		id:     "converse",
		label:  "发起对话、发消息",
		detail: "一次对话就是一条运行记录，所以它同时进运行历史",
		pairs: []string{
			domainConversation + ":write",
			// POST /api/runs/{runId}/events: send a message into an existing session.
			domainRun + ":write",
		},
	},
	{
		id:    "edit-agent",
		label: "创建与修改 Agent",
		pairs: []string{domainAgent + ":write"},
	},
	{
		id:     "edit-skill",
		label:  "创建与修改技能",
		detail: "含上传技能包",
		pairs:  []string{domainSkill + ":write"},
	},
	{
		id:    "ark-proxy-session-data",
		label: "操作会话相关方舟资源",
		detail: "给会话挂文件（含上传 Files）、升级会话运行配置、写记忆库与记忆条目；" +
			"消耗这个空间方舟账号的配额",
		pairs: []string{domainArkProxy + ":write"},
	},
	{
		id:    "edit-schedule",
		label: "管理定时任务",
		pairs: []string{domainSchedule + ":write"},
	},
	{
		id:    "edit-issue",
		label: "提问题、评论与附件",
		pairs: []string{domainIssue + ":write"},
	},
	{
		id:     "resume-sandbox",
		label:  "恢复已暂停的会话容器",
		detail: "会占用计算配额，所以不给只读角色",
		pairs:  []string{domainSandbox + ":write"},
	},

	// -- Manage (admin and up; owner is exactly the same) ---------------------------------
	{
		id:     "manage-workspace",
		label:  "空间设置与删除空间",
		detail: "改名、改 key（等于改所有链接）、删空间",
		pairs:  []string{domainWorkspace + ":manage"},
	},
	{
		id:     "manage-member",
		label:  "成员管理",
		detail: "邀请、改角色、移除",
		pairs:  []string{domainMember + ":manage"},
	},
	{
		id:     "manage-ark",
		label:  "配置方舟凭证",
		detail: "换掉或删掉会影响这个空间所有 Agent 的对话，账也记在那把 key 上",
		pairs:  []string{domainArk + ":manage"},
	},
	{
		id:    "manage-ark-proxy-vault",
		label: "方舟保管库 Vault 与凭证",
		detail: "Vault 里装的是第三方凭据（MCP OAuth / bearer token），Agent 运行时会以" +
			"凭证主人的身份行动；让普通成员自助写等于让他决定 Agent 以谁的身份访问外部系统",
		pairs: []string{domainArkProxy + ":manage"},
	},
	{
		id:     "manage-feishu",
		label:  "飞书接入",
		detail: "注册应用、解除接入、改免 @ 触发",
		pairs:  []string{domainFeishu + ":manage"},
	},
	{
		id:    "manage-integration",
		label: "把授权注入给 Agent",
		// ⚠️ The consequence of this row is heavier than "configure Ark credentials", so detail must say clearly "whose identity it acts with",
		// otherwise readers would think it is just a switch.
		//
		// ⚠️ **Personal authorizations are outside the scope of this permission**, and this must be written out: it is not decided by role;
		// only the agent's creator can add or remove them (see core.ensureIntegrationsAllowed).
		// Without it, admins following this table would think they can change all authorizations, then hit a 403.
		detail: "个人授权代表一个真人，**只有 Agent 的创建者本人能加减，管理员也不行**",
		pairs:  []string{domainIntegration + ":manage"},
	},
	{
		id:     "manage-host",
		label:  "宿主机绑定",
		detail: "把成员自己注册的宿主机绑进空间，Agent 才能选它做执行端（v1 只能绑本人的机器）",
		pairs:  []string{domainHost + ":manage"},
	},
	{
		id:    "manage-sandbox-shell",
		label: "会话容器的终端与命令执行",
		// ⚠️ This is the most counter-intuitive row in the table: it lives in the "run detail" page and looks like a viewing feature,
		// but it is actually arbitrary command execution in the container (sharing the file system with the agent).
		detail: "等于空间数据的完整读写权，所以只给管理员",
		pairs:  []string{domainSandbox + ":manage"},
	},
}

// Capability is one row of the permission table, read-only outside the package.
type Capability struct {
	// ID is a stable identifier.
	ID string
	// Label is the feature domain's name.
	Label string
	// Detail is extra explanation, may be an empty string.
	Detail string
	// MinRole is the lowest role that can do this, like "member".
	MinRole string
	// Roles are all roles that can do this, from highest permission to lowest.
	//
	// **Computed** (levelOf + requiredLevel), so owner and admin are always both in or both out.
	Roles []string
	// Action is the action level of this row, like "write".
	Action string
	// Routes are the routes this row covers, like "PATCH /api/agents/{id}", sorted.
	//
	// **It is included for traceability**: every row in the table can point back to which entries of matrix.go. It also makes "someone
	// added a route" visible in the diff of the generated output, so it is not missed in code review.
	Routes []string
}

// RoleOrder is the order of roles from highest to lowest; the frontend orders the table header by it.
//
// Exported instead of letting the frontend write its own: when the frontend's copy disagrees with levelOf there is no error at all.
func RoleOrder() []string {
	return []string{
		string(apimodel.RoleOwner),
		string(apimodel.RoleAdmin),
		string(apimodel.RoleMember),
		string(apimodel.RoleViewer),
	}
}

// Capabilities derives the whole permission table.
//
// It returns an error instead of panicking: the callers are the generator and tests, which can printf the error to guide people to fix it.
// The startup path does not call it, so "making the service fail to start" buys no safety here.
//
// All three kinds of errors point to the same thing: **someone touched routes but not this table**:
//
//	a route cannot be filed into any feature domain   → domainOf is missing a case
//	a (domain, level) is described by nobody          → capabilityDecls is missing a row, or it should be merged into an existing row
//	a description row has no matching route           → that capability is gone; delete this row
func Capabilities() ([]Capability, error) {
	// Step one: group routes by (feature domain, level).
	type pairKey struct {
		domain string
		action Action
	}
	byPair := map[pairKey][]string{}
	for pattern, perm := range routePermissions {
		// ActionNone is not in this table: those endpoints do not look at workspace roles (account-level authorization management, API Keys,
		// user preferences, feedback reports, plus the workspace list and creating workspaces). **They are not "the lowest permission";
		// they are "unrelated to workspace roles"**, and mixing them in would make users think read-only roles can change other people's things.
		// The frontend mentions their existence in one sentence below the table.
		if perm.action == ActionNone {
			continue
		}
		method, path, ok := splitPattern(pattern)
		if !ok {
			// The fallback route "/api/" has no method prefix; it is also ActionNone and was already filtered out above.
			// Reaching here means the table has a non-None route without a method, which is a typo.
			return nil, fmt.Errorf("authz: 路由 %q 没有 HTTP 方法前缀，但档位不是 none", pattern)
		}
		d := domainOf(path)
		if d == "" {
			return nil, fmt.Errorf(
				"authz: 路由 %q 归不进任何功能域。\n"+
					"这条路由不会出现在成员页那张「角色权限说明」表里，"+
					"于是用户看到的说明是不完整的（而它看起来完整）。\n"+
					"修复：在 authz/capability.go 的 domainOf 里给它加一条 case，"+
					"必要时在 capabilityDecls 里加一行说明。", pattern)
		}
		key := pairKey{domain: d, action: perm.action}
		byPair[key] = append(byPair[key], method+" "+path)
	}

	// Step two: merge the combinations into rows by the hand-written pairs, checking both directions along the way.
	claimed := map[pairKey]string{} // combination → id of the row that claimed it
	out := make([]Capability, 0, len(capabilityDecls))
	for _, decl := range capabilityDecls {
		var routes []string
		var action Action
		for i, p := range decl.pairs {
			d, a, err := splitPair(p)
			if err != nil {
				return nil, fmt.Errorf("authz: 说明行 %q 的 pairs 里 %v", decl.id, err)
			}
			key := pairKey{domain: d, action: a}
			// A row claimed a combination another row already claimed: that would make the same batch of routes appear twice in the table,
			// and users cannot tell which row counts.
			if prev, dup := claimed[key]; dup {
				return nil, fmt.Errorf(
					"authz: 组合 %q 被 %q 与 %q 同时认领，同一批路由会在表里出现两行",
					p, prev, decl.id)
			}
			rs, exists := byPair[key]
			if !exists {
				return nil, fmt.Errorf(
					"authz: 说明行 %q 声明了组合 %q，但 matrix.go 里没有任何路由落在它上面。\n"+
						"要么那个能力已经删了（把这条 pair 去掉，行空了就删整行），"+
						"要么域名/档位写错了字。", decl.id, p)
			}
			claimed[key] = decl.id
			routes = append(routes, rs...)
			// The combinations within one row must have the same level, otherwise "lowest role" cannot be stated.
			if i == 0 {
				action = a
			} else if a != action {
				return nil, fmt.Errorf(
					"authz: 说明行 %q 混了两种档位（%s 与 %s）。\n"+
						"一行只能有一个「最低角色」，混档会让表里那一行是错的。\n"+
						"修复：拆成两行。", decl.id, ActionName(action), ActionName(a))
			}
		}
		if len(decl.pairs) == 0 {
			return nil, fmt.Errorf("authz: 说明行 %q 没有声明任何 pairs，它描述不了任何东西", decl.id)
		}
		sort.Strings(routes)
		out = append(out, Capability{
			ID:      decl.id,
			Label:   decl.label,
			Detail:  decl.detail,
			MinRole: minRoleName(action),
			Roles:   rolesFor(action),
			Action:  ActionName(action),
			Routes:  routes,
		})
	}

	// The reverse direction: combinations that appear in matrix but are claimed by no row.
	//
	// **This is the most important direction**: it corresponds to "someone added a new route", and missing it shows as
	// "the permission table looks complete, just one capability fewer", with no error at all.
	missing := make([]string, 0)
	for key, routes := range byPair {
		if _, ok := claimed[key]; ok {
			continue
		}
		sort.Strings(routes)
		missing = append(missing, fmt.Sprintf("%s:%s（%v）", key.domain, ActionName(key.action), routes))
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf(
			"authz: 下面这些 (功能域, 档位) 组合没有任何说明行认领：\n  %v\n\n"+
				"于是成员页那张「角色权限说明」表里少了这些能力，"+
				"而表看起来是完整的——用户会以为自己做不了，或者管理员以为已经说明过了。\n"+
				"修复：在 authz/capability.go 的 capabilityDecls 里加一行，"+
				"或把这个组合并进语义相同的已有行的 pairs 里。", missing)
	}

	return out, nil
}

// rolesFor lists all roles that meet this level, from highest permission to lowest.
//
// **It asks levelOf one by one** and copies no conclusion: that owner and admin are the same level is therefore computed,
// not restated here. If levelOf changes one day, the generated output changes with it and the golden test turns red.
func rolesFor(a Action) []string {
	need := requiredLevel(a)
	var out []string
	for _, name := range RoleOrder() {
		if levelOf(apimodel.WorkspaceRole(name)) >= need {
			out = append(out, name)
		}
	}
	return out
}

// splitPair splits "agent:read" into domain and level.
func splitPair(p string) (string, Action, error) {
	for i := 0; i < len(p); i++ {
		if p[i] == ':' {
			d, name := p[:i], p[i+1:]
			a, ok := actionFromName(name)
			if !ok {
				return "", 0, fmt.Errorf("%q 的档位 %q 不认识（只能是 read / write / manage）", p, name)
			}
			return d, a, nil
		}
	}
	return "", 0, fmt.Errorf("%q 不是 \"域:档位\" 的形状", p)
}

// actionFromName is the inverse of ActionName.
//
// ⚠️ **It deliberately does not accept "none"**: ActionNone routes never enter this table (see Capabilities),
// and allowing "x:none" would make people think they can write a row for account-level endpoints, a row that would never match anything.
func actionFromName(name string) (Action, bool) {
	switch name {
	case "read":
		return ActionRead, true
	case "write":
		return ActionWrite, true
	case "manage":
		return ActionManage, true
	}
	return 0, false
}
