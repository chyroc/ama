/**
 * Translation tables.
 *
 * A flat key-value table, with keys named "module.purpose". Flat instead of nested because lookup and filling gaps are both straightforward:
 * whichever key is missing, TypeScript reports it directly on the en table (see the Messages type below).
 *
 * Text with parameters uses {name} placeholders, replaced by the second argument of t().
 */

export const zhCN = {
  // Common
  "common.save": "保存",
  "common.cancel": "取消",
  "common.create": "创建",
  "common.delete": "删除",
  "common.edit": "编辑",
  "common.remove": "移除",
  "common.saved": "已保存",
  "common.loading": "加载中",
  "common.retry": "重试",
  "common.loadFailed": "加载失败",
  "common.accessDenied": "无权访问",
  "common.loadFailedUnknown": "加载失败，请稍后重试",
  "common.actionFailed": "操作失败",
  "common.all": "全部",
  "common.back": "返回",
  "common.optional": "可选",
  "common.notFilled": "未填写",
  "common.irreversible": "此操作不可撤销。",

  // App shell and navigation
  "nav.overview": "概览",
  "nav.groupBuild": "构建",
  "nav.groupObserve": "观测",
  // The "manage" group: workspace-level configuration (only workspace settings; members is one of its tabs), at the same level as the previous two groups
  "nav.groupManage": "管理",
  // The "account" group: configuration that **belongs only to you** and is the same in every workspace.
  //
  // A separate group instead of stuffing it into "manage": that group is workspace-level (changes affect everyone),
  // the two are adjacent, have similar icons, and are both named "xx settings", exactly the shape most likely to fool people.
  // A group title is exactly where "what scope is this area" is answered, so let it say so directly.
  "nav.groupAccount": "账号",
  "nav.agents": "Agents",
  "nav.chat": "对话",
  "nav.skills": "技能库",
  "nav.issues": "问题",
  "nav.feishu": "飞书",
  "nav.weixin": "微信",
  "nav.runs": "运行历史",
  "nav.usage": "用量与洞察",
  "nav.workspaceSettings": "空间设置",
  "nav.switchWorkspace": "切换空间",
  "nav.manageWorkspaces": "管理所有空间",
  "nav.preferences": "偏好设置",
  // The item in the sidebar "account" group. Keep the same name as the page title (prefs.title):
  // an entry and its destination named differently make people think they clicked the wrong thing.
  "nav.globalSettings": "全局设置",
  "nav.openNav": "打开导航",
  "nav.closeNav": "关闭导航",
  "nav.accountMenu": "账号菜单",

  // Workspace onboarding / list
  // Empty state: an empty list has **two** causes, the backend returns [] for both and the frontend cannot tell them apart, so both paths must be given.
  // Saying only "create a workspace first" would send people who "are not in any workspace's member list" in the wrong direction.
  "ws.emptyTitle": "还没有可进入的空间",
  "ws.emptyDesc":
    "新建一个空间即可开始：Agent、技能、飞书应用和运行历史都归属于空间。如果你认为自己本该在某个空间里，请联系该空间的管理员把你加为成员。",
  "ws.pickTitle": "选择空间",
  "ws.pickDesc": "所有操作都在空间内进行",
  "ws.new": "新建空间",
  "ws.create": "创建空间",
  "ws.enter": "进入空间",
  "ws.name": "空间名称",
  "ws.namePlaceholder": "例如：增长中台",
  "ws.nameRequired": "请填写空间名称",
  "ws.key": "空间 Key",
  "ws.keyDesc": "作为 URL 的一部分，只能用小写字母、数字和连字符",
  "ws.desc": "描述",
  "ws.descPlaceholder": "这个空间是做什么的（可选）",
  "ws.memberCount": "{count} 名成员",
  "ws.createdAt": "创建于 {time}",
  "ws.createFailed": "创建失败",
  "ws.keyTaken": "Key「{key}」已被占用",

  // Overview
  "overview.subtitle": "空间概览",
  "overview.startTitle": "从创建第一个 Agent 开始",
  "overview.startDesc": "给 Agent 一段系统提示词和一个模型，再绑定技能或飞书应用，它就能干活了。",
  "overview.metricAgents": "Agent",
  "overview.metricAgentsHint": "{count} 个已启用",
  "overview.metricSkills": "技能",
  "overview.metricSkillsHint": "空间级资产",
  // The label of the Feishu metric card reuses nav.feishu directly (OverviewPage.tsx), so there is no separate
  // overview.metricFeishu here: keeping it would only let it and the navigation text be changed separately and quietly drift.
  "overview.metricFeishuHint": "{count} 个已连接",
  "overview.metricTokens": "Token 消耗",
  "overview.metricRuns": "{count} 次运行",
  "overview.metricFailed": " · {count} 次失败",
  "overview.recentRuns": "最近运行",
  "overview.viewAll": "查看全部",
  "overview.noRuns": "还没有运行记录",

  // Agent
  "agent.listDesc": "每个 Agent 有独立的系统提示词与模型配置，可绑定空间技能与飞书应用",
  "agent.new": "新建 Agent",
  "agent.emptyTitle": "还没有 Agent",
  "agent.emptyDesc": "创建一个 Agent，给它一段系统提示词和一个模型，就可以接飞书或 API 触发了。",
  // ⚠️ This line used to be "after creating, you can bind workspace skills and Feishu apps on the detail page". Workspace skills can now **be chosen on this
  // page** (the "choose from the skill library" dialog), and saying the old thing would push users to the detail page for an extra round;
  // Feishu apps can still only be bound on the detail page, so that half stays.
  "agent.newDesc": "飞书应用在创建后于详情页绑定",
  "agent.name": "名称",
  "agent.namePlaceholder": "工单分诊助手",
  "agent.nameRequired": "请填写名称",
  "agent.desc": "描述",
  "agent.descPlaceholder": "接飞书群消息，自动分诊用户反馈",
  "agent.system": "系统提示词",
  "agent.systemDesc": "定义 Agent 的角色与行为规则，方舟侧对应 system 字段",
  "agent.systemPlaceholder": "你是⋯⋯\n\n原则：\n- ⋯",
  "agent.loading": "正在加载 Agent",
  "agent.systemRequired": "请填写系统提示词",
  "agent.systemCount": "{count} / 100000 字符",
  "agent.model": "模型",
  "agent.reasoning": "推理强度",
  "agent.serviceTier": "服务档位",
  "agent.noFastTier": "该模型不支持 fast 档位，方舟文档「模型支持的参数列表」里只有 lite 与 turbo 支持。",
  "agent.status": "状态",
  "agent.basicInfo": "基本信息",
  "agent.systemSection": "系统提示词",
  "agent.systemSectionDesc": "这个 Agent 独有的提示词，方舟侧对应 system 字段",
  "agent.modelSection": "模型",
  "agent.modelSectionDesc": "不同 Agent 可以用不同模型与推理强度",
  "agent.toolsSection": "内置工具",
  "agent.toolsSectionDesc": "方舟提供的沙箱工具，勾选后 Agent 可以调用",
  // The built-in skills section of the create page. The text must say two things, "checked by default" and "can be unchecked":
  // if the default is not stated, users do not know what they are agreeing to.
  "agent.builtinSkillsSection": "平台内置技能",
  "agent.builtinSkillsSectionDesc":
    "容器里已预装这两个 CLI，默认挂给新 Agent；不需要的可以取消勾选",
  // Platform credential injection. **The text must make the risk clear**: after injection the agent can read that credential
  // (one env in bash is enough), so what it can do equals what the workspace owner can do.
  // Not saying so lets users lend out their own identity without knowing it.
  "agent.integrationsSection": "授权",
  "agent.integrationsSectionDesc":
    "勾选后，容器里的工具能以空间所有者的身份使用对应平台。默认不勾任何一项",
  "agent.integrationBindLabel": "启用 {name}",
  "agent.integrationReady": "已就绪",
  "agent.integrationNeedsAuth": "未授权",
  // ⚠️ This line is only for the kind **you cannot authorize yourself** (canAuthorize is false).
  // The kind that can be self-authorized now gets a button in place; the two are mutually exclusive, and showing both would be two contradicting instructions.
  "agent.integrationNeedsAuthHint":
    "你还没授权这一项，勾不上。去「全局设置 → 授权管理」里点一次「发起授权」，回来即可勾选。",
  // ⚠️⚠️ "Which one is about to be authorized": this is where the requirement "which Feishu authorization can be chosen when authorizing" lands.
  //
  // One person may have several Feishu apps in different workspaces, and the token is **bound** to the app.
  // This row used to have just one button, and clicking it authorized the **account-level default** app; after authorizing the wrong one
  // the agent still could not use lark-cli, and the runtime message "no user authorization has been done for Feishu app cli_xxx yet"
  // **does not point to "you authorized the wrong one"**. So the target must be written out before clicking.
  //
  // ⚠️ appId must be shown too: in practice one person's several apps were **all called "AMA 助手"**
  // (all built following the same doc), so giving only the name says nothing.
  "agent.integrationAuthorizeTarget": "将授权：{name}（{appId}）",
  // ⚠️ This workspace has not connected a Feishu app yet. **Do not fall back to the account default one** (that is a cross-workspace mismatch),
  // so the button is hidden and directions are given instead: Feishu apps are **workspace-level** resources, connected per workspace.
  "agent.integrationNoAppInWorkspace":
    "这个空间还没接入飞书应用，所以现在没法授权。先去接入一个：",
  // Authorize in place. **No need to leave this form**, so the wording does not mention "where to go", only what this step does.
  "agent.integrationAuthorize": "在此授权",
  // The button text while polling. Being **close in length** to the one above is deliberate: the button keeps its space throughout and only changes text,
  // and too big a difference would make the width of this row jump (skill: switching variants must not introduce layout shift).
  "agent.integrationAuthorizeWaiting": "等待确认",
  // The authorization page was just opened and polling has not started (sources that need no polling stop at this line).
  "agent.integrationAuthorizeOpened": "已在新标签页打开授权页，完成后回到这里。",
  // ⚠️⚠️ The polling line must say "it updates automatically, no need to refresh".
  // The device code is in another tab, and when users come back to this page they only see a spinning button; without saying so they would refresh,
  // and **refreshing loses the unsaved form together with this authorization**.
  "agent.integrationAuthorizePolling":
    "已打开飞书授权页。在飞书里点「同意」之后，这里会自动变成可勾选——不用刷新页面，页面上没保存的改动也不会丢。",
  // ⚠️ This line must make clear "whose identity": the person ticking it may not be the workspace owner, and may not realize
  // it injects someone else's credentials. Not saying so lets them lend out someone else's identity without knowing it.
  "agent.integrationRisk":
    "注意：勾选后本 Agent 能读取这些凭据，并以空间所有者的身份调用其有权限的服务。只把可信的提示词与技能配给它。",
  // The read-only view non-admins see.
  //
  // ⚠️ The three sentences each have a job, and missing one leaves people stuck:
  //   ReadOnly      what the current state is (which ones are injected / none at all)
  //   ReadOnlyHint  why you cannot change it + **who to find** (without it they would not know to find an admin)
  //   ReadOnlyNone  the empty state when none is ticked, so that block is not left with only a title
  "agent.integrationsReadOnly": "已注入的授权",
  "agent.integrationsReadOnlyHint":
    "修改需要管理员权限。注入的凭据代表一个真人，Agent 拿到后能做的事等于那个员工能做的事，所以只有空间管理员或拥有者可以改。需要调整请联系空间管理员。",
  "agent.integrationsReadOnlyNone": "未注入任何授权",
  "agent.envSection": "环境变量",
  "agent.envSectionDesc": "执行命令时注入容器。普通项还会写进系统提示词让模型知道",
  "agent.envKey": "变量名",
  "agent.envValue": "值",
  "agent.envSecret": "敏感",
  "agent.envAdd": "添加变量",
  "agent.envRemove": "删除这条",
  "agent.envReplace": "更换",
  "agent.envAtLimit": "已达上限（{max} 条）",
  "agent.envKeyInvalid": "只能用字母、数字、下划线，且不能以数字开头",
  "agent.envKeyReserved": "这是平台保留的变量名，由平台自动注入，不能自定义",
  "agent.envKeyDuplicate": "变量名重复了",
  "agent.envValueTooLong": "值太长了",
  // ⚠️⚠️ **The last sentence is the main reason this text exists; it must not be deleted or softened.**
  //
  // "Sensitive" only does two things: it does not go into the system prompt, and it is not shown in plaintext on the page. It is **not** access control:
  // one env in the container reads the plaintext (exactly the same situation as platform credentials).
  // Calling it "secure" would make users put in credentials that really should not be given to the agent, which is worse than not having this flag.
  "agent.envNote":
    "普通项会写进系统提示词（模型能看到），敏感项不会、页面上也只显示掩码。注意：敏感项同样会注入容器，Agent 执行 env 命令即可读到明文——这个标记只是避免它进入模型上下文与页面，不等于 Agent 拿不到。",
  // -- Credential aliases (advanced) ------------------------------------------------------
  //
  // ⚠️ The text must make three things clear; missing any one makes people configure it wrong:
  //   1. the value is filled by the platform, users cannot touch it (this is exactly what essentially distinguishes it from "custom environment variables");
  //   2. **it does not take effect when the source is not ticked for authorization**; otherwise users find no value after configuring and think the feature is broken;
  //   3. the agent can still read it with env (the same discipline as sensitive env, must not be softened).
  "agent.envAliasSection": "凭据别名",
  "agent.envAliasSectionDesc":
    "把平台已有的凭据，用你指定的变量名再注入一份——给只认自己那套变量名的第三方 CLI 用。",
  "agent.envAliasName": "变量名",
  "agent.envAliasSource": "引用哪个凭据",
  "agent.envAliasAdd": "添加别名",
  "agent.envAliasRemove": "删除这条别名",
  "agent.envAliasAtLimit": "已达上限（{max} 条）",
  "agent.envAliasNameInvalid": "只能用字母、数字、下划线，且不能以数字开头",
  "agent.envAliasNameReserved": "这是平台保留的变量名，不能用作别名",
  "agent.envAliasNameDuplicate": "变量名重复了",
  "agent.envAliasClashesEnv": "这个名字已经是一条自定义环境变量，两者只能留一个",
  // ⚠️ The second sentence (no effect if the source is not ticked) is the key of this part: that is the case most likely to make people think "the feature is broken".
  // ⚠️ **Do not write Markdown `**` here**: these keys are rendered as plain text (no other i18n text in the repo
  // uses Markdown), and asterisks would be shown as-is. To emphasize, change the word order.
  "agent.envAliasNote":
    "值由平台在执行时填入，你看不到也填不了原文——要填自己的值请用上面的「环境变量」。注意：引用的凭据必须在上面的「授权」里勾上，没勾的话这条别名不会生效；另外 Agent 执行 env 命令仍能读到明文。",
  "agent.envAliasReadOnly": "已配置的凭据别名",
  "agent.envAliasReadOnlyNone": "没有配置任何凭据别名。",
  "agent.envAliasReadOnlyHint":
    "别名决定容器里出现哪些凭据变量，与「授权」同一档位，只有空间管理员或拥有者可以改。",
  "agent.notFound": "Agent 不存在",
  "agent.notFoundDesc": "它可能已被删除。",
  "agent.backToList": "返回列表",
  "agent.deleteTitle": "删除 Agent「{name}」",
  "agent.deleteDesc": "删除后其运行历史仍会保留，但该 Agent 不再能被触发。此操作不可撤销。",
  "agent.deleteAction": "删除 Agent",
  /*
   * The result of syncing to Ark asynchronously after saving. **Success is silent** (no hint at all),
   * so there is only text for the failure side here.
   *
   * Each one must say **what to do**, not just "it failed": the failure reason is given by the server
   * (failureReason is a Chinese sentence that can be shown directly), and what is given here is **guidance**,
   * by failureStage: credential problems users can fix themselves, Ark failures just need a retry.
   */
  "agent.syncFailed": "配置没能同步到方舟",
  "agent.syncFailedCredential": "方舟凭证有问题，请到空间设置检查 API Key",
  "agent.syncFailedTransient": "方舟服务暂时不可用，可以重试",
  "agent.syncFailedSkills": "技能没能挂到方舟上，请检查技能配置",
  "agent.syncRetry": "重试",
  "agent.syncRetrying": "重试中",
  /* The save itself succeeded: this line must say so, otherwise users would think the change was for nothing. */
  "agent.syncFailedHint": "改动已保存，但方舟侧还是旧配置；下次开始对话时会再试一次。",
  // A mark next to the agent name: the agent was deleted, but the name snapshot on sessions/runs is still there,
  // otherwise nobody would know whom this record belongs to.
  "agent.deleted": "已删除",
  "agent.tryIt": "试一下",
  "agent.summaryModel": "模型",
  "agent.summaryReasoning": "推理",
  "agent.summarySkills": "技能",
  "agent.summaryFeishu": "飞书",
  "agent.summaryVersion": "版本",
  "agent.tabConfig": "配置",
  "agent.tabSkills": "技能",
  "agent.tabFeishu": "飞书",
  "agent.tabRuns": "运行历史",
  "agent.toolCount": "{count} 个内置工具",
  "agent.skillCount": "{count} 技能",
  "agent.feishuBound": "飞书已接",
  "agent.updatedAt": "更新于 {time}",
  "agent.createdBy": "创建者",
  // Only filter "created by me", **with no matching "modified by me"**: the meaning of the latter drifts with every save
  // (change someone else's agent once and it shows up in my list forever), while what users ask is "which ones are mine".
  "agent.mineOnly": "我创建的",
  "agent.mineOnlyEmpty": "这个空间里没有你创建的 Agent",
  // ⚠️ Existing agents cannot get their creator filled in, and the UI must honestly say "unknown" instead of leaving it empty or erroring.
  "user.unknownActor": "未记录创建者",

  // Skills
  "skill.listDesc": "技能是空间级资产，Agent 通过引用来绑定；改动技能会立即影响所有绑定它的 Agent",
  "skill.new": "新建技能",
  "skill.emptyTitle": "还没有技能",
  "skill.emptyDesc": "技能把领域知识和操作流程打包成可复用单元，比把规则堆进系统提示词更好维护。",
  "skill.title": "技能名称",
  "skill.titlePlaceholder": "工单分诊",
  "skill.titleRequired": "请填写技能名称",
  "skill.name": "标识",
  "skill.namePlaceholder": "ticket-triage",
  "skill.nameRequired": "请填写标识",
  "skill.nameInvalid": "只能用小写字母、数字和连字符",
  "skill.when": "何时使用",
  "skill.whenPlaceholder": "当用户反馈问题、报障或提交工单时使用。判定严重级别、归类到业务模块⋯⋯",
  "skill.whenDesc": "模型靠这段话决定是否调用该技能，写清楚触发场景比描述能力更有效",
  "skill.whenRequired": "请说明何时使用该技能",
  "skill.body": "技能正文",
  "skill.bodyDesc": "Markdown 格式，对应方舟技能包里的 SKILL.md",
  "skill.bodyRequired": "请填写技能正文",
  "skill.tags": "标签",
  "skill.tagsDesc": "逗号分隔",
  "skill.tagsPlaceholder": "客服, 分诊",
  "skill.forkAction": "复制一份来改",
  "skill.builtin": "平台内置",
  "skill.boundCount": "{count} 个 Agent 绑定",
  "skill.createTitle": "新建技能",
  "skill.editTitle": "编辑技能",
  "skill.editHint": "{count} 个 Agent 正在使用该技能，保存后立即生效",
  "skill.deleteTitle": "删除技能「{name}」",
  "skill.deleteBound": "有 {count} 个 Agent 绑定了该技能，删除后这些绑定会被一并解除。此操作不可撤销。",
  "skill.editAction": "编辑技能",
  "skill.deleteAction": "删除技能",

  // Upload a skill package (SKILL.md or zip)
  "skill.upload": "上传技能包",
  "skill.uploadTitle": "上传技能包",
  "skill.uploadDesc": "支持 SKILL.md 或 zip 包。zip 里附带的 scripts/ references/ 等文件会一并保留，并在对话时写进容器供 agent 读取。",
  "skill.uploadPick": "选择文件",
  "skill.uploadDrop": "把文件拖到这里，或点击选择",
  "skill.uploadHint": "支持 .md 与 .zip，最大 {size}",
  "skill.uploadParsing": "正在解析⋯",
  "skill.uploadBadType": "只支持 .md 与 .zip 文件",
  "skill.uploadTooLarge": "文件超过 {size} 上限",
  "skill.uploadEmpty": "请先选择一个文件",
  "skill.previewTitle": "解析结果",
  "skill.previewDesc": "确认下面的内容无误后再创建。名称与描述是从 SKILL.md 的 frontmatter 里读出来的。",
  "skill.previewFiles": "附带文件（{count}）",
  "skill.previewNoFiles": "没有附带文件",
  "skill.previewBody": "正文预览",
  "skill.filesCount": "{count} 个附件",
  "skill.bindTitle": "绑定空间技能",
  "skill.bindDesc": "绑定的是引用而非拷贝：技能内容更新后，这个 Agent 立即使用新版本",
  "skill.manageLibrary": "管理技能库",
  "skill.bindLabel": "绑定技能 {name}",
  "skill.bindCount": "已绑定 {bound} / {total}（方舟单个 Agent 上限 20 个）",
  "skill.noneInWorkspace": "空间里还没有技能",
  "skill.noneInWorkspaceDesc": "技能是空间级资产，先在技能库里创建，再回到这里绑定。",
  "skill.goToLibrary": "去技能库",

  // The "choose from the skill library" dialog when creating an Agent. The whole skill library is in a dialog instead of inline in the form;
  // see the file header of components/SkillLibraryPicker.tsx for why.
  "skill.pickerOpen": "从技能库选择",
  "skill.pickerTitle": "从技能库选择技能",
  "skill.pickerDesc": "绑定的是引用而非拷贝：技能内容更新后，这个 Agent 立即使用新版本。之后在 Agent 详情页也能改。",
  "skill.pickerConfirm": "确定",
  "skill.pickerSearch": "搜技能名称或描述",
  // The count includes the built-in skills checked by default: they take up those 20 slots on Ark too.
  "skill.pickerCount": "已选 {count} / {max}",
  "skill.pickerAtLimit": "已达上限，要换一个请先取消。",
  "skill.pickerNoMatch": "没有匹配的技能",
  // The summary of that row on the form. With 0, say "none chosen yet" rather than "0 chosen": the latter reads
  // like there is a number there, while at this point the user has not done anything.
  "skill.pickerSummaryNone": "还没有绑定空间技能",
  "skill.pickerSummary": "已绑定 {count} 个空间技能",
  // The ones folded when the summary area does not fit. **Do not write it as "+{count}"**: that shape looks like a clickable
  // chip, while it is not clickable (removing a "+3" makes no sense).
  "skill.pickerSummaryMore": "等 {count} 个",
  // The cross on a summary area chip. The opposite direction of the checkbox line skill.bindLabel; each is its own action.
  "skill.unbindLabel": "取消绑定 {name}",
  "skill.pickerSection": "空间技能",
  // ⚠️ The half "none is checked by default" **must not be deleted**: indeed no workspace skill is checked by default (only built-in skills are
  // checked by default, in the block above), and without saying so users would think they missed one. Checked against the implementation:
  // the skillIds of `emptyAgentDraft()` is `[]`, and the only effect writing defaults only adds
  // `skills.filter(isBuiltinSkill)`, so no workspace skill gets in.
  "skill.pickerSectionDesc": "本空间自建或上传的技能，默认都不勾",
  // Empty state: this workspace has no non-built-in skill at all. **Do not give a dialog that opens empty**;
  // say directly where to get one (a skill can be created or uploaded on the skill library page).
  "skill.pickerEmptyTitle": "技能库里还没有技能",
  "skill.pickerEmptyDesc": "先去技能库新建或上传一个，再回来绑定。",

  // Skill detail page: view all files in a skill (SKILL.md + attachments).
  "skill.detailOpen": "查看 {name} 的详情",
  "skill.backToLibrary": "技能库",
  "skill.notFound": "技能不存在",
  "skill.notFoundDesc": "它可能已经被删除了。回技能库看看还有哪些。",
  "skill.filesTitle": "技能文件",
  // SKILL.md is a **virtual node**: its content comes from the instructions field, not a real attachment.
  "skill.bodyFileName": "SKILL.md",
  "skill.bodyFileTag": "正文",
  "skill.filesNone": "这个技能没有附件",
  "skill.filesNoneDesc": "只有 SKILL.md 一份正文。带附件的技能要用 zip 上传。",
  // The fallback when the body is empty. In local demo mode the body of built-in skills is empty (see builtinSkills.ts),
  // and a blank would make people think the page is broken, so it is stated explicitly.
  "skill.bodyEmpty": "正文由平台维护，这里不展示。",
  "skill.fileBinary": "二进制文件，不支持预览",
  "skill.fileBinaryDesc": "它会被原样写进容器，agent 可以读到。",
  "skill.fileTruncated": "文件较大，只显示前 256 KiB。",
  "skill.fileLoadFailed": "读不到这个文件的内容",
  // When to show it: **the misunderstanding takes hold the moment the file list is seen**, so part 1 must already say it.
  "skill.injectHint": "技能文件在新建会话时写入容器；正在进行的对话不会更新。",
  // The way out for read-only skills. The text must say "why it cannot be changed" + "then what should I do".
  "skill.readOnlyBuiltin": "平台内置技能，正文是平台代码的一部分，不可修改。",
  "skill.readOnlyForkHint": "要在它的基础上改，先复制一份。",
  // Editing skill files (part 2).
  "skill.saveFile": "保存 {name}",
  "skill.savedFile": "已保存",
  // When to show it: this line must be shown after a successful save, otherwise users think it did not take effect and keep saving.
  "skill.savedNeedsNewSession": "已保存。正在进行的对话仍用旧版本，新内容在下次新建会话时生效。",
  "skill.saveFileFailed": "保存失败",
  "skill.fileNotEditable": "这个文件不支持编辑",
  "skill.fileBinaryNotEditable": "二进制文件只能查看，不能在这里编辑。",
  "skill.unsavedTitle": "有未保存的改动",
  "skill.unsavedHint": "先保存，或者放弃这次改动再切到别的文件。",
  "skill.discard": "放弃改动",
  "skill.bodyEditLabel": "技能正文（SKILL.md）",
  "skill.fileEditLabel": "文件内容",

  // Issues: workspace-level issue records.
  //
  // ⚠️ Not the same thing as "issue feedback" (feedback.*): that one is machine collection reported with one click on error,
  // this one is an entity written by people, discussable, with status transitions. Neither group of text should reuse the other.
  "issue.title": "问题",
  "issue.listDesc": "记录这个空间里待处理的问题：写清现象、贴上截图、在下面讨论，处理完标记关闭",
  "issue.new": "提问题",
  "issue.emptyTitle": "还没有问题",
  "issue.emptyDesc": "遇到卡住的事就记一条：标题和正文填一个就行，可以贴图和附件，其他人能在下面接着讨论。",
  "issue.createTitle": "提一个问题",
  "issue.editTitle": "编辑问题",
  "issue.fieldTitle": "标题",
  "issue.fieldTitlePlaceholder": "一句话说清是什么问题",
  // This line is part of the feature, not decoration: without it users do not know they can fill in only the body.
  "issue.fieldTitleDesc": "不填就从正文第一行自动取",
  "issue.fieldBody": "正文",
  "issue.fieldBodyPlaceholder": "复现步骤、期望结果、实际结果。支持 Markdown，可以直接粘贴截图",
  "issue.fieldBodyDesc": "支持 Markdown。图片可以直接粘贴或拖进来",
  // Must match the backend errIssueEmpty() and LocalApiClient word for word: all three places must be the same sentence.
  "issue.requireTitleOrBody": "请至少填写标题或正文",
  "issue.fieldStatus": "状态",
  "issue.fieldPriority": "优先级",
  "issue.deleteTitle": "删除问题「{name}」",
  "issue.deleteDesc": "这条问题的评论与附件会一并删除。此操作不可撤销。",
  "issue.deleteAction": "删除问题",
  "issue.editAction": "编辑问题",
  // The author changed from the whole sentence "created by xxx" to avatar + name, so this is the role prefix of the tooltip
  // (same as agent.createdBy / revision.changedBy), no longer a sentence with {name}.
  "issue.authorLabel": "创建者",
  "issue.commentAuthorLabel": "评论者",
  "issue.createdAt": "创建于 {time}",
  "issue.updatedAt": "更新于 {time}",
  "issue.filterAll": "全部状态",
  "issue.countComments": "{count} 条评论",
  "issue.countAttachments": "{count} 个附件",
  "issue.noBody": "（没有正文）",
  "issue.backToList": "返回问题列表",

  // Comments
  "issue.comments": "讨论",
  "issue.commentPlaceholder": "补充信息、说说你的排查结果⋯⋯",
  "issue.commentSubmit": "发表",
  "issue.commentEmpty": "还没有人讨论这个问题",
  "issue.commentRequired": "评论内容不能为空",
  "issue.commentEdited": "已编辑",
  "issue.commentEditAction": "编辑评论",
  "issue.commentDeleteAction": "删除评论",
  "issue.commentDeleteTitle": "删除这条评论",
  "issue.commentDeleteDesc": "此操作不可撤销。",
  "issue.commentSave": "保存",
  "issue.commentCancel": "取消",

  // Attachments
  "issue.attachments": "附件",
  "issue.attachmentDrop": "把图片或文件拖到这里，或点击选择",
  "issue.attachmentHint": "支持图片、pdf、txt/md/csv/json 与 zip，单个最大 {size}",
  "issue.attachmentUploading": "正在上传⋯",
  "issue.attachmentBadType": "不支持的文件类型",
  "issue.attachmentTooLarge": "附件超过 {size} 上限",
  "issue.attachmentTooMany": "一个问题最多上传 {count} 个附件",
  "issue.attachmentDeleteAction": "删除附件",
  "issue.attachmentInsert": "插入到正文",
  // The hint after a pasted upload succeeds: users cannot see "what happened", so say explicitly that the image was inserted into the body.
  "issue.attachmentPasted": "图片已上传并插入正文",
  "issue.attachmentNone": "还没有附件",

  // Enums
  // Product suggestions (Suggestion): platform level, feedback about the AMA platform itself.
  //
  // ⚠️ **Do not reuse any line of feedback.***: that group is about "automatically reported errors",
  // this group is about "what you want the platform to become". Mixing them would make users think clicking sends logs.
  "suggestion.title": "产品建议",
  "suggestion.desc": "对 AMA 平台本身的意见与建议。这里只显示你提过的，平台团队会看到并回复",
  "suggestion.descStaff": "全部用户提交的产品建议。你是平台团队成员，能看到所有人提的、并回复与标记状态",
  "suggestion.create": "提一条建议",
  "suggestion.createDesc": "说说你希望 AMA 变成什么样。可以只写正文，标题会自动从第一行提取",
  "suggestion.submit": "提交",
  "suggestion.view": "查看",
  "suggestion.fieldTitle": "标题",
  "suggestion.fieldBody": "正文",
  "suggestion.fieldStatus": "状态",
  "suggestion.titlePlaceholder": "一句话说清你想要什么（可不填）",
  "suggestion.bodyPlaceholder": "遇到了什么、希望怎么改。支持 Markdown",
  "suggestion.titleHint": "标题与正文至少填一个。提交后可以补充截图",
  "suggestion.emptyTitle": "还没有提过建议",
  "suggestion.emptyDesc": "用得不顺手、或者想要什么功能，都可以在这里说。平台团队会看到",
  "suggestion.footnote": "你的建议只有平台团队看得到，其他用户看不到你提了什么",
  "suggestion.loadFailed": "读取建议失败，请稍后重试",
  "suggestion.deleteTitle": "删除这条建议",
  "suggestion.deleteDesc": "「{title}」及其下的全部回复与附件都会被删除。此操作不可撤销。",
  "suggestion.attachments": "截图与附件",
  "suggestion.addImage": "添加图片",
  "suggestion.uploading": "上传中…",
  "suggestion.replies": "对话",
  "suggestion.noReplies": "还没有回复",
  "suggestion.staffBadge": "平台团队",
  "suggestion.edited": "已编辑",
  "suggestion.replyAsStaff": "以平台团队身份回复",
  "suggestion.replyAsUser": "追问或补充",
  "suggestion.replyPlaceholder": "写点什么…",
  "suggestion.replySubmit": "发送",
  // Two states, not three like issues. See SuggestionStatus in types/domain.ts for why.
  "enum.suggestionStatus.open": "未解决",
  "enum.suggestionStatus.resolved": "已解决",
  "enum.issueStatus.open": "待处理",
  "enum.issueStatus.in_progress": "处理中",
  "enum.issueStatus.closed": "已关闭",
  "enum.issuePriority.p0": "P0",
  "enum.issuePriority.p1": "P1",
  "enum.issuePriority.p2": "P2",

  // Feishu
  // ⚠️ One Feishu app can only be bound to one Agent (1:1 exclusive, see core.Service.ensureFeishuAppsFree).
  // This line used to say "one app can be bound to multiple Agents", which was true before an earlier change,
  // and contradicted feishu.bindDesc: the list page taught users to do something the server answers with 409.
  "feishu.listDesc": "接入飞书后，Agent 可以在飞书里被触发；一个应用只能绑一个 Agent",
  "feishu.connect": "接入应用",
  "feishu.emptyTitle": "还没有接入飞书",
  "feishu.emptyDesc": "去飞书授权一次，平台自动创建应用并拿到凭证，不用手填 App ID 和密钥。",
  "feishu.copyUrl": "复制回调地址",
  "feishu.copied": "已复制",
  "feishu.copy": "复制",
  "feishu.boundAgents": "已绑定的 Agent",
  // The guidance when not bound. **"Pick one" not "tick several"**: 1:1 exclusive, so both text and interaction must speak as single choice.
  "feishu.bindPromptHint": "还没有 Agent 接管它。选一个 Agent，它就能在飞书里回话了。",
  "feishu.bindPickAgent": "选择 Agent",
  "feishu.bindPromptAction": "绑定 Agent",
  "feishu.bindNoAgents": "这个空间还没有 Agent，先创建一个。",
  "feishu.nextStepTitle": "下一步：交给一个 Agent",
  "feishu.boundToAgent": "已交给「{name}」，它现在能在飞书里收发消息了。",
  "feishu.connectedAt": "接入于 {time}",
  "feishu.deleteAction": "断开应用",
  "feishu.deleteTitle": "断开应用「{name}」",
  "feishu.deleteBound": "该应用已绑定 {count} 个 Agent，断开后绑定会被解除，该 Agent 将不再接收飞书消息。",
  "feishu.deleteDesc": "断开后该应用的回调地址立即失效。飞书侧的应用不会被删除，可以重新接入。",
  // ⚠️ The "飞书应用" (Feishu app) in these three means **the app entity on the Feishu side**, not this site's "飞书" (Feishu) section;
  // shortening it to "飞书" would not read right ("bind Feishu" sounds like binding a platform). Keep it as is.
  "feishu.bindTitle": "绑定飞书应用",
  "feishu.bindDesc": "绑定后该应用收到的消息会触发这个 Agent；一个应用只能绑一个 Agent",
  "feishu.manageApps": "管理飞书应用",
  "feishu.bindLabel": "绑定 {name}",
  "feishu.stateBound": "已绑定",
  "feishu.stateUnbound": "未绑定",
  // Apps bound by another agent are **not shown at all** on the Agent detail page, so there is no
  // hint "already bound by agent X" here: writing about B on A's page gives the reader nothing
  // actionable. What really stops taking it over is the server's 409 on save, not this text.
  //
  // ⚠️ When all are bound the list is empty, which looks like "the workspace has not connected Feishu yet" (feishu.noneInWorkspace)
  // but has a different cause; they must be said separately, otherwise a normal state looks like a failure.
  "feishu.allTakenTitle": "没有可绑定的飞书应用",
  "feishu.allTakenDesc":
    "空间里的 {count} 个飞书应用都已经交给别的 Agent 了。一个应用只能绑一个 Agent，可以去飞书页面查看绑定关系，或者再接入一个新应用。",
  "feishu.noneInWorkspace": "空间里还没有接入飞书",
  // "它" (it) needs something to refer to, so "飞书应用" (Feishu app) stays in this line; changing it to "connect Feishu first... bind it"
  // would turn it into binding a platform.
  "feishu.noneInWorkspaceDesc": "先接入飞书应用，再回到这里把它绑定到 Agent。",
  "feishu.goToApps": "去接入应用",

  // Trigger mode. The text of the two options **must make the behavior difference clear**: writing only "mode A / mode B",
  // users cannot judge which to pick, and picking wrong (the bot auto-replying nonstop in chats) is noisy.
  "feishu.triggerTitle": "触发方式",
  "feishu.triggerDesc": "决定机器人在群里什么时候开口。单聊不受影响，始终直接回复。",
  "feishu.triggerLabel": "{name} 的触发方式",
  "enum.feishuTrigger.mention_only": "仅 @ 我时回复",
  "enum.feishuTrigger.topic_and_mention": "新话题自动回复 + 其他情况 @ 触发",
  // The supplementary line under the selected option, covering "which chats, whether @ is needed".
  "feishu.triggerHint.mention_only": "所有群里都必须 @ 机器人才会回复。",
  // ⚠️ **It must say "only new topics"**: this used to say "messages inside topics need no @",
  // and implementing that sentence led to false triggers in production: a third-party bot replied in a topic,
  // and the bot also added a reaction and auto-replied (2026-09-02). No-@ is only for the opening message.
  "feishu.triggerHint.topic_and_mention": "话题群里新开一个话题时无需 @ 就会回复；话题内的后续跟帖、以及其他群，仍需 @。",

  // How to handle the next message sent in a row. **Both option names are only two characters, but the consequences are far apart**
  // (redirect kills the running turn), so the two hints below are required, not decoration.
  "agent.followUpTitle": "上一句还在跑时，又说了一句",
  "enum.followUp.redirect": "转向（打断上一轮）",
  "enum.followUp.queue": "排队（等上一轮跑完）",
  // The line under the selected option states the cost. **The redirect line must say "will be interrupted"**:
  // from the word "redirect" alone, users would not think a tool halfway through would stop too.
  "agent.followUpHint.redirect":
    "立刻打断正在进行的回答（工具跑到一半也会停），改为处理新消息。适合改主意时立即纠偏。",
  "agent.followUpHint.queue":
    "等当前这一轮答完，再自动处理新消息。机器人会先回一句「上一句还在处理中」。",

  // Whether to show detailed work (chain of thought). **The hint must say "that message will no longer appear in the chat"**:
  // "do not show" is easily understood as "collapse it", while it actually does not send that message at all.
  "agent.progressDetailTitle": "回答时是否展示详细工作内容",
  "enum.progressDetail.full": "展示（思考过程与工具调用）",
  "enum.progressDetail.brief": "不展示（只给最终答案）",
  "agent.progressDetailHint.full":
    "群里会多一条思维链消息，边跑边展示它在想什么、调用了哪些工具。适合需要看过程或排查问题时。",
  "agent.progressDetailHint.brief":
    "群里不会出现思维链消息，只有收到消息的表情和最终答案。适合面向使用者、过程无需外露的场景。",

  // The warning for missing no-@ permission. **The text must state the consequence** (messages not received, the setting has no effect);
  // just saying "permission missing" does not explain why to care, and this was the only information that could save users
  // in that "the bot said nothing" failure.
  "feishu.groupScopeMissing": "飞书还没授予「群消息」权限，当前设置不会生效",
  "feishu.groupScopeMissingHint":
    "没有这个权限，飞书不会把未 @ 机器人的群消息推给应用——机器人收不到，也就不会回复。去开放平台勾选该权限并发布新版本，然后回来重新检测。",
  "feishu.grantOnOpenPlatform": "去飞书授权",
  // The warning for "cannot receive bot messages". It **must be separate** from the one above: Feishu splits receiving chat messages into two levels,
  // and with only the level above not a single bot message is pushed. Merging them would make the failure "the alert chat receives no alerts"
  // completely invisible: that is how 2026-09-02 took two rounds of debugging.
  "feishu.botScopeMissing": "飞书还没授予「接收机器人消息」权限，机器人发的消息收不到",
  "feishu.botScopeMissingHint":
    "「群消息」权限只覆盖真人发的消息。没有这一档，其他机器人（各类告警、监控播报）发的消息飞书一条都不会推下来——报警群这类没有真人说话的群会完全没反应。去开放平台勾选该权限并发布新版本，然后回来重新检测。",
  // Optional permissions when connecting. Make clear "adding them later means going through it again": the authorization page is fixed once opened.
  "feishu.requestGroupMessage": "同时申请「群消息」权限（话题群免 @ 自动回复要用）",
  "feishu.requestGroupMessageHint":
    "敏感权限，需管理员审批。会一并申请「接收机器人消息」那一档——否则报警群里机器人发的告警收不到。不勾也能接入，但机器人只在被 @ 时才收得到群消息；以后要加得重新授权一次。",

  // Device code connection flow
  "feishu.connectTitle": "接入飞书应用",
  "feishu.connectDesc": "去飞书授权一次，平台自动创建应用并取得凭证",
  "feishu.backToList": "返回列表",
  "feishu.step1Title": "去飞书授权",
  "feishu.step1Desc": "会打开飞书的授权页，确认后平台自动创建应用并拿到凭证，你不用手填任何密钥。",
  "feishu.openAuthPage": "打开飞书授权页",
  "feishu.reopenAuthPage": "没自动打开？点这里重新打开授权页",
  "feishu.step2Title": "等待授权",
  "feishu.step2Waiting": "已在等你在飞书里点「同意」……授权后本页会自动继续。",
  "feishu.step3Title": "接入完成",
  "feishu.finish": "完成",
  "feishu.connectAnother": "再接一个",
  "feishu.retry": "重新发起",
  "feishu.failed.expired": "授权链接已过期，请重新发起接入。",
  "feishu.failed.denied": "授权被拒绝。如果这不是你的操作，请重新发起接入。",
  "feishu.failed.invalid": "授权失败，请重新发起接入。",
  "feishu.failed.pending": "授权尚未完成。",
  "feishu.failed.completed": "接入已完成。",
  "feishu.whyTitle": "为什么不用手填 App ID 和密钥",
  "feishu.whyDesc":
    "飞书开放平台提供了设备码自助注册：授权一次即可由平台代为创建应用并取得凭证。密钥只在服务端短暂停留，不会回传浏览器，也不会展示给任何人。",

  // Readiness diagnosis
  "feishu.readinessTitle": "就绪度",
  "feishu.readinessDesc": "装好不等于能用。下面列出还差哪些配置",
  "feishu.readinessUnchecked": "未检测",
  "feishu.recheck": "重新检测",
  "feishu.checkedAt": "{time}检测",
  "feishu.readyHint": "全部就绪，Agent 已经可以在飞书里收发消息。",
  "feishu.missingScopes": "缺少 {count} 项权限",
  "feishu.missingScopesHint": "这些权限还没申请，需要你到飞书开放平台的「权限管理」里添加。",
  "feishu.unapprovedScopes": "有 {count} 项权限等管理员批准",
  "feishu.unapprovedScopesHint": "这些权限已申请，需要租户管理员批准后才生效。",
  "feishu.unapprovedRequestedHint": "申请已提交给管理员，等批准即可，不用重复提交。",
  "feishu.requestAdmin": "申请管理员授权",
  "feishu.missingEvents": "缺少 {count} 项事件订阅",
  "feishu.missingEventsHint": "到飞书开放平台的「事件与回调」里补齐这些事件，否则收不到消息。",
  "feishu.missingCallbacks": "缺少 {count} 项回调订阅",
  "feishu.missingCallbacksHint": "缺少卡片回调时，消息卡片上的按钮点了不会有反应。",
  "feishu.botDisabled": "机器人能力未启用",
  "feishu.botDisabledHint": "到飞书开放平台的「添加应用能力」里启用机器人。",
  "feishu.notPublished": "应用版本未发布",
  "feishu.notPublishedHint": "应用需要发布一个版本并通过审核后才能正式使用。",
  "feishu.runtimeUnavailable": "无法连接到这个应用",
  "feishu.runtimeUnavailableHint": "凭证可能已失效，重新接入一次即可换取新凭证。",
  "feishu.notInspectable": "读不到应用的完整配置",
  "feishu.notInspectableHint":
    "缺少「应用信息自主管理」权限，因此事件与版本状态无法确认；上面的判断可能不完整。",

  // Chat
  "chat.title": "对话",
  "chat.subtitle": "跟 agent 直接对话，实时看它怎么干活",
  "chat.new": "新建对话",
  "chat.recent": "最近对话",
  "chat.untitled": "新对话",
  "chat.pickAgentTitle": "选一个 agent 开始",
  "chat.pickAgentDesc": "对话会记进运行历史，可以随时回来接着看。",
  "chat.pickAgent": "选择 agent",
  "chat.placeholder": "说点什么…",
  "chat.send": "发送",
  "chat.interrupt": "中断",
  "chat.thinking": "思考中",
  "chat.hint": "Enter 发送，Shift + Enter 换行",
  "chat.emptyTitle": "还没有对话",
  "chat.emptyDesc": "选一个 agent 发起第一次对话。",
  "chat.sendFailed": "发送失败",
  // The fallback when even message cannot be obtained (not an Error, or message is an empty string).
  // Deliberately not "unknown error": that sentence gives users no guidance at all, while this at least says "you can retry".
  "chat.sendFailedUnknown": "发送没成功，请再试一次",
  "chat.noAgents": "这个空间还没有 agent",
  "chat.noAgentsDesc": "得先有一个 agent 才能开始对话。",
  "chat.createAgent": "创建 Agent",
  "chat.viewRun": "查看运行详情",
  "chat.startedWith": "由「{name}」应答",
  // Frozen state: after the agent is deleted, the session can only be viewed, not sent to. The hint deliberately does not suggest "recreate an agent with the same name":
  // recreating gets a new id, and cannot save this session.
  "chat.frozenTitle": "关联的 Agent 已被删除，会话已冻结",
  "chat.frozenHint": "历史消息仍可查看；要继续对话，请新建一个 Agent 再开一个新会话。",
  // Disabling and deleting **must have two separate sets of text**: deleting is irreversible (only creating a new one), disabling can be undone in one step.
  // Sharing one set would guide users to "create a new Agent", while what they really should do is change the status back,
  // leaving an extra agent for nothing, with the original one's skills and Feishu bindings still sitting there.
  "chat.frozenDisabledTitle": "关联的 Agent 已停用，会话已冻结",
  "chat.frozenDisabledHint":
    "历史消息仍可查看；到 Agent 详情页把状态改回「已启用」就能继续对话。",
  // Session does not exist: the URL is invalid (database switched, data cleared, a link to someone else's workspace).
  // The hint must say "refreshing does not help", otherwise users keep refreshing, and every refresh is a round of subscription.
  "chat.goneTitle": "这条会话已经不存在了",
  "chat.goneHint": "它可能已被清理，或这个链接来自另一个空间。刷新不会让它回来，请从左侧挑一条会话或新建一个。",
  /*
   * No usable Ark credential: say so **before the user starts typing** (from a production issue).
   *
   * Before this there was nothing here: the input box rendered as usual, and users only got a 422 after typing a whole message and clicking send,
   * "please configure the Ark API Key in workspace settings first": rejected after writing a message for nothing. Many such
   * reports came from many different workspaces, all of the shape "the first message of every new workspace hits it once", so it is
   * an **onboarding flow** problem rather than a failure: every new workspace hits it again.
   *
   * # The two kinds of "none" need different guidance, matching arkKey in backend core/ark.go item by item
   *
   *   never configured                     → go to workspace settings and paste one
   *   the referenced global key is gone    → go to global settings and reconfigure, or switch to this workspace's own
   *
   * The cost of wrong guidance is users following it all the way and finding it does not help.
   *
   * # The third axis is **permission**: people without permission need a different sentence
   *
   * Configuring the key is for owner/admin (`PUT .../ark-credential` declares ActionManage).
   * Giving viewer/member a "go to workspace settings" button means they click in and still get 403, which is worse than no button.
   * So for them only say "who to find", without an entry that is bound not to work (same as
   * the rule in AgentRuntimeSections, "a button that can never be clicked is more confusing than no button").
   */
  "chat.noArkTitle": "这个空间还没有配置方舟 API Key",
  "chat.noArkHint": "配好之后才能开始对话。整个空间共用一把，配一次就行。",
  "chat.noArkAction": "去空间设置",
  // The referenced global key no longer exists: **it must not be called "not configured yet"**, which would send users to workspace settings
  // to paste a new one, while what they most likely want is to reconfigure the global one (fix it in one place, and every workspace referencing it recovers).
  "chat.noArkInheritTitle": "本空间引用的全局方舟 API Key 已不存在",
  "chat.noArkInheritHint":
    "请到全局设置重新配置，或在空间设置里改用本空间自定义的 key。",
  "chat.noArkInheritAction": "去全局设置",
  // The line for no permission: say "why not" + "who to find", without a button.
  "chat.noArkNeedAdmin": "配置方舟 API Key 需要空间管理员权限，请联系空间的管理员或拥有者。",
  // Send queue: when hitting "the previous tool is still running" (409 conflict), queue and resend automatically; see lib/useSendQueue.ts.
  //
  // The wording deliberately **does not contain the words "failed" or "error"**: that window is only tens of milliseconds, and one resend is enough;
  // calling it a failure would make users think something went wrong and retry manually (while the queue is already doing it for them).
  // The wording is written for **the most common cause**: most queueing is "the agent is still answering this turn",
  // not "hit exactly the tens-of-milliseconds window of tool execution". Saying "a tool is running" is wrong when the agent
  // answers with plain text, and users would go looking for a tool that does not exist.
  "chat.queueHint": "agent 还在回答，{count} 条消息排队中，这一轮结束后会自动发出。",
  "chat.queuePosition": "第 {index}/{total} 条",
  // "Modify" rather than "edit": what happens here is taking the original text back into the input box to rework it, not editing in place.
  "chat.queueEdit": "修改",
  // "Send now" = interrupt what the agent is doing and do this one instead (redirect).
  //
  // **Not called "cut in line"**: cutting in line sounds like just moving to the front while the previous turn runs to completion, while this action really
  // interrupts the current turn. A name that undersells it gives users an unexpected consequence when they click.
  "chat.queueSendNow": "立即发送",
  // The button's supplementary note (tooltip). The words "send now" cannot convey the cost,
  // and the cost (interrupting the current turn) is exactly what users need to know before clicking.
  "chat.queueSendNowHint": "打断 agent 当前这一轮，改为立刻处理这条消息",
  // Redirect failed. Separate from "send failed": users can see which step did not work.
  "chat.redirectFailed": "立即发送失败",
  // Queue full: **a clear hint is required**; silently dropping is the worst way this feature can fail (users think it was sent).
  // The original text is put back into the input box at the same time, so this sentence only needs to say "not sent" and "what to do".
  "chat.queueFull": "已有 {limit} 条消息在排队，这条没有发出。文案已放回输入框，可以先取消几条再发。",
  // Still not sent after queueing until timeout. **Not a failure**, so it does not take the red "send failed" text,
  // but the same tone as queue full: state the situation + the original text was not lost + the next step.
  "chat.queueTimeout": "等了 3 分钟这一轮还没结束，这条消息没有发出。文案已放回输入框，可以稍后再发。",

  // Run history
  "run.listDescAll": "空间内所有 Agent 的运行记录与事件流",
  "run.listDescAgent": "仅显示「{name}」的运行",
  "run.showAllAgents": "显示全部 Agent",
  "run.searchLabel": "搜索运行",
  "run.searchPlaceholder": "搜索标题、Agent 或 ID",
  "run.filterByAgent": "按 Agent 筛选",
  "run.allAgents": "全部 Agent",
  "run.emptyNoMatch": "没有匹配的运行",
  "run.emptyNoMatchDesc": "调整筛选条件再看看。",
  "run.emptyTitle": "还没有运行记录",
  "run.emptyDesc": "Agent 被飞书消息、API 或手动触发后，这里会出现运行记录与完整事件流。",
  "run.colDuration": "耗时",
  "run.colTokens": "Token",
  "run.colEvents": "事件",
  "run.colStarted": "开始",
  "run.threadCount": "{count} 线程",
  "run.statusFailed": "失败",
  "run.loadingDetail": "正在加载运行详情",
  "run.notFound": "运行不存在",
  "run.notFoundDesc": "这条运行记录可能已被删除。",
  "run.backToList": "返回运行历史",
  "run.viewAgent": "查看 Agent",
  "run.statTokenTotal": "Token 总量",
  "run.statTokenHint": "入 {input} · 出 {output}",
  "run.statCacheHit": "缓存命中",
  "run.statCacheHint": "写入 {value}",
  "run.statModelReq": "模型请求",
  "run.statToolCalls": "工具调用",
  "run.statToolErrors": "{count} 次失败",
  "run.statErrors": "错误",
  "run.statOutcome": "产出评估",
  "run.statOutcomeRounds": "{count} 轮",
  "run.statEvents": "事件数",
  "run.statRawEvents": "原始 {count} 条",
  // Step count after pairing (a tool call's start and its receipt combined into one step). The event view does not merge, so this is only a hint.
  "run.statMergedSteps": "合并后 {count} 步",
  "run.startedAt": "开始时间",
  "run.endedAt": "结束时间",
  "run.inProgress": "进行中",
  "run.sessionId": "Session ID",
  "run.threads": "线程",
  "run.eventStream": "事件流",
  "run.eventShown": "显示 {shown} / {total}",
  "run.recentRuns": "最近 {count} 次运行",
  "run.viewInHistory": "在运行历史中查看",
  "run.agentNoRuns": "该 Agent 还没有运行记录",
  "run.agentNoRunsDesc": "被飞书消息、API 或手动触发后，运行记录会出现在这里。",
  "run.outcomeSatisfied": "达标",

  // Usage and insights
  "usage.title": "用量与洞察",
  "usage.subtitle": "谁在消耗、谁在变慢、谁在失败",
  "usage.emptyTitle": "还没有用量数据",
  "usage.emptyDesc": "Agent 跑过之后，这里会出现调用频次、耗时分布与 Token 消耗趋势。",
  "usage.statRuns": "运行次数",
  "usage.statRunsHint": "{count} 次已结束",
  "usage.statSuccessRate": "成功率",
  "usage.statSuccessHint": "{failed} 次失败",
  "usage.statTokens": "Token 总量",
  "usage.statTokensHint": "入 {input} · 出 {output}",
  "usage.statAvgDuration": "平均耗时",
  "usage.statAvgHint": "P50 {p50} · P90 {p90}",
  "usage.chartCallsTitle": "各 Agent 调用频次",
  "usage.chartCallsDesc": "按运行次数排序",
  "usage.chartLatencyTitle": "执行耗时分布",
  "usage.chartLatencyDesc": "只统计已结束的运行 · 纵轴为对数刻度",
  "usage.chartTokensTitle": "Token 消耗趋势",
  "usage.chartTokensDesc": "堆叠高度即为总量",
  "usage.chartOutcomeTitle": "运行成功 / 失败",
  "usage.chartOutcomeDesc": "失败以运行是否带错误摘要为准",
  "usage.legendInput": "输入",
  "usage.legendOutput": "输出",
  "usage.legendTotal": "总量",
  "usage.legendP50": "P50",
  "usage.legendP90": "P90",
  "usage.legendAvg": "平均",
  "usage.succeeded": "成功",
  "usage.failed": "失败",
  "usage.runsUnit": "{count} 次",
  "usage.noLatencyData": "暂无已结束的运行，无法统计耗时",
  "usage.sampleSize": "样本 {count} 次",
  "usage.tableAgent": "Agent",
  "usage.tableRuns": "调用",
  "usage.tableFailed": "失败",
  "usage.tableTokens": "Token",

  // Timeline
  "timeline.rawPayload": "原始 payload",
  "timeline.expand": "展开",
  "timeline.collapse": "收起",
  "timeline.waitingResult": "等待结果返回",
  "timeline.execFailed": "执行失败",
  "timeline.execResult": "执行结果",
  "timeline.emptyResult": "（空结果）",
  "timeline.noEvents": "当前筛选条件下没有事件",
  // Two states that **must be kept apart**. Mixing them into one was exactly the meaning bug fixed this time:
  //   needsConfirm  —— really waiting for the user to click (evaluated_permission=ask with no confirmation)
  //   executing     —— the tool is running in our own container (the requires_action idle on the Ark side)
  "timeline.needsConfirm": "待你确认",
  "timeline.awaitingYou": "需要你批准后才会执行",
  "timeline.executing": "执行中",
  "timeline.executingHint":
    "工具在 AMA 自己的容器里执行。方舟此刻是 idle（stop_reason=requires_action），那是「已交还控制权、等结果回传」，不是在等你操作",
  "timeline.approved": "已批准",
  "timeline.denied": "已拒绝",
  "timeline.denyReason": "拒绝理由：{msg}",
  "timeline.pending": "待处理",
  "timeline.pendingHint": "事件已入队，等前序事件处理完后才会被处理",
  "timeline.threadLabel": "线程",

  // Timeline: container execution details. exit_code / stdout / stderr are not structured backend fields;
  // they are parsed from the result text (see lib/toolResult.ts).
  "timeline.command": "命令",
  "timeline.input": "入参",
  "timeline.exitCode": "退出码 {code}",
  "timeline.exitCodeHint": "非 0 不一定是失败：例如 grep 没匹配到内容就退出 1",
  "timeline.notExited": "未退出",
  "timeline.stdout": "标准输出",
  "timeline.stderr": "标准错误",
  "timeline.sandboxError": "容器错误",
  "timeline.sandboxErrorHint": "命令没能送进容器执行，与命令自身报错是两回事",
  "timeline.durationHint": "从发起调用到收到回执的时长，含排队与容器恢复，不等于命令执行时间",

  // The three entries for "open in the Ark console". **The text must say what is being opened**;
  // do not just write "open in the Ark console": with the same sentence in all three places, users on the session page would
  // think clicking shows the agent.
  //
  // ⚠️ "No such entry" is **a normal state** (Ark resources are created lazily), and without explanation in the UI
  // users would think the feature is broken. The explaining line is agent.arkConsoleAbsent; see AgentDetailPage.
  "arkConsole.openAgent": "在方舟控制台查看这个 Agent",
  "arkConsole.openSkill": "在方舟控制台查看这个技能",
  "arkConsole.openSession": "在方舟控制台查看这次会话",
  // ⚠️ This line must make clear **this is not a failure**, and also how to make it appear (go chat once).
  // Writing only "none yet" makes people think it is broken or waiting for something.
  "arkConsole.agentNotCreated":
    "这个 Agent 还没在方舟侧创建——第一次开始对话时才会创建，之后这里会出现「在方舟控制台查看」的入口。",
  // The skill one is the same reasoning as the agent one, but **the trigger is more common**: a skill is only sent to Ark after some
  // Agent bound to it really starts a conversation, so skills that are "created but not used by any Agent"
  // never have an entry. Measured locally, 9 of 16 skills were like this; without explanation it would be taken as a broken feature.
  "arkConsole.skillNotCreated":
    "这个技能还没上传到方舟——要先被某个 Agent 绑定、且那个 Agent 开过一次对话才会上传，之后这里会出现「在方舟控制台查看」的入口。",

  // Trace panel (observability). "Not connected / no data yet / has data" are three things, and the text must keep them apart:
  // calling "not connected" "no data yet" makes people wait forever for something that will never come.
  "trace.title": "调用树",
  "trace.durationHint":
    "span 树：一条运行是 root，模型请求与工具调用挂在它下面。点节点看详情。耗时是派生的墙钟时间，含排队与容器恢复。",
  "trace.noSpans": "这次运行还没有产生模型请求或工具调用。",
  "trace.spanCount": "节点",
  "trace.totalDuration": "合计耗时",
  "trace.durationUnknown": "未知",
  "trace.durationUnknownHint": "回执还没到，或事件已入队未处理——算不出来，不是 0",
  "trace.command": "命令行",
  // Levels of the call tree. root and turns are groupings derived on the frontend (Ark's event stream is flat);
  // see the file header of lib/traceTree.ts.
  // Node names. The official root span is called `agent.<name>`; here it gets a human-readable sentence.
  "trace.spanRoot": "这次运行",
  "trace.spanModel": "模型请求",
  "trace.sortBySequence": "按顺序",
  "trace.sortByDuration": "按耗时",
  // The field grid of the right-side details; fields and order follow the official useSpanOverviewField.
  "trace.fieldStatus": "状态",
  "trace.fieldStatusCode": "错误信息",
  "trace.fieldSpanId": "Span ID",
  "trace.fieldType": "类型",
  "trace.fieldLatency": "耗时",
  "trace.fieldStartTime": "开始时刻",
  "trace.fieldTokens": "Tokens",
  "trace.fieldInput": "输入",
  "trace.fieldOutput": "输出",
  "trace.statusSuccess": "成功",
  "trace.statusError": "失败",
  "trace.tokensBreakdown": "输入 {input} / 输出 {output}",
  // Timeline axis (Gantt bars). All rows share one axis, so concurrent calls overlap horizontally.
  "trace.concurrentHint": "与同层另外 {n} 个调用在时间上重叠（并发执行）",
  "trace.axisLabel": "时间轴",
  "trace.fieldEndTime": "结束时刻",
  "trace.fieldOffset": "相对起点",
  // Gantt bar tooltip: start → end, total N.
  "trace.barRange": "{start} → {end}（共 {dur}）",
  // The duration of turns and root is **the span from first to last**, not the sum of children: the two differ, and the difference
  // is exactly the queueing and container resume time. Summing would systematically underestimate "how long the user waited".
  "trace.spanHint": "跨度",
  "trace.collapse": "折叠",
  "trace.expand": "展开",
  "trace.hasError": "含失败",
  "timeline.slow": "慢",
  "timeline.truncated": "已截断",
  "timeline.truncatedHint": "输出超过 64 KiB，服务端已截断",
  "timeline.rawJson": "原始 JSON",
  "timeline.copy": "复制",
  "timeline.copied": "已复制",
  "timeline.copyFailed": "复制失败",
  // ---- Event view (the raw event list, see components/RawEventList.tsx) ----
  "evt.detailTitle": "事件详情",
  "evt.pickOne": "点左侧任一条事件看详情",
  "evt.eventId": "事件 ID",
  "evt.sequence": "序号",
  "evt.processedAt": "处理时刻",
  "evt.thread": "线程",
  "evt.body": "正文",
  "evt.emptyContent": "（无内容）",
  "evt.thinkingBody": "思考过程",
  "evt.thinkingEmpty": "（思考中，无正文）",
  "evt.thinkingEmptyHint": "这条思考事件没有正文。方舟允许只给信号不给内容，不是渲染出错。",
  "evt.interruptReason": "中断原因",
  "evt.interruptNoReason": "（未给出原因）",
  "evt.toolName": "工具名",
  "evt.toolUseId": "调用 ID",
  "evt.fallbackId": "（用事件自身 ID，方舟未下发 tool_use_id）",
  "evt.permission": "权限判定",
  "evt.mcpServer": "MCP 服务",
  "evt.denyMessage": "拒绝理由",
  "evt.agentName": "Agent",
  "evt.parentThread": "父线程",
  "evt.noParentThread": "（无，这是主线程）",
  "evt.toAgent": "发给",
  "evt.toThread": "目标线程",
  "evt.fromAgent": "来自",
  "evt.fromThread": "来源线程",
  "evt.mainAgent": "主 Agent",
  "evt.compactedSummary": "压缩摘要",
  "evt.compactedNoSummary": "（上下文已压缩，未给摘要）",
  "evt.sessionName": "会话名",
  "evt.stopReason": "停止原因",
  "evt.requiresActionHint":
    "self_hosted 下这不是「等用户确认」：方舟把控制权交还给 AMA，工具正在我们自己的容器里执行，等 worker 回传 user.tool_result。",
  "evt.pendingEventIds": "在等这些事件：{ids}",
  "evt.noExtraFields": "这类事件除公共信封外没有额外字段。",
  "evt.errorMessage": "错误信息",
  "evt.noErrorMessage": "（未给出错误信息）",
  "evt.retry_retrying": "正在重试",
  "evt.retry_retries_exhausted": "重试耗尽",
  "evt.retry_terminal": "不可重试",
  "evt.credentialId": "凭证 ID",
  "evt.modelStartHint": "模型请求的起点。耗时与用量在配对的 span.model_request_end 上。",
  "evt.startEventId": "起点事件 ID",
  "evt.duration": "耗时",
  "evt.usage": "token 用量",
  "evt.usageBrief": "入 {in} · 出 {out}",
  "evt.inputTokens": "输入",
  "evt.outputTokens": "输出",
  "evt.cacheRead": "缓存命中",
  "evt.cacheWrite": "缓存写入",
  "evt.speed": "档位",
  "evt.outcomeId": "产出 ID",
  "evt.iterationLabel": "迭代轮次",
  "evt.iteration": "第 {n} 轮",
  "evt.maxIterations": "最大轮次",
  "evt.description": "描述",
  "evt.rubric": "评分标准",
  "evt.explanation": "评估说明",
  "evt.unknownType": "前端还不认识这个事件类型，下面的原始 JSON 是完整的。",
  "evt.threadCreatedMain": "{name}（主线程）",
  "evt.threadCreatedSub": "{name}（子线程）",
  // ---- Human-readable names of the 34 event types. Keys are `evtType.<Ark type string>`, see lib/rawEvents.ts ----
  "evtType.user.message": "用户消息",
  "evtType.system.message": "系统提示词",
  "evtType.user.interrupt": "用户中断",
  "evtType.user.tool_confirmation": "用户确认工具",
  "evtType.user.tool_result": "工具结果回传",
  "evtType.user.custom_tool_result": "自定义工具结果",
  "evtType.user.define_outcome": "定义产出目标",
  "evtType.agent.message": "Agent 回复",
  "evtType.agent.thinking": "思考",
  "evtType.agent.tool_use": "工具调用",
  "evtType.agent.tool_result": "工具结果",
  "evtType.agent.mcp_tool_use": "MCP 工具调用",
  "evtType.agent.mcp_tool_result": "MCP 工具结果",
  "evtType.agent.custom_tool_use": "自定义工具调用",
  "evtType.agent.thread_message_sent": "发给线程",
  "evtType.agent.thread_message_received": "线程来信",
  "evtType.agent.thread_context_compacted": "上下文压缩",
  "evtType.session.status_running": "会话开始运行",
  "evtType.session.status_idle": "会话空闲",
  "evtType.session.status_rescheduled": "会话重新排队",
  "evtType.session.status_terminated": "会话结束",
  "evtType.session.error": "会话错误",
  "evtType.session.updated": "会话已更新",
  "evtType.session.deleted": "会话已删除",
  "evtType.session.thread_created": "线程创建",
  "evtType.session.thread_status_running": "线程开始运行",
  "evtType.session.thread_status_idle": "线程空闲",
  "evtType.session.thread_status_rescheduled": "线程重新排队",
  "evtType.session.thread_status_terminated": "线程结束",
  "evtType.span.model_request_start": "模型请求开始",
  "evtType.span.model_request_end": "模型请求结束",
  "evtType.span.outcome_evaluation_start": "产出评估开始",
  "evtType.span.outcome_evaluation_ongoing": "产出评估进行中",
  "evtType.span.outcome_evaluation_end": "产出评估结束",

  // Timeline filters
  "filter.searchLabel": "搜索事件",
  "filter.searchPlaceholder": "搜索参数、输出与原始 JSON",
  "filter.domain": "事件域",
  "filter.title": "筛选",
  "filter.category": "语义类别",
  "filter.derived": "派生条件",
  "filter.chipHidden": "已隐藏：{name}",
  "filter.removeCondition": "移除条件：{name}",
  "filter.preset": "快速筛选",
  "filter.type": "事件类型",
  "filter.typeLabel": "按具体事件类型筛选",
  "filter.typeAll": "全部类型",
  "filter.typeSelected": "已选 {count} 类",
  "filter.slowOnly": "只看慢调用（>{seconds}s）",
  "filter.slowOnlyHint":
    "耗时按「发起调用到收到回执」算，含排队与容器从暂停恢复（首次唤醒 16~29 秒），不等于命令执行时间",
  "filter.errorsOnly": "只看失败",
  "filter.reset": "重置筛选",
  "filter.presetTool": "工具",
  "filter.presetModel": "模型",
  "filter.presetError": "错误",
  "filter.presetSystem": "系统",
  "filter.presetSystemHint":
    "会话与线程的状态迁移。环境准备与容器生命周期不产生事件，只在服务端日志里",
  "filter.noMatch": "没有匹配的事件",
  "filter.noMatchDesc": "调整筛选条件或清空关键词后再看",

  // View switching and turn folding
  "view.conversation": "对话",
  "view.events": "事件",
  // The third option. **Not called "Trace"**: the panel title is already "call tree", and a switch named differently from its destination
  // makes people think they clicked the wrong thing (same rule as nav.globalSettings and prefs.title).
  "view.trace": "调用树",
  // The fourth and fifth options. **Split into two options instead of one "container" option**: files are "glance at what is there",
  // the terminal is "go in and work" and needs the full height; squeezed together the terminal would only ever get the lower half of the screen.
  //
  // Not called "sandbox": that is the platform-side name (Docker container); what users see is where the agent works.
  "view.files": "文件",
  "view.terminal": "终端",
  "view.switchLabel": "呈现方式",

  // -- Session container (Docker) -----------------------------------------------
  //
  // ⚠️ The four states each have their own wording, and **two pairs must never be merged**:
  //   none called gone → users think the files were lost, while there never were any files;
  //   paused called gone → users start over for nothing, while the workspace is actually still there.
  "sandbox.title": "容器",
  "sandbox.none.title": "这次对话还没用到容器",
  "sandbox.none.desc": "只有 agent 真的调用工具（执行命令、读写文件）时才会创建容器。",
  "sandbox.running": "运行中",
  "sandbox.paused": "已暂停",
  "sandbox.gone": "已回收",
  "sandbox.paused.title": "容器已暂停",
  // "The workspace is still there" is the point of this text and cannot be dropped: it is exactly the difference between paused and gone.
  "sandbox.paused.desc": "一轮对话结束后空闲满 60 秒会自动暂停或停止，交还计算资源。工作区仍然保留，恢复后接着用。",
  "sandbox.gone.title": "容器已被回收",
  "sandbox.gone.desc": "容器与工作区闲置太久，已被回收，里面的文件已经不在了。下次调用工具会新建一个。",
  "sandbox.resuming": "正在恢复…",
  "sandbox.autoResuming": "容器空闲后会自动暂停以交还计算资源。正在为你恢复，通常几秒内完成，工作区不受影响。",
  "sandbox.sessionId": "容器 ID",
  "sandbox.files": "文件",
  "sandbox.filesEmpty": "这个目录是空的",
  "sandbox.filesTruncated": "文件太多，只显示前 {count} 个",
  "sandbox.parentDir": "上一级",
  "sandbox.fileClose": "关闭预览",
  "sandbox.fileBinary": "二进制文件，不支持预览。",
  "sandbox.fileTruncated": "文件太大，只显示前 256 KB。",
  "sandbox.fileLoadFailed": "读取文件失败",
  "sandbox.readOnly": "只读浏览",
  "sandbox.loadFailed": "读取容器状态失败",
  "sandbox.terminal": "终端",
  "sandbox.openTerminal": "打开终端",
  "sandbox.terminalClosed": "终端已关闭",
  "sandbox.terminalConnecting": "正在连接…",
  "sandbox.terminalConnected": "已连接",
  "sandbox.terminalDisconnected": "连接已断开",
  // The line when the container has been reclaimed. **Separate from "connection lost"**: after a session ends the container is released,
  // and failing to connect then is the normal ending; calling it "disconnected" would make people think the platform is broken and report it.
  "sandbox.terminalRecycled": "这次会话的容器已回收，无法进入。会话结束后容器会被释放。",
  "sandbox.terminalNeedsRunning": "容器运行中才能打开终端。",
  // The button when disconnected. **Only shown under "connection lost"**: when the container has been reclaimed reconnecting is bound to
  // fail again, and a button that does nothing is worse than none.
  "sandbox.terminalReconnect": "重新连接",
  // The line written into the terminal after automatic reconnect succeeds.
  //
  // ⚠️ **It must say "this is a new shell", not just "reconnected".** After reconnecting it is a new PTY:
  // shell history, the current directory, and running foreground processes are all gone. Users thinking they are still in the old shell is worse:
  // they might go on typing a command that depends on earlier context (like `rm -rf ./x` after a `cd`).
  //
  // This is also the honest limit of "seamless reconnect": it can achieve "no manual refresh", not "context continues".
  "sandbox.terminalReconnected": "连接已重建（这是一个新的 shell，之前的历史与当前目录已重置）",
  // Which transport is currently used. **Only shown when on the fallback**: WS is the expected state, not worth a line of text.
  //
  // Why show it: the two paths feel noticeably different (the fallback one reorders on fast typing, one RTT per character).
  // When users complain "the terminal is laggy", the first thing to answer is "which one are you on now",
  // and without showing it the only way is digging through the network panel.
  "sandbox.terminalViaFallback": "兼容模式",
  "sandbox.terminalViaFallbackHint":
    "WebSocket 通道不可用，已回退到兼容模式。终端仍然可用，但连续快速输入时回显可能变慢或错序。",
  // The two entries in the top right of the terminal.
  //
  // ⚠️ "Fullscreen" means **filling the browser viewport** (CSS), not the browser's own fullscreen (the F11 kind).
  // The text deliberately does not say "enter fullscreen": that would make people think Esc exits, while Esc in the terminal must pass through to
  // the shell (vim uses it to leave insert mode), so the only way out is that button. See SandboxTerminal.
  "sandbox.terminalFullscreen": "铺满窗口",
  "sandbox.terminalExitFullscreen": "退出铺满",
  "sandbox.terminalOpenInNewTab": "在新标签页打开终端",
  "turn.steps": "{count} 个步骤",
  "turn.toolCalls": "{count} 次工具调用",
  "turn.toolErrors": "{count} 个失败",
  "turn.running": "正在执行",
  "turn.earlierSteps": "更早的 {count} 步",
  "turn.noProcess": "没有中间过程",

  // Workspace settings
  "settings.title": "空间设置",
  "settings.desc": "名称、Key 与成员管理",
  "settings.tabGeneral": "基本信息",
  "settings.tabMembers": "成员",
  "settings.tabDanger": "危险操作",
  "settings.currentUrl": "当前地址为 /w/{key}",
  "settings.keyChangeWarn":
    "修改 Key 会让所有指向 /w/{old} 的旧链接失效，保存后将跳转到 /w/{next}。",
  "settings.membersDesc": "{count} 人可以访问这个空间",
  "settings.invite": "邀请成员",
  "settings.inviteTitle": "邀请成员",
  "settings.inviteAction": "邀请",
  "settings.memberName": "姓名",
  "settings.memberNameRequired": "请填写姓名",
  "settings.memberEmail": "邮箱",
  "settings.memberEmailRequired": "请填写邮箱",
  "settings.memberEmailInvalid": "邮箱格式不正确",
  "settings.memberRole": "角色",
  "settings.roleOf": "{name} 的角色",
  "settings.joinedAt": "{time}加入",
  "settings.removeMember": "移除 {name}",  "settings.removeMemberTitle": "移除「{name}」",
  "settings.removeMemberDesc": "移除后该成员将无法访问这个空间。",
  "settings.cannotRemoveOwner": "不能移除唯一的拥有者",
  "settings.deleteWorkspace": "删除空间",
  "settings.deleteWorkspaceDesc": "空间内的 Agent、技能、飞书应用与运行历史都会被一并删除",
  "settings.deleteWorkspaceWarn": "此操作不可撤销，删除后数据无法恢复。",
  "settings.deleteWorkspaceTitle": "删除空间「{name}」",
  "settings.deleteWorkspaceConfirm":
    "将永久删除该空间及其全部 Agent、技能、飞书应用和运行历史。此操作不可撤销。",
  "settings.saveFailed": "保存失败",
  "settings.tabHosts": "宿主机",
  "settings.tabArk": "方舟凭证",
  "settings.wsName": "空间名称",
  "settings.wsNameRequired": "空间名称不能为空",
  "settings.wsKey": "空间 Key",
  "settings.wsDesc": "描述",
  "settings.keyTaken": "Key「{key}」已被占用",
  "settings.membersSectionDesc": "按登录邮箱邀请成员，并为每个人选择角色",
  "settings.membersEmpty": "还没有成员",
  "settings.memberEmailNotValid": "请填写有效的邮箱地址",
  "settings.memberEmailHint": "对方登录 AMA 时使用的邮箱",
  "settings.memberDisplayName": "名字",
  "settings.memberNamePlaceholder": "可选，默认取邮箱 @ 前面那段",
  "settings.removeMemberTooltip": "移除成员",
  "settings.arkLoadFailed": "读取方舟凭证失败，请稍后重试",
  "settings.arkUseGlobalFailed": "引用全局配置失败",
  "settings.arkApiKey": "方舟 API Key",
  "settings.arkApiKeyDesc":
    "本空间的 Agent 与会话都用这把 key 调用方舟。它是空间级的：这个空间的所有成员共用同一把，换掉会影响所有人",
  "settings.arkUseGlobal": "引用全局配置",
  "settings.arkSwitchToCustom": "改用本空间自定义",
  "settings.arkReplace": "更换",
  // The inherit bar is one sentence split around a <strong>: prefix + source + suffix + (from) + hint
  "settings.arkInheritPrefix": "当前使用",
  "settings.arkInheritSource": "来自全局设置",
  "settings.arkInheritSuffix": "的 API Key",
  "settings.arkInheritFrom": "（{name}）",
  "settings.arkInheritHint": "。在全局设置里换掉它，本空间会**立即**跟着变。",
  "settings.arkGoToGlobal": "去全局设置",
  // The dangling warning is one sentence split around a link: prefix + link + suffix
  "settings.arkDanglingPrefix": "本空间引用的全局 API Key 已不存在。请到",
  "settings.arkDanglingLink": "全局设置",
  "settings.arkDanglingSuffix": "重新配置，或在下方填写本空间自己的 key。",
  "settings.arkStatus": "状态",
  "settings.arkCheckedAt": "{time}验证",
  "settings.arkRegion": "区域",
  "settings.arkRegionInherited": "区域跟随全局设置，请到那里修改",
  "settings.arkUpdatedAt": "{time}更新",
  "settings.arkClear": "清除",
  "settings.arkVerify": "验证连通性",
  "settings.arkEmptyTitle": "还没有配置方舟 API Key",
  "settings.arkEmptyDesc":
    "配置后本空间的 Agent 才能调用方舟。可以引用你在全局设置里配好的那一份，也可以为这个空间单独填一把。",
  "settings.arkSetup": "填写 API Key",
  "settings.arkReplaceTitle": "更换 API Key",
  "settings.arkSetupTitle": "填写方舟 API Key",
  "settings.arkModalDesc": "Key 保存后只回显掩码，不会再明文展示",
  "settings.arkKeyPlaceholder": "从方舟控制台复制",
  "settings.arkClearTitle": "清除方舟 API Key",
  "settings.arkClearDesc": "清除后本空间的 Agent 将无法调用方舟，直到重新配置。此操作不可撤销。",

  // Authorization management (multiple authorization providers, card list driven by the backend)
  //
  // ⚠️ The card **title, subtitle, status summary, and fix guidance all come from the backend**, not here.
  // Reason: that text is tightly bound to "which platform the backend is connected to, and what the platform answered right now", and putting it in the frontend
  // would drift from the real state (the backend switches platforms while the frontend still names the old one). Only the page skeleton text stays here.
  "integrations.title": "授权管理",
  "integrations.desc": "把外部服务的访问凭证托管给平台，AMA 代表你去调用",
  // "This is account-level" still has to be written on the page, but **the reason changed**.
  //
  // It used to live in "workspace settings", where the location itself lied (the other tabs on that page are all workspace-level),
  // and these three lines corrected that wrong expectation. Now it moved to "global settings", where the location already states the scope correctly,
  // so the first two lines are downgraded to "confirmation", and **the third one is what truly cannot be dropped**:
  //
  //   scopeTitle  ownership                  → implied by the location, kept as confirmation
  //   scopeBody   what happens on switching  → implied by the location, kept as confirmation
  //   scopePeers  can others see it          → ⚠️ **the location answers not a word of it**
  //
  // "In global settings" does not imply "colleagues cannot see my authorization": that is a permission question, not a scope question,
  // and it is exactly what users worry about most. Deleting this Notice would leave the third question unanswered again.
  "integrations.scopeTitle": "这些授权属于你的账号，所有空间通用",
  "integrations.scopeBody": "在任何空间里，你的 agent 都用这一份授权去调用外部服务，换空间不用重新授权。",
  "integrations.scopePeers": "其他人看不到你的授权状态，你也看不到他们的。",
  // ⚠️ **The wording is "状态更新于 {time}" (status updated {time}), not "{time}检查" ({time} check).**
  //
  // The latter renders as **"现在检查" (check now)** at the most common moment: `formatRelative` returns
  // "现在" (now) for a time just checked. And this line sits in the same bottom row of the card as the "check again" button, so "现在检查"
  // reads as a **command** (like something clickable), while it actually states "this status was just obtained".
  //
  // Do not write it as "于 {time} 检查" (checked at {time}) either: `formatRelative` returns phrases like "现在" (now) and "3 分钟前" (3 minutes ago)
  // that **carry their own word order**, and sandwiching one in "于…检查" gives "于 现在 检查".
  // "状态更新于 X" puts the variable at the end, and reads well for both "现在" and "3 分钟前".
  //
  // This status is not cached (checked live on entering the page), so this line answers "when is this green light I see
  // from": in a tab left open for a long time, that green light may no longer hold.
  "integrations.checkedAt": "状态更新于{time}",
  "integrations.authorize": "发起授权",
  "integrations.reauthorize": "重新授权",
  "integrations.revoke": "解除授权",
  "integrations.refresh": "重新检查",
  "integrations.openConsole": "打开控制台",
  "integrations.openedTitle": "已在新标签页打开授权页",
  "integrations.openedHint": "完成后回到本页点「重新检查」",
  "integrations.authorizeFailed": "发起授权失败",
  "integrations.pollingHint": "在飞书里点「同意」之后，这里会自动变成已授权，不用手动刷新",
  "integrations.authorizeTimeout":
    "等待授权超时了。如果你还没在飞书里确认，请重新点「发起授权」再试一次。",
  "integrations.revokeFailed": "无法解除授权",
  "integrations.loadFailed": "读取授权状态失败，请稍后重试",
  // Switch Feishu app. The wording deliberately **does not say "the agent will use this"**: injection picks again by the workspace of the run,
  // ignoring this preference (see the comment above GetLarkUserAppPref in server/core/store.go).
  // Saying "the agent uses this" would make users think switching changes the agent's behavior, which does not hold.
  "integrations.appLabel": "使用的应用",
  // ⚠️⚠️ This guidance answers the user's own words: "global settings has no way to switch to another Feishu bot".
  //
  // It is **not a missing feature; two concepts were not distinguished on this page**:
  //   this page (authorization management)  account-level: I authorize AMA to use **my Feishu identity** (getting a user token)
  //   "Feishu" in the sidebar               workspace-level: register a **bot app** for the agent to send and receive messages
  // Users came here looking for "switch bot" and could not find it, because it is not here at all; and this page used to say nothing about it.
  //
  // So even with only one app (the picker does not appear) it must be shown: **that is exactly when it is most needed**.
  "integrations.appManageHint":
    "要增删飞书应用（bot）本身，去空间侧栏的",
  // The degraded text when there is no source workspace: do not make up a link that may 404.
  "integrations.appManageHintNoLink":
    "要增删飞书应用（bot）本身，去任一空间侧栏的「飞书」——它是空间级资源，每个空间各自接入。",
  "integrations.appSwitchFailed": "切换应用失败",
  "integrations.appAuthorized": "已授权",
  "integrations.appUnauthorized": "未授权",
  "integrations.appLoadFailed": "读取可选应用失败",
  // "Not a single provider registered" and "some source not connected" are different things: the latter is shown by the card itself as
  // "not connected", and only the former takes this empty state.
  "integrations.noneTitle": "还没有可用的授权源",
  "integrations.noneDesc": "服务端还没接入任何授权源。接入后这里会自动出现对应的卡片。",
  "integrations.whyTitle": "为什么要这一步",
  "integrations.whyBody":
    "容器工具按调用者身份判权限。用服务自己的应用账号需要逐个资源赋权；用你的授权则以你的身份执行，你有权限的资源就能用。",

  // ---------------------------------------------------------------------
  // Top summary + card folding (the information architecture after there are more authorization sources)
  // ---------------------------------------------------------------------
  //
  // Why: with all 7 authorization sources expanded the page was about 3000px, and you had to scroll the whole page to know "which ones need me to act".
  //
  // ⚠️ **The summary has three levels, not two, because among the four states `missing` is a different kind from error/warning.**
  //
  //   ready           ok                nothing to do for these
  //   needs action    error + warning   **you yourself** can move it forward with a click
  //   not connected   missing           an **admin** must configure it; users clicking authorize a hundred times does nothing
  //
  // Merging missing into "needs action" is the easiest mistake here: it would make users do something they cannot do,
  // and then find nothing clickable in the card (that card deliberately has no "start authorization" button).
  // So it is its own level, and the text points out "needs an admin to configure"; that is the only meaning of the levels in the UI.
  //
  // ⚠️ These lines go into Notice / plain text nodes, **do not write `**bold**`**: the asterisks would be shown as-is.
  "integrations.summaryReady": "已就绪 {count}",
  "integrations.summaryTodo": "要处理 {count}",
  "integrations.summaryMissing": "未接入 {count}",
  // The line when "needs action" is 0. **It must not just say "all ready"**: those missing ones are not ready,
  // they just should not be handled by this user.
  "integrations.summaryAllDone": "没有需要你处理的授权",
  // ⚠️ The wording is "the first one", not "these few": by default only **one** card is expanded (see
  // initialExpandedIds of IntegrationsTab: "expand all todos" for a new user equals
  // expanding all, which measured even taller than before the change). Saying "these few" would send users looking for expanded cards that do not exist.
  "integrations.summaryTodoHint": "已替你展开第一个，其余点标题展开",
  // Not connected gets its own line: the first reaction of users seeing this number is "what do I need to do", and the answer is "nothing for you".
  "integrations.summaryMissingHint": "未接入的要管理员去配，你点授权按钮不会生效",
  "integrations.expandAll": "全部展开",
  "integrations.collapseAll": "全部收起",
  // -- Titles and descriptions of the two groups ---------------------------------------------------
  //
  // ⚠️ The two description lines are **not decoration**; they are where the value of the grouping lies: with only two titles, users still
  // do not know "why it is in this group". The handling of the two groups really differs: personal authorization cannot be bound to a workspace,
  // and the page used to say not a word about it; users only found out when "this source cannot be found in workspace settings",
  // and then the error said "cannot be shared", not pointing to "it is a personal authorization".
  //
  // ⚠️ The wording matches the definitions of the two levels of backend domain.IntegrationKind (on behalf of a real person / on behalf of an app);
  // do not invent another way of saying it.
  "integrations.groupPersonalTitle": "个人授权",
  "integrations.groupPersonalDesc": "以你本人的身份访问平台。只有你自己的 Agent 用得上，不能添加到空间给其他人共用。",
  "integrations.groupServiceTitle": "服务账号",
  "integrations.groupServiceDesc": "以一个应用的身份访问平台，与具体是谁无关。管理员可以把它添加到空间，供空间里的 Agent 共用。",
  // ⚠️ This **deliberately has no** aria-label like "expand or collapse a card".
  // The whole row of the fold trigger is visible text (card name + status + a one-line summary), and the accessible name is provided by them
  // themselves; adding an aria-label would **override** that content, so screen reader users would only hear
  // "expand or collapse Feishu user authorization", and the status and summary sighted people can read would all be lost.

  // Labels of the four states. **All four must be kept apart**, especially "not connected" and "unavailable":
  // the former needs an admin to configure it (users clicking the authorize button does nothing), the latter needs the user or an admin to act.
  // Merged, the page could only give a one-size-fits-all empty sentence.
  "enum.integrationStatus.ok": "已就绪",
  "enum.integrationStatus.warning": "需注意",
  "enum.integrationStatus.error": "不可用",
  "enum.integrationStatus.missing": "未接入",

  // ---------------------------------------------------------------------
  // Pick an existing Ark API Key from the Volcano account (ArkFetchPicker)
  // ---------------------------------------------------------------------
  //
  // The text of this part has two **functional** jobs, not decoration:
  //
  // 1. **Make clear that accounts and keys are two levels**. A user has 10 accounts under their name, and an account has 10 keys;
  //    the two levels used to be laid out flat with the same visual weight, so users saw 20 identical-looking items and could not tell which were accounts
  //    and which were keys, nor see the order "pick an account first, then there are keys".
  // 2. **Make clear the plaintext does not pass through the browser**. This is the security precondition of this path (the server fetches on the spot, computes the mask, encrypts
  //    and stores it), and users will ask "how do you get my key", so the UI must answer it first.
  "arkFetch.title": "从火山账号获取",
  "arkFetch.recommended": "推荐",
  "arkFetch.intro": "列出你名下火山账号里已有的方舟 API Key，选一把即可，不用去控制台复制。",
  // **This line cannot be dropped**: it is the only place where users can learn "the plaintext never went through my browser".
  "arkFetch.plaintextNote": "明文不经过浏览器",
  "arkFetch.plaintextBody": "由服务端直接取走并加密保存。",

  // First level: pick an account. It is a **precondition**: without an account picked there are no keys to list.
  // Source dropdown. Default "pick from the Volcano account"; the input box appears only when "fill in manually" is chosen.
  //
  // ⚠️ When unavailable the dropdown is locked on "fill in manually" with sourceLockedHint; leaving it on an unselectable
  // "pick from Volcano" would tell users "you are using automatic fetching" while it is not.
  "arkFetch.sourceLabel": "Key 来源",
  "arkFetch.sourceFetch": "从火山账号选一把已有的",
  "arkFetch.sourceManual": "手动填写",
  "arkFetch.sourceLockedHint": "自动获取当前不可用，只能手动填写",
  // ⚠️ This is reported when the source is "pick from Volcano" but no key was picked, and **must not report "API Key cannot be empty"**:
  // that input box is not on screen at this moment, and users would not know where to fill it in.
  "arkFetch.pickRequired": "请先从上面选一把 API Key",
  "arkFetch.keyToggle": "展开 API Key 列表",
  "arkFetch.keyHint": "名称相近时看创建时间",
  "arkFetch.step1": "第 1 步",
  "arkFetch.step2": "第 2 步",
  "arkFetch.accountLabel": "火山账号",
  "arkFetch.accountPlaceholder": "搜索账号名或 ID",
  "arkFetch.accountToggle": "展开账号列表",
  "arkFetch.accountCount": "共 {count} 个账号",
  "arkFetch.accountNoMatch": "没有匹配的火山账号",
  "arkFetch.accountEmpty": "没有找到你名下已授权的火山账号。",
  "arkFetch.accountLoading": "正在读取火山账号…",

  // Second level: pick a key. **This part does not appear at all before an account is picked**; that is the most direct expression of the two-level relationship.
  "arkFetch.keyLabel": "API Key",
  "arkFetch.keyWaiting": "先选一个火山账号，这里会列出它名下的 API Key",
  "arkFetch.keyLoading": "正在读取 API Key 列表…",
  "arkFetch.keySearchPlaceholder": "按名称或创建时间搜索",
  "arkFetch.keySearchLabel": "搜索 API Key",
  // 6 of 20 keys are named api-key-2026082..., **only the time can tell them apart**, so this line reminds of that.
  "arkFetch.keyCount": "共 {count} 把，名称相近时看创建时间",
  "arkFetch.keyFiltered": "{shown}/{total} 把",
  "arkFetch.keyNoMatch": "没有匹配的 API Key，换个关键词试试",
  "arkFetch.keyEmptyTitle": "这个账号下还没有方舟 API Key",
  "arkFetch.keyEmptyDesc": "去方舟控制台创建一把，或换一个火山账号。",
  "arkFetch.keyCreatedAt": "创建于 {time}",
  "arkFetch.keyCreatedUnknown": "创建时间未知",
  "arkFetch.keyLastUsed": "{time}用过",
  "arkFetch.keyProject": "项目 {name}",
  // Creator. **This is the only answer to "which one is mine"**: a dozen keys under the same Volcano account
  // belong to a dozen people (measured: 10 keys from 10 different people), and neither the name nor the mask shows ownership.
  //
  // ⚠️ Your own key shows "me" rather than your username: recognizing your own name takes an extra thought,
  // and the point of this column is to let them spot it at a glance.
  "arkFetch.keyCreatedBy": "创建人 {name}",
  "arkFetch.keyCreatedByMe": "我创建的",
  "arkFetch.keyRetry": "重试",

  // After picking: the manual input gives way, and a way back is offered.
  "arkFetch.pickedTitle": "已选定这一把",
  "arkFetch.pickedHint": "下面的手填框不用管。",
  "arkFetch.useManual": "改为手动填写",

  // ⚠️ The two of the five states most easily written wrong must each have different text (see authorize/unavailable below).
  //
  // "Not connected ≠ not authorized": not connected needs an admin to configure the server, and users clicking "go authorize" **does nothing**
  // (it only gets a 422, and they cannot know the server needs configuring). So the not-connected branch
  // **gives no authorize button**, only an entry to the console. This pitfall was hit once on the authorization page.
  "arkFetch.authorizeTitle": "想直接从火山账号选一把已有的 key？",
  "arkFetch.authorizeBody": "先授权 AMA 代表你访问火山账号，之后就能在这里列出你的 key。",
  "arkFetch.authorizeAction": "去授权",
  "arkFetch.unavailableTitle": "自动获取暂不可用",
  // Fallback reason. If the server gives a reason, use its sentence (more specific); this one only appears when none is given.
  "arkFetch.unavailableBody": "服务端还没有接入火山账号，或暂时连不上。手动填写不受影响。",
  "arkFetch.unavailableHint": "需要管理员在服务端配置后才能使用，你现在可以手动填写。",
  "arkFetch.openConsole": "打开方舟控制台",
  "arkFetch.loadFailed": "读取失败",

  // ⚠️ "Replacing it affects others" must also be said once in the picker.
  //
  // Users make the choice **in a dialog**, and they may have scrolled past the hint in the dialog title;
  // the moment they really press "save" affects every workspace referencing this credential,
  // so after picking, the impact must be stated again in place. Keep it consistent with the line in GlobalArkTab.
  "arkFetch.cascadeGlobal": "保存后，所有引用这份全局配置的空间会立即改用新 key。",

  // Shared by the Ark credential cards (agent / global) and ArkKeyPicker.
  // Check states and region names use enum.arkCheck.* / enum.arkRegion.* (ARK_CHECK_KEYS / ARK_REGION_KEYS).
  "arkCred.replace": "更换",
  "arkCred.status": "状态",
  "arkCred.region": "区域",

  // ArkKeyPicker dialog (manual input path)
  "arkKeyPicker.keyPlaceholder": "粘贴方舟控制台创建的 API Key",
  "arkKeyPicker.consoleBefore": "在",
  "arkKeyPicker.consoleLink": "方舟 API Key 管理",
  "arkKeyPicker.consoleAfter": "页创建。保存后只回显掩码。",

  // "Managed Agents not activated" confirm dialog
  "arkNotActivated.title": "账号还没开通 Managed Agents",
  "arkNotActivated.hint": "开通后回到这里重新保存即可；也可以先把 key 存下来，开通完成后不用再改配置。",
  "arkNotActivated.saveAnyway": "仍然保存",
  "arkNotActivated.activate": "去开通",

  // Agent detail → Ark credential card (AgentArkCard)
  "agentArk.loadFailed": "读取 Agent 方舟凭证失败，请稍后重试",
  "agentArk.actionFailed": "操作失败，请稍后重试",
  "agentArk.title": "方舟凭证",
  "agentArk.description":
    "这个 Agent 用哪把方舟 API Key。默认跟随空间设置，也可以单独配一把——不同业务线的账要分开时用得上。",
  "agentArk.useWorkspace": "改用空间设置",
  "agentArk.setup": "单独配一把",
  // The source bar sentences are split around the <strong> / <Link> parts.
  "agentArk.sourceAgentBefore": "当前使用",
  "agentArk.sourceAgentStrong": "这个 Agent 自己的",
  "agentArk.sourceAgentAfter": "API Key，与空间设置无关。",
  "agentArk.inheritGlobalBefore": "当前跟随空间设置，而那个空间引用的是",
  "agentArk.inheritGlobalStrong": "全局设置",
  "agentArk.ownerEmail": "（{email}）",
  "agentArk.inheritGlobalAfter": "的 API Key",
  "agentArk.inheritWorkspaceBefore": "当前使用",
  "agentArk.inheritWorkspaceStrong": "来自空间设置",
  "agentArk.inheritWorkspaceAfter": "的 API Key",
  "agentArk.inheritTail": "。在那里换掉它，本 Agent 会**立即**跟着变。",
  "agentArk.missingBefore": "还没有可用的方舟 API Key，这个 Agent 暂时跑不了对话。请到",
  "agentArk.missingLink": "空间设置",
  "agentArk.missingAfter": "配一把（整个空间共用），或在这里给它单独配一把。",
  "agentArk.effectiveKey": "生效的 API Key",
  "agentArk.verify": "验证",
  "agentArk.updated": "最近更新",
  "agentArk.pickerReplaceTitle": "更换这个 Agent 的 API Key",
  "agentArk.pickerSetupTitle": "给这个 Agent 单独配一把 API Key",

  // Global settings → global Ark API Key (GlobalArkTab)
  "globalArk.loadFailed": "读取全局方舟凭证失败，请稍后重试",
  "globalArk.title": "全局方舟 API Key",
  "globalArk.description": "配一次，各个空间都可以引用它。换掉之后，所有引用它的空间会立即改用新 key",
  "globalArk.checkedAt": "{time}验证",
  "globalArk.regionHint": "换区域请点「更换」，连 key 一起重填",
  "globalArk.updatedAt": "{time}更新",
  "globalArk.clear": "清除",
  "globalArk.verify": "验证连通性",
  "globalArk.emptyTitle": "还没有配置全局方舟 API Key",
  "globalArk.emptyDesc": "配置之后，各个空间就可以直接引用它，不必每个空间贴一遍。",
  "globalArk.setup": "填写 API Key",
  "globalArk.modalReplaceTitle": "更换全局 API Key",
  "globalArk.modalSetupTitle": "填写全局方舟 API Key",
  "globalArk.modalDesc": "Key 保存后只回显掩码，不会再明文展示。所有引用它的空间会立即改用新 key",
  "globalArk.keyPlaceholder": "从方舟控制台复制",
  "globalArk.clearTitle": "清除全局方舟 API Key",
  "globalArk.clearDesc":
    "清除后，所有引用这份配置的空间都将无法调用方舟，直到你重新配置或让它们改用自己的 key。此操作不可撤销。",

  // Global settings → platform API Keys (ApiKeysTab)
  "apiKeys.loadFailed": "读取 API Key 失败，请稍后重试",
  "apiKeys.description": "给外部 Agent、脚本或 CI 调用本平台接口用。它属于你个人，权限与你本人完全相同",
  "apiKeys.create": "创建 API Key",
  "apiKeys.emptyTitle": "还没有 API Key",
  "apiKeys.emptyDesc": "创建一把 key，外部 Agent 就能用它调用本平台的接口。接口文档见 /api/openapi.json",
  "apiKeys.lastUsed": "{time}使用",
  "apiKeys.neverUsed": "从未使用",
  "apiKeys.revoke": "吊销",
  // The usage line is split around the <code> and the two links.
  "apiKeys.usageLabel": "用法：",
  "apiKeys.usageDocs": "。接口文档见",
  "apiKeys.usageLlms": "，写给 Agent 读的说明见",
  "apiKeys.usageEnd": "。",
  "apiKeys.createDesc": "给它起个名字，便于日后在列表里分辨。它的权限与你本人完全相同。",
  "apiKeys.name": "名称",
  "apiKeys.namePlaceholder": "例如 ci-deploy、我的本地脚本",
  "apiKeys.createdTitle": "API Key 已创建",
  "apiKeys.savedIt": "我已保存",
  "apiKeys.onceBefore": "现在就复制走——",
  "apiKeys.onceStrong": "关掉这个弹窗后就再也看不到了",
  "apiKeys.onceAfter": "。平台只保存它的哈希，找不回来只能吊销重建。",
  "apiKeys.revokeTitle": "吊销「{name}」",
  "apiKeys.revokeDesc": "吊销后使用这把 key 的调用会立即失败，且无法恢复。",

  // Global preferences
  "prefs.title": "全局设置",
  // ⚠️ The point is **ownership**, not "sync across devices".
  //
  // The original line said "it follows you when logging in on another device": that answers "where is it stored", while what users on this page
  // really need to tell apart is "whom these things belong to": the account, not the workspace. Calling it cross-device sync also has a side effect:
  // it sounds like describing a storage implementation detail rather than drawing a scope.
  "prefs.desc": "这些设置属于你的账号，不属于任何空间",
  "prefs.language": "语言",
  "prefs.languageDesc": "界面显示语言",
  "prefs.appearance": "外观",
  "prefs.appearanceDesc": "浅色、深色，或跟随系统",
  "prefs.appearanceLight": "浅色",
  "prefs.appearanceDark": "深色",
  "prefs.appearanceSystem": "跟随系统",
  "prefs.appearanceSystemHint": "当前系统为{mode}",
  "prefs.backToWorkspaces": "返回空间列表",
  // The back text when coming in from a workspace. **Include the workspace name**: this page looks the same in both contexts,
  // and the name is the only clue users have to judge "where clicking goes".
  "prefs.backToWorkspace": "返回{name}",

  // Tabs of global settings
  //
  // "Appearance and language" rather than "preferences": this whole page is already called "global settings",
  // and a tab inside called "preference settings" would make people think it is something else.
  "prefs.tabGeneral": "外观与语言",
  "prefs.tabApiKeys": "API Key",
  "prefs.tabSuggestions": "产品建议",
  "prefs.tabArk": "方舟凭证",
  "prefs.tabIntegrations": "授权管理",

  // Onboarding (app/OnboardingTour.tsx)
  //
  // All six concepts are **real** key concepts of this project, not generic welcome words: the first matches
  // that core constraint in DESIGN.md 1.1 (all resources hang under a workspace), the fourth matches 3.6.3
  // (chat and run history are the same data). An onboarding of empty words is no onboarding.
  "onboarding.title": "新手引导",
  "onboarding.skip": "跳过引导",
  "onboarding.prev": "上一步",
  "onboarding.next": "下一步",
  "onboarding.done": "开始使用",
  "onboarding.progress": "{current}/{total}",
  "onboarding.step1Title": "空间是一切的容器",
  "onboarding.step1Body":
    "Agent、技能、飞书应用、运行历史都归属于某个空间。所以第一步是进入一个空间——" +
    "没有空间时先新建一个，或者让管理员把你加进已有的空间。",
  "onboarding.step2Title": "创建 Agent 并选模型",
  "onboarding.step2Body":
    "Agent 是干活的主体：给它一段提示词、选一个模型（豆包 / DeepSeek / 智谱），" +
    "它就能在容器里执行任务。同一个空间里可以有多个各管一摊的 Agent。",
  "onboarding.step3Title": "用技能扩展 Agent",
  "onboarding.step3Body":
    "技能是一份写给 Agent 的操作手册，教它怎么做一类特定的事。可以直接填写，" +
    "也可以上传 SKILL.md 或整个 zip 包；建好后在 Agent 上勾选即可生效。",
  "onboarding.step4Title": "对话就是一条运行记录",
  "onboarding.step4Body":
    "在「对话」里选一个 Agent 就能开聊，回复逐字出现。每次对话本身就是一条运行记录，" +
    "所以它自动进运行历史，之后从历史点回来还能接着聊。",
  "onboarding.step5Title": "接入飞书，让它在群里干活",
  "onboarding.step5Body":
    "走扫码授权把飞书应用接进来，不用手填任何凭证。注意一个飞书应用只能绑一个 Agent——" +
    "要多个机器人就建多个应用。",
  "onboarding.step6Title": "在运行历史里回看，在用量里看花销",
  "onboarding.step6Body":
    "运行历史能展开每次执行的完整事件时间线（工具调用、出错重试都在里面），" +
    "用量与洞察则回答「一段时间里谁在花钱、谁在变慢、谁在失败」。",
  "onboarding.replay": "新手引导",
  "onboarding.replayDesc": "再看一遍平台的核心概念与上手步骤",
  "onboarding.replayAction": "重新查看引导",

  // Issue feedback (components/FeedbackButton.tsx)
  //
  // ⚠️ **The `feedback.neverCollected` line is a promise, not decoration.** What it lists must match
  // the backend's real collection boundary (the FeedbackInput allowlist in server/core/feedback.go).
  // When changing what is collected, **this sentence must be changed along with it**: saying "never reported" while actually reporting it
  // is far worse than not writing this sentence.
  "feedback.report": "反馈这个问题",
  "feedback.sent": "已收到，谢谢",
  "feedback.viewPayload": "查看将上报的信息",
  "feedback.payloadHint": "点「反馈这个问题」会上报以下信息，不需要你输入任何文字：",
  "feedback.neverCollected":
    "绝不上报：登录凭据、Cookie、API Key、接口返回的内容、你在对话里说过的话。",
  "feedback.fieldRoute": "页面",
  "feedback.fieldStatus": "状态码",
  "feedback.fieldCode": "错误码",
  "feedback.fieldMessage": "错误信息",
  "feedback.fieldBrowser": "浏览器",
  "feedback.fieldVersion": "前端版本",
  "feedback.fieldWorkspace": "空间",
  // The failed endpoint (method + route pattern with ids removed). **Collected at both levels**:
  // the "page" above is which screen the user is on, this one is **which endpoint failed**: one page calls
  // a dozen endpoints, and level one does not collect "recently failed API calls", so without it the level-one record cannot show which one.
  "feedback.fieldAPI": "出错的接口",
  "feedback.fieldPath": "完整路径",
  "feedback.fieldAgent": "当前 Agent",
  // The logid of that server request. **Collected at both levels** (it is a string generated by the server itself, with no user input,
  // needing no user consent), but it only exists when "the request really reached the server": under network_error
  // (cannot connect to the backend) the server has no log of this request at all, and the whole row is not shown.
  "feedback.fieldLogID": "服务端日志 ID",
  "feedback.fieldRecent": "最近失败的接口调用（条）",
  "feedback.fieldConsole": "最近的控制台错误（条）",

  // Timeline event titles and summaries (used by lib/timeline.ts)
  "tl.userMessage": "用户消息",
  // "Platform injected" rather than "system prompt": the latter makes people think it is the agent's persona.
  "tl.injectedContext": "平台注入的上下文",
  "tl.injectedGuidance": "给 Agent 的取用指引",
  "tl.injectedFrom": "来源",
  "tl.systemPrompt": "运行时系统提示词",
  "tl.agentReply": "Agent 回复",
  "tl.thinking": "思考中",
  "tl.thoughtDone": "已深度思考",
  // Only used when content is really empty (measured: 3 of 72). **Do not write "Ark does not return thinking content"**:
  // that was written from the official docs, while in practice Ark includes the text most of the time; see types/events.ts.
  "tl.thinkingSummary": "本次未返回思考内容",
  "tl.userInterrupt": "用户中断",
  "tl.userInterruptSummary": "用户终止了本次执行",
  "tl.compacted": "上下文已压缩",
  "tl.compactedSummary": "上下文超限，已自动摘要压缩",
  "tl.threadCreated": "线程创建",
  "tl.threadMain": "（主线程）",
  "tl.threadSub": "（子线程）",
  "tl.threadRunning": "线程开始运行",
  "tl.threadIdle": "线程空闲",
  // The fallback title when an idle of the requires_action kind cannot be attached to a specific call. **Neither "idle" nor
  // "waiting for the user"**: see the part below tl.stopRequiresAction.
  "tl.threadHandoff": "线程交还控制权",
  "tl.threadRescheduled": "线程重新调度",
  "tl.threadTerminated": "线程终止",
  "tl.sessionRunning": "会话开始运行",
  "tl.sessionIdle": "会话空闲",
  "tl.sessionHandoff": "会话交还控制权",
  "tl.sessionRescheduled": "会话重新调度",
  "tl.sessionRescheduledSummary": "从瞬时错误恢复，已重新排队",
  "tl.sessionTerminated": "会话终止",
  "tl.sessionRenamed": "会话已更名",
  "tl.sessionDeleted": "会话已删除",
  "tl.sessionDeletedSummary": "事件流终止",
  "tl.modelStart": "模型请求开始",
  "tl.modelEnd": "模型请求结束",
  "tl.defineOutcome": "定义产出标准",
  "tl.outcomeOngoing": "产出评估进行中",
  "tl.outcomeStart": "产出评估开始（第 {n} 轮）",
  "tl.outcomeEnd": "产出评估：{result}",
  "tl.round": "第 {n} 轮",
  "tl.sentTo": "发往{name}",
  "tl.receivedFrom": "收到{name}的消息",
  "tl.mainThread": "主线程",
  "tl.callTool": "调用 {name}",
  "tl.callMcp": "MCP 调用 {server} / {name}",
  "tl.callCustom": "自定义工具 {name}",
  "tl.stopEndTurn": "正常结束",
  /**
   * ⚠ **This line was earlier translated as "waiting for user confirmation", which is wrong.**
   *
   * In the self_hosted architecture Ark only does model inference, and tool calls are thrown into the work queue, picked up by amad's worker
   * and dispatched to a Docker container for execution. So for **every tool call** the Ark side goes idle and gives
   * `requires_action`, meaning "I handed control back to you, waiting for `user.tool_result`";
   * **nobody is waiting for the user to click anything**. Translating it as "waiting for user confirmation" would make users, during the time tools run longest,
   * think "it stopped, and I still need to click".
   *
   * Really waiting for the user to click is another path: `evaluated_permission === "ask"` with no
   * `user.tool_confirmation` yet, whose text uses `timeline.needsConfirm` / `timeline.awaitingYou`.
   */
  "tl.stopRequiresAction": "等待工具结果回传",
  "tl.stopRetriesExhausted": "重试耗尽",
  "tl.stopUserInterrupt": "被用户中断",
  // Same as above: these n are the number of "tool calls waiting for results to come back", not "awaiting user confirmation".
  "tl.stopPending": "（{n} 个调用在执行）",
  "tl.retrying": "正在重试",
  "tl.retriesExhausted": "重试耗尽",
  "tl.terminal": "不可恢复",
  "tl.retryStatus": "重试状态：{status}",
  "tl.mcpServer": "MCP 服务：{name}",
  "tl.vault": "保管库：{id}",
  "tl.credential": "凭证：{id}",
  "tl.usageSummary": "输入 {input} / 输出 {output} / 缓存命中 {cache}",
  "tl.outcomeSatisfied": "达标",
  "tl.outcomeNeedsRevision": "需修订",
  "tl.outcomeMaxIter": "达到迭代上限",
  "tl.outcomeFailed": "失败",
  "tl.outcomeInterrupted": "被中断",
  "tl.outcomeGoal": "目标：{desc}",
  "tl.outcomeRubric": "评分标准：\n{content}",
  "tl.outcomeRubricFile": "评分标准文件：{id}",
  "tl.outcomeMaxIterations": "最大迭代：{n}",

  // Enum labels.
  // Originally hard-coded in the *_LABELS constants in types/domain.ts and types/events.ts, moved here after i18n,
  // and fetched by each page with t(`enum.xxx.${value}`). The type side still keeps the key sets of those constants
  // (see *_KEYS in domain.ts), to keep iteration order stable.
  "enum.role.owner": "拥有者",
  "enum.role.admin": "管理员",
  "enum.role.member": "成员",
  "enum.role.viewer": "只读",

  "enum.effort.none": "关闭",
  "enum.effort.minimal": "极简",
  "enum.effort.low": "低",
  "enum.effort.medium": "中",
  "enum.effort.high": "高",
  "enum.effort.xhigh": "很高",
  "enum.effort.max": "最高",

  "enum.agentStatus.active": "已启用",
  "enum.agentStatus.disabled": "已停用",

  "enum.feishuStatus.unverified": "待验证",
  "enum.feishuStatus.connected": "已连接",
  "enum.feishuStatus.error": "连接异常",
  "enum.feishuReadiness.setup_required": "待配置",
  "enum.feishuReadiness.waiting_for_admin": "等管理员批准",
  "enum.feishuReadiness.ready": "就绪",
  "enum.feishuReadiness.runtime_unavailable": "连接异常",

  "enum.runStatus.running": "运行中",
  "enum.runStatus.idle": "空闲",
  // Same as tl.stopRequiresAction: this derived state means "the tool is running in our container, Ark is waiting for the result
  // to come back", **not** "waiting for user confirmation". Like running, it is an in-progress state, not a blocked state.
  "enum.runStatus.requires_action": "工具执行中",
  "enum.runStatus.rescheduling": "调度中",
  "enum.runStatus.terminated": "已结束",

  // Scheduled tasks (proactive ability)
  "nav.schedules": "定时任务",
  "sched.listDesc": "让 agent 主动干活：按 Cron 定时、固定间隔轮询、或一次性提醒。每次触发都会留下一条运行记录",
  "sched.new": "新建定时任务",
  "sched.newDesc": "到点后 agent 会跑一轮，结果可以只留在运行历史里，也可以发回飞书群",
  "sched.editTitle": "编辑定时任务",
  // -- Entity history versions (rollback after mistakes) --------------------------------------------
  //
  // ⚠️ Always say "history version", never just "version": in this product "version" already means the v7 badge at the top of
  // the agent detail page (Agent.version, a counter incremented on every change), and the two are not the same thing.
  "skill.tabFiles": "文件",
  "revision.tab": "历史版本",
  "revision.emptyTitle": "还没有历史版本",
  "revision.emptyDesc": "改动这里的配置之后，改动前的样子会自动存一份，方便改错了切回去。",
  "revision.maxKept": "最多保留最近 {n} 个历史版本，更早的会被自动清理。",
  "revision.showDetail": "看这一版",
  "revision.hideDetail": "收起",
  "revision.rollback": "回滚到这一版",
  "revision.changedBy": "改动者",
  "revision.reasonRollback": "由回滚产生",
  "revision.rollbackTitle": "回滚到第 {no} 版？",
  // ⚠️ This line must make clear "you can come back again", otherwise users do not dare click; rollback itself also saves a copy of the current state,
  // which is exactly why it is safe to click. Do not trim the second half.
  "revision.rollbackDesc": "当前内容会被这一版覆盖。回滚前的样子会自动存成一个新的历史版本，所以点错了还能再切回来。",
  "revision.skillFilesNote": "回滚只还原技能正文与描述，不还原附件文件。",
  "revision.snapshotUnavailable": "这一版的内容读不出来了。",

  "sched.emptyTitle": "还没有定时任务",
  "sched.emptyDesc": "配一条试试：工作日 9:30 自动汇总告警并发回飞书群，或者「1 小时后提醒我看发布结果」。",
  "sched.needAgentTitle": "先创建一个 Agent",
  "sched.needAgentDesc": "定时任务要指定由哪个 Agent 来跑，所以先去建一个。",
  "sched.name": "任务名",
  "sched.namePlaceholder": "每日告警汇总",
  "sched.nameRequired": "请填写任务名",
  "sched.agent": "由哪个 Agent 跑",
  "sched.kind": "触发方式",
  "sched.kindCron": "按 Cron 表达式",
  "sched.kindInterval": "固定间隔",
  "sched.kindOneshot": "一次性",
  "sched.cron": "Cron 表达式",
  "sched.cronDesc": "五段格式：分 时 日 月 周。按 {tz} 时区计算",
  "sched.cronRequired": "请填写 Cron 表达式",
  "sched.presetWorkdayMorning": "工作日 9:30",
  "sched.presetDaily": "每天 9:00",
  "sched.presetHourly": "每小时",
  "sched.presetMonday": "每周一 10:00",
  "sched.interval": "间隔",
  "sched.everyMinutes": "每 {count} 分钟",
  "sched.delay": "多久之后",
  "sched.afterMinutes": "{count} 分钟后",
  "sched.prompt": "到点做什么",
  "sched.promptPlaceholder": "汇总昨天的告警，按严重级别分类，给出需要人工介入的清单",
  "sched.promptDesc": "写成一句给 Agent 的指令。每次触发都是全新会话，所以要写得自洽",
  "sched.promptRequired": "请填写要 Agent 做的事",
  "sched.delivery": "结果发到哪",
  "sched.deliveryRunOnly": "只记录到运行历史",
  "sched.deliveryFeishu": "发到飞书群",
  "sched.chat": "飞书群",
  "sched.chatSearch": "输入群名搜索",
  "sched.chatNoMatch": "没有匹配的群",
  "sched.chatRequired": "请选择要发到哪个群",
  "sched.noChatsTitle": "还没有可选的群",
  "sched.noChatsDesc": "群是在机器人收到消息时自动记下的。先把机器人拉进群、在群里 @ 它一次，这里就会出现那个群。",
  "sched.nextRun": "下次运行",
  "sched.notScheduled": "未排期",
  // ⚠️ This and notScheduled are **two states**; do not merge them. An empty next_run_at has two causes:
  // the user paused it (not scheduled, it really will not run again), or it was just picked up by the scheduler and is running (in progress).
  // The test is status: active means the latter. Merging them into one sentence would describe "it is running" as "you did not configure it",
  // which was the most misleading part of that production failure on 2026-08-30.
  "sched.running": "运行中",
  "sched.runCount": "运行 / 失败",
  "sched.lastRun": "上次运行",
  "sched.view": "查看",
  "sched.runNow": "立刻试跑一次",
  "sched.runNowQueued": "「{name}」已排入队列，稍后会开始运行",
  "sched.paused": "已暂停",
  "sched.toggleLabel": "启用或暂停 {name}",
  "sched.lastError": "上次失败：{error}",
  "sched.deleteTitle": "删除定时任务「{name}」",
  "sched.deleteDesc": "删除后它不再触发，已产生的运行记录仍会保留。此操作不可撤销。",

  // Detail page
  "sched.loading": "正在加载定时任务",
  "sched.notFound": "这条定时任务不存在",
  "sched.notFoundDesc": "它可能已被删除。一次性任务跑完就会自动删掉，那是正常的。",
  "sched.backToList": "返回定时任务",
  "sched.openDetail": "查看详情",
  "sched.tabConfig": "配置",
  "sched.tabHistory": "运行历史",
  "sched.statusActive": "已启用",
  "sched.enabled": "启用",
  "sched.lastRunAt": "上次运行",
  "sched.never": "还没跑过",
  "sched.countRuns": "累计运行",
  "sched.countFails": "累计失败",
  "sched.countConsecutive": "连续失败",
  "sched.times": "{count} 次",
  "sched.autoPauseHint": "再失败 {left} 次就会被自动暂停",
  "sched.autoPaused": "已因连续失败被自动暂停。修好之后点启用，连续失败数会清零",
  "sched.lastErrorTitle": "上次运行失败",
  "sched.agentDeletedTitle": "关联的 Agent 已被删除",
  "sched.agentDeletedDesc": "关联的 Agent 已被删除，这条任务跑不出结果，请改绑一个 Agent",
  "sched.oneshotTitle": "这是一次性任务",
  "sched.oneshotDesc": "它跑完（无论成败）就会被自动删除，所以在这里看到它说明还没触发过。",
  "sched.tzHint": "以下时刻均为 {tz}",
  "sched.saveConfig": "保存配置",
  "sched.savedNeedsNothing": "已保存，下次触发按新配置跑",
  "sched.historyEmpty": "还没有运行记录",
  "sched.historyEmptyDesc": "它到点跑过之后，每一轮都会作为一条运行记录出现在这里。",
  "sched.historyGap": "更早的 {count} 次运行查不到——那时还没记下「哪条任务触发的」，已产生的数据补不上",
  "sched.historyCount": "查到 {shown} 条，累计跑过 {total} 次",
  "sched.runNowFromDetail": "试跑一次",

  "enum.trigger.manual": "手动",
  "enum.trigger.feishu": "飞书",
  "enum.trigger.api": "API",
  "enum.trigger.schedule": "定时",
  "enum.trigger.webhook": "Webhook",
  "enum.trigger.weixin": "微信",
  "weixin.listDesc": "扫码把微信机器人接入空间，绑定 Agent 后，在微信里给它发消息就由 Agent 回答",
  "weixin.connect": "扫码接入",
  "weixin.emptyTitle": "还没有接入微信",
  "weixin.emptyDesc": "用手机微信扫码，就能在微信里直接和 Agent 对话。",
  "weixin.loginTitle": "扫码接入微信",
  "weixin.qrAlt": "微信登录二维码",
  "weixin.verifyCode": "手机上显示的数字",
  "weixin.verifySubmit": "提交",
  "weixin.retry": "重新扫码",
  "weixin.close": "关闭",
  "weixin.owner": "对话的微信用户：{user}",
  "weixin.boundAgent": "由哪个 Agent 回答",
  "weixin.pickAgent": "选择 Agent",
  "weixin.noAgent": "不绑定",
  "weixin.openAgent": "查看 Agent",
  "weixin.unboundHint": "还没绑定 Agent：收到消息时机器人只会回一句提示。",
  "weixin.expiredHint": "微信登录已失效，请重新扫码接入。",
  "weixin.deleteAction": "断开接入",
  "weixin.deleteTitle": "断开这个微信机器人？",
  "weixin.deleteDesc": "断开后它不再收发消息；要再用需要重新扫码接入。",
  "weixin.connectedAt": "接入于 {time}",
  "weixin.lastInbound": "最近收到消息 {time}",
  "enum.weixinLogin.waiting": "等待扫码",
  "enum.weixinLogin.scanned": "已扫码",
  "enum.weixinLogin.need_verify_code": "需要验证",
  "enum.weixinLogin.confirmed": "已接入",
  "enum.weixinLogin.already_connected": "已接入过",
  "enum.weixinLogin.expired": "已过期",
  "enum.weixinLogin.failed": "失败",
  "enum.weixinAccount.active": "已连接",
  "enum.weixinAccount.expired": "需重新扫码",

  "enum.category.conversation": "对话",
  "enum.category.thinking": "思考",
  "enum.category.tool": "工具调用",
  "enum.category.thread": "线程",
  "enum.category.status": "状态",
  "enum.category.trace": "追踪",
  "enum.category.error": "错误",
  "enum.category.outcome": "结果评估",

  // Labels for ARK_REGIONS / ARK_CHECK_LABELS in types/domain.ts (look up via ARK_REGION_KEYS / ARK_CHECK_KEYS).
  "enum.arkRegion.cn-beijing": "火山引擎 · 华北（北京）",
  "enum.arkRegion.ap-southeast-1": "BytePlus · 亚太（新加坡）",
  "enum.arkCheck.unchecked": "未验证",
  "enum.arkCheck.valid": "可用",
  "enum.arkCheck.invalid": "不可用",

  // Validators and labels in types/domain.ts. The zh text must stay word for word: the mock backend
  // in data/api.ts returns it as the backend's error, and e2e asserts on it.
  "domain.wsKeyRequired": "空间 Key 不能为空",
  "domain.wsKeyTooShort": "至少 2 个字符",
  "domain.wsKeyTooLong": "最多 32 个字符",
  "domain.wsKeyInvalid": "只能用小写字母、数字和连字符，且以字母或数字开头",
  "domain.arkKeyRequired": "API Key 不能为空",
  "domain.arkKeyTooShort": "API Key 长度不足，请确认是否复制完整",
  "domain.arkKeyHasSpace": "API Key 不应包含空格",
  "domain.apiKeyNameRequired": "请给这把 key 起个名字",
  "domain.apiKeyNameTooLong": "名字不能超过 128 个字符",
  "domain.envSourceAmaToken": "AMA 平台凭据",
  "domain.envSourceLarkUserToken": "飞书用户凭据（lark-cli）",
  "domain.cronFieldCount": "要 5 段：分 时 日 月 周，例如工作日 9:30 是 30 9 * * 1-5",
  "domain.cronOutOfRange": "第 {field} 段的 {value} 超出范围 {min}-{max}",

  // Placeholders for non-text content blocks (contentToText in types/events.ts).
  "evt.blockImage": "[图片]",
  "evt.blockImageUrl": "[图片 {url}]",
  "evt.blockDocument": "[文档]",
  "evt.blockDocumentTitled": "[文档 {title}]",
  "evt.blockSearchResult": "[搜索结果 {title}]",

  // Sandbox terminal disconnect reasons (lib/terminalTransport.ts passes the key, SandboxTerminal translates).
  "sandbox.terminalDisconnectedWith": "容器终端连接断开：{reason}",
  "sandbox.termReasonTicket": "签发终端入场票失败",
  "sandbox.termReasonWsUrl": "WebSocket 地址不可用",
  "sandbox.termReasonWsHandshake": "WebSocket 握手失败",
  "sandbox.termReasonNoOutput": "收不到容器输出",
  "sandbox.termReasonBadReadyFrame": "终端握手帧解析失败",
  "sandbox.termReasonClosed": "容器终端已关闭",
  "sandbox.termReasonConnect": "连接容器终端失败",
  "sandbox.termReasonInput": "按键发不进容器",

  "wsGate.openFailed": "打开空间失败，请稍后重试",
  "wsGate.backToList": "返回我的空间",
  "rolePerm.title": "各角色能做什么",
  "rolePerm.markAllowed": "{role}：可以",
  "rolePerm.markDenied": "{role}：不可以",
  "skill.bodyPlaceholder": "# 技能标题\n\n## 步骤\n\n1. ⋯",
  "skill.builtin.larkCli.title": "lark-cli（飞书 CLI）",
  "skill.builtin.larkCli.description":
    "操作飞书：消息、云文档、多维表格、日历、审批、任务、邮箱、会议等。需要读写飞书里的数据时用它，优先于直接调 OpenAPI。",
  "skill.builtin.ama.title": "ama（AMA 平台自身的 CLI）",
  "skill.builtin.ama.description":
    "操作 AMA 平台自己的数据：空间、Agent、技能、运行历史、问题（issue）。用户让你「提个问题」「看看有哪些 agent」「查运行历史」这类请求时用它，它以当前用户的身份操作，权限与那个人登录时完全一样。",
  "skill.builtin.tagBuiltin": "内置",
  "skill.builtin.tagFeishu": "飞书",

  // Ark's four event domains (the prefix of {domain}.{action}).
  // system.message belonging to the User domain is an explicit exception in Ark: the test is direction, not prefix.
  "enum.domain.user": "用户侧",
  "enum.domain.agent": "Agent 侧",
  "enum.domain.session": "会话",
  "enum.domain.span": "跨度",
  "enum.domain.userHint": "客户端上行的事件；动态系统提示词 system.message 也属此域",
  "enum.domain.agentHint": "Agent 下发的事件：回复、思考信号、工具调用",
  "enum.domain.sessionHint": "会话与线程的生命周期、状态与错误",
  "enum.domain.spanHint": "执行跨度：模型请求与结果评估",

  "enum.tool.bash": "在沙箱里执行 shell 命令",
  "enum.tool.read": "读取文件内容",
  "enum.tool.write": "写入文件",
  "enum.tool.edit": "编辑已有文件",
  "enum.tool.glob": "按通配符查找文件",
  "enum.tool.grep": "在文件里搜索内容",
  "enum.tool.web_fetch": "抓取网页",
  "enum.tool.web_search": "联网搜索",

  // The landing page when not logged in (clicking the CTA enters the login flow below)
  "landing.slogan": "企业级智能体托管与执行平台",
  /*
   * This intro has an external link in the middle, so it is split into three parts, "first half + link text + second half", rather than one sentence.
   * Not a {link} placeholder plus replace: t() returns a plain string, and putting an <a> into it could only rely on
   * dangerouslySetInnerHTML; introducing an XSS surface for one link is not worth it. Joining three parts in JSX is easiest,
   * and lets each language decide where in the sentence the link goes (in English it goes at the end).
   */
  "landing.introBefore": "基于",
  "landing.introLink": "方舟 Managed Agents",
  "landing.introAfter": "构建企业智能体。",
  "landing.hostTitle": "托管 Agent",
  "landing.hostDesc": "提示词、模型与工具集中配置，改完即生效，版本可追。",
  "landing.collabTitle": "多智能体协同",
  "landing.collabDesc": "按空间隔离 Agent 与技能，成员各司其职，互不干扰。",
  "landing.sandboxTitle": "自建容器执行",
  "landing.sandboxDesc": "六个工具跑在我们自己的沙箱里，共享文件系统，闲置即暂停。",
  "landing.feishuTitle": "飞书原生接入",
  "landing.feishuDesc": "扫码即完成应用注册，在群里直接和 Agent 对话。",

  // Login (email + password / OIDC, see pages/LandingPage.tsx)
  // ⚠️ There is no longer a "checking login status" line: AuthGate's loading state became **a text-free spinner delayed by 300ms**
  // (see StartupSpinner in components/Page.tsx), and the screen reader announcement reuses
  // `common.loading`. Do not add a dedicated text back: it would flash on every refresh.
  "login.title": "登录 AMA",
  "login.signupTitle": "注册 AMA 账号",
  "login.email": "邮箱",
  "login.password": "密码",
  "login.name": "姓名（可选）",
  "login.passwordHint": "8–72 个字符",
  "login.submit": "登录",
  "login.signupSubmit": "注册并登录",
  "login.toSignup": "没有账号？注册",
  "login.toLogin": "已有账号？去登录",
  "login.firstUserHint": "这个平台还没有任何账号，第一个注册的账号将成为平台管理员",
  "login.or": "或",
  "login.oidc": "使用 {name} 登录",
  "login.noMethod": "这个部署还没有配置任何登录方式。请管理员在服务端配置 auth.local 或 auth.oidc。",
  "login.providersFailed": "暂时拿不到登录方式，请稍后重试",
  "login.retry": "重试",
  // Picked by the backend's code when password login / registration fails. Unknown codes fall back to errGeneric.
  "login.errInvalidCredentials": "邮箱或密码不正确",
  "login.errTooManyAttempts": "尝试次数过多，请稍后再试",
  "login.errEmailTaken": "这个邮箱已经注册过了，请直接登录",
  "login.errSignupDisabled": "这个平台没有开放注册，请联系管理员开通账号",
  "login.errWeakPassword": "密码长度要在 8–72 个字符之间",
  "login.errLocalDisabled": "这个平台没有开启邮箱密码登录",
  "login.errInvalidArgument": "请填写有效的邮箱和密码",
  "login.errGeneric": "登录失败，请稍后重试",
  // When the OIDC callback fails the backend 303s back to `/?login_error=<code>`, and these are picked by code.
  // ⚠️ Each one must say "what to do next": when login fails users can only retry or find someone,
  // and a vague "login failed" would make them repeat something bound to fail.
  "login.errDenied": "你取消了登录授权，请重新登录",
  "login.errExpired": "这个登录链接已失效，请重新登录",
  "login.errNoIdentity": "没能确定你的邮箱，请联系管理员",
  "login.errEmailUnverified": "你的邮箱在身份提供方那边还没有验证，验证后再登录",
  "login.errUnavailable": "登录服务暂时不可用，请稍后重试",
  "login.logout": "退出登录",
  // GitHub device flow login (GitHubDevicePanel in pages/LandingPage.tsx).
  "login.github": "使用 {name} 登录",
  "login.githubTitle": "在 GitHub 上输入这个代码",
  "login.githubHint": "打开 GitHub 的设备授权页，输入下面的代码并点授权。授权后这个页面会自动登录。",
  "login.githubCode": "设备代码",
  "login.githubCopy": "复制代码",
  "login.githubOpen": "打开 github.com/login/device",
  "login.githubWaiting": "正在等你在 GitHub 上授权",
  "login.githubExpiresIn": "{time} 后过期",
  "login.githubCancel": "取消",
  "login.githubRestart": "重新获取代码",
  "login.errGithubExpired": "这个代码已经失效，请重新获取",
  "login.errGithubDenied": "你在 GitHub 上取消了授权，可以重新获取代码再试",
  "login.errGithubNoEmail": "你的 GitHub 账号没有已验证的主邮箱。请先在 GitHub 的邮箱设置里验证并设为主邮箱，再重新登录",
  "login.errGithubNotAllowed": "这个 GitHub 账号没有登录本平台的权限，请联系管理员把你加进白名单",
  "login.errGithubDisabled": "这个平台没有开启 GitHub 登录",
  "login.errGithubRetrying": "暂时连不上 GitHub，正在重试",

  // Mobile (components/ChatSessions.tsx).
  // On narrow screens the left column of the chat page is hidden and the session list moves into a drawer; these two are its entry and title.
  "chat.mobileOpenSessions": "会话列表",
  "chat.mobileSessionsTitle": "最近对话",

  // Host executors (pages/HostsTab.tsx, WorkspaceHostsTab.tsx,
  // components/AgentRuntimeSections.tsx, SandboxPanel.tsx)
  "prefs.tabHosts": "宿主机",
  "hosts.title": "我注册的宿主机",
  "hosts.desc": "在这些机器上执行 agent 的工具调用。机器用 daemon 注册，按你的账号归属",
  "hosts.loadFailed": "读取宿主机列表失败，请稍后重试",
  "hosts.emptyTitle": "还没有注册宿主机",
  "hosts.emptyDesc": "按下面三步在自己的机器上安装 daemon，就能让 agent 在你的电脑上执行。",
  "hosts.online": "在线",
  "hosts.offline": "离线",
  "hosts.lastSeen": "最后在线 {time}",
  "hosts.gnuOk": "命令行工具自检通过",
  "hosts.gnuBad": "命令行工具自检未通过",
  "hosts.gnuBadHint": "agent 的工具依赖这套命令行工具，请在机器上修复后重试。",
  "hosts.gnuBadMacHint": "Mac 需要先装齐 GNU 工具（agent 的 bash/grep 等依赖它们）：",
  "hosts.nativeOk": "daemon 支持原生工具（无需 brew 依赖）",
  "hosts.nativeBad": "daemon 版本过旧",
  "hosts.nativeBadHint": "这台机器上的 ama daemon 不支持原生工具协议，请到机器上升级新版 ama CLI 后执行 ama daemon restart（新版不需要 brew install 任何东西）。",
  "hosts.delete": "删除",
  "hosts.deleteTitle": "删除「{name}」",
  "hosts.deleteBody":
    "删除后这台机器会从所有空间解绑，daemon 即使仍在运行也领不到任务。重新使用需要再注册一次。",
  "hosts.guideTitle": "怎么接入宿主机",
  "hosts.guideDesc": "在要接入的机器上依次执行下面两步。PATH 里已经有 ama 命令的话从第 2 步开始。",
  "hosts.step1Title": "安装 AMA 命令行",
  "hosts.step1Desc": "从源码编译 AMA 命令行（需要 Go 1.24+），装到 $GOPATH/bin。请确认它在 PATH 里。",
  "hosts.step2Title": "注册并装成后台服务",
  "hosts.step2Desc": "一条命令完成注册、装成开机自启的常驻服务并立即运行。API Key 只在首次注册时用一次，去「API Key」tab 创建（明文只显示一次）。",
  "hosts.step2ApiKeyLink": "去创建 API Key",

  // Workspace settings → hosts tab
  "wsHosts.scopeTitle": "只能把你本人注册的机器绑进空间",
  "wsHosts.scopeBody":
    "下拉里只出现你本人注册的宿主机；同事的机器需要他本人先在全局设置注册，再由他本人或管理员绑进空间。绑进空间后，管理员可以把它指派给任意 Agent；Agent 创建者也可以不经过空间、个人直连自己的机器。",
  "wsHosts.gotoPrefs": "注册我的宿主机",
  "wsHosts.title": "空间已绑定的宿主机",
  "wsHosts.desc": "agent 选择「宿主机」执行端时，只能使用绑在本空间内的机器。",
  "wsHosts.empty": "这个空间还没有绑定宿主机",
  "wsHosts.emptyHint": "从下面的列表里选一台你注册过的机器绑定。",
  "wsHosts.addTitle": "添加绑定",
  "wsHosts.addDesc": "绑定后，这个空间里新建的 agent 就可以选择在这台机器上执行。",
  "wsHosts.bind": "绑定",
  "wsHosts.pickHost": "选择宿主机",
  "wsHosts.noneTitle": "你还没有注册任何宿主机",
  "wsHosts.noneHint": "先在全局设置的「宿主机」tab 按三步引导装好 daemon，再回来绑定。",
  "wsHosts.allBound": "你注册的机器都已经绑在这个空间了。",
  "wsHosts.remove": "解绑",
  "wsHosts.removeTitle": "解绑「{name}」",
  "wsHosts.removeBody": "解绑后这个空间的 agent 不能再使用这台机器；机器本身不会被删除，仍在全局设置里。",
  "wsHosts.registeredBy": "注册者",
  "wsHosts.boundBy": "绑定者",
  "wsHosts.notRecorded": "未记录",

  // Agent form → executor choice
  "agent.executorSection": "执行端",
  "agent.executorSectionDesc": "agent 的工具调用在哪里执行。",
  "agent.executorCloud": "Docker 容器",
  "agent.executorCloudDesc": "云端沙箱，开箱即用；空闲自动暂停。",
  "agent.executorHost": "我的宿主机",
  "agent.executorHostDesc": "在我自己注册的机器上执行，工作区持久保留。",
  "agent.executorHostPick": "选择宿主机",
  "agent.executorHostPlaceholder": "选择一台已绑定的机器",
  "agent.executorOnline": "在线",
  "agent.executorOffline": "离线",
  "agent.executorOfflineWarn": "这台机器当前离线：daemon 不在线时 agent 的工具调用会失败，机器重新上线后恢复。",
  "agent.executorNoneTitle": "这个空间还没有绑定任何宿主机",
  "agent.executorNoneHint": "先在空间设置的「宿主机」tab 绑定一台你注册过的机器，再回到这里选择。",
  "agent.executorNoneLink": "去空间设置绑定宿主机",
  "agent.executorHostMissing": "请选择一台宿主机；当前选择的机器已被解绑或删除，请重新选择。",
  // Two levels of source (workspace assignment / personal direct connection)
  "agent.executorSourceLabel": "宿主机来源",
  "agent.executorSourceWorkspace": "空间指派",
  "agent.executorSourceWorkspaceDesc": "选空间里已绑定的机器；由空间管理员或机器属主指派，解绑空间后停用。",
  "agent.executorSourcePersonal": "个人直连",
  "agent.executorSourcePersonalDesc": "直接选我自己注册的机器，不经过空间绑定；只有我能改，网页终端也只有我能开。",
  "agent.executorMyHost": "我的机器",
  "agent.executorNotAssignable": "仅管理员或机器属主可指派",
  "agent.executorPersonalPick": "选择我注册的机器",
  "agent.executorPersonalNoneTitle": "你还没有注册任何宿主机",
  "agent.executorPersonalNoneHint": "在「偏好设置 → 宿主机」里装好 ama daemon 并注册一台自己的机器，就能直接用，不必绑进空间。",
  "agent.executorPersonalGotoRegister": "去注册我的宿主机",
  "agent.executorPersonalTerminalNote": "个人直连不经过空间：空间解绑不影响它；这台机器的网页终端只有你本人能打开。",
  "agent.executorPersonalLockedTitle": "执行端已锁定为创建者的个人直连机器",
  "agent.executorPersonalLockedHint": "这台宿主机是创建者（{email}）的个人机器，不属于空间，只有他本人能更换或移除。需要空间统一分配执行机器时，请请他改用「空间指派」。",
  "agent.executorPersonalForbiddenHint": "个人直连只有这个 agent 的创建者（{email}）本人能选。需要用某个人的机器时，请让创建者本人来设，或改用上面的「空间指派」。",
  "agent.executorPersonalInvisibleTitle": "创建者的个人宿主机",
  "agent.executorPersonalInvisibleHint": "这台机器只属于 agent 创建者，不对你展示，也不能由你修改。",

  // Chat page → host execution panel (SandboxPanel)
  "sandbox.host.badge": "宿主机",
  "sandbox.host.title": "宿主机执行中",
  "sandbox.host.desc": "这轮对话的工具在宿主机「{name}」上执行，工作区在机器上持久保留。",
  "sandbox.host.fallbackName": "我的宿主机",
  "sandbox.host.filesUnsupported": "宿主机执行端暂不支持文件浏览，需要查看文件请使用终端。",
  "sandbox.host.openTerminal": "打开终端",
} as const;

/** The Chinese table is the reference for the key set: a missing key in the en table is reported at the type level. */
export type MessageKey = keyof typeof zhCN;
export type Messages = Record<MessageKey, string>;

export const enUS: Messages = {
  "common.save": "Save",
  "common.cancel": "Cancel",
  "common.create": "Create",
  "common.delete": "Delete",
  "common.edit": "Edit",
  "common.remove": "Remove",
  "common.saved": "Saved",
  "common.loading": "Loading",
  "common.retry": "Retry",
  "common.loadFailed": "Failed to load",
  "common.accessDenied": "Access denied",
  "common.loadFailedUnknown": "Failed to load, please try again later",
  "common.actionFailed": "Action failed",
  "common.all": "All",
  "common.back": "Back",
  "common.optional": "optional",
  "common.notFilled": "Not set",
  "common.irreversible": "This cannot be undone.",

  "nav.overview": "Overview",
  "nav.groupBuild": "Build",
  "nav.groupObserve": "Observe",
  "nav.groupManage": "Manage",
  "nav.groupAccount": "Account",
  "nav.agents": "Agents",
  "nav.chat": "Chat",
  "nav.skills": "Skills",
  "nav.issues": "Issues",
  "nav.feishu": "Feishu",
  "nav.weixin": "WeChat",
  "nav.runs": "Run History",
  "nav.usage": "Usage & Insights",
  "nav.workspaceSettings": "Workspace Settings",
  "nav.switchWorkspace": "Switch Workspace",
  "nav.manageWorkspaces": "Manage all workspaces",
  "nav.preferences": "Preferences",
  "nav.globalSettings": "Global Settings",
  "nav.openNav": "Open navigation",
  "nav.closeNav": "Close navigation",
  "nav.accountMenu": "Account menu",

  "ws.emptyTitle": "No workspaces to open yet",
  "ws.emptyDesc":
    "Create one to get started — agents, skills, Feishu apps and run history all belong to a workspace. If you believe you should already have access to one, ask its admin to add you as a member.",
  "ws.pickTitle": "Select a workspace",
  "ws.pickDesc": "Everything happens inside a workspace",
  "ws.new": "New workspace",
  "ws.create": "Create workspace",
  "ws.enter": "Open",
  "ws.name": "Name",
  "ws.namePlaceholder": "e.g. Growth Platform",
  "ws.nameRequired": "Name is required",
  "ws.key": "Workspace key",
  "ws.keyDesc": "Used in the URL — lowercase letters, digits and hyphens only",
  "ws.desc": "Description",
  "ws.descPlaceholder": "What is this workspace for (optional)",
  "ws.memberCount": "{count} members",
  "ws.createdAt": "Created {time}",
  "ws.createFailed": "Failed to create",
  "ws.keyTaken": 'Key "{key}" is already taken',

  "overview.subtitle": "Workspace overview",
  "overview.startTitle": "Start with your first agent",
  "overview.startDesc":
    "Give an agent a system prompt and a model, bind skills or a Feishu app, and it is ready to work.",
  "overview.metricAgents": "Agents",
  "overview.metricAgentsHint": "{count} active",
  "overview.metricSkills": "Skills",
  "overview.metricSkillsHint": "Workspace assets",
  "overview.metricFeishuHint": "{count} connected",
  "overview.metricTokens": "Tokens used",
  "overview.metricRuns": "{count} runs",
  "overview.metricFailed": " · {count} failed",
  "overview.recentRuns": "Recent runs",
  "overview.viewAll": "View all",
  "overview.noRuns": "No runs yet",

  "agent.listDesc":
    "Each agent has its own system prompt and model, and can bind workspace skills and Feishu apps",
  "agent.new": "New agent",
  "agent.emptyTitle": "No agents yet",
  "agent.emptyDesc":
    "Create an agent with a system prompt and a model, then trigger it from Feishu or the API.",
  "agent.newDesc": "Feishu apps are bound on the detail page after creating",
  "agent.name": "Name",
  "agent.namePlaceholder": "Ticket triage assistant",
  "agent.nameRequired": "Name is required",
  "agent.desc": "Description",
  "agent.descPlaceholder": "Triages user feedback from Feishu groups",
  "agent.system": "System prompt",
  "agent.systemDesc": "Defines the agent's role and rules — maps to Ark's `system` field",
  "agent.systemPlaceholder": "You are…\n\nPrinciples:\n- …",
  "agent.loading": "Loading agent",
  "agent.systemRequired": "System prompt is required",
  "agent.systemCount": "{count} / 100000 characters",
  "agent.model": "Model",
  "agent.reasoning": "Reasoning effort",
  "agent.serviceTier": "Service tier",
  "agent.noFastTier":
    "This model does not support the fast tier — per Ark's model parameter table, only lite and turbo do.",
  "agent.status": "Status",
  "agent.basicInfo": "Basic info",
  "agent.systemSection": "System prompt",
  "agent.systemSectionDesc": "This agent's own prompt — maps to Ark's `system` field",
  "agent.modelSection": "Model",
  "agent.modelSectionDesc": "Agents can use different models and reasoning levels",
  "agent.toolsSection": "Built-in tools",
  "agent.toolsSectionDesc": "Ark's sandbox tools — the agent can call the ones you enable",
  "agent.builtinSkillsSection": "Platform built-in skills",
  "agent.builtinSkillsSectionDesc":
    "Both CLIs are preinstalled in the container and bound to new agents by default — uncheck any you don't need",
  "agent.integrationsSection": "Authorizations",
  "agent.integrationsSectionDesc":
    "When checked, tools in the container act as the workspace owner on that platform. Nothing is checked by default",
  "agent.integrationBindLabel": "Enable {name}",
  "agent.integrationReady": "Ready",
  "agent.integrationNeedsAuth": "Not authorized",
  "agent.integrationNeedsAuthHint":
    "You have not authorized this yet, so it cannot be selected. Go to Settings \u2192 Integrations and hit Authorize, then come back.",
  "agent.integrationAuthorizeTarget": "Will authorize: {name} ({appId})",
  "agent.integrationNoAppInWorkspace":
    "This workspace has no Feishu app connected yet, so authorization is unavailable. Connect one first:",
  "agent.integrationAuthorize": "Authorize here",
  "agent.integrationAuthorizeWaiting": "Waiting\u2026",
  "agent.integrationAuthorizeOpened":
    "Authorization page opened in a new tab. Come back here once you are done.",
  "agent.integrationAuthorizePolling":
    "Feishu authorization page opened. Once you tap Approve there, this becomes selectable automatically \u2014 no need to refresh, and nothing unsaved on this page is lost.",
  "agent.integrationRisk":
    "Note: once checked, this agent can read these credentials and call any service the workspace owner is allowed to use. Only give it prompts and skills you trust.",
  // The wording says out loud that the credential may belong to someone else:
  // credentials are per-account, agents are per-workspace, and everyone in the
  // workspace running this agent uses it — its owner vouches for those calls.
  "agent.integrationsReadOnly": "Injected authorizations",
  "agent.integrationsReadOnlyHint":
    "Changing this requires admin permission. Injected credentials represent a real person — whatever that employee can do, the agent can do. Only workspace admins and owners can change it. Contact a workspace admin if you need it adjusted.",
  "agent.integrationsReadOnlyNone": "No authorizations injected",
  "agent.envSection": "Environment variables",
  "agent.envSectionDesc":
    "Injected into the container on every command. Non-secret entries are also written into the system prompt",
  "agent.envKey": "Name",
  "agent.envValue": "Value",
  "agent.envSecret": "Secret",
  "agent.envAdd": "Add variable",
  "agent.envRemove": "Remove",
  "agent.envReplace": "Replace",
  "agent.envAtLimit": "Limit reached ({max})",
  "agent.envKeyInvalid":
    "Letters, digits and underscores only, and it cannot start with a digit",
  "agent.envKeyReserved":
    "This name is reserved by the platform and injected automatically — pick another",
  "agent.envKeyDuplicate": "Duplicate name",
  "agent.envValueTooLong": "Value is too long",
  "agent.envNote":
    "Non-secret entries go into the system prompt (the model can see them); secret ones do not, and are masked on this page. Note: secret entries are still injected into the container — the agent can read them in plaintext by running env. This flag only keeps them out of the model context and this page; it does not hide them from the agent.",
  "agent.envAliasSection": "Credential aliases",
  "agent.envAliasSectionDesc":
    "Inject an existing platform credential a second time under a variable name you choose — for third-party CLIs that only read their own names.",
  "agent.envAliasName": "Variable name",
  "agent.envAliasSource": "Credential to reference",
  "agent.envAliasAdd": "Add alias",
  "agent.envAliasRemove": "Remove this alias",
  "agent.envAliasAtLimit": "Limit reached ({max})",
  "agent.envAliasNameInvalid":
    "Letters, digits and underscores only, and it cannot start with a digit",
  "agent.envAliasNameReserved": "This name is reserved by the platform and cannot be an alias",
  "agent.envAliasNameDuplicate": "Duplicate name",
  "agent.envAliasClashesEnv":
    "This name is already a custom environment variable — keep only one of them",
  "agent.envAliasNote":
    "The value is filled in by the platform at execution time; you can neither see nor set it — use Environment variables above to supply your own. Note: the referenced credential must be enabled under Authorizations above, otherwise this alias has no effect. The agent can still read the value by running env.",
  "agent.envAliasReadOnly": "Configured credential aliases",
  "agent.envAliasReadOnlyNone": "No credential aliases configured.",
  "agent.envAliasReadOnlyHint":
    "Aliases decide which credential variables appear in the container — same tier as Authorizations, so only workspace admins or the owner can change them.",
  "agent.notFound": "Agent not found",
  "agent.notFoundDesc": "It may have been deleted.",
  "agent.backToList": "Back to list",
  "agent.deleteTitle": 'Delete agent "{name}"',
  "agent.deleteDesc":
    "Run history is kept, but this agent can no longer be triggered. This cannot be undone.",
  "agent.deleteAction": "Delete agent",
  "agent.syncFailed": "Couldn't sync this config to Ark",
  "agent.syncFailedCredential": "Ark credential problem \u2014 check the API key in workspace settings",
  "agent.syncFailedTransient": "Ark is temporarily unavailable \u2014 you can retry",
  "agent.syncFailedSkills": "Skills didn't attach on the Ark side \u2014 check the skill config",
  "agent.syncRetry": "Retry",
  "agent.syncRetrying": "Retrying",
  "agent.syncFailedHint":
    "Your changes are saved, but Ark still has the old config; it will try again on the next conversation.",
  "agent.deleted": "Deleted",
  "agent.tryIt": "Try it",
  "agent.summaryModel": "Model",
  "agent.summaryReasoning": "Reasoning",
  "agent.summarySkills": "Skills",
  "agent.summaryFeishu": "Feishu",
  "agent.summaryVersion": "Version",
  "agent.tabConfig": "Config",
  "agent.tabSkills": "Skills",
  "agent.tabFeishu": "Feishu",
  "agent.tabRuns": "Runs",
  "agent.toolCount": "{count} built-in tools",
  "agent.skillCount": "{count} skills",
  "agent.feishuBound": "Feishu linked",
  "agent.updatedAt": "Updated {time}",
  "agent.createdBy": "Created by",
  "agent.mineOnly": "Created by me",
  "agent.mineOnlyEmpty": "You have not created any agent in this workspace",
  "user.unknownActor": "Creator not recorded",

  "skill.listDesc":
    "Skills are workspace assets bound by reference — editing one takes effect immediately for every agent using it",
  "skill.new": "New skill",
  "skill.emptyTitle": "No skills yet",
  "skill.emptyDesc":
    "A skill packages domain knowledge and procedures into a reusable unit — easier to maintain than piling rules into a system prompt.",
  "skill.title": "Skill name",
  "skill.titlePlaceholder": "Ticket triage",
  "skill.titleRequired": "Skill name is required",
  "skill.name": "Identifier",
  "skill.namePlaceholder": "ticket-triage",
  "skill.nameRequired": "Identifier is required",
  "skill.nameInvalid": "Lowercase letters, digits and hyphens only",
  "skill.when": "When to use",
  "skill.whenPlaceholder":
    "Use when a user reports a problem or files a ticket. Judge severity, classify by module…",
  "skill.whenDesc":
    "The model decides whether to invoke the skill from this text — describing the trigger beats describing the capability",
  "skill.whenRequired": "Describe when to use this skill",
  "skill.body": "Skill body",
  "skill.bodyDesc": "Markdown — becomes SKILL.md in the Ark skill bundle",
  "skill.bodyRequired": "Skill body is required",
  "skill.tags": "Tags",
  "skill.tagsDesc": "Comma separated",
  "skill.tagsPlaceholder": "support, triage",
  "skill.forkAction": "Duplicate to edit",
  "skill.builtin": "Built-in",
  "skill.boundCount": "Bound by {count} agents",
  "skill.createTitle": "New skill",
  "skill.editTitle": "Edit skill",
  "skill.editHint": "{count} agents use this skill — changes take effect on save",
  "skill.deleteTitle": 'Delete skill "{name}"',
  "skill.deleteBound":
    "{count} agents bind this skill; those bindings will be removed too. This cannot be undone.",
  "skill.editAction": "Edit skill",
  "skill.deleteAction": "Delete skill",

  // Skill package upload (SKILL.md or zip)
  "skill.upload": "Upload package",
  "skill.uploadTitle": "Upload skill package",
  "skill.uploadDesc": "Accepts a SKILL.md or a zip. Files bundled in the zip (scripts/, references/, …) are kept and written into the container so the agent can read them.",
  "skill.uploadPick": "Choose file",
  "skill.uploadDrop": "Drop a file here, or click to choose",
  "skill.uploadHint": "Accepts .md and .zip, up to {size}",
  "skill.uploadParsing": "Parsing…",
  "skill.uploadBadType": "Only .md and .zip files are supported",
  "skill.uploadTooLarge": "File exceeds the {size} limit",
  "skill.uploadEmpty": "Please choose a file first",
  "skill.previewTitle": "Parsed result",
  "skill.previewDesc": "Check the details below before creating. Name and description come from the SKILL.md frontmatter.",
  "skill.previewFiles": "Bundled files ({count})",
  "skill.previewNoFiles": "No bundled files",
  "skill.previewBody": "Body preview",
  "skill.filesCount": "{count} file(s)",
  "skill.bindTitle": "Bind workspace skills",
  "skill.bindDesc":
    "Bound by reference, not copied — this agent picks up the new version as soon as the skill changes",
  "skill.manageLibrary": "Manage skills",
  "skill.bindLabel": "Bind skill {name}",
  "skill.bindCount": "{bound} of {total} bound (Ark allows 20 per agent)",
  "skill.noneInWorkspace": "No skills in this workspace",
  "skill.noneInWorkspaceDesc":
    "Skills are workspace assets — create one in the library first, then come back to bind it.",
  "skill.goToLibrary": "Go to skills",

  "skill.pickerOpen": "Pick from library",
  "skill.pickerTitle": "Pick skills from the library",
  "skill.pickerDesc":
    "Bound by reference, not copied — this agent picks up the new version as soon as the skill changes. You can change this later on the agent's detail page.",
  "skill.pickerConfirm": "Done",
  "skill.pickerSearch": "Search by name or description",
  "skill.pickerCount": "{count} of {max} selected",
  "skill.pickerAtLimit": "Limit reached — uncheck one to pick another.",
  "skill.pickerNoMatch": "No matching skills",
  "skill.pickerSummaryNone": "No workspace skills bound yet",
  "skill.pickerSummary": "{count} workspace skills bound",
  "skill.pickerSummaryMore": "and {count} more",
  "skill.unbindLabel": "Unbind {name}",
  "skill.pickerSection": "Workspace skills",
  "skill.pickerSectionDesc":
    "Skills built or uploaded in this workspace — none checked by default",
  "skill.pickerEmptyTitle": "No skills in the library yet",
  "skill.pickerEmptyDesc":
    "Create or upload one in the skills library, then come back to bind it.",

  // Skill detail page
  "skill.detailOpen": "View details for {name}",
  "skill.backToLibrary": "Skills",
  "skill.notFound": "Skill not found",
  "skill.notFoundDesc": "It may have been deleted. Head back to see what's left.",
  "skill.filesTitle": "Skill files",
  "skill.bodyFileName": "SKILL.md",
  "skill.bodyFileTag": "Body",
  "skill.filesNone": "No attachments",
  "skill.filesNoneDesc": "Just the SKILL.md body. Upload a zip to include attachments.",
  "skill.bodyEmpty": "The body is maintained by the platform and isn't shown here.",
  "skill.fileBinary": "Binary file — no preview",
  "skill.fileBinaryDesc": "It's written into the container as-is, so the agent can read it.",
  "skill.fileTruncated": "Large file — showing the first 256 KiB.",
  "skill.fileLoadFailed": "Couldn't load this file",
  "skill.injectHint":
    "Skill files are written into the container when a session starts; conversations already running won't pick up changes.",
  "skill.readOnlyBuiltin": "Built-in skill — its body is part of the platform and can't be edited.",
  "skill.readOnlyForkHint": "To build on it, make a copy first.",
  // Editing skill files
  "skill.saveFile": "Save {name}",
  "skill.savedFile": "Saved",
  "skill.savedNeedsNewSession":
    "Saved. Conversations already running keep the old version — the new content takes effect in your next session.",
  "skill.saveFileFailed": "Couldn't save",
  "skill.fileNotEditable": "This file can't be edited",
  "skill.fileBinaryNotEditable": "Binary files are view-only here.",
  "skill.unsavedTitle": "Unsaved changes",
  "skill.unsavedHint": "Save them, or discard before switching to another file.",
  "skill.discard": "Discard changes",
  "skill.bodyEditLabel": "Skill body (SKILL.md)",
  "skill.fileEditLabel": "File content",

  // Issues
  "issue.title": "Issues",
  "issue.listDesc":
    "Track problems in this workspace: describe what happened, attach screenshots, discuss below, close when resolved",
  "issue.new": "New issue",
  "issue.emptyTitle": "No issues yet",
  "issue.emptyDesc":
    "Log anything that blocks you. A title or a body is enough — attach images and files, and others can follow up in the thread.",
  "issue.createTitle": "New issue",
  "issue.editTitle": "Edit issue",
  "issue.fieldTitle": "Title",
  "issue.fieldTitlePlaceholder": "One line describing the problem",
  "issue.fieldTitleDesc": "Leave empty to derive it from the first line of the body",
  "issue.fieldBody": "Body",
  "issue.fieldBodyPlaceholder":
    "Steps to reproduce, expected result, actual result. Markdown supported — paste screenshots directly",
  "issue.fieldBodyDesc": "Markdown supported. Paste or drop images right in",
  "issue.requireTitleOrBody": "Please fill in a title or a body",
  "issue.fieldStatus": "Status",
  "issue.fieldPriority": "Priority",
  "issue.deleteTitle": 'Delete issue "{name}"',
  "issue.deleteDesc": "Its comments and attachments will be deleted too. This cannot be undone.",
  "issue.deleteAction": "Delete issue",
  "issue.editAction": "Edit issue",
  "issue.authorLabel": "Created by",
  "issue.commentAuthorLabel": "Comment by",
  "issue.createdAt": "Created {time}",
  "issue.updatedAt": "Updated {time}",
  "issue.filterAll": "All statuses",
  "issue.countComments": "{count} comments",
  "issue.countAttachments": "{count} attachments",
  "issue.noBody": "(no body)",
  "issue.backToList": "Back to issues",

  "issue.comments": "Discussion",
  "issue.commentPlaceholder": "Add context, share what you found\u2026",
  "issue.commentSubmit": "Comment",
  "issue.commentEmpty": "No discussion yet",
  "issue.commentRequired": "Comment cannot be empty",
  "issue.commentEdited": "edited",
  "issue.commentEditAction": "Edit comment",
  "issue.commentDeleteAction": "Delete comment",
  "issue.commentDeleteTitle": "Delete this comment",
  "issue.commentDeleteDesc": "This cannot be undone.",
  "issue.commentSave": "Save",
  "issue.commentCancel": "Cancel",

  "issue.attachments": "Attachments",
  "issue.attachmentDrop": "Drop an image or file here, or click to choose",
  "issue.attachmentHint": "Images, pdf, txt/md/csv/json and zip. Up to {size} each",
  "issue.attachmentUploading": "Uploading\u2026",
  "issue.attachmentBadType": "Unsupported file type",
  "issue.attachmentTooLarge": "Attachment exceeds the {size} limit",
  "issue.attachmentTooMany": "An issue can have at most {count} attachments",
  "issue.attachmentDeleteAction": "Delete attachment",
  "issue.attachmentInsert": "Insert into body",
  "issue.attachmentPasted": "Image uploaded and inserted into the body",
  "issue.attachmentNone": "No attachments yet",

  // Product suggestions (platform-level).
  "suggestion.title": "Suggestions",
  "suggestion.desc": "Your ideas about AMA itself. Only you and the platform team can see these",
  "suggestion.descStaff": "All user suggestions. You're on the platform team — you can see everyone's, reply, and change status",
  "suggestion.create": "New suggestion",
  "suggestion.createDesc": "Tell us what you'd like AMA to become. Body alone is fine — the title is derived from the first line",
  "suggestion.submit": "Submit",
  "suggestion.view": "View",
  "suggestion.fieldTitle": "Title",
  "suggestion.fieldBody": "Details",
  "suggestion.fieldStatus": "Status",
  "suggestion.titlePlaceholder": "One line on what you want (optional)",
  "suggestion.bodyPlaceholder": "What happened, and what you'd like instead. Markdown supported",
  "suggestion.titleHint": "Fill in a title or details. You can attach screenshots after submitting",
  "suggestion.emptyTitle": "No suggestions yet",
  "suggestion.emptyDesc": "Something awkward to use, or a feature you want? Say it here — the platform team reads these",
  "suggestion.footnote": "Only the platform team can see your suggestions. Other users cannot",
  "suggestion.loadFailed": "Failed to load suggestions, please try again",
  "suggestion.deleteTitle": "Delete this suggestion",
  "suggestion.deleteDesc": "\u201c{title}\u201d and all its replies and attachments will be deleted. This cannot be undone.",
  "suggestion.attachments": "Screenshots & attachments",
  "suggestion.addImage": "Add image",
  "suggestion.uploading": "Uploading\u2026",
  "suggestion.replies": "Conversation",
  "suggestion.noReplies": "No replies yet",
  "suggestion.staffBadge": "Platform team",
  "suggestion.edited": "edited",
  "suggestion.replyAsStaff": "Reply as platform team",
  "suggestion.replyAsUser": "Follow up",
  "suggestion.replyPlaceholder": "Write something\u2026",
  "suggestion.replySubmit": "Send",
  "enum.suggestionStatus.open": "Open",
  "enum.suggestionStatus.resolved": "Resolved",
  "enum.issueStatus.open": "Open",
  "enum.issueStatus.in_progress": "In progress",
  "enum.issueStatus.closed": "Closed",
  "enum.issuePriority.p0": "P0",
  "enum.issuePriority.p1": "P1",
  "enum.issuePriority.p2": "P2",

  // ⚠️ One Feishu app binds exactly one agent (1:1, see core.Service.ensureFeishuAppsFree).
  // This used to say "one app can bind multiple agents" — true before 2029b02, and it
  // contradicted feishu.bindDesc right below it.
  "feishu.listDesc":
    "Once connected, agents can be triggered from Feishu; one app binds exactly one agent",
  "feishu.connect": "Connect app",
  "feishu.emptyTitle": "Feishu is not connected yet",
  "feishu.emptyDesc":
    "Authorize once in Feishu and the platform creates the app and obtains its credentials for you — no App ID or secret to type.",
  "feishu.copyUrl": "Copy callback URL",
  "feishu.copied": "Copied",
  "feishu.copy": "Copy",
  "feishu.boundAgents": "Bound agents",
  // Unbound prompt. Pick ONE — the binding is 1:1, so never phrase it as multi-select.
  "feishu.bindPromptHint":
    "No agent is handling it yet. Pick one and it starts replying in Feishu.",
  "feishu.bindPickAgent": "Select an agent",
  "feishu.bindPromptAction": "Bind agent",
  "feishu.bindNoAgents": "This workspace has no agents yet — create one first.",
  "feishu.nextStepTitle": "Next: hand it to an agent",
  "feishu.boundToAgent": '"{name}" is handling it — it can send and receive in Feishu now.',
  "feishu.connectedAt": "Connected {time}",
  "feishu.deleteAction": "Disconnect app",
  "feishu.deleteTitle": 'Disconnect app "{name}"',
  "feishu.deleteBound":
    "{count} agent binds this app; the binding will be removed and that agent will stop receiving Feishu messages.",
  "feishu.deleteDesc":
    "The callback URL stops working immediately. The app itself is not deleted from Feishu, so you can reconnect it later.",
  // "Feishu app" here means the app entity on Feishu's side, not this site's nav
  // section — see the zh-CN comment on this block.
  "feishu.bindTitle": "Bind a Feishu app",
  "feishu.bindDesc":
    "Messages this app receives will trigger the agent; one app can bind only one agent",
  "feishu.manageApps": "Manage Feishu apps",
  "feishu.bindLabel": "Bind {name}",
  "feishu.stateBound": "Bound",
  "feishu.stateUnbound": "Not bound",
  // Apps held by another agent are hidden entirely on the agent detail page —
  // see the zh-CN comment on this block. The 409 on save is the real guard.
  "feishu.allTakenTitle": "No Feishu app available to bind",
  "feishu.allTakenDesc":
    "All {count} Feishu apps in this workspace are already handled by other agents. One app binds exactly one agent — review the bindings on the Feishu page, or connect another app.",
  "feishu.noneInWorkspace": "Feishu is not connected in this workspace",
  "feishu.noneInWorkspaceDesc": "Connect an app first, then come back to bind it to an agent.",
  "feishu.goToApps": "Connect an app",

  // Trigger mode — the option labels must spell out the behavioral difference;
  // see the zh-CN comment on this block.
  "feishu.triggerTitle": "When to reply",
  "feishu.triggerDesc":
    "Controls when the bot speaks up in group chats. Direct messages are unaffected — it always replies.",
  "feishu.triggerLabel": "When {name} replies",
  "enum.feishuTrigger.mention_only": "Only when I'm @-mentioned",
  "enum.feishuTrigger.topic_and_mention": "Auto-reply to new topics, @ elsewhere",
  "agent.followUpTitle": "When you send another message mid-reply",
  "enum.followUp.redirect": "Redirect (interrupt the current turn)",
  "enum.followUp.queue": "Queue (wait for the current turn)",
  "agent.followUpHint.redirect":
    "Immediately interrupts the in-progress reply (tools are stopped mid-run) and handles the new message instead.",
  "agent.followUpHint.queue":
    "Waits for the current turn to finish, then handles the new message. The bot replies that the previous one is still running.",
  "agent.progressDetailTitle": "Show detailed work when replying",
  "enum.progressDetail.full": "Show (thinking and tool calls)",
  "enum.progressDetail.brief": "Hide (final answer only)",
  "agent.progressDetailHint.full":
    "Adds a chain-of-thought message showing what the agent is thinking and which tools it calls, live. Useful when you need to follow along or debug.",
  "agent.progressDetailHint.brief":
    "No chain-of-thought message appears — only the acknowledgement reaction and the final answer. Suited to end users who don't need the process.",
  "feishu.triggerHint.mention_only": "The bot must be @-mentioned in every group chat.",
  "feishu.triggerHint.topic_and_mention":
    "Starting a new topic needs no @; follow-up replies inside that topic, and all other groups, still require one.",

  // Missing group-message scope. The copy must state the consequence, not just
  // name the permission — see the zh side for why.
  "feishu.groupScopeMissing": "Feishu hasn't granted the group-message permission — this setting won't take effect",
  "feishu.groupScopeMissingHint":
    "Without it, Feishu never delivers group messages that don't @-mention the bot, so the bot never sees them and stays silent. Enable the permission in the Open Platform, publish a new app version, then re-check here.",
  "feishu.grantOnOpenPlatform": "Grant in Feishu",
  // Missing include-bot scope. Kept separate from the one above on purpose:
  // Feishu splits group-message delivery into two tiers, and the first tier
  // excludes bot-authored messages entirely.
  "feishu.botScopeMissing":
    "Feishu hasn't granted the bot-message permission — messages sent by bots never arrive",
  "feishu.botScopeMissingHint":
    "The group-message permission only covers messages from people. Without this second tier, Feishu delivers nothing that other bots send — alert and monitoring broadcasts included — so a room where only bots talk stays completely silent. Enable the permission in the Open Platform, publish a new app version, then re-check here.",
  "feishu.requestGroupMessage": "Also request the group-message permission (needed for topic-group auto-reply)",
  "feishu.requestGroupMessageHint":
    "A sensitive permission requiring admin approval. The bot-message tier is requested alongside it — without that tier, alerts posted by bots never arrive. You can connect without it, but the bot will only see group messages that @-mention it — adding it later means re-authorizing.",

  // Device-flow onboarding
  "feishu.connectTitle": "Connect a Feishu app",
  "feishu.connectDesc":
    "Authorize once in Feishu; the platform creates the app and obtains its credentials",
  "feishu.backToList": "Back to list",
  "feishu.step1Title": "Authorize in Feishu",
  "feishu.step1Desc":
    "This opens Feishu's authorization page. Once you approve, the platform creates the app and obtains its credentials — you never type a secret.",
  "feishu.openAuthPage": "Open Feishu authorization",
  "feishu.reopenAuthPage": "Didn't open? Click here to reopen the authorization page",
  "feishu.step2Title": "Waiting for authorization",
  "feishu.step2Waiting": 'Waiting for you to approve in Feishu — this page continues automatically.',
  "feishu.step3Title": "Connected",
  "feishu.finish": "Done",
  "feishu.connectAnother": "Connect another",
  "feishu.retry": "Start over",
  "feishu.failed.expired": "The authorization link expired. Please start over.",
  "feishu.failed.denied": "Authorization was denied. If that wasn't you, please start over.",
  "feishu.failed.invalid": "Authorization failed. Please start over.",
  "feishu.failed.pending": "Authorization is not complete yet.",
  "feishu.failed.completed": "Already connected.",
  "feishu.whyTitle": "Why there's no App ID or secret to type",
  "feishu.whyDesc":
    "Feishu's open platform supports device-flow self-registration: one authorization lets the platform create the app and obtain its credentials. The secret only lives briefly on the server — it is never sent back to the browser or shown to anyone.",

  // Readiness diagnostics
  "feishu.readinessTitle": "Readiness",
  "feishu.readinessDesc": "Connected isn't the same as working. Here's what's still missing",
  "feishu.readinessUnchecked": "Not checked",
  "feishu.recheck": "Re-check",
  "feishu.checkedAt": "Checked {time}",
  "feishu.readyHint": "Everything is ready — agents can send and receive Feishu messages.",
  "feishu.missingScopes": "{count} permissions missing",
  "feishu.missingScopesHint":
    "These have not been requested yet. Add them under Permissions on the Feishu open platform.",
  "feishu.unapprovedScopes": "{count} permissions awaiting admin approval",
  "feishu.unapprovedScopesHint":
    "These have been requested and need your tenant administrator to approve them.",
  "feishu.unapprovedRequestedHint":
    "The request is already with your administrator — just wait for approval, no need to resubmit.",
  "feishu.requestAdmin": "Request admin approval",
  "feishu.missingEvents": "{count} event subscriptions missing",
  "feishu.missingEventsHint":
    "Add these under Events & callbacks on the Feishu open platform, otherwise messages never arrive.",
  "feishu.missingCallbacks": "{count} callback subscriptions missing",
  "feishu.missingCallbacksHint":
    "Without the card callback, buttons on message cards do nothing when clicked.",
  "feishu.botDisabled": "Bot capability is not enabled",
  "feishu.botDisabledHint": "Enable the bot under App capabilities on the Feishu open platform.",
  "feishu.notPublished": "No published version",
  "feishu.notPublishedHint": "The app needs a published, approved version before it can be used.",
  "feishu.runtimeUnavailable": "Cannot reach this app",
  "feishu.runtimeUnavailableHint":
    "Its credential may have expired. Reconnect the app to obtain a fresh one.",
  "feishu.notInspectable": "Cannot read the app's full configuration",
  "feishu.notInspectableHint":
    "Without the app self-management permission, event and version status cannot be confirmed, so the findings above may be incomplete.",

  // Chat
  "chat.title": "Chat",
  "chat.subtitle": "Talk to an agent and watch it work in real time",
  "chat.new": "New chat",
  "chat.recent": "Recent",
  "chat.untitled": "New chat",
  "chat.pickAgentTitle": "Pick an agent to start",
  "chat.pickAgentDesc": "Chats are recorded in run history, so you can come back to them anytime.",
  "chat.pickAgent": "Select agent",
  "chat.placeholder": "Ask anything…",
  "chat.send": "Send",
  "chat.interrupt": "Stop",
  "chat.thinking": "Thinking",
  "chat.hint": "Enter to send, Shift + Enter for a new line",
  "chat.emptyTitle": "No chats yet",
  "chat.emptyDesc": "Pick an agent and start the first conversation.",
  "chat.sendFailed": "Failed to send",
  "chat.sendFailedUnknown": "The message didn't go through. Please try again.",
  "chat.noAgents": "No agents in this workspace yet",
  "chat.noAgentsDesc": "You need an agent before you can start chatting.",
  "chat.createAgent": "Create agent",
  "chat.viewRun": "View run details",
  "chat.startedWith": "Answered by {name}",
  "chat.frozenTitle": "The linked agent was deleted, so this chat is frozen",
  "chat.frozenHint":
    "Past messages are still readable. To keep chatting, create a new agent and start a new chat.",
  "chat.frozenDisabledTitle": "The linked agent is disabled, so this chat is frozen",
  "chat.frozenDisabledHint":
    "Past messages are still readable. Set the agent back to Active on its detail page to keep chatting.",
  "chat.goneTitle": "This chat no longer exists",
  "chat.goneHint":
    "It may have been cleaned up, or this link belongs to another workspace. Refreshing will not bring it back — pick a chat on the left or start a new one.",
  "chat.noArkTitle": "This workspace has no Ark API key yet",
  "chat.noArkHint":
    "You need one before you can chat. The whole workspace shares a single key, so this is a one-time setup.",
  "chat.noArkAction": "Open workspace settings",
  "chat.noArkInheritTitle": "The global Ark API key this workspace references no longer exists",
  "chat.noArkInheritHint":
    "Reconfigure it in global settings, or switch this workspace to its own key in workspace settings.",
  "chat.noArkInheritAction": "Open global settings",
  "chat.noArkNeedAdmin":
    "Configuring the Ark API key requires workspace admin permission. Please ask a workspace admin or owner.",
  "chat.queueHint":
    "The agent is still replying. {count} message(s) queued — they'll be sent when this turn ends.",
  "chat.queuePosition": "{index}/{total}",
  "chat.queueEdit": "Edit",
  "chat.queueSendNow": "Send now",
  "chat.queueSendNowHint": "Interrupt the agent's current turn and handle this message instead",
  "chat.redirectFailed": "Send now failed",
  "chat.queueFull":
    "{limit} messages are already queued, so this one was not sent. Its text is back in the input box — cancel a few first.",
  "chat.queueTimeout":
    "This turn has been running for 3 minutes, so this message was not sent. Its text is back in the input box — try again later.",

  // Run history
  "run.listDescAll": "Runs and event streams for every agent in this workspace",
  "run.listDescAgent": 'Showing runs of "{name}"',
  "run.showAllAgents": "Show all agents",
  "run.searchLabel": "Search runs",
  "run.searchPlaceholder": "Search title, agent or ID",
  "run.filterByAgent": "Filter by agent",
  "run.allAgents": "All agents",
  "run.emptyNoMatch": "No matching runs",
  "run.emptyNoMatchDesc": "Try adjusting the filters.",
  "run.emptyTitle": "No runs yet",
  "run.emptyDesc":
    "Once an agent is triggered by Feishu, the API or manually, runs and their full event streams appear here.",
  "run.colDuration": "Duration",
  "run.colTokens": "Tokens",
  "run.colEvents": "Events",
  "run.colStarted": "Started",
  "run.threadCount": "{count} threads",
  "run.statusFailed": "Failed",
  "run.loadingDetail": "Loading run",
  "run.notFound": "Run not found",
  "run.notFoundDesc": "This run may have been deleted.",
  "run.backToList": "Back to run history",
  "run.viewAgent": "View agent",
  "run.statTokenTotal": "Total tokens",
  "run.statTokenHint": "in {input} · out {output}",
  "run.statCacheHit": "Cache hits",
  "run.statCacheHint": "written {value}",
  "run.statModelReq": "Model requests",
  "run.statToolCalls": "Tool calls",
  "run.statToolErrors": "{count} failed",
  "run.statErrors": "Errors",
  "run.statOutcome": "Outcome",
  "run.statOutcomeRounds": "{count} rounds",
  "run.statEvents": "Events",
  "run.statRawEvents": "{count} raw",
  "run.statMergedSteps": "{count} merged steps",
  "run.startedAt": "Started at",
  "run.endedAt": "Ended at",
  "run.inProgress": "In progress",
  "run.sessionId": "Session ID",
  "run.threads": "Threads",
  "run.eventStream": "Event stream",
  "run.eventShown": "showing {shown} / {total}",
  "run.recentRuns": "{count} recent runs",
  "run.viewInHistory": "View in run history",
  "run.agentNoRuns": "No runs for this agent yet",
  "run.agentNoRunsDesc": "Runs appear here once it is triggered by Feishu, the API or manually.",
  "run.outcomeSatisfied": "Satisfied",

  // Usage & insights
  "usage.title": "Usage & Insights",
  "usage.subtitle": "Who spends, who slows down, who fails",
  "usage.emptyTitle": "No usage data yet",
  "usage.emptyDesc":
    "Once your agents have run, you'll see call volume, latency distribution and token trends here.",
  "usage.statRuns": "Runs",
  "usage.statRunsHint": "{count} completed",
  "usage.statSuccessRate": "Success rate",
  "usage.statSuccessHint": "{failed} failed",
  "usage.statTokens": "Total tokens",
  "usage.statTokensHint": "in {input} · out {output}",
  "usage.statAvgDuration": "Avg. duration",
  "usage.statAvgHint": "P50 {p50} · P90 {p90}",
  "usage.chartCallsTitle": "Calls per agent",
  "usage.chartCallsDesc": "Sorted by run count",
  "usage.chartLatencyTitle": "Latency distribution",
  "usage.chartLatencyDesc": "Completed runs only · log scale",
  "usage.chartTokensTitle": "Token consumption trend",
  "usage.chartTokensDesc": "Stacked height is the total",
  "usage.chartOutcomeTitle": "Successful / failed runs",
  "usage.chartOutcomeDesc": "A run counts as failed when it carries an error summary",
  "usage.legendInput": "Input",
  "usage.legendOutput": "Output",
  "usage.legendTotal": "Total",
  "usage.legendP50": "P50",
  "usage.legendP90": "P90",
  "usage.legendAvg": "Avg",
  "usage.succeeded": "Succeeded",
  "usage.failed": "Failed",
  "usage.runsUnit": "{count} runs",
  "usage.noLatencyData": "No completed runs yet, so there's no latency to report",
  "usage.sampleSize": "n = {count}",
  "usage.tableAgent": "Agent",
  "usage.tableRuns": "Calls",
  "usage.tableFailed": "Failed",
  "usage.tableTokens": "Tokens",

  "timeline.rawPayload": "Raw payload",
  "timeline.expand": "Expand",
  "timeline.collapse": "Collapse",
  "timeline.waitingResult": "Waiting for result",
  "timeline.execFailed": "Execution failed",
  "timeline.execResult": "Result",
  "timeline.emptyResult": "(empty)",
  "timeline.noEvents": "No events match the current filters",
  "timeline.needsConfirm": "Needs your approval",
  "timeline.awaitingYou": "Waiting for you to approve before it runs",
  "timeline.executing": "Running",
  "timeline.executingHint":
    "The tool is running in AMA's own container. Ark is idle right now (stop_reason=requires_action) — that means it handed control back and is waiting for results, not waiting on you",
  "timeline.approved": "Approved",
  "timeline.denied": "Denied",
  "timeline.denyReason": "Reason: {msg}",
  "timeline.pending": "Queued",
  "timeline.pendingHint": "Queued — it runs after the preceding events are processed",
  "timeline.threadLabel": "Thread",

  "timeline.command": "Command",
  "timeline.input": "Input",
  "timeline.exitCode": "exit {code}",
  "timeline.exitCodeHint": "Non-zero is not always a failure — grep exits 1 when nothing matches",
  "timeline.notExited": "not exited",
  "timeline.stdout": "stdout",
  "timeline.stderr": "stderr",
  "timeline.sandboxError": "Container error",
  "timeline.sandboxErrorHint":
    "The command never reached the container — different from the command itself failing",
  "timeline.durationHint":
    "Time from the call to its receipt. Includes queueing and container resume, so it is not the command's execution time.",

  // Ark console deep links — see the zh-CN comment on this block.
  "arkConsole.openAgent": "View this agent in the Ark console",
  "arkConsole.openSkill": "View this skill in the Ark console",
  "arkConsole.openSession": "View this session in the Ark console",
  "arkConsole.agentNotCreated":
    "This agent does not exist on Ark yet — it is created when you first start a conversation, after which a link to the Ark console will appear here.",
  "arkConsole.skillNotCreated":
    "This skill has not been uploaded to Ark yet — it is uploaded once an agent that uses it starts a conversation, after which a link to the Ark console will appear here.",

  // Trace panel — see the zh-CN comment on this block for why the three states
  // must read differently.
  "trace.title": "Call tree",
  "trace.durationHint":
    "Span tree: the run is the root, with model requests and tool calls under it. Click a node for details. Durations are derived wall-clock time and include queueing and container resume.",
  "trace.noSpans": "This run has not produced any model request or tool call yet.",
  "trace.spanCount": "Nodes",
  "trace.totalDuration": "Total",
  "trace.durationUnknown": "unknown",
  "trace.durationUnknownHint":
    "The receipt has not arrived, or the event is queued but unprocessed — it cannot be computed, and it is not 0",
  "trace.command": "Command",
  "trace.spanRoot": "This run",
  "trace.spanModel": "Model request",
  "trace.sortBySequence": "By order",
  "trace.sortByDuration": "By duration",
  "trace.fieldStatus": "Status",
  "trace.fieldStatusCode": "Error",
  "trace.fieldSpanId": "Span ID",
  "trace.fieldType": "Type",
  "trace.fieldLatency": "Latency",
  "trace.fieldStartTime": "Start time",
  "trace.fieldTokens": "Tokens",
  "trace.fieldInput": "Input",
  "trace.fieldOutput": "Output",
  "trace.statusSuccess": "Success",
  "trace.statusError": "Error",
  "trace.tokensBreakdown": "Input {input} / output {output}",
  "trace.concurrentHint": "Overlaps in time with {n} sibling calls (running concurrently)",
  "trace.axisLabel": "Timeline",
  "trace.fieldEndTime": "End time",
  "trace.fieldOffset": "From start",
  "trace.barRange": "{start} → {end} ({dur})",
  "trace.spanHint": "span",
  "trace.collapse": "Collapse",
  "trace.expand": "Expand",
  "trace.hasError": "has failures",
  "timeline.slow": "slow",
  "timeline.truncated": "Truncated",
  "timeline.truncatedHint": "Output exceeded 64 KiB and was truncated server-side",
  "timeline.rawJson": "Raw JSON",
  "timeline.copy": "Copy",
  "timeline.copied": "Copied",
  "timeline.copyFailed": "Copy failed",
  // ---- Events view (raw event list, see components/RawEventList.tsx) ----
  "evt.detailTitle": "Event detail",
  "evt.pickOne": "Pick an event on the left to see its detail",
  "evt.eventId": "Event ID",
  "evt.sequence": "Sequence",
  "evt.processedAt": "Processed at",
  "evt.thread": "Thread",
  "evt.body": "Body",
  "evt.emptyContent": "(no content)",
  "evt.thinkingBody": "Thinking",
  "evt.thinkingEmpty": "(thinking, no body)",
  "evt.thinkingEmptyHint":
    "This thinking event carries no body. Ark may send the signal without content \u2014 not a rendering bug.",
  "evt.interruptReason": "Interrupt reason",
  "evt.interruptNoReason": "(no reason given)",
  "evt.toolName": "Tool",
  "evt.toolUseId": "Tool use ID",
  "evt.fallbackId": "(the event's own ID \u2014 Ark did not send tool_use_id)",
  "evt.permission": "Permission",
  "evt.mcpServer": "MCP server",
  "evt.denyMessage": "Deny reason",
  "evt.agentName": "Agent",
  "evt.parentThread": "Parent thread",
  "evt.noParentThread": "(none \u2014 this is the main thread)",
  "evt.toAgent": "To",
  "evt.toThread": "To thread",
  "evt.fromAgent": "From",
  "evt.fromThread": "From thread",
  "evt.mainAgent": "Main agent",
  "evt.compactedSummary": "Compaction summary",
  "evt.compactedNoSummary": "(context compacted, no summary given)",
  "evt.sessionName": "Session name",
  "evt.stopReason": "Stop reason",
  "evt.requiresActionHint":
    "Under self_hosted this is NOT \u201cwaiting for the user\u201d: Ark handed control back to AMA, the tool is running in our own container, and Ark is waiting for the worker to post user.tool_result.",
  "evt.pendingEventIds": "Waiting on: {ids}",
  "evt.noExtraFields": "This event type carries no fields beyond the common envelope.",
  "evt.errorMessage": "Error message",
  "evt.noErrorMessage": "(no error message given)",
  "evt.retry_retrying": "Retrying",
  "evt.retry_retries_exhausted": "Retries exhausted",
  "evt.retry_terminal": "Not retryable",
  "evt.credentialId": "Credential ID",
  "evt.modelStartHint":
    "Start of a model request. Duration and usage live on the paired span.model_request_end.",
  "evt.startEventId": "Start event ID",
  "evt.duration": "Duration",
  "evt.usage": "Token usage",
  "evt.usageBrief": "in {in} \u00b7 out {out}",
  "evt.inputTokens": "Input",
  "evt.outputTokens": "Output",
  "evt.cacheRead": "Cache read",
  "evt.cacheWrite": "Cache write",
  "evt.speed": "Speed",
  "evt.outcomeId": "Outcome ID",
  "evt.iterationLabel": "Iteration",
  "evt.iteration": "Iteration {n}",
  "evt.maxIterations": "Max iterations",
  "evt.description": "Description",
  "evt.rubric": "Rubric",
  "evt.explanation": "Explanation",
  "evt.unknownType":
    "The frontend does not know this event type yet; the raw JSON below is complete.",
  "evt.threadCreatedMain": "{name} (main thread)",
  "evt.threadCreatedSub": "{name} (sub-thread)",
  // ---- Human-readable names for all 34 event types ----
  "evtType.user.message": "User message",
  "evtType.system.message": "System prompt",
  "evtType.user.interrupt": "User interrupt",
  "evtType.user.tool_confirmation": "Tool confirmation",
  "evtType.user.tool_result": "Tool result",
  "evtType.user.custom_tool_result": "Custom tool result",
  "evtType.user.define_outcome": "Define outcome",
  "evtType.agent.message": "Agent message",
  "evtType.agent.thinking": "Thinking",
  "evtType.agent.tool_use": "Tool use",
  "evtType.agent.tool_result": "Tool result (agent)",
  "evtType.agent.mcp_tool_use": "MCP tool use",
  "evtType.agent.mcp_tool_result": "MCP tool result",
  "evtType.agent.custom_tool_use": "Custom tool use",
  "evtType.agent.thread_message_sent": "Thread message sent",
  "evtType.agent.thread_message_received": "Thread message received",
  "evtType.agent.thread_context_compacted": "Context compacted",
  "evtType.session.status_running": "Session running",
  "evtType.session.status_idle": "Session idle",
  "evtType.session.status_rescheduled": "Session rescheduled",
  "evtType.session.status_terminated": "Session terminated",
  "evtType.session.error": "Session error",
  "evtType.session.updated": "Session updated",
  "evtType.session.deleted": "Session deleted",
  "evtType.session.thread_created": "Thread created",
  "evtType.session.thread_status_running": "Thread running",
  "evtType.session.thread_status_idle": "Thread idle",
  "evtType.session.thread_status_rescheduled": "Thread rescheduled",
  "evtType.session.thread_status_terminated": "Thread terminated",
  "evtType.span.model_request_start": "Model request start",
  "evtType.span.model_request_end": "Model request end",
  "evtType.span.outcome_evaluation_start": "Outcome eval start",
  "evtType.span.outcome_evaluation_ongoing": "Outcome eval ongoing",
  "evtType.span.outcome_evaluation_end": "Outcome eval end",

  "filter.searchLabel": "Search events",
  "filter.searchPlaceholder": "Search args, output and raw JSON",
  "filter.domain": "Domain",
  "filter.title": "Filter",
  "filter.category": "Category",
  "filter.derived": "Derived",
  "filter.chipHidden": "Hidden: {name}",
  "filter.removeCondition": "Remove condition: {name}",
  "filter.preset": "Quick filters",
  "filter.type": "Event type",
  "filter.typeLabel": "Filter by specific event type",
  "filter.typeAll": "All types",
  "filter.typeSelected": "{count} selected",
  "filter.slowOnly": "Slow calls only (>{seconds}s)",
  "filter.slowOnlyHint":
    "Measured from the call to its receipt — includes queueing and container resume from pause (16–29s on first wake), so it is not the command's execution time.",
  "filter.errorsOnly": "Failures only",
  "filter.reset": "Reset filters",
  "filter.presetTool": "Tools",
  "filter.presetModel": "Model",
  "filter.presetError": "Errors",
  "filter.presetSystem": "System",
  "filter.presetSystemHint":
    "Session and thread status transitions. Environment prep and container lifecycle emit no events — they only appear in server logs.",
  "filter.noMatch": "No matching events",
  "filter.noMatchDesc": "Adjust the filters or clear the keyword.",

  "view.conversation": "Conversation",
  "view.events": "Events",
  "view.trace": "Call tree",
  "view.files": "Files",
  "view.terminal": "Terminal",
  "view.switchLabel": "View mode",

  "sandbox.title": "Container",
  "sandbox.none.title": "No container used in this conversation yet",
  "sandbox.none.desc": "A container is created only when the agent actually calls a tool (runs a command, reads or writes files).",
  "sandbox.running": "Running",
  "sandbox.paused": "Paused",
  "sandbox.gone": "Reclaimed",
  "sandbox.paused.title": "Container paused",
  "sandbox.paused.desc": "The container pauses or stops after 60 seconds idle to release compute. The workspace is kept and resumes where it left off.",
  "sandbox.gone.title": "Container reclaimed",
  "sandbox.gone.desc": "The container and its workspace sat idle too long and were reclaimed; their files are gone. The next tool call creates a new one.",
  "sandbox.resuming": "Resuming\u2026",
  "sandbox.autoResuming": "The container pauses when idle to release compute. Resuming for you \u2014 usually a few seconds; the workspace is unaffected.",
  "sandbox.sessionId": "Container ID",
  "sandbox.files": "Files",
  "sandbox.filesEmpty": "This directory is empty",
  "sandbox.filesTruncated": "Too many files; showing the first {count}",
  "sandbox.parentDir": "Parent directory",
  "sandbox.fileClose": "Close preview",
  "sandbox.fileBinary": "Binary file \u2014 preview not supported.",
  "sandbox.fileTruncated": "File is large; showing the first 256 KB.",
  "sandbox.fileLoadFailed": "Failed to read the file",
  "sandbox.readOnly": "Read-only",
  "sandbox.loadFailed": "Failed to read container status",
  "sandbox.terminal": "Terminal",
  "sandbox.openTerminal": "Open terminal",
  "sandbox.terminalClosed": "Terminal closed",
  "sandbox.terminalConnecting": "Connecting\u2026",
  "sandbox.terminalConnected": "Connected",
  "sandbox.terminalDisconnected": "Disconnected",
  "sandbox.terminalRecycled":
    "This session's container has been reclaimed and can no longer be entered. Containers are released once a session ends.",
  "sandbox.terminalNeedsRunning": "The container must be running to open a terminal.",
  "sandbox.terminalReconnect": "Reconnect",
  "sandbox.terminalReconnected":
    "Connection re-established (this is a new shell — history and working directory have been reset)",
  "sandbox.terminalViaFallback": "Compatibility mode",
  "sandbox.terminalViaFallbackHint":
    "The WebSocket channel is unavailable, so the terminal fell back to compatibility mode. It still works, but echo may lag or arrive out of order when you type quickly.",
  "sandbox.terminalFullscreen": "Fill the window",
  "sandbox.terminalExitFullscreen": "Exit fill",
  "sandbox.terminalOpenInNewTab": "Open terminal in a new tab",
  "turn.steps": "{count} steps",
  "turn.toolCalls": "{count} tool calls",
  "turn.toolErrors": "{count} failed",
  "turn.running": "Working",
  "turn.earlierSteps": "{count} earlier steps",
  "turn.noProcess": "No intermediate steps",

  "settings.title": "Workspace settings",
  "settings.desc": "Name, key and members",
  "settings.tabGeneral": "General",
  "settings.tabMembers": "Members",
  "settings.tabDanger": "Danger zone",
  "settings.currentUrl": "Currently at /w/{key}",
  "settings.keyChangeWarn":
    "Changing the key breaks every link pointing at /w/{old}; you will be redirected to /w/{next}.",
  "settings.membersDesc": "{count} people can access this workspace",
  "settings.invite": "Invite member",
  "settings.inviteTitle": "Invite member",
  "settings.inviteAction": "Invite",
  "settings.memberName": "Name",
  "settings.memberNameRequired": "Name is required",
  "settings.memberEmail": "Email",
  "settings.memberEmailRequired": "Email is required",
  "settings.memberEmailInvalid": "Invalid email address",
  "settings.memberRole": "Role",
  "settings.roleOf": "Role of {name}",
  "settings.joinedAt": "joined {time}",
  "settings.removeMember": "Remove {name}",
  "settings.removeMemberTitle": 'Remove "{name}"',
  "settings.removeMemberDesc": "They will lose access to this workspace.",
  "settings.cannotRemoveOwner": "Cannot remove the only owner",
  "settings.deleteWorkspace": "Delete workspace",
  "settings.deleteWorkspaceDesc":
    "Agents, skills, Feishu apps and run history in this workspace are all deleted",
  "settings.deleteWorkspaceWarn": "This cannot be undone — the data is not recoverable.",
  "settings.deleteWorkspaceTitle": 'Delete workspace "{name}"',
  "settings.deleteWorkspaceConfirm":
    "Permanently deletes this workspace and all its agents, skills, Feishu apps and run history. This cannot be undone.",
  "settings.saveFailed": "Failed to save",
  "settings.tabHosts": "Hosts",
  "settings.tabArk": "Ark credentials",
  "settings.wsName": "Workspace name",
  "settings.wsNameRequired": "Workspace name is required",
  "settings.wsKey": "Workspace key",
  "settings.wsDesc": "Description",
  "settings.keyTaken": 'Key "{key}" is already taken',
  "settings.membersSectionDesc": "Invite people by their sign-in email and pick a role for each",
  "settings.membersEmpty": "No members yet",
  "settings.memberEmailNotValid": "Enter a valid email address",
  "settings.memberEmailHint": "The email they use to sign in to AMA",
  "settings.memberDisplayName": "Name",
  "settings.memberNamePlaceholder": "Optional; defaults to the part of the email before @",
  "settings.removeMemberTooltip": "Remove member",
  "settings.arkLoadFailed": "Failed to load Ark credentials. Please try again later.",
  "settings.arkUseGlobalFailed": "Failed to use the global config",
  "settings.arkApiKey": "Ark API Key",
  "settings.arkApiKeyDesc":
    "Agents and sessions in this workspace call Ark with this key. It is workspace-level: every member shares it, so replacing it affects everyone",
  "settings.arkUseGlobal": "Use global config",
  "settings.arkSwitchToCustom": "Use a workspace key",
  "settings.arkReplace": "Replace",
  "settings.arkInheritPrefix": "Using the API Key ",
  "settings.arkInheritSource": "from global settings",
  "settings.arkInheritSuffix": "",
  "settings.arkInheritFrom": " ({name})",
  "settings.arkInheritHint": ". Replace it there and this workspace follows immediately.",
  "settings.arkGoToGlobal": "Go to global settings",
  "settings.arkDanglingPrefix":
    "The global API Key this workspace references no longer exists. Go to ",
  "settings.arkDanglingLink": "global settings",
  "settings.arkDanglingSuffix": " to set it up again, or enter a key for this workspace below.",
  "settings.arkStatus": "Status",
  "settings.arkCheckedAt": "verified {time}",
  "settings.arkRegion": "Region",
  "settings.arkRegionInherited": "The region follows global settings; change it there",
  "settings.arkUpdatedAt": "updated {time}",
  "settings.arkClear": "Clear",
  "settings.arkVerify": "Test connection",
  "settings.arkEmptyTitle": "No Ark API Key configured yet",
  "settings.arkEmptyDesc":
    "Agents in this workspace need it to call Ark. Use the one from your global settings, or set a separate key for this workspace.",
  "settings.arkSetup": "Enter API Key",
  "settings.arkReplaceTitle": "Replace API Key",
  "settings.arkSetupTitle": "Enter Ark API Key",
  "settings.arkModalDesc": "After saving, only a masked key is shown",
  "settings.arkKeyPlaceholder": "Copy it from the Ark console",
  "settings.arkClearTitle": "Clear Ark API Key",
  "settings.arkClearDesc":
    "Agents in this workspace won't be able to call Ark until a key is configured again. This cannot be undone.",

  // Integrations. Card titles, summaries and fix hints all come from the backend —
  // see the zh-CN comment on this block for why they are not translated here.
  "integrations.title": "Integrations",
  "integrations.desc": "Let the platform hold credentials so AMA can call external services as you",
  // See the zh-CN block: after the move to Global Settings the first two lines are
  // confirmation, but scopePeers is the one the page's location cannot answer.
  "integrations.scopeTitle": "These authorizations belong to your account and apply to every workspace",
  "integrations.scopeBody":
    "In any workspace, your agents use this same authorization to call external services — switching workspaces does not require authorizing again.",
  "integrations.scopePeers":
    "Nobody else can see your authorization status, and you cannot see theirs.",
  "integrations.checkedAt": "status as of {time}",
  "integrations.authorize": "Authorize",
  "integrations.reauthorize": "Re-authorize",
  "integrations.revoke": "Revoke",
  "integrations.refresh": "Re-check",
  "integrations.openConsole": "Open console",
  "integrations.openedTitle": "Authorization page opened in a new tab",
  "integrations.openedHint": "Come back here and hit Re-check when you are done",
  "integrations.authorizeFailed": "Could not start authorization",
  "integrations.pollingHint":
    "Once you tap Approve in Feishu, this card updates automatically \u2014 no need to refresh",
  "integrations.authorizeTimeout":
    "Timed out waiting for authorization. If you have not approved it in Feishu yet, hit Authorize and try again.",
  "integrations.revokeFailed": "Cannot revoke",
  "integrations.loadFailed": "Could not load authorization status. Please try again.",
  // Deliberately does NOT say "agents will use this app": injection picks the app by
  // the run's workspace and ignores this preference (see the comment above
  // GetLarkUserAppPref in server/core/store.go).
  "integrations.appLabel": "App in use",
  "integrations.appManageHint": "To add or remove Feishu apps (bots), go to the workspace sidebar entry",
  "integrations.appManageHintNoLink":
    "To add or remove Feishu apps (bots), open any workspace and use the Feishu entry in its sidebar \u2014 apps are a per-workspace resource.",
  "integrations.appSwitchFailed": "Could not switch app",
  "integrations.appAuthorized": "Authorized",
  "integrations.appUnauthorized": "Not authorized",
  "integrations.appLoadFailed": "Could not load available apps",
  "integrations.noneTitle": "No integrations available",
  "integrations.noneDesc":
    "The server has no integrations wired up yet. Cards will appear here once it does.",
  "integrations.whyTitle": "Why this step exists",
  "integrations.whyBody":
    "Container tools authorize by caller identity. Using the service's own app account means granting access resource by resource; using your authorization runs as you, so whatever you can access works.",

  // Summary row + collapsible cards. Three buckets, not two: `missing` is the
  // server not having wired the source up, so it is the admin's job — folding it
  // into "needs you" would send users after something they cannot do.
  "integrations.summaryReady": "{count} ready",
  "integrations.summaryTodo": "{count} need you",
  "integrations.summaryMissing": "{count} not set up",
  "integrations.summaryAllDone": "Nothing needs your attention",
  "integrations.summaryTodoHint": "First one expanded for you; click a title to open others",
  "integrations.summaryMissingHint":
    "Sources that are not set up need an admin; the authorize button will not help",
  "integrations.expandAll": "Expand all",
  "integrations.collapseAll": "Collapse all",
  "integrations.groupPersonalTitle": "Personal authorizations",
  "integrations.groupPersonalDesc":
    "Access platforms as yourself. Only your own agents can use these, and they cannot be shared with a workspace.",
  "integrations.groupServiceTitle": "Service accounts",
  "integrations.groupServiceDesc":
    "Access platforms as an application rather than a person. Admins can add these to a workspace for its agents to share.",

  // NOT two states — see the zh-CN comment. "Not set up" needs an admin to configure
  // it (the authorize button does nothing for the user); "Unavailable" needs someone
  // to act.
  "enum.integrationStatus.ok": "Ready",
  "enum.integrationStatus.warning": "Needs attention",
  "enum.integrationStatus.error": "Unavailable",
  "enum.integrationStatus.missing": "Not set up",

  // Pick an existing Ark API Key from a Volcengine account — see the zh-CN comment
  // for why this wording is functional rather than decorative.
  "arkFetch.title": "Pick from a Volcengine account",
  "arkFetch.recommended": "Recommended",
  "arkFetch.intro":
    "Lists the Ark API Keys that already exist under your Volcengine accounts — pick one instead of copying it from the console.",
  "arkFetch.plaintextNote": "The plaintext never touches your browser",
  "arkFetch.plaintextBody": "The server fetches and encrypts it directly.",

  "arkFetch.sourceLabel": "Key source",
  "arkFetch.sourceFetch": "Pick an existing key from Volcengine",
  "arkFetch.sourceManual": "Enter it manually",
  "arkFetch.sourceLockedHint": "Automatic lookup is unavailable — manual entry only",
  "arkFetch.pickRequired": "Pick an API Key above first",
  "arkFetch.keyToggle": "Show API Key list",
  "arkFetch.keyHint": "When names look alike, go by creation time",
  "arkFetch.step1": "Step 1",
  "arkFetch.step2": "Step 2",
  "arkFetch.accountLabel": "Volcengine account",
  "arkFetch.accountPlaceholder": "Search by name or ID",
  "arkFetch.accountToggle": "Show account list",
  "arkFetch.accountCount": "{count} accounts",
  "arkFetch.accountNoMatch": "No matching Volcengine account",
  "arkFetch.accountEmpty": "No authorized Volcengine account found for you.",
  "arkFetch.accountLoading": "Loading Volcengine accounts…",

  "arkFetch.keyLabel": "API Key",
  "arkFetch.keyWaiting": "Pick a Volcengine account first — its API Keys will show up here",
  "arkFetch.keyLoading": "Loading API Keys…",
  "arkFetch.keySearchPlaceholder": "Search by name or creation date",
  "arkFetch.keySearchLabel": "Search API Keys",
  // Names are near-identical (api-key-2026082…), so the timestamp is the only
  // thing that tells them apart. Say so.
  "arkFetch.keyCount": "{count} keys — when names look alike, go by creation time",
  "arkFetch.keyFiltered": "{shown}/{total} keys",
  "arkFetch.keyNoMatch": "No matching API Key — try another keyword",
  "arkFetch.keyEmptyTitle": "No Ark API Key under this account yet",
  "arkFetch.keyEmptyDesc": "Create one in the Ark console, or switch to another Volcengine account.",
  "arkFetch.keyCreatedAt": "created {time}",
  "arkFetch.keyCreatedUnknown": "creation time unknown",
  "arkFetch.keyLastUsed": "used {time}",
  "arkFetch.keyProject": "project {name}",
  "arkFetch.keyCreatedBy": "created by {name}",
  "arkFetch.keyCreatedByMe": "created by me",
  "arkFetch.keyRetry": "Retry",

  "arkFetch.pickedTitle": "This one is selected",
  "arkFetch.pickedHint": "You can ignore the manual field below.",
  "arkFetch.useManual": "Enter it manually instead",

  // NOT two states — "Not set up" needs an admin; offering Authorize there only
  // yields a 422 the user cannot act on. See the zh-CN comment.
  "arkFetch.authorizeTitle": "Want to pick an existing key straight from Volcengine?",
  "arkFetch.authorizeBody":
    "Authorize AMA to access Volcengine on your behalf and your keys will be listed here.",
  "arkFetch.authorizeAction": "Authorize",
  "arkFetch.unavailableTitle": "Automatic lookup is unavailable",
  "arkFetch.unavailableBody":
    "The server has no Volcengine integration yet, or cannot reach it right now. Entering the key manually still works.",
  "arkFetch.unavailableHint":
    "An admin has to configure this on the server first — you can enter the key manually for now.",
  "arkFetch.openConsole": "Open the Ark console",
  "arkFetch.loadFailed": "Could not load",

  // The cascade warning has to appear here too — the user is deep in a modal by
  // the time they press Save. Kept consistent with GlobalArkTab.
  "arkFetch.cascadeGlobal":
    "Once saved, every workspace referencing this global configuration switches to the new key immediately.",

  // Ark credential cards and ArkKeyPicker — see the zh-CN comment
  "arkCred.replace": "Replace",
  "arkCred.status": "Status",
  "arkCred.region": "Region",

  "arkKeyPicker.keyPlaceholder": "Paste an API Key created in the Ark console",
  "arkKeyPicker.consoleBefore": "Create one on the ",
  "arkKeyPicker.consoleLink": "Ark API Key management",
  "arkKeyPicker.consoleAfter": " page. Only a masked value is shown after saving.",

  "arkNotActivated.title": "This account hasn't activated Managed Agents",
  "arkNotActivated.hint":
    "Come back and save again after activating. You can also save the key now — no need to change anything once activation is done.",
  "arkNotActivated.saveAnyway": "Save anyway",
  "arkNotActivated.activate": "Activate",

  "agentArk.loadFailed": "Failed to load the agent's Ark credential, please try again later",
  "agentArk.actionFailed": "Action failed, please try again later",
  "agentArk.title": "Ark credential",
  "agentArk.description":
    "Which Ark API Key this agent uses. It follows workspace settings by default, or you can give it its own — handy when business lines need separate billing.",
  "agentArk.useWorkspace": "Use workspace settings",
  "agentArk.setup": "Use a separate key",
  "agentArk.sourceAgentBefore": "Using ",
  "agentArk.sourceAgentStrong": "this agent's own",
  "agentArk.sourceAgentAfter": " API Key, independent of workspace settings.",
  "agentArk.inheritGlobalBefore": "Following workspace settings, which reference the API Key from ",
  "agentArk.inheritGlobalStrong": "global settings",
  "agentArk.ownerEmail": " ({email})",
  "agentArk.inheritGlobalAfter": "",
  "agentArk.inheritWorkspaceBefore": "Using the API Key ",
  "agentArk.inheritWorkspaceStrong": "from workspace settings",
  "agentArk.inheritWorkspaceAfter": "",
  "agentArk.inheritTail": ". Change it there and this agent switches right away.",
  "agentArk.missingBefore": "No usable Ark API Key yet, so this agent can't chat. Add one in ",
  "agentArk.missingLink": "workspace settings",
  "agentArk.missingAfter": " (shared by the whole workspace), or give this agent its own here.",
  "agentArk.effectiveKey": "Active API Key",
  "agentArk.verify": "Verify",
  "agentArk.updated": "Last updated",
  "agentArk.pickerReplaceTitle": "Replace this agent's API Key",
  "agentArk.pickerSetupTitle": "Give this agent its own API Key",

  "globalArk.loadFailed": "Failed to load the global Ark credential, please try again later",
  "globalArk.title": "Global Ark API Key",
  "globalArk.description":
    "Set it once and any workspace can reference it. Replacing it switches every referencing workspace to the new key right away",
  "globalArk.checkedAt": "verified {time}",
  "globalArk.regionHint": "To change the region, click “Replace” and enter the key again",
  "globalArk.updatedAt": "Updated {time}",
  "globalArk.clear": "Clear",
  "globalArk.verify": "Test connection",
  "globalArk.emptyTitle": "No global Ark API Key yet",
  "globalArk.emptyDesc":
    "Once set, workspaces can reference it directly instead of pasting a key into each one.",
  "globalArk.setup": "Add API Key",
  "globalArk.modalReplaceTitle": "Replace global API Key",
  "globalArk.modalSetupTitle": "Add global Ark API Key",
  "globalArk.modalDesc":
    "After saving, only a masked value is shown. Every referencing workspace switches to the new key right away",
  "globalArk.keyPlaceholder": "Copy it from the Ark console",
  "globalArk.clearTitle": "Clear global Ark API Key",
  "globalArk.clearDesc":
    "After clearing, every workspace referencing it can no longer call Ark until you set it again or switch them to their own key. This cannot be undone.",

  "apiKeys.loadFailed": "Failed to load API Keys, please try again later",
  "apiKeys.description":
    "For external agents, scripts, or CI to call this platform's API. It belongs to you and has exactly your permissions",
  "apiKeys.create": "Create API Key",
  "apiKeys.emptyTitle": "No API Keys yet",
  "apiKeys.emptyDesc":
    "Create a key so external agents can call this platform's API. API docs: /api/openapi.json",
  "apiKeys.lastUsed": "used {time}",
  "apiKeys.neverUsed": "Never used",
  "apiKeys.revoke": "Revoke",
  "apiKeys.usageLabel": "Usage: ",
  "apiKeys.usageDocs": ". API docs:",
  "apiKeys.usageLlms": ", guide for agents:",
  "apiKeys.usageEnd": ".",
  "apiKeys.createDesc":
    "Give it a name so you can tell it apart in the list later. It has exactly your permissions.",
  "apiKeys.name": "Name",
  "apiKeys.namePlaceholder": "e.g. ci-deploy, my local script",
  "apiKeys.createdTitle": "API Key created",
  "apiKeys.savedIt": "I've saved it",
  "apiKeys.onceBefore": "Copy it now — ",
  "apiKeys.onceStrong": "you won't be able to see it again after closing this dialog",
  "apiKeys.onceAfter":
    ". The platform only stores its hash; if lost, you can only revoke it and create a new one.",
  "apiKeys.revokeTitle": "Revoke “{name}”",
  "apiKeys.revokeDesc": "Calls using this key will fail immediately after revoking. This cannot be undone.",

  "prefs.title": "Global Settings",
  // The point is ownership (account, not workspace), not cross-device sync —
  // see the zh-CN comment.
  "prefs.desc": "These settings belong to your account, not to any workspace",
  "prefs.language": "Language",
  "prefs.languageDesc": "Interface language",
  "prefs.appearance": "Appearance",
  "prefs.appearanceDesc": "Light, dark, or follow the system",
  "prefs.appearanceLight": "Light",
  "prefs.appearanceDark": "Dark",
  "prefs.appearanceSystem": "Follow system",
  "prefs.appearanceSystemHint": "System is currently {mode}",
  "prefs.backToWorkspaces": "Back to workspaces",
  "prefs.backToWorkspace": "Back to {name}",

  "prefs.tabGeneral": "Appearance & Language",
  "prefs.tabApiKeys": "API Keys",
  "prefs.tabSuggestions": "Suggestions",
  "prefs.tabArk": "Ark Credential",
  "prefs.tabIntegrations": "Integrations",

  // Onboarding (app/OnboardingTour.tsx)
  "onboarding.title": "Getting started",
  "onboarding.skip": "Skip",
  "onboarding.prev": "Back",
  "onboarding.next": "Next",
  "onboarding.done": "Get started",
  "onboarding.progress": "{current}/{total}",
  "onboarding.step1Title": "Workspaces contain everything",
  "onboarding.step1Body":
    "Agents, skills, Feishu apps, and run history all belong to a workspace. So step one " +
    "is to get into one — create a workspace, or ask an admin to add you to an existing one.",
  "onboarding.step2Title": "Create an agent and pick a model",
  "onboarding.step2Body":
    "Agents do the work: give one a prompt and a model (Doubao / DeepSeek / Zhipu) and it " +
    "can run tasks in a container. A workspace can hold several agents, each with its own job.",
  "onboarding.step3Title": "Extend agents with skills",
  "onboarding.step3Body":
    "A skill is a playbook that teaches an agent how to handle one kind of task. Write it " +
    "inline or upload a SKILL.md or a whole zip; then tick it on any agent to enable it.",
  "onboarding.step4Title": "Every chat is a run",
  "onboarding.step4Body":
    "Pick an agent under Chat and start talking — replies stream in token by token. Each " +
    "conversation IS a run record, so it lands in run history and you can resume it later.",
  "onboarding.step5Title": "Connect Feishu to work in group chats",
  "onboarding.step5Body":
    "Scan to authorize a Feishu app — no credentials to fill in by hand. Note that one " +
    "Feishu app binds to exactly one agent, so create several apps for several bots.",
  "onboarding.step6Title": "Review in run history, track spend in usage",
  "onboarding.step6Body":
    "Run history expands into the full event timeline of each execution (tool calls and " +
    "retries included), while Usage answers who is spending, slowing down, or failing over time.",
  "onboarding.replay": "Getting started",
  "onboarding.replayDesc": "Walk through the core concepts and first steps again",
  "onboarding.replayAction": "Replay the tour",

  // Issue feedback. The neverCollected line is a promise; changing what is collected must change it too (see the comment in the Chinese table).
  "feedback.report": "Report this problem",
  "feedback.sent": "Received, thank you",
  "feedback.viewPayload": "See what will be sent",
  "feedback.payloadHint":
    "Pressing “Report this problem” sends the following. No typing required:",
  "feedback.neverCollected":
    "Never sent: login credentials, cookies, API keys, API response bodies, " +
    "or anything you said in a conversation.",
  "feedback.fieldRoute": "Page",
  "feedback.fieldStatus": "Status code",
  "feedback.fieldCode": "Error code",
  "feedback.fieldMessage": "Error message",
  "feedback.fieldBrowser": "Browser",
  "feedback.fieldVersion": "Frontend version",
  "feedback.fieldWorkspace": "Workspace",
  "feedback.fieldAPI": "Failing endpoint",
  "feedback.fieldPath": "Full path",
  "feedback.fieldAgent": "Current agent",
  "feedback.fieldLogID": "Server log ID",
  "feedback.fieldRecent": "Recent failed API calls",
  "feedback.fieldConsole": "Recent console errors",

  "tl.userMessage": "User message",
  "tl.injectedContext": "Platform-injected context",
  "tl.injectedGuidance": "Guidance for the agent",
  "tl.injectedFrom": "Source",
  "tl.systemPrompt": "Runtime system prompt",
  "tl.agentReply": "Agent reply",
  "tl.thinking": "Thinking",
  "tl.thoughtDone": "Thought about it",
  "tl.thinkingSummary": "No reasoning text returned for this turn",
  "tl.userInterrupt": "User interrupt",
  "tl.userInterruptSummary": "The user stopped this run",
  "tl.compacted": "Context compacted",
  "tl.compactedSummary": "Context exceeded the limit and was summarized",
  "tl.threadCreated": "Thread created",
  "tl.threadMain": " (main)",
  "tl.threadSub": " (sub)",
  "tl.threadRunning": "Thread running",
  "tl.threadIdle": "Thread idle",
  "tl.threadHandoff": "Thread handed control back",
  "tl.threadRescheduled": "Thread rescheduled",
  "tl.threadTerminated": "Thread terminated",
  "tl.sessionRunning": "Session running",
  "tl.sessionIdle": "Session idle",
  "tl.sessionHandoff": "Session handed control back",
  "tl.sessionRescheduled": "Session rescheduled",
  "tl.sessionRescheduledSummary": "Recovered from a transient error and requeued",
  "tl.sessionTerminated": "Session terminated",
  "tl.sessionRenamed": "Session renamed",
  "tl.sessionDeleted": "Session deleted",
  "tl.sessionDeletedSummary": "Event stream ended",
  "tl.modelStart": "Model request started",
  "tl.modelEnd": "Model request finished",
  "tl.defineOutcome": "Outcome defined",
  "tl.outcomeOngoing": "Outcome evaluation running",
  "tl.outcomeStart": "Outcome evaluation started (round {n})",
  "tl.outcomeEnd": "Outcome: {result}",
  "tl.round": "round {n}",
  "tl.sentTo": "Sent to {name}",
  "tl.receivedFrom": "Received from {name}",
  "tl.mainThread": "main thread",
  "tl.callTool": "Called {name}",
  "tl.callMcp": "MCP call {server} / {name}",
  "tl.callCustom": "Custom tool {name}",
  "tl.stopEndTurn": "Ended normally",
  // NOT "Waiting for approval" — see the zh-CN comment on this key. Under
  // self_hosted, requires_action means Ark handed control back to our worker and
  // is waiting for user.tool_result. Nobody is waiting on the user.
  "tl.stopRequiresAction": "Waiting for tool results",
  "tl.stopRetriesExhausted": "Retries exhausted",
  "tl.stopUserInterrupt": "Interrupted by user",
  "tl.stopPending": " ({n} running)",
  "tl.retrying": "Retrying",
  "tl.retriesExhausted": "Retries exhausted",
  "tl.terminal": "Unrecoverable",
  "tl.retryStatus": "Retry status: {status}",
  "tl.mcpServer": "MCP server: {name}",
  "tl.vault": "Vault: {id}",
  "tl.credential": "Credential: {id}",
  "tl.usageSummary": "in {input} / out {output} / cache {cache}",
  "tl.outcomeSatisfied": "Satisfied",
  "tl.outcomeNeedsRevision": "Needs revision",
  "tl.outcomeMaxIter": "Max iterations reached",
  "tl.outcomeFailed": "Failed",
  "tl.outcomeInterrupted": "Interrupted",
  "tl.outcomeGoal": "Goal: {desc}",
  "tl.outcomeRubric": "Rubric:\n{content}",
  "tl.outcomeRubricFile": "Rubric file: {id}",
  "tl.outcomeMaxIterations": "Max iterations: {n}",

  "enum.role.owner": "Owner",
  "enum.role.admin": "Admin",
  "enum.role.member": "Member",
  "enum.role.viewer": "Viewer",

  "enum.effort.none": "Off",
  "enum.effort.minimal": "Minimal",
  "enum.effort.low": "Low",
  "enum.effort.medium": "Medium",
  "enum.effort.high": "High",
  "enum.effort.xhigh": "Very high",
  "enum.effort.max": "Max",

  "enum.agentStatus.active": "Active",
  "enum.agentStatus.disabled": "Disabled",

  "enum.feishuStatus.unverified": "Unverified",
  "enum.feishuStatus.connected": "Connected",
  "enum.feishuStatus.error": "Error",
  "enum.feishuReadiness.setup_required": "Setup required",
  "enum.feishuReadiness.waiting_for_admin": "Awaiting admin",
  "enum.feishuReadiness.ready": "Ready",
  "enum.feishuReadiness.runtime_unavailable": "Unreachable",

  "enum.runStatus.running": "Running",
  "enum.runStatus.idle": "Idle",
  "enum.runStatus.requires_action": "Running tools",
  "enum.runStatus.rescheduling": "Rescheduling",
  "enum.runStatus.terminated": "Ended",

  // Schedules
  "nav.schedules": "Schedules",
  "sched.listDesc": "Let agents act on their own: cron schedules, fixed intervals, or one-off reminders. Every trigger leaves a run record",
  "sched.new": "New schedule",
  "sched.newDesc": "When it fires, the agent runs once. Results can stay in run history or be posted back to a Feishu chat",
  "sched.editTitle": "Edit schedule",
  // -- Entity revision history (undo a mistake) ------------------------------
  "skill.tabFiles": "Files",
  "revision.tab": "History",
  "revision.emptyTitle": "No history yet",
  "revision.emptyDesc": "Once you change the settings here, the previous state is saved automatically so you can roll back a mistake.",
  "revision.maxKept": "Keeps the {n} most recent versions; older ones are cleaned up automatically.",
  "revision.showDetail": "View this version",
  "revision.hideDetail": "Collapse",
  "revision.rollback": "Roll back to this version",
  "revision.changedBy": "Changed by",
  "revision.reasonRollback": "From a rollback",
  "revision.rollbackTitle": "Roll back to version {no}?",
  "revision.rollbackDesc": "The current content will be replaced by this version. The current state is saved as a new history entry first, so you can undo this rollback too.",
  "revision.skillFilesNote": "Rolling back restores the skill body and description only, not attached files.",
  "revision.snapshotUnavailable": "The content of this version could not be loaded.",

  "sched.emptyTitle": "No schedules yet",
  "sched.emptyDesc": "Try one: summarize alerts every weekday at 9:30 and post to a Feishu chat, or \"remind me in an hour to check the release\".",
  "sched.needAgentTitle": "Create an agent first",
  "sched.needAgentDesc": "A schedule needs an agent to run it, so create one first.",
  "sched.name": "Name",
  "sched.namePlaceholder": "Daily alert digest",
  "sched.nameRequired": "Name is required",
  "sched.agent": "Run with",
  "sched.kind": "Trigger",
  "sched.kindCron": "Cron expression",
  "sched.kindInterval": "Fixed interval",
  "sched.kindOneshot": "One-off",
  "sched.cron": "Cron expression",
  "sched.cronDesc": "Five fields: minute hour day month weekday. Evaluated in {tz}",
  "sched.cronRequired": "Cron expression is required",
  "sched.presetWorkdayMorning": "Weekdays 9:30",
  "sched.presetDaily": "Daily 9:00",
  "sched.presetHourly": "Hourly",
  "sched.presetMonday": "Mondays 10:00",
  "sched.interval": "Interval",
  "sched.everyMinutes": "Every {count} min",
  "sched.delay": "Fire after",
  "sched.afterMinutes": "In {count} min",
  "sched.prompt": "What to do",
  "sched.promptPlaceholder": "Summarize yesterday's alerts by severity and list the ones needing human attention",
  "sched.promptDesc": "Write it as an instruction to the agent. Each trigger is a fresh session, so make it self-contained",
  "sched.promptRequired": "Please describe what the agent should do",
  "sched.delivery": "Deliver results to",
  "sched.deliveryRunOnly": "Run history only",
  "sched.deliveryFeishu": "Feishu chat",
  "sched.chat": "Feishu chat",
  "sched.chatSearch": "Search by chat name",
  "sched.chatNoMatch": "No matching chats",
  "sched.chatRequired": "Please pick a chat",
  "sched.noChatsTitle": "No chats available yet",
  "sched.noChatsDesc": "Chats are remembered when the bot receives a message. Add the bot to a chat and @-mention it once, then it will show up here.",
  "sched.nextRun": "Next run",
  "sched.notScheduled": "Not scheduled",
  "sched.running": "Running",
  "sched.runCount": "Runs / fails",
  "sched.lastRun": "Last run",
  "sched.view": "View",
  "sched.runNow": "Run once now",
  "sched.runNowQueued": "\"{name}\" is queued and will start shortly",
  "sched.paused": "Paused",
  "sched.toggleLabel": "Enable or pause {name}",
  "sched.lastError": "Last error: {error}",
  "sched.deleteTitle": "Delete schedule \"{name}\"",
  "sched.deleteDesc": "It will stop firing. Existing run records are kept. This cannot be undone.",

  // Detail page
  "sched.loading": "Loading schedule",
  "sched.notFound": "This schedule does not exist",
  "sched.notFoundDesc":
    "It may have been deleted. One-off schedules delete themselves after firing \u2014 that is normal.",
  "sched.backToList": "Back to schedules",
  "sched.openDetail": "View details",
  "sched.tabConfig": "Configuration",
  "sched.tabHistory": "Run history",
  "sched.statusActive": "Active",
  "sched.enabled": "Enabled",
  "sched.lastRunAt": "Last run",
  "sched.never": "Never run",
  "sched.countRuns": "Total runs",
  "sched.countFails": "Total failures",
  "sched.countConsecutive": "Consecutive failures",
  "sched.times": "{count}",
  "sched.autoPauseHint": "{left} more failure(s) will auto-pause it",
  "sched.autoPaused":
    "Auto-paused after consecutive failures. Enabling it again resets the failure streak",
  "sched.lastErrorTitle": "Last run failed",
  "sched.agentDeletedTitle": "The bound agent was deleted",
  "sched.agentDeletedDesc":
    "The bound agent was deleted, so this schedule cannot produce results. Bind another agent.",
  "sched.oneshotTitle": "This is a one-off schedule",
  "sched.oneshotDesc":
    "It is deleted automatically once it fires (success or failure), so seeing it here means it has not fired yet.",
  "sched.tzHint": "Times below are in {tz}",
  "sched.saveConfig": "Save configuration",
  "sched.savedNeedsNothing": "Saved. The next trigger uses the new configuration",
  "sched.historyEmpty": "No runs yet",
  "sched.historyEmptyDesc": "Once it fires, every round shows up here as a run record.",
  "sched.historyGap":
    "{count} earlier run(s) cannot be listed \u2014 back then the triggering schedule was not recorded, and that cannot be backfilled",
  "sched.historyCount": "Showing {shown}, {total} run(s) total",
  "sched.runNowFromDetail": "Run once",

  "enum.trigger.manual": "Manual",
  "enum.trigger.feishu": "Feishu",
  "enum.trigger.api": "API",
  "enum.trigger.schedule": "Scheduled",
  "enum.trigger.webhook": "Webhook",
  "enum.trigger.weixin": "WeChat",
  "weixin.listDesc": "Scan a QR code to connect a WeChat bot; once it is bound to an agent, the agent answers messages sent to it in WeChat",
  "weixin.connect": "Connect with QR code",
  "weixin.emptyTitle": "No WeChat bot connected yet",
  "weixin.emptyDesc": "Scan with WeChat on your phone to talk to an agent right in WeChat.",
  "weixin.loginTitle": "Connect WeChat",
  "weixin.qrAlt": "WeChat login QR code",
  "weixin.verifyCode": "Number shown on the phone",
  "weixin.verifySubmit": "Submit",
  "weixin.retry": "Scan again",
  "weixin.close": "Close",
  "weixin.owner": "WeChat user: {user}",
  "weixin.boundAgent": "Answered by",
  "weixin.pickAgent": "Choose an agent",
  "weixin.noAgent": "No agent",
  "weixin.openAgent": "Open agent",
  "weixin.unboundHint": "No agent yet: the bot only replies with a hint.",
  "weixin.expiredHint": "The WeChat login has expired. Scan the QR code again.",
  "weixin.deleteAction": "Disconnect",
  "weixin.deleteTitle": "Disconnect this WeChat bot?",
  "weixin.deleteDesc": "It stops receiving and sending messages. To use it again, scan the QR code again.",
  "weixin.connectedAt": "Connected {time}",
  "weixin.lastInbound": "last message {time}",
  "enum.weixinLogin.waiting": "Waiting for scan",
  "enum.weixinLogin.scanned": "Scanned",
  "enum.weixinLogin.need_verify_code": "Verification needed",
  "enum.weixinLogin.confirmed": "Connected",
  "enum.weixinLogin.already_connected": "Already connected",
  "enum.weixinLogin.expired": "Expired",
  "enum.weixinLogin.failed": "Failed",
  "enum.weixinAccount.active": "Connected",
  "enum.weixinAccount.expired": "Scan again",

  "enum.category.conversation": "Conversation",
  "enum.category.thinking": "Thinking",
  "enum.category.tool": "Tool calls",
  "enum.category.thread": "Threads",
  "enum.category.status": "Status",
  "enum.category.trace": "Traces",
  "enum.category.error": "Errors",
  "enum.category.outcome": "Outcome",

  "enum.arkRegion.cn-beijing": "Volcengine · North China (Beijing)",
  "enum.arkRegion.ap-southeast-1": "BytePlus · Asia Pacific (Singapore)",
  "enum.arkCheck.unchecked": "Not verified",
  "enum.arkCheck.valid": "Valid",
  "enum.arkCheck.invalid": "Invalid",

  "domain.wsKeyRequired": "Workspace key is required",
  "domain.wsKeyTooShort": "At least 2 characters",
  "domain.wsKeyTooLong": "At most 32 characters",
  "domain.wsKeyInvalid": "Use lowercase letters, digits, and hyphens only, starting with a letter or digit",
  "domain.arkKeyRequired": "API key is required",
  "domain.arkKeyTooShort": "API key is too short. Make sure you copied all of it",
  "domain.arkKeyHasSpace": "API key must not contain spaces",
  "domain.apiKeyNameRequired": "Give this key a name",
  "domain.apiKeyNameTooLong": "Name must be at most 128 characters",
  "domain.envSourceAmaToken": "AMA platform credential",
  "domain.envSourceLarkUserToken": "Feishu user credential (lark-cli)",
  "domain.cronFieldCount": "Needs 5 fields: minute hour day month weekday. Weekdays at 9:30 is 30 9 * * 1-5",
  "domain.cronOutOfRange": "Field {field}: {value} is out of range {min}-{max}",

  "evt.blockImage": "[Image]",
  "evt.blockImageUrl": "[Image {url}]",
  "evt.blockDocument": "[Document]",
  "evt.blockDocumentTitled": "[Document {title}]",
  "evt.blockSearchResult": "[Search result {title}]",

  "sandbox.terminalDisconnectedWith": "Container terminal disconnected: {reason}",
  "sandbox.termReasonTicket": "Failed to get a terminal ticket",
  "sandbox.termReasonWsUrl": "WebSocket URL is unavailable",
  "sandbox.termReasonWsHandshake": "WebSocket handshake failed",
  "sandbox.termReasonNoOutput": "No output from the container",
  "sandbox.termReasonBadReadyFrame": "Could not parse the terminal handshake frame",
  "sandbox.termReasonClosed": "The container terminal was closed",
  "sandbox.termReasonConnect": "Could not connect to the container terminal",
  "sandbox.termReasonInput": "Keystrokes are not reaching the container",

  "wsGate.openFailed": "Could not open the workspace. Please try again later",
  "wsGate.backToList": "Back to my workspaces",
  "rolePerm.title": "What each role can do",
  "rolePerm.markAllowed": "{role}: allowed",
  "rolePerm.markDenied": "{role}: not allowed",
  "skill.bodyPlaceholder": "# Skill title\n\n## Steps\n\n1. ⋯",
  "skill.builtin.larkCli.title": "lark-cli (Feishu CLI)",
  "skill.builtin.larkCli.description":
    "Work with Feishu: messages, docs, Base, calendar, approvals, tasks, mail, meetings and more. Use it whenever you need to read or write Feishu data, instead of calling the OpenAPI directly.",
  "skill.builtin.ama.title": "ama (the AMA platform CLI)",
  "skill.builtin.ama.description":
    "Work with AMA's own data: workspaces, agents, skills, run history and issues. Use it when the user asks to file an issue, list agents or check run history. It acts as the current user, with exactly the same permissions as when they sign in.",
  "skill.builtin.tagBuiltin": "Built-in",
  "skill.builtin.tagFeishu": "Feishu",

  "enum.domain.user": "User",
  "enum.domain.agent": "Agent",
  "enum.domain.session": "Session",
  "enum.domain.span": "Span",
  "enum.domain.userHint": "Client-to-agent events; the dynamic system prompt belongs here too",
  "enum.domain.agentHint": "Agent-pushed events: replies, thinking signals, tool calls",
  "enum.domain.sessionHint": "Session and thread lifecycle, status and errors",
  "enum.domain.spanHint": "Execution spans: model requests and outcome evaluation",

  "enum.tool.bash": "Run shell commands in the sandbox",
  "enum.tool.read": "Read file contents",
  "enum.tool.write": "Write files",
  "enum.tool.edit": "Edit existing files",
  "enum.tool.glob": "Find files by glob pattern",
  "enum.tool.grep": "Search inside files",
  "enum.tool.web_fetch": "Fetch web pages",
  "enum.tool.web_search": "Search the web",

  // The landing page when not logged in (clicking the CTA enters the login flow below)
  "landing.slogan": "Enterprise agent hosting and execution",
  "landing.introBefore": "Build enterprise agents on ",
  "landing.introLink": "Ark Managed Agents",
  "landing.introAfter": ".",
  "landing.hostTitle": "Hosted agents",
  "landing.hostDesc": "Prompts, models and tools in one place. Changes apply at once, versions tracked.",
  "landing.collabTitle": "Multi-agent teamwork",
  "landing.collabDesc": "Agents and skills are scoped per space, so teams stay out of each other's way.",
  "landing.sandboxTitle": "Your own containers",
  "landing.sandboxDesc":
    "All six tools run in our sandbox on one shared filesystem, paused when idle.",
  "landing.feishuTitle": "Native Lark support",
  "landing.feishuDesc": "Scan once to register the app, then talk to your agent right in a group chat.",

  // Sign-in (email + password / OIDC)
  "login.title": "Sign in to AMA",
  "login.signupTitle": "Create an AMA account",
  "login.email": "Email",
  "login.password": "Password",
  "login.name": "Name (optional)",
  "login.passwordHint": "8–72 characters",
  "login.submit": "Sign in",
  "login.signupSubmit": "Sign up and sign in",
  "login.toSignup": "No account? Sign up",
  "login.toLogin": "Already have an account? Sign in",
  "login.firstUserHint": "No accounts exist yet. The first account to sign up becomes the platform administrator",
  "login.or": "or",
  "login.oidc": "Sign in with {name}",
  "login.noMethod": "No sign-in method is configured for this deployment. An administrator needs to configure auth.local or auth.oidc on the server.",
  "login.providersFailed": "Could not load sign-in options, please try again later",
  "login.retry": "Retry",
  "login.errInvalidCredentials": "Incorrect email or password",
  "login.errTooManyAttempts": "Too many attempts, please try again later",
  "login.errEmailTaken": "This email is already registered, please sign in instead",
  "login.errSignupDisabled": "Sign-up is not open on this platform, please ask an administrator for an account",
  "login.errWeakPassword": "Password must be 8–72 characters long",
  "login.errLocalDisabled": "Email and password sign-in is not enabled on this platform",
  "login.errInvalidArgument": "Please enter a valid email and password",
  "login.errGeneric": "Sign-in failed, please try again later",
  "login.errDenied": "You cancelled the sign-in, please sign in again",
  "login.errExpired": "This sign-in link is no longer valid, please sign in again",
  "login.errNoIdentity": "Could not determine your email, please contact an administrator",
  "login.errEmailUnverified": "Your email is not verified with the identity provider yet. Verify it, then sign in again",
  "login.errUnavailable": "The sign-in service is temporarily unavailable, please try again later",
  "login.logout": "Sign out",
  "login.github": "Sign in with {name}",
  "login.githubTitle": "Enter this code on GitHub",
  "login.githubHint": "Open GitHub's device activation page, enter the code below and authorize. This page signs you in automatically afterwards.",
  "login.githubCode": "Device code",
  "login.githubCopy": "Copy code",
  "login.githubOpen": "Open github.com/login/device",
  "login.githubWaiting": "Waiting for you to authorize on GitHub",
  "login.githubExpiresIn": "Expires in {time}",
  "login.githubCancel": "Cancel",
  "login.githubRestart": "Get a new code",
  "login.errGithubExpired": "This code is no longer valid, please get a new one",
  "login.errGithubDenied": "You cancelled the authorization on GitHub. Get a new code to try again",
  "login.errGithubNoEmail": "Your GitHub account has no verified primary email. Verify one and make it primary in GitHub's email settings, then sign in again",
  "login.errGithubNotAllowed": "This GitHub account is not allowed to sign in to this platform. Ask an administrator to add you to the allowlist",
  "login.errGithubDisabled": "GitHub sign-in is not enabled on this platform",
  "login.errGithubRetrying": "Can't reach GitHub right now, retrying",

  // Mobile (components/ChatSessions.tsx)
  "chat.mobileOpenSessions": "Chats",
  "chat.mobileSessionsTitle": "Recent",

  // Host executors (pages/HostsTab.tsx, WorkspaceHostsTab.tsx,
  // components/AgentRuntimeSections.tsx, SandboxPanel.tsx)
  "prefs.tabHosts": "Hosts",
  "hosts.title": "My registered hosts",
  "hosts.desc":
    "Agent tool calls run on these machines. Machines register with a daemon and belong to your account",
  "hosts.loadFailed": "Failed to load hosts, please try again later",
  "hosts.emptyTitle": "No hosts registered yet",
  "hosts.emptyDesc":
    "Follow the three steps below to install the daemon on your machine so agents can run on your computer.",
  "hosts.online": "Online",
  "hosts.offline": "Offline",
  "hosts.lastSeen": "last seen {time}",
  "hosts.gnuOk": "Command-line tools check passed",
  "hosts.gnuBad": "Command-line tools check failed",
  "hosts.gnuBadHint":
    "The agent tools depend on these command-line utilities. Fix them on the machine and try again.",
  "hosts.gnuBadMacHint":
    "Mac needs the GNU toolset first (the agent's bash/grep etc. depend on them):",
  "hosts.nativeOk": "daemon supports native tools (no brew dependencies)",
  "hosts.nativeBad": "daemon is out of date",
  "hosts.nativeBadHint":
    "The ama daemon on this machine does not support the native tools protocol. Upgrade the ama CLI on the machine and run ama daemon restart (the new version needs no brew install).",
  "hosts.delete": "Delete",
  "hosts.deleteTitle": "Delete \"{name}\"",
  "hosts.deleteBody":
    "The machine will be unbound from every workspace and its daemon will stop receiving tasks. Register again to use it later.",
  "hosts.guideTitle": "How to connect a host",
  "hosts.guideDesc":
    "Run these two steps on the machine you want to connect. If the ama command is already on your PATH, start at step 2.",
  "hosts.step1Title": "Install the AMA CLI",
  "hosts.step1Desc": "Build the AMA CLI from source (Go 1.24+) into $GOPATH/bin. Make sure it is on your PATH.",
  "hosts.step2Title": "Register and install as a service",
  "hosts.step2Desc":
    "One command registers this machine, installs it as a background service that starts on boot, and runs it immediately. The API key is used only once, for the first registration; create one in the API Key tab (the plaintext is shown only once).",
  "hosts.step2ApiKeyLink": "Create an API Key",

  // Workspace settings → Hosts tab
  "wsHosts.scopeTitle": "Only machines you registered can be bound in",
  "wsHosts.scopeBody":
    "Only hosts registered by you appear here. A colleague's machine must first be registered by them in global settings, then they or an admin can bind it to this workspace. Once bound, admins can assign it to any agent; an agent creator can also use their own machine directly without a workspace binding.",
  "wsHosts.gotoPrefs": "Register my hosts",
  "wsHosts.title": "Hosts bound to this workspace",
  "wsHosts.desc":
    "When an agent chooses the host executor, it can only use machines bound to this workspace.",
  "wsHosts.empty": "No hosts bound to this workspace yet",
  "wsHosts.emptyHint": "Pick one of your registered machines from the list below.",
  "wsHosts.addTitle": "Add binding",
  "wsHosts.addDesc":
    "After binding, agents created in this workspace can run on this machine.",
  "wsHosts.bind": "Bind",
  "wsHosts.pickHost": "Select a host",
  "wsHosts.noneTitle": "You haven't registered any hosts yet",
  "wsHosts.noneHint":
    "First set up the daemon via the three-step guide in global settings → Hosts, then come back to bind.",
  "wsHosts.allBound": "All of your registered machines are already bound to this workspace.",
  "wsHosts.remove": "Unbind",
  "wsHosts.removeTitle": "Unbind \"{name}\"",
  "wsHosts.removeBody":
    "Agents in this workspace will no longer be able to use this machine. The machine itself is not deleted and stays in global settings.",
  "wsHosts.registeredBy": "Registered by",
  "wsHosts.boundBy": "Bound by",
  "wsHosts.notRecorded": "Not recorded",

  // Agent form → executor picker
  "agent.executorSection": "Executor",
  "agent.executorSectionDesc": "Where this agent's tool calls run.",
  "agent.executorCloud": "Docker container",
  "agent.executorCloudDesc": "Cloud sandbox, ready out of the box; auto-pauses when idle.",
  "agent.executorHost": "My host machine",
  "agent.executorHostDesc": "Runs on a machine I registered myself; the workspace persists.",
  "agent.executorHostPick": "Select a host",
  "agent.executorHostPlaceholder": "Select a bound machine",
  "agent.executorOnline": "Online",
  "agent.executorOffline": "Offline",
  "agent.executorOfflineWarn":
    "This machine is currently offline: tool calls fail while its daemon is away and recover when the machine comes back online.",
  "agent.executorNoneTitle": "No hosts are bound to this workspace yet",
  "agent.executorNoneHint":
    "First bind one of your registered machines in workspace settings → Hosts, then choose it here.",
  "agent.executorNoneLink": "Go bind a host in workspace settings",
  "agent.executorHostMissing":
    "Please select a host; the currently selected machine was unbound or deleted, please choose again.",
  // Two sources (workspace-assigned / personal)
  "agent.executorSourceLabel": "Host source",
  "agent.executorSourceWorkspace": "Workspace-assigned",
  "agent.executorSourceWorkspaceDesc":
    "Pick a machine bound to this workspace; assigned by a workspace admin or the machine owner. Stops working when unbound from the workspace.",
  "agent.executorSourcePersonal": "Personal",
  "agent.executorSourcePersonalDesc":
    "Pick one of my own registered machines without binding it to the workspace. Only I can change it, and only I can open its web terminal.",
  "agent.executorMyHost": "My machine",
  "agent.executorNotAssignable": "Only an admin or the machine owner can assign it",
  "agent.executorPersonalPick": "Select one of my machines",
  "agent.executorPersonalNoneTitle": "You haven't registered any host machines yet",
  "agent.executorPersonalNoneHint":
    "Install the ama daemon and register your machine in Preferences → Hosts. You can then use it directly without binding it to a workspace.",
  "agent.executorPersonalGotoRegister": "Register my host machine",
  "agent.executorPersonalTerminalNote":
    "Personal hosts bypass the workspace: unbinding from the workspace doesn't affect them, and only you can open the machine's web terminal.",
  "agent.executorPersonalLockedTitle": "Executor locked to the creator's personal host",
  "agent.executorPersonalLockedHint":
    "This host belongs to the agent's creator ({email}), not to the workspace. Only the creator can change or remove it. Ask them to switch to a workspace-assigned host instead.",
  "agent.executorPersonalForbiddenHint":
    "Only the agent's creator ({email}) can select a personal host. Ask the creator to set it, or use a workspace-assigned host above.",
  "agent.executorPersonalInvisibleTitle": "Creator's personal host",
  "agent.executorPersonalInvisibleHint":
    "This machine belongs only to the agent's creator. It is not shown to you and cannot be changed by you.",

  // Chat page → host executor panel (SandboxPanel)
  "sandbox.host.badge": "Host",
  "sandbox.host.title": "Running on your host",
  "sandbox.host.desc":
    "This run's tools execute on host \"{name}\"; the workspace persists on that machine.",
  "sandbox.host.fallbackName": "My host machine",
  "sandbox.host.filesUnsupported":
    "File browsing is not available for host executors. Use the terminal to inspect files.",
  "sandbox.host.openTerminal": "Open terminal",
};

export const MESSAGES: Record<"zh-CN" | "en-US", Messages> = {
  "zh-CN": zhCN,
  "en-US": enUS,
};
