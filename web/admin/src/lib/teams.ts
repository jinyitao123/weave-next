import { api } from './api'
import { serialGraph, withVerifyLoop, type Graph } from './graph'

export interface TeamRecord {
  id: string
  name: string
  display_name?: string
  objective?: string
  status: string
  default_workflow_id?: string
  updated_at: string
}

export interface MemberConfiguration {
  display_name: string
  role: string
  engine: string
  runtime_id: string
  model: string
  system_prompt: string
  [key: string]: unknown
}

export interface DevelopmentMember {
  id: string
  configuration: MemberConfiguration
  relationship: { duty: string; result_requirement: string; enabled: boolean; [key: string]: unknown }
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

export interface DevelopmentTrial { request_id: string; revision: number; workflow_id: string; run_id: string; status: string; created_at: string }

export interface DevelopmentDraft {
  revision: number
  published_revision: number
  document: DevelopmentDocument
  published_document?: DevelopmentDocument
  updated_at: string
  trials: DevelopmentTrial[]
  publication_readiness?: { ready: boolean }
}

export const listTeamRecords = () => api<TeamRecord[]>('/v1/teams?status=all&purpose=development')
  .then((teams) => teams.filter((team) => team.status !== 'archived'))

const base = (teamId: string) => `/v1/teams/${encodeURIComponent(teamId)}/development`

export const readDevelopment = (teamId: string) => api<DevelopmentDraft>(base(teamId))
export const saveDevelopment = (teamId: string, revision: number, document: DevelopmentDocument) =>
  api<DevelopmentDraft>(base(teamId), { method: 'PUT', body: JSON.stringify({ expected_revision: revision, document }) })
export const publishDevelopment = (teamId: string, revision: number) =>
  api<DevelopmentDraft>(`${base(teamId)}/publish`, { method: 'POST', body: JSON.stringify({ revision }) })
export const startTrial = (teamId: string, revision: number, workflowId: string, input: string, requestId: string) =>
  api<{ run_id: string }>(`${base(teamId)}/trials`, { method: 'POST', body: JSON.stringify({ revision, workflow_id: workflowId, request_id: requestId, input, business_actions: [] }) })

interface CreatedAgent { id: string }

async function createAgent(name: string, displayName: string, role: 'avatar' | 'worker', engine: string, prompt: string): Promise<CreatedAgent> {
  return api<CreatedAgent>('/v1/agents', {
    method: 'POST',
    body: JSON.stringify({ name, display_name: displayName, role, engine, graph_type: 'standard', spec: { system_prompt: prompt } }),
  })
}

export const starterMembers = [
  { key: 'code', displayName: '编码', engine: 'claude', prompt: '分析需求、设计方案并修改代码。收到验证意见时先逐条修复。说明改了什么、为什么这样改，以及还没完成的部分。', requirement: '完成代码修改，说明改动内容、依据和未完成项。' },
  { key: 'verify', displayName: '验证', engine: 'codex', prompt: '独立验证上一步的代码：运行测试和检查，逐条记录执行的命令与结果，不根据描述推断结果。', requirement: '列出执行的检查命令和真实结果。全部通过时 passed 为 true；否则为 false，并在 summary 中写明未通过项和原因。' },
] as const

// Creates a team pre-filled with a coding member and an independent verifier,
// using the same agent and team endpoints as the desktop. Agents created before
// a failure are removed again.
export async function createStarterTeam(name: string, objective: string): Promise<string> {
  const suffix = crypto.randomUUID()
  const created: string[] = []
  try {
    const lead = await createAgent(`team-${suffix}-lead`, '团队负责人', 'avatar', 'loom', `负责理解“${name}”的目标并汇总可核验结果。`)
    created.push(`team-${suffix}-lead`)
    const workers: Array<{ id: string; requirement: string; duty: string }> = []
    for (const member of starterMembers) {
      const agentName = `team-${suffix}-${member.key}`
      const agent = await createAgent(agentName, member.displayName, 'worker', member.engine, member.prompt)
      created.push(agentName)
      workers.push({ id: agent.id, requirement: member.requirement, duty: member.prompt })
    }
    const team = await api<{ id: string }>('/v1/teams', {
      method: 'POST',
      body: JSON.stringify({
        name: `team-${suffix}`, display_name: name, objective, primary_scenario: objective,
        success_criteria: '交付经过独立验证的代码改动', lead_avatar_id: lead.id,
        workers: workers.map((worker) => ({
          worker_agent_id: worker.id, duty: worker.duty, when_to_use: '负责人分配相关工作时', context_instruction: '保留任务上下文、代码版本和材料来源。',
          allowed_kinds: ['consult', 'dispatch', 'handoff'], default_kind: 'dispatch', result_requirement: worker.requirement,
        })),
      }),
    })
    return team.id
  } catch (error) {
    await Promise.allSettled(created.map((agentName) => api(`/v1/agents/${encodeURIComponent(agentName)}`, { method: 'DELETE' })))
    throw error
  }
}

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
  if (workers.length < 2) return document
  const steps = starterMembers.map((starter, index) => {
    const member = workers.find((item) => item.configuration.display_name === starter.displayName) ?? workers[index]
    return { member: { id: member.id, displayName: member.configuration.display_name }, label: starter.displayName, requirement: starter.requirement }
  })
  return {
    ...document,
    workflows: [{
      id: crypto.randomUUID(), name: '开发与验证', description: '编码后由另一引擎独立验证，未通过时退回编码',
      trigger_config: { schema_version: 1, type: 'conversation_explicit', config: {} },
      graph_definition: withVerifyLoop(serialGraph(steps), 3),
    }],
  }
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
