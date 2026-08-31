import type {
  ChatConversationViewNode, ToolCallBlock,
} from '@deepseek-ai/dsh-client-ui-chat/client'

/** User-facing lifecycle state derived from the current Weave run. */
export type WorkTaskStatus =
  | 'preparing'
  | 'queued'
  | 'running'
  | 'waiting'
  | 'stopping'
  | 'completed'
  | 'failed'
  | 'stopped'

/** One workflow stage shown in the work-task progress summary. */
export interface WorkTaskStage {
  readonly name: string
  readonly status: 'completed' | 'running' | 'waiting' | 'failed'
}

/** One participating runtime and its visible availability state. */
export interface WorkTaskRuntime {
  readonly name: string
  readonly detail: string
  readonly status: WorkTaskStatus
}

/** One team returned by Weave before a run has been dispatched. */
export interface WorkTaskTeamCandidate {
  readonly teamId: string
  readonly name: string
  readonly objective: string
  readonly status: string
  readonly workflowAvailable: boolean
}

export type WorkTaskMemberStatus = 'pending' | 'running' | 'partially-completed' | 'completed' | 'failed' | 'stopped' | 'not-recorded'

export interface WorkTaskMemberInput {
  readonly name: string
  readonly expectedType: string
  readonly source: string
  readonly nodeId: string
  readonly path: string
  readonly summary: string
}

export interface WorkTaskMemberStage {
  readonly nodeId: string
  readonly name: string
  readonly status: WorkTaskMemberStatus
  readonly inputs: readonly WorkTaskMemberInput[]
  readonly outputRefs: readonly string[]
  readonly startedAt: string
  readonly completedAt: string
  readonly durationMs: number
  readonly toolCalls: number
  readonly tools: readonly WorkTaskMemberTool[]
}

export interface WorkTaskMemberTool {
  readonly callId: string
  readonly name: string
  readonly status: 'running' | 'ok' | 'error'
  readonly startedAt: string
  readonly completedAt: string
}

export interface WorkTaskMember {
  readonly agentId: string
  readonly name: string
  readonly duty: string
  readonly role: 'lead' | 'worker'
  readonly status: WorkTaskMemberStatus
  readonly runtime: string
  readonly stages: readonly WorkTaskMemberStage[]
}

/** One deliverable bound to the active Weave run. */
export interface WorkTaskDeliverable {
  readonly id: string
  readonly title: string
  readonly kind: 'final' | 'stage'
  readonly contentType: string
  readonly preview: string
  readonly createdAt: string
}

/** Durable, read-only projection used by the Weave work-task surfaces. */
export interface WorkTaskModel {
  readonly detected: boolean
  readonly brief: string
  readonly status: WorkTaskStatus
  readonly teamName: string
  readonly workflowName: string
  readonly runId: string
  readonly completedStages: number
  readonly totalStages: number
  readonly latestStage: string
  readonly stages: readonly WorkTaskStage[]
  readonly teamCandidates: readonly WorkTaskTeamCandidate[]
  readonly members: readonly WorkTaskMember[]
  readonly corrections: readonly WorkTaskCorrection[]
  readonly runtimes: readonly WorkTaskRuntime[]
  readonly humanTaskCount: number
  readonly deliverableCount: number
  readonly deliverables: readonly WorkTaskDeliverable[]
  readonly blocker: 'none' | 'queued' | 'runtime-missing' | 'failed'
  readonly attempts: readonly WorkTaskAttempt[]
  readonly pendingAction: WorkTaskPendingAction | null
  readonly actionError: string
  readonly completeness: Readonly<Record<string, 'complete' | 'partial' | 'unavailable'>>
  readonly observedAt: number
}

export interface WorkTaskAttempt {
  readonly clientRequestId: string
  readonly runId: string
  readonly brief: string
  readonly status: WorkTaskStatus
  readonly completedStages: number
  readonly totalStages: number
  readonly latestStage: string
  readonly deliverableCount: number
  readonly deliverables: readonly WorkTaskDeliverable[]
  readonly createdAt: number
  readonly updatedAt: number
}

export interface WorkTaskPendingAction {
  readonly kind: 'stop' | 'rerun' | 'correction-request' | 'correction-confirm'
  readonly targetRunId: string
  readonly idempotencyKey: string
  readonly clientRequestId: string
  readonly brief: string
  readonly requestedAt: number
  readonly targetKind: 'team' | 'member' | ''
  readonly targetMemberId: string
  readonly correctionId: string
  readonly disposition: 'apply' | 'discard' | ''
  readonly instruction: string
}

export interface WorkTaskCorrection {
  readonly correctionId: string
  readonly targetKind: 'team' | 'member'
  readonly targetMemberId: string
  readonly instruction: string
  readonly status: 'requested' | 'ready' | 'confirmed' | 'discarded' | 'applied'
  readonly safeNodeId: string
  readonly restartNodeId: string
  readonly affectedNodeIds: readonly string[]
  readonly preservedNodeIds: readonly string[]
  readonly requestedAt: string
}

/** Host-computed durable subset used after the foreground conversation is no longer active. */
export type WorkTaskProjection = Omit<WorkTaskModel, 'detected' | 'stages' | 'teamCandidates'> & {
  readonly clientRequestId: string
  readonly teamId: string
  readonly updatedAt: number
}

declare module '@deepseek-ai/dsh-session-projection/types' {
  interface SessionProjectionMap { workTask: WorkTaskProjection | null }
}

/**
 * Prefer the host-owned durable lifecycle while retaining richer loaded-conversation stage detail.
 * @param model - task facts derived from the loaded conversation window.
 * @param projection - Host-synchronized durable task facts, when available.
 * @returns one display model with Host lifecycle authority and conversation detail.
 */
export function projectedWorkTask(model: WorkTaskModel, projection: WorkTaskProjection | null | undefined): WorkTaskModel {
  if (projection == null) return model
  return {
    ...model,
    ...projection,
    detected: true,
    stages: model.runId === projection.runId ? model.stages : [],
    teamCandidates: model.teamCandidates,
    deliverables: projection.deliverables,
  }
}

interface ToolObservation {
  readonly name: string
  readonly args: unknown
  readonly value: unknown
  readonly isError: boolean
}

const EMPTY_MODEL: WorkTaskModel = {
  detected: false,
  brief: '',
  status: 'preparing',
  teamName: '',
  workflowName: '',
  runId: '',
  completedStages: 0,
  totalStages: 0,
  latestStage: '',
  stages: [],
  teamCandidates: [],
  members: [],
  corrections: [],
  runtimes: [],
  humanTaskCount: 0,
  deliverableCount: 0,
  deliverables: [],
  blocker: 'none',
  attempts: [],
  pendingAction: null,
  actionError: '',
  completeness: {},
  observedAt: 0,
}

function record(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null
}

function parseJson(raw: string | null | undefined): unknown {
  if (raw === null || raw === undefined || raw.trim() === '') return null
  try {
    return JSON.parse(raw) as unknown
  } catch {
    return raw
  }
}

function resultValue(block: ToolCallBlock): unknown {
  if (!('kind' in block)) return null
  const text = block.content
    .map(item => item.type === 'text' ? item.text : JSON.stringify(item))
    .join('\n')
  return parseJson(text)
}

function blockName(block: ToolCallBlock): string {
  return 'kind' in block ? block.call?.name ?? '' : block.name
}

function observations(nodes: readonly ChatConversationViewNode[]): readonly ToolObservation[] {
  const values: ToolObservation[] = []
  const visit = (block: ToolCallBlock): void => {
    values.push({
      name: blockName(block),
      args: parseJson('kind' in block ? block.call?.argsRaw : block.argsRaw),
      value: resultValue(block),
      isError: 'kind' in block && block.isError,
    })
    for (const child of block.subCalls) visit(child)
  }
  for (const node of nodes) {
    if (node.kind !== 'tool-call') continue
    const data = record(node.data)
    const root = data?.root
    if (record(root) !== null && typeof record(root)?.callId === 'string') visit(root as ToolCallBlock)
  }
  return values
}

function latest(calls: readonly ToolObservation[], names: readonly string[]): ToolObservation | undefined {
  for (let index = calls.length - 1; index >= 0; index--) {
    const call = calls[index]
    if (call !== undefined && names.includes(call.name)) return call
  }
  return undefined
}

function deepValue(value: unknown, keys: ReadonlySet<string>, depth = 0): unknown {
  if (depth > 5) return undefined
  const object = record(value)
  if (object !== null) {
    for (const [key, candidate] of Object.entries(object)) {
      if (keys.has(key)) return candidate
    }
    for (const candidate of Object.values(object)) {
      const found = deepValue(candidate, keys, depth + 1)
      if (found !== undefined) return found
    }
  } else if (Array.isArray(value)) {
    for (const candidate of value) {
      const found = deepValue(candidate, keys, depth + 1)
      if (found !== undefined) return found
    }
  }
  return undefined
}

function deepString(value: unknown, keys: readonly string[]): string {
  const found = deepValue(value, new Set(keys))
  return typeof found === 'string' ? found.trim() : ''
}

function deepNumber(value: unknown, keys: readonly string[]): number {
  const found = deepValue(value, new Set(keys))
  if (typeof found === 'number' && Number.isFinite(found)) return Math.max(0, Math.floor(found))
  if (typeof found === 'string' && /^\d+$/.test(found)) return Number(found)
  return 0
}

function normalizedStatus(raw: string): WorkTaskStatus {
  const value = raw.toLowerCase().replaceAll('-', '_')
  if (['completed', 'complete', 'succeeded', 'success', 'done'].includes(value)) return 'completed'
  if (['cancelled', 'canceled', 'abandoned', 'stopped'].includes(value)) return 'stopped'
  if (['failed', 'error'].includes(value)) return 'failed'
  if (value === 'cancel_requested') return 'stopping'
  if (['parked', 'waiting', 'yielded', 'waiting_for_human', 'needs_input', 'blocked', 'paused'].includes(value)) return 'waiting'
  if (['queued', 'pending'].includes(value)) return 'queued'
  if (['running', 'active', 'in_progress', 'working'].includes(value)) return 'running'
  return 'preparing'
}

function stageStatus(raw: string): WorkTaskStage['status'] {
  const normalized = normalizedStatus(raw)
  if (normalized === 'completed') return 'completed'
  if (normalized === 'running') return 'running'
  if (normalized === 'failed') return 'failed'
  return 'waiting'
}

function stageList(value: unknown): readonly WorkTaskStage[] {
  const source = deepValue(value, new Set(['stages', 'workflow_stages', 'steps']))
  if (!Array.isArray(source)) return []
  return source.flatMap((candidate): WorkTaskStage[] => {
    const item = record(candidate)
    if (item === null) return []
    const name = deepString(item, ['name', 'title', 'stage_name', 'label'])
    if (name === '') return []
    return [{ name, status: stageStatus(deepString(item, ['status', 'state'])) }]
  })
}

function teamIdentity(calls: readonly ToolObservation[], dispatch: ToolObservation | undefined): { id: string; name: string } {
  const dispatchId = deepString(dispatch?.args, ['team_id', 'teamId', 'team'])
    || deepString(dispatch?.value, ['team_id', 'teamId'])
  const listed = latest(calls, ['mcp__weave__team_list'])?.value
  const listedTeams = Array.isArray(listed)
    ? listed
    : deepValue(listed, new Set(['teams', 'items']))
  if (Array.isArray(listedTeams)) {
    for (const candidate of listedTeams) {
      const item = record(candidate)
      if (item === null) continue
      const id = deepString(item, ['team_id', 'teamId', 'id'])
      const name = deepString(item, ['name'])
      if (dispatchId !== '' && id === dispatchId) return { id, name: name || id }
    }
  }
  return { id: dispatchId, name: deepString(dispatch?.value, ['team_name', 'teamName']) || dispatchId }
}

function teamCandidates(calls: readonly ToolObservation[]): readonly WorkTaskTeamCandidate[] {
  const listed = latest(calls, ['mcp__weave__team_list'])?.value
  const source = Array.isArray(listed) ? listed : deepValue(listed, new Set(['teams', 'items']))
  if (!Array.isArray(source)) return []
  return source.flatMap((candidate): WorkTaskTeamCandidate[] => {
    const item = record(candidate)
    if (item === null) return []
    const teamId = deepString(item, ['team_id', 'teamId', 'id'])
    const name = deepString(item, ['name'])
    if (teamId === '' || name === '') return []
    return [{
      teamId,
      name,
      objective: deepString(item, ['objective', 'primary_scenario', 'primaryScenario']),
      status: deepString(item, ['status']),
      workflowAvailable: item.workflow_available === true || item.workflowAvailable === true,
    }]
  })
}

function runtimeList(value: unknown): readonly WorkTaskRuntime[] {
  const source = deepValue(value, new Set(['runtimes', 'agents', 'workers', 'executors']))
  if (!Array.isArray(source)) return []
  return source.flatMap((candidate): WorkTaskRuntime[] => {
    const item = record(candidate)
    if (item === null) return []
    const name = deepString(item, ['role', 'name', 'agent_name', 'runtime_name', 'id'])
    if (name === '') return []
    const model = deepString(item, ['model', 'model_name'])
    const provider = deepString(item, ['provider', 'runtime', 'environment'])
    return [{
      name,
      detail: [provider, model].filter(Boolean).join(' · '),
      status: normalizedStatus(deepString(item, ['status', 'state'])),
    }]
  })
}

function memberStatus(value: unknown): WorkTaskMemberStatus {
  const raw = deepString(value, ['status', 'state']).toLowerCase().replaceAll('_', '-')
  if (raw === 'completed' || raw === 'finished') return 'completed'
  if (raw === 'partially-completed') return 'partially-completed'
  if (raw === 'running' || raw === 'active') return 'running'
  if (raw === 'failed') return 'failed'
  if (raw === 'stopped' || raw === 'cancelled' || raw === 'abandoned') return 'stopped'
  if (raw === 'not-recorded' || raw === 'known') return 'not-recorded'
  return 'pending'
}

function memberInputs(value: unknown): readonly WorkTaskMemberInput[] {
  if (!Array.isArray(value)) return []
  return value.flatMap((candidate): WorkTaskMemberInput[] => {
    const item = record(candidate)
    if (item === null) return []
    const name = deepString(item, ['name'])
    if (name === '') return []
    return [{
      name, expectedType: deepString(item, ['expected_type', 'expectedType']),
      source: deepString(item, ['source']), nodeId: deepString(item, ['node_id', 'nodeId']),
      path: deepString(item, ['path']), summary: deepString(item, ['summary']),
    }]
  })
}

function memberStages(value: unknown): readonly WorkTaskMemberStage[] {
  if (!Array.isArray(value)) return []
  return value.flatMap((candidate): WorkTaskMemberStage[] => {
    const item = record(candidate)
    if (item === null) return []
    const nodeId = deepString(item, ['node_id', 'nodeId'])
    if (nodeId === '') return []
    const rawOutputs = item.output_refs ?? item.outputRefs
    const rawTools = Array.isArray(item.tools) ? item.tools : []
    return [{
      nodeId, name: deepString(item, ['name', 'label']) || nodeId,
      status: memberStatus(item), inputs: memberInputs(item.inputs),
      outputRefs: Array.isArray(rawOutputs) ? rawOutputs.filter((entry): entry is string => typeof entry === 'string') : [],
      startedAt: deepString(item, ['started_at', 'startedAt']), completedAt: deepString(item, ['completed_at', 'completedAt']),
      durationMs: deepNumber(item, ['duration_ms', 'durationMs']), toolCalls: deepNumber(item, ['tool_calls', 'toolCalls']),
      tools: rawTools.flatMap((candidate): WorkTaskMemberTool[] => {
        const tool = record(candidate)
        if (tool === null) return []
        const name = deepString(tool, ['name'])
        const status = deepString(tool, ['status'])
        if (name === '' || !['running', 'ok', 'error'].includes(status)) return []
        return [{ callId: deepString(tool, ['call_id', 'callId']), name, status: status as WorkTaskMemberTool['status'],
          startedAt: deepString(tool, ['started_at', 'startedAt']), completedAt: deepString(tool, ['completed_at', 'completedAt']) }]
      }),
    }]
  })
}

function correctionList(value: unknown): readonly WorkTaskCorrection[] {
  const source = deepValue(value, new Set(['corrections']))
  if (!Array.isArray(source)) return []
  return source.flatMap((candidate): WorkTaskCorrection[] => {
    const item = record(candidate)
    if (item === null) return []
    const correctionId = deepString(item, ['correction_id', 'correctionId'])
    const targetKind = deepString(item, ['target_kind', 'targetKind'])
    const status = deepString(item, ['status'])
    if (correctionId === '' || (targetKind !== 'team' && targetKind !== 'member')
			|| !['requested', 'ready', 'confirmed', 'discarded', 'applied'].includes(status)) return []
    const strings = (raw: unknown): string[] => Array.isArray(raw)
      ? raw.filter((entry): entry is string => typeof entry === 'string') : []
    return [{
      correctionId, targetKind, targetMemberId: deepString(item, ['target_member_id', 'targetMemberId']),
      instruction: deepString(item, ['instruction']), status: status as WorkTaskCorrection['status'],
      safeNodeId: deepString(item, ['safe_node_id', 'safeNodeId']), restartNodeId: deepString(item, ['restart_node_id', 'restartNodeId']),
      affectedNodeIds: strings(item.affected_node_ids ?? item.affectedNodeIds),
      preservedNodeIds: strings(item.preserved_node_ids ?? item.preservedNodeIds),
      requestedAt: deepString(item, ['requested_at', 'requestedAt']),
    }]
  })
}

function memberList(value: unknown): readonly WorkTaskMember[] {
  const source = deepValue(value, new Set(['members']))
  if (!Array.isArray(source)) return []
  return source.flatMap((candidate): WorkTaskMember[] => {
    const item = record(candidate)
    if (item === null) return []
    const agentId = deepString(item, ['agent_id', 'agentId'])
    if (agentId === '') return []
    const runtime = record(item.runtime)
    const runtimeDetail = runtime === null ? '' : [
      deepString(runtime, ['runtime_id', 'runtimeId', 'engine']),
      [deepString(runtime, ['provider']), deepString(runtime, ['model'])].filter(Boolean).join('/'),
    ].filter(Boolean).join(' · ')
    return [{
      agentId, name: deepString(item, ['name', 'display_name']) || agentId,
      duty: deepString(item, ['duty']), role: deepString(item, ['role']) === 'lead' ? 'lead' : 'worker',
      status: memberStatus(item), runtime: runtimeDetail, stages: memberStages(item.stages),
    }]
  })
}

function routeModels(nodes: readonly ChatConversationViewNode[]): readonly string[] {
  const routes = new Set<string>()
  for (const node of nodes) {
    if (node.kind !== 'turn-tail') continue
    const data = record(node.data)
    const tokenUsage = record(data?.tokenUsage)
    const candidates = tokenUsage?.routes
    if (!Array.isArray(candidates)) continue
    for (const candidate of candidates) {
      const item = record(candidate)
      if (item === null) continue
      const provider = typeof item.provider === 'string' ? item.provider : ''
      const model = typeof item.model === 'string' ? item.model : ''
      const label = [provider, model].filter(Boolean).join(' · ')
      if (label !== '') routes.add(label)
    }
  }
  return [...routes]
}

function matchingCount(value: unknown, runId: string): number {
  const nested = deepValue(value, new Set(['items', 'tasks', 'deliverables']))
  const candidates = Array.isArray(value) ? value : Array.isArray(nested) ? nested : [value]
  return candidates.filter((candidate) => {
    const item = record(candidate)
    if (item === null) return false
    const candidateRun = deepString(item, ['run_id', 'runId'])
    return runId !== '' && candidateRun === runId
  }).length
}

function deliverableList(value: unknown, runId: string): WorkTaskDeliverable[] {
  if (runId === '') return []
  const nested = deepValue(value, new Set(['deliverables', 'items']))
  const candidates = Array.isArray(value) ? value : Array.isArray(nested) ? nested : [value]
  const seen = new Set<string>()
  return candidates.flatMap((candidate): WorkTaskDeliverable[] => {
    const item = record(candidate)
    if (item === null || deepString(item, ['run_id', 'runId']) !== runId) return []
    const id = deepString(item, ['id', 'deliverable_id'])
    if (id === '' || seen.has(id)) return []
    seen.add(id)
    const rawMetadata = item.metadata
    const metadata = typeof rawMetadata === 'string' ? parseJson(rawMetadata) : rawMetadata
    const kind = deepString(metadata, ['artifact_kind']) === 'final' ? 'final' : 'stage'
    const content = typeof item.content === 'string' ? item.content.trim() : ''
    return [{
      id,
      title: deepString(item, ['title', 'name']) || id,
      kind,
      contentType: deepString(item, ['content_type', 'contentType']) || 'text/plain',
      preview: content.slice(0, 6_000),
      createdAt: deepString(item, ['created_at', 'createdAt']),
    }]
  })
}

/**
 * Derive the visible Weave work-task projection from durable Chat nodes.
 * @param nodes - canonical conversation nodes for the current Session.
 * @returns the user-facing task, team, progress, runtime, blocker, and deliverable state.
 */
export function workTaskModel(nodes: readonly ChatConversationViewNode[]): WorkTaskModel {
  const calls = observations(nodes)
  const dispatch = latest(calls, ['mcp__weave__team_dispatch'])
  const detected = calls.some(call => call.name.startsWith('mcp__weave__'))
  if (!detected) return EMPTY_MODEL

  const dispatchStatus = latest(calls, ['mcp__weave__dispatch_status'])
  const runActivity = latest(calls, ['mcp__weave__team_run_activity'])
  const runStatus = latest(calls, ['mcp__weave__team_run_status'])
  // Activity is the exact-run server projection. Older dispatch status remains
  // the fallback for conversations created before the activity contract existed.
  const statusValue = runActivity?.value ?? dispatchStatus?.value ?? runStatus?.value ?? dispatch?.value
  const team = teamIdentity(calls, dispatch)
  const candidates = teamCandidates(calls)
  const runId = deepString(statusValue, ['run_id', 'runId'])
    || deepString(dispatch?.value, ['run_id', 'runId'])
  const humanValue = latest(calls, ['mcp__weave__human_task_list'])?.value
  const humanTaskCount = matchingCount(humanValue, runId)
  let status = normalizedStatus(deepString(statusValue, ['status', 'state', 'run_status']))
  if (humanTaskCount > 0) status = 'waiting'

  const stages = stageList(statusValue)
  const members = memberList(runActivity?.value ?? statusValue)
  const activeMemberStage = members.flatMap(member => member.stages)
    .find(stage => stage.status === 'running')?.name
  const completedStages = deepNumber(statusValue, ['completed_stages', 'stages_completed', 'completed_count'])
    || stages.filter(stage => stage.status === 'completed').length
  const totalStages = deepNumber(statusValue, ['total_stages', 'stages_total', 'stage_count'])
    || stages.length
  const latestStage = activeMemberStage
	|| deepString(statusValue, ['latest_stage', 'current_stage', 'stage_name'])
    || stages.find(stage => stage.status === 'running')?.name
	|| stages.find(stage => stage.status === 'completed')?.name
    || ''

  const listValue = latest(calls, ['mcp__weave__deliverable_list'])?.value
  const getValue = latest(calls, ['mcp__weave__deliverable_get'])?.value
  const listedDeliverables = deliverableList(listValue, runId)
  const deliverables = [
    ...listedDeliverables,
    ...deliverableList(getValue, runId).filter(item => !listedDeliverables.some(listed => listed.id === item.id)),
  ]
  const deliverableCount = deliverables.length

  const runtimeMissing = calls.some((call) => {
    const args = record(call.args)
    return call.isError && team.id !== '' && args?.agent === team.id
  }) || deepString(runStatus?.value, ['classification']) === 'terminal_missing'
  const blocker: WorkTaskModel['blocker'] = runtimeMissing
    ? 'runtime-missing'
    : status === 'failed'
      ? 'failed'
      : status === 'queued'
        ? 'queued'
        : 'none'

  const runtimes = [...runtimeList(runActivity?.value ?? runStatus?.value ?? statusValue)]
  const corrections = correctionList(runActivity?.value ?? statusValue)
  for (const route of routeModels(nodes)) {
    if (!runtimes.some(runtime => runtime.detail.includes(route))) {
      runtimes.push({ name: 'workbench', detail: route, status: status === 'completed' ? 'completed' : 'running' })
    }
  }

  return {
    detected,
    brief: deepString(dispatch?.args, ['task']),
    status,
    teamName: team.name,
    workflowName: deepString(dispatch?.value, ['workflow_name', 'workflow'])
      || deepString(dispatch?.args, ['workflow_name', 'workflow']),
    runId,
    completedStages,
    totalStages,
    latestStage,
    stages,
    teamCandidates: candidates,
    members,
    corrections,
    runtimes,
    humanTaskCount,
    deliverableCount,
    deliverables,
    blocker,
    attempts: [],
    pendingAction: null,
    actionError: '',
    completeness: {},
    observedAt: 0,
  }
}
