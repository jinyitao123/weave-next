import { createUUID } from './ids'
import { api } from './api'
import { engineName } from './format'
import { serialGraph, withVerifyLoop, type Graph } from './graph'
import { initialGraph } from './workflow-graph'

export interface TeamRecord {
  id: string
  name: string
  display_name?: string
  objective?: string
  status: string
  default_workflow_id?: string
  updated_at: string
}

export interface MemberSkill { name: string; description: string; body: string; always_active: boolean }
export interface CapabilityBinding { capability_id: string; parameters: Array<{ name: string; source: string }> }
export interface ToolLoopControl { slice_rounds: number; initial_total_rounds: number }

// Only the fields the console edits are typed; anything else the server sent is
// carried through a save unchanged.
export interface MemberConfiguration {
  display_name: string
  role: string
  engine: string
  runtime_id: string
  model: string
  system_prompt: string
  skills?: MemberSkill[] | null
  business_capability_ids?: string[] | null
  business_capability_bindings?: CapabilityBinding[] | null
  tool_loop_control?: ToolLoopControl | null
  output_schema?: unknown
  permission_deny?: string[] | null
  permission_allow?: string[] | null
  permission_ask?: string[] | null
  mcp_server_ids?: string[] | null
  skill_names?: string[] | null
  max_tokens?: number
  max_output_tokens?: number
  reasoning_effort?: string
  first_chunk_timeout_ms?: number
  idle_timeout_ms?: number
  /** Bounds each model call from its request to its end; 0 leaves only the step limit. */
  call_timeout_ms?: number
  step_budget?: number
  max_cost_usd?: number
  max_tool_repeats?: number
  [key: string]: unknown
}

export interface MemberRelationship {
  duty: string
  when_to_use?: string
  context_instruction?: string
  allowed_kinds?: string[] | null
  default_kind?: string
  result_requirement: string
  enabled: boolean
  [key: string]: unknown
}

export interface DevelopmentMember {
  id: string
  configuration: MemberConfiguration
  relationship: MemberRelationship
}

export interface DevelopmentWorkflow {
  id: string
  name: string
  description: string
  graph_definition: Graph
  trigger_config: Record<string, unknown>
}

export interface DevelopmentDocument {
  name: string
  objective: string
  audience: string[]
  members: DevelopmentMember[]
  workflows: DevelopmentWorkflow[]
}

export interface DevelopmentTrial { request_id: string; revision: number; workflow_id: string; run_id: string; status: string; created_at: string; actor?: string; mine?: boolean; rejected_code?: string }

const trialRejections: Record<string, string> = {
  workflow_version_conflict: '草稿或它依赖的模型配置已变化，请重新准备后发起新的试跑',
  workflow_not_found: '要试跑的流程已不存在',
  workflow_publication_invalid: '试跑请求没有通过校验，请重新准备后再试',
  business_completion_check_required: '流程缺少业务完成检查，补上后再试跑',
  workflow_publication_owner_conflict: '这次试跑由其他人发起，不能代为提交',
}

// trialStatus states a trial's standing before a run exists: refused trials
// never ran and say why; one still submitting has no receipt yet.
export function trialStatus(trial: DevelopmentTrial, runLabel: (status: string) => string): { label: string; tone: 'neutral' | 'success' | 'danger'; note?: string } {
  if (trial.status === 'rejected') {
    return { label: '未被接受', tone: 'danger', note: trialRejections[trial.rejected_code ?? ''] ?? '这次试跑没有被接受，请重新准备后再试' }
  }
  if (trial.status === 'submitting') return { label: '提交中', tone: 'neutral' }
  return { label: runLabel(trial.status), tone: trial.status === 'succeeded' ? 'success' : trial.status === 'failed' ? 'danger' : 'neutral' }
}

export interface WorkflowReadiness { workflow_id: string; required_capability_ids: string[]; covered_capability_ids: string[]; missing_capability_ids: string[]; passed: boolean }

export interface DevelopmentDraft {
  revision: number
  published_revision: number
  prepared_revision?: number
  document: DevelopmentDocument
  published_document?: DevelopmentDocument
  updated_at: string
  trials: DevelopmentTrial[]
  publication_readiness?: { ready: boolean; workflows?: WorkflowReadiness[] }
  /** What the saved draft still lacks, worked out by the server for every surface. */
  issues?: DraftIssue[]
  /** The live team changed after the draft was opened; trials and publishing wait for a merge. */
  baseline_changed?: boolean
  /** What a merge just took in or kept; only on the merge's reply. */
  rebase_changes?: RebaseChange[]
}

export interface RebaseChange { kind: 'team' | 'member' | 'workflow'; id?: string; field?: string; name: string; action: 'taken' | 'added' | 'removed' | 'kept' }
export interface RebaseConflict { kind: 'team' | 'member' | 'workflow'; id?: string; field?: string; name: string; live_removed?: boolean }

export interface DraftIssue { scope: 'team' | 'member' | 'workflow' | 'step'; target: string; workflow?: string; code: string; message: string; blocks: 'trial' | 'publish' | 'none' }

// The page works these out itself as the document is edited, so the server's
// copy of them would only repeat what is already on screen.
const shownByThePage = new Set(['member_incomplete', 'member_model_missing', 'workflow_missing', 'business_completion_missing'])

/** The server's findings about the saved draft that the page does not already show, as sentences. */
export function serverIssues(draft: DevelopmentDraft | undefined): string[] {
  return (draft?.issues ?? []).filter((issue) => !shownByThePage.has(issue.code)).map((issue) => {
    if (issue.scope === 'member') return `“${issue.target}”：${issue.message}`
    if (issue.scope === 'workflow') return `流程“${issue.target}”：${issue.message}`
    if (issue.scope === 'step') return `流程“${issue.workflow ?? ''}”的步骤“${issue.target}”：${issue.message}`
    return issue.message
  })
}

/** Creates a business team on the server; the same request id never creates a second team. */
// The server makes the team, its starting members and flow once per request id.
export const createTeam = (requestId: string, name: string, objective: string, kind: TeamKind = 'business') =>
  api<{ team_id: string }>('/v1/development/teams', { method: 'POST', body: JSON.stringify({ request_id: requestId, name, objective, kind }) }).then((created) => created.team_id)

// A team package carries a team's saved draft to another workspace. It is
// read as JSON and sent back whole; only the server interprets it.
export interface TeamPackage { format: string; version: number; team: { name: string; objective: string } }
export interface TeamImported { team_id: string; name: string; adjustments?: string[] }

export const exportTeam = (teamId: string) => api<TeamPackage>(`/v1/teams/${encodeURIComponent(teamId)}/development/export`)

/** Makes a new team draft from a package; the same request id never makes a second team. */
export const importTeam = (requestId: string, pkg: TeamPackage) =>
  api<TeamImported>('/v1/development/teams/import', { method: 'POST', body: JSON.stringify({ request_id: requestId, package: pkg }) })

// readTeamPackage checks only what a person fixes by choosing another file;
// the server decides everything else.
export const readTeamPackage = (text: string): TeamPackage => {
  let parsed: Partial<TeamPackage> | null
  try {
    parsed = JSON.parse(text) as Partial<TeamPackage> | null
  } catch {
    throw new Error('这个文件不是团队导出文件')
  }
  if (!parsed || parsed.format !== 'weave-team-package' || typeof parsed.team?.name !== 'string') throw new Error('这个文件不是团队导出文件')
  if (parsed.version !== 1) throw new Error('这个团队导出文件的版本当前不支持')
  return parsed as TeamPackage
}

export const teamPackageFileName = (name: string) => `${name.replace(/[\\/:*?"<>|]/g, '_').trim() || '团队'}.weave-team.json`

export const listTeamRecords = () => api<TeamRecord[]>('/v1/teams?status=all&purpose=development')
  .then((teams) => teams.filter((team) => team.status !== 'archived'))

// A team as the list shows it: its record and who is on it.
export interface TeamCard extends TeamRecord { members: string[] }
interface RosterMember { name: string; display_name?: string }

export const listTeamCards = (): Promise<TeamCard[]> => api<Array<{ team: TeamRecord; lead?: RosterMember | null; workers?: RosterMember[] | null }>>('/v1/teams?status=all&purpose=development&include=roster')
  .then((rosters) => rosters.filter((roster) => roster.team.status !== 'archived').map((roster) => ({
    ...roster.team, members: [roster.lead, ...(roster.workers ?? [])].flatMap((member) => member ? [member.display_name || member.name] : []),
  })))
  // The roster needs the member registry; the plain list still names the teams.
  .catch(() => listTeamRecords().then((teams) => teams.map((team) => ({ ...team, members: [] }))))

const base = (teamId: string) => `/v1/teams/${encodeURIComponent(teamId)}/development`

export const readDevelopment = (teamId: string) => api<DevelopmentDraft>(base(teamId))
export const saveDevelopment = (teamId: string, revision: number, document: DevelopmentDocument) =>
  api<DevelopmentDraft>(base(teamId), { method: 'PUT', body: JSON.stringify({ expected_revision: revision, document }) })
/** Merges what changed live into the draft; keepDraft keeps the draft's edits where both changed. */
export const rebaseDevelopment = (teamId: string, revision: number, keepDraft: boolean) =>
  api<DevelopmentDraft>(`${base(teamId)}/rebase`, { method: 'POST', body: JSON.stringify({ expected_revision: revision, keep_draft: keepDraft }) })
export const publishDevelopment = (teamId: string, revision: number) =>
  api<DevelopmentDraft>(`${base(teamId)}/publish`, { method: 'POST', body: JSON.stringify({ revision }) })
export const startTrial = (teamId: string, revision: number, workflowId: string, input: string, requestId: string, actions: TrialAction[]) =>
  api<{ run_id: string }>(`${base(teamId)}/trials`, { method: 'POST', body: JSON.stringify({ revision, workflow_id: workflowId, request_id: requestId, input, business_actions: actions }) })

// The workspace's last reading of the Forge business action catalog.
export interface CatalogParameter { name: string; label?: string; type: string; multiple?: boolean; required?: boolean; description?: string; enum?: string[] }
export interface CatalogCapability {
  id: string
  name: string
  description?: string
  effect: string
  executionMode?: string
  requiresRecord?: boolean
  requiresConfirmation?: boolean
  status: string
  unavailableReason?: string
  params?: CatalogParameter[]
}
export interface BusinessCatalog { available: boolean; fetchedAt?: string; fetchedBy?: string; capabilities: CatalogCapability[] }

const readCatalog = (catalog: BusinessCatalog): BusinessCatalog => ({ ...catalog, capabilities: catalog.capabilities ?? [] })

export const loadBusinessCatalog = () => api<BusinessCatalog>('/v1/development/business-capabilities').then(readCatalog)

// Reads the catalog again on request. The console keeps no Forge credential,
// so the signed-in person approves once more on Forge and the session Forge
// releases for that is what reads the catalog.
export const refreshBusinessCatalog = (forge_token: string) =>
  api<BusinessCatalog>('/v1/development/business-capabilities/refresh', { method: 'POST', body: JSON.stringify({ forge_token }) }).then(readCatalog)

export const archiveTeam = (teamId: string) => api<void>(`${base(teamId)}/team`, { method: 'DELETE' })

// A frozen action definition sent with a trial; simulated calls never reach Forge.
export interface TrialAction {
  capability_id: string
  name: string
  object_name: string
  label: string
  description: string
  requires_record: boolean
  requires_confirmation: boolean
  simulation_authorized: boolean
  params: CatalogParameter[]
}

// The business actions one workflow can call: those of the lead and of every
// enabled member a step is assigned to.
export function workflowCapabilityIds(document: DevelopmentDocument, workflow: DevelopmentWorkflow): string[] {
  const used = new Set<string>()
  for (const node of workflow.graph_definition.nodes ?? []) {
    if (node.type === 'lead') for (const member of document.members) if (member.configuration.role === 'avatar') used.add(member.id)
    if (node.type === 'worker' && typeof node.config?.agent_id === 'string') used.add(node.config.agent_id)
  }
  const ids = new Set<string>()
  for (const member of document.members) {
    if (!used.has(member.id) || member.relationship.enabled === false) continue
    for (const id of member.configuration.business_capability_ids ?? []) if (typeof id === 'string' && id.trim()) ids.add(id)
  }
  return [...ids]
}

// Definitions for a trial come from the catalog; an action the catalog does
// not list cannot be trialled and is returned as missing.
export function trialActions(ids: string[], catalog: BusinessCatalog | undefined, simulate: ReadonlySet<string>): { actions: TrialAction[]; missing: string[] } {
  const actions: TrialAction[] = [], missing: string[] = []
  for (const id of ids) {
    const found = catalog?.capabilities.find((capability) => capability.id === id)
    if (!found) { missing.push(id); continue }
    const parts = describeCapability(id)
    actions.push({
      capability_id: id, name: parts.action, object_name: parts.object, label: found.name, description: found.description ?? '',
      requires_record: found.requiresRecord === true, requires_confirmation: found.requiresConfirmation === true,
      simulation_authorized: simulate.has(id), params: found.params ?? [],
    })
  }
  return { actions, missing }
}

export function capabilityName(id: string, catalog?: BusinessCatalog): string {
  return catalog?.capabilities.find((capability) => capability.id === id)?.name || describeCapability(id).action
}

const systemParameters = ['idempotency_key', 'idempotencyKey']

// File parameters of a newly added action read the task's materials: all of
// them for a multiple parameter, the single one otherwise.
export function defaultCapabilityBinding(capability: CatalogCapability): CapabilityBinding | undefined {
  const parameters = (capability.params ?? [])
    .filter((parameter) => parameter.type?.toLowerCase() === 'file' && !systemParameters.includes(parameter.name))
    .map((parameter) => ({ name: parameter.name, source: parameter.multiple ? 'materials.ids' : 'materials.single.id' }))
  return parameters.length ? { capability_id: capability.id, parameters } : undefined
}

const bindingSources: Record<string, string> = {
  'materials.ids': '本次提交的全部材料',
  'materials.single.id': '本次提交的那份材料',
  'materials.single.name': '材料的名称',
  'materials.single.sha256': '材料的校验值',
  'materials.single.manifest_json': '材料清单',
  'materials.manifest_json': '材料清单',
}
export const bindingSourceLabel = (source: string) => bindingSources[source] ?? source

// The handoff kind a step needs follows from where it sits in the flow.
export function requiredHandoffKinds(document: DevelopmentDocument, memberId: string): string[] {
  const kinds = new Set<string>()
  for (const flow of document.workflows) for (const node of flow.graph_definition.nodes ?? []) {
    if (node.type === 'worker' && node.config?.agent_id === memberId && typeof node.config.kind === 'string') kinds.add(node.config.kind)
  }
  return [...kinds]
}

// Assigning a member to a step also allows the handoff kind that step needs,
// so a flow built on the page never fails publication on this setting.
export function allowRequiredHandoffKinds(document: DevelopmentDocument): DevelopmentDocument {
  let changed = false
  const members = document.members.map((member) => {
    const allowed = member.relationship.allowed_kinds ?? []
    if (member.configuration.role !== 'worker' || !allowed.length) return member
    const missing = requiredHandoffKinds(document, member.id).filter((kind) => !allowed.includes(kind))
    if (!missing.length) return member
    changed = true
    return { ...member, relationship: { ...member.relationship, allowed_kinds: [...allowed, ...missing] } }
  })
  return changed ? { ...document, members } : document
}

export const starterMembers = [
  { key: 'code', displayName: '编码', engine: 'claude', prompt: '分析需求、设计方案并修改代码。收到验证意见时先逐条修复。说明改了什么、为什么这样改，以及还没完成的部分。', requirement: '完成代码修改，说明改动内容、依据和未完成项。' },
  { key: 'verify', displayName: '验证', engine: 'codex', prompt: '独立验证上一步的代码：运行测试和检查，逐条记录执行的命令与结果，不根据描述推断结果。', requirement: '列出执行的检查命令和真实结果。全部通过时 passed 为 true；否则为 false，并在 summary 中写明未通过项和原因。' },
] as const

export type TeamKind = 'business' | 'code'

// Adds the starter workflow (coding, then independent verification) when the
// draft has no workflow yet.
export function withStarterWorkflow(document: DevelopmentDocument): DevelopmentDocument {
  if (document.workflows.length) return document
  // The lead is required by the team but not used by the serial workflow; it
  // still needs a duty before a trial is accepted.
  document = { ...document, members: document.members.map((member) => member.configuration.role === 'avatar' && !member.relationship.duty?.trim()
    ? { ...member, relationship: { ...member.relationship, duty: '理解任务目标并汇总可核验结果' } }
    : member) }
  const workers = document.members.filter((member) => member.configuration.role === 'worker')
  // A team that starts with one member gets the plain flow: the lead reads the
  // task, the member handles it, the result is delivered.
  if (workers.length === 1) {
    return { ...document, workflows: [{
      id: createUUID(), name: '业务流程', description: document.objective,
      trigger_config: { schema_version: 1, type: 'conversation_explicit', config: {} },
      graph_definition: initialGraph(workers[0]),
    }] }
  }
  if (workers.length < 2) return document
  const steps = starterMembers.map((starter, index) => {
    const member = workers.find((item) => item.configuration.display_name === starter.displayName) ?? workers[index]
    return { member: { id: member.id, displayName: member.configuration.display_name }, label: starter.displayName, requirement: starter.requirement }
  })
  return {
    ...document,
    workflows: [{
      id: createUUID(), name: '开发与验证', description: '编码后由另一引擎独立验证，未通过时退回编码',
      trigger_config: { schema_version: 1, type: 'conversation_explicit', config: {} },
      graph_definition: withVerifyLoop(serialGraph(steps), 3),
    }],
  }
}

// A member added on the page; the server creates it when the draft is trialled.
// A catalog entry sometimes repeats its name as its description; that says nothing new.
export const capabilityNote = (name: string | undefined, description: string | undefined): string => description && description.trim() !== (name ?? '').trim() ? description.trim() : ''

export function newMember(model: string): DevelopmentMember {
  return {
    id: createUUID(),
    configuration: { display_name: '新成员', role: 'worker', engine: 'loom', runtime_id: '', model, system_prompt: '' },
    relationship: { duty: '', when_to_use: '流程执行到本成员负责的步骤时', context_instruction: '保留本次任务的原话和材料来源。', allowed_kinds: ['consult', 'dispatch', 'handoff'], default_kind: 'consult', result_requirement: '', enabled: true },
  }
}

// The steps a member is assigned to, by name; a member in use cannot be removed.
export function memberSteps(document: DevelopmentDocument, memberId: string): string[] {
  const member = document.members.find((item) => item.id === memberId)
  return document.workflows.flatMap((flow) => (flow.graph_definition.nodes ?? [])
    .filter((node) => member?.configuration.role === 'avatar' ? node.type === 'lead' : node.type === 'worker' && node.config?.agent_id === memberId)
    .map((node) => node.label || '未命名步骤'))
}

export const cliEngines = ['claude', 'codex', 'opencode']
export const isCLIEngine = (engine: string) => cliEngines.includes(engine)

export interface NodeChoice { id: string; engines: string[] }

// Pins every CLI member without a node to one that accepts its engine,
// preferring nodes not already serving another engine (a publication freezes
// one engine per node).
export function pinMembers(document: DevelopmentDocument, nodes: NodeChoice[]): DevelopmentDocument {
  const used = new Map<string, string>()
  for (const member of document.members) if (isCLIEngine(member.configuration.engine) && member.configuration.runtime_id) used.set(member.configuration.runtime_id, member.configuration.engine)
  let changed = false
  const members = document.members.map((member) => {
    const config = member.configuration
    if (member.configuration.role !== 'worker' || !isCLIEngine(config.engine) || config.runtime_id) return member
    const candidates = nodes.filter((node) => node.engines.includes(config.engine))
    const choice = candidates.find((node) => !used.has(node.id) || used.get(node.id) === config.engine)
    if (!choice) return member
    used.set(choice.id, config.engine)
    changed = true
    return { ...member, configuration: { ...config, runtime_id: choice.id } }
  })
  return changed ? { ...document, members } : document
}

export const handoffKinds = [
  { value: 'consult', label: '咨询' },
  { value: 'dispatch', label: '派发' },
  { value: 'handoff', label: '交接' },
] as const

// The MCP services and library skills a member may be given; a service that is
// not usable is listed so a member that already has it can still drop it.
export interface MemberTools { mcp_servers: Array<{ id: string; name: string; usable: boolean }>; skills: string[] }
export const loadMemberTools = () => api<Partial<MemberTools>>('/v1/development/member-tools').then((body): MemberTools => ({ mcp_servers: body.mcp_servers ?? [], skills: body.skills ?? [] }))

export const loadModelCatalog = () => api<{ models: string[] }>('/v1/development/model-catalog').then((catalog) => catalog.models ?? [])

// A Forge business action id looks like forge:action:<object>.<action>; the
// parts are shown as written until a catalog supplies the Chinese name.
export function describeCapability(id: string): { object: string; action: string } {
  const body = id.replace(/^forge:action:/, "")
  const dot = body.indexOf(".")
  return dot < 0 ? { object: "", action: body } : { object: body.slice(0, dot), action: body.slice(dot + 1) }
}

// Empty text clears the schema; anything else must be a JSON object.
export function parseOutputSchema(text: string): { value: Record<string, unknown> | null; error?: string } {
  if (!text.trim()) return { value: null }
  try {
    const parsed: unknown = JSON.parse(text)
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return { value: null, error: "这里需要一个以 { 开头、} 结尾的 JSON 对象；不需要固定格式时留空" }
    return { value: parsed as Record<string, unknown> }
  } catch {
    return { value: null, error: "格式不对：这里需要一段 JSON；不需要固定格式时留空" }
  }
}

export const toolLoopLimits = { sliceMax: 1000 }


// What trial preparation would reject for one member, worded for the page.
// Isolated trials refuse MCP servers, library skills and allow/ask tool
// permissions, so a member carrying them cannot be published from here.
export function memberProblems(member: DevelopmentMember): string[] {
  const config = member.configuration, relationship = member.relationship
  const problems: string[] = []
  if (!relationship.duty?.trim()) problems.push('还没有填写职责')
  if (!config.system_prompt?.trim()) problems.push('还没有填写工作方法')
  if (!isCLIEngine(config.engine) && !config.model?.trim()) problems.push('使用内置引擎时需要选择模型')
  const loopControl = config.tool_loop_control
  if (loopControl && (!(loopControl.slice_rounds >= 1 && loopControl.slice_rounds <= toolLoopLimits.sliceMax) || !(loopControl.initial_total_rounds >= 1))) problems.push("工具循环的每片轮次需在 1 到 1000 之间，总轮次不能小于 1")
  if (config.output_schema != null && (typeof config.output_schema !== "object" || Array.isArray(config.output_schema))) problems.push("输出结构必须是一个 JSON 对象")
  const kinds = relationship.allowed_kinds ?? []
  if (config.role === 'worker' && kinds.length && !kinds.includes(relationship.default_kind ?? '')) problems.push('默认交接方式必须是已选方式之一')
  const skills = config.skills ?? []
  if (skills.some((skill) => !skill.name?.trim() || !skill.body?.trim())) problems.push('技能需要填写名称和内容')
  const names = skills.map((skill) => skill.name?.trim()).filter(Boolean)
  if (new Set(names).size !== names.length) problems.push('技能名称不能重复')
  return problems
}

export function memberConfigIssues(document: DevelopmentDocument): string[] {
  return document.members.flatMap((member) => memberProblems(member).map((problem) => `“${member.configuration.display_name || '未命名成员'}”${problem}`))
}

// Built-in engines run on the server; only CLI members depend on registered
// nodes. A member with no node and no accepting node gets one combined line.
export function nodeReadinessIssues(document: DevelopmentDocument, accepting: Record<string, number>): string[] {
  const lines: string[] = []
  const covered = new Set<string>()
  for (const member of document.members) {
    const config = member.configuration
    if (config.role !== 'worker' || member.relationship.enabled === false || !isCLIEngine(config.engine) || accepting[config.engine]) continue
    covered.add(`“${config.display_name}”还没有指定节点`)
    lines.push(`“${config.display_name}”使用的 ${engineName(config.engine)} 当前没有可接任务的节点${config.runtime_id ? '' : '，也还没有指定节点'}`)
  }
  return [...lines, ...memberIssues(document).filter((issue) => !covered.has(issue))]
}

export function memberIssues(document: DevelopmentDocument): string[] {
  const issues: string[] = []
  const byNode = new Map<string, DevelopmentMember>()
  for (const member of document.members.filter((item) => item.configuration.role === 'worker')) {
    const config = member.configuration
    if (!isCLIEngine(config.engine)) continue
    if (!config.runtime_id) { issues.push(`“${config.display_name}”还没有指定节点`); continue }
    const other = byNode.get(config.runtime_id)
    if (other && other.configuration.engine !== config.engine) issues.push(`“${other.configuration.display_name}”和“${config.display_name}”使用不同引擎，请分配到不同节点`)
    byNode.set(config.runtime_id, member)
  }
  return issues
}
