/** Host-side durable Weave work-task projection and status synchronizer. */

import { randomUUID } from 'node:crypto'
import type { Context } from '@deepseek-ai/cordis'
import type {} from '@deepseek-ai/dsh-commands'
import { FIRST_PARTY_SECTION_ORDER } from '@deepseek-ai/dsh-system-prompt'
import { z } from 'zod'
import type { Session, SessionEvent } from '@deepseek-ai/dsh-session'
import type { ProjectionDefinition } from '@deepseek-ai/dsh-session-projection'
import { buildPilotReport } from './pilot-report.ts'
import { inspectWeaveReadiness } from './readiness.ts'

export { buildPilotReport } from './pilot-report.ts'
export type { PilotReport, PilotReportTask } from './pilot-report.ts'
export { inspectWeaveReadiness } from './readiness.ts'
export type { WeaveReadiness, WeaveReadinessCheck, WeaveReadinessTone } from './readiness.ts'

/** User-facing lifecycle of one Weave-dispatched task. */
export type WorkTaskStatus = 'preparing' | 'queued' | 'running' | 'waiting' | 'stopping' | 'completed' | 'failed' | 'stopped'

/** Durable product view of one Weave dispatch owned by a Workbench session. */
export interface WorkTaskProjection {
  readonly brief: string
  readonly clientRequestId: string
  readonly runId: string
  readonly teamId: string
  readonly teamName: string
  readonly workflowName: string
  readonly status: WorkTaskStatus
  readonly completedStages: number
  readonly totalStages: number
  readonly latestStage: string
  readonly members: WorkTaskMember[]
  readonly corrections: WorkTaskCorrection[]
  readonly runtimes: WorkTaskRuntime[]
  readonly humanTaskCount: number
  readonly deliverableCount: number
  readonly deliverables: WorkTaskDeliverable[]
  readonly blocker: 'none' | 'queued' | 'runtime-missing' | 'failed'
  readonly attempts: WorkTaskAttempt[]
  readonly pendingAction: WorkTaskPendingAction | null
  readonly actionError: string
  readonly completeness: Readonly<Record<string, 'complete' | 'partial' | 'unavailable'>>
  readonly startedAt: string
  readonly finishedAt: string
  readonly tokensIn: number
  readonly tokensOut: number
  readonly costUSD: number
  readonly outcome: 'unrated' | 'adopted' | 'needs-revision'
  readonly outcomeNote: string
  readonly observedAt: number
  readonly updatedAt: number
}

/** One immutable Weave run link retained under the supervising Session. */
export interface WorkTaskAttempt {
  readonly clientRequestId: string
  readonly runId: string
  readonly brief: string
  readonly status: WorkTaskStatus
  readonly completedStages: number
  readonly totalStages: number
  readonly latestStage: string
  readonly deliverableCount: number
  readonly deliverables: WorkTaskDeliverable[]
  readonly createdAt: number
  readonly updatedAt: number
}

/** The sole unresolved local network action, persisted before it is sent. */
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

/** One runtime assignment reported by Weave for the task. */
export interface WorkTaskRuntime {
  readonly name: string
  readonly detail: string
  readonly status: WorkTaskStatus
}

/** User-facing lifecycle state for one workflow member or stage. */
export type WorkTaskMemberStatus = 'pending' | 'running' | 'partially-completed' | 'completed' | 'failed' | 'stopped' | 'not-recorded'

/** One declared input observed for a member stage. */
export interface WorkTaskMemberInput {
  readonly name: string
  readonly expectedType: string
  readonly source: string
  readonly nodeId: string
  readonly path: string
  readonly summary: string
}

/** One workflow stage assigned to a member, including observed tools and outputs. */
export interface WorkTaskMemberStage {
  readonly nodeId: string
  readonly name: string
  readonly status: WorkTaskMemberStatus
  readonly inputs: WorkTaskMemberInput[]
  readonly outputRefs: string[]
  readonly startedAt: string
  readonly completedAt: string
  readonly durationMs: number
  readonly toolCalls: number
  readonly tools: WorkTaskMemberTool[]
}

/** One bounded tool call observation recorded by Weave. */
export interface WorkTaskMemberTool {
  readonly callId: string
  readonly name: string
  readonly status: 'running' | 'ok' | 'error'
  readonly startedAt: string
  readonly completedAt: string
  readonly input: string
  readonly output: string
}

/** One durable correction request and its computed restart impact. */
export interface WorkTaskCorrection {
  readonly correctionId: string
  readonly targetKind: 'team' | 'member'
  readonly targetMemberId: string
  readonly instruction: string
  readonly status: 'requested' | 'ready' | 'confirmed' | 'discarded' | 'applied'
  readonly safeNodeId: string
  readonly restartNodeId: string
  readonly affectedNodeIds: string[]
  readonly preservedNodeIds: string[]
  readonly requestedAt: string
}

/** One frozen team member and their observed execution stages. */
export interface WorkTaskMember {
  readonly agentId: string
  readonly name: string
  readonly duty: string
  readonly role: 'lead' | 'worker'
  readonly status: WorkTaskMemberStatus
  readonly runtime: string
  readonly stages: WorkTaskMemberStage[]
}

/** One exact-run output available from the Workbench task surface. */
export interface WorkTaskDeliverable {
  readonly id: string
  readonly title: string
  readonly kind: 'final' | 'stage'
  readonly contentType: string
  readonly preview: string
  readonly content: string
  readonly truncated: boolean
  readonly createdAt: string
}

interface PendingCall { readonly name: string; readonly args: unknown }
interface WorkTaskState {
  readonly task: WorkTaskProjection | null
  readonly pendingCalls: Readonly<Record<string, PendingCall>>
  readonly teams: Readonly<Record<string, string>>
}

declare module '@deepseek-ai/dsh-session/types' {
  interface SessionEventMap {
    /** Whole latest Weave dispatch lifecycle, team, progress, runtime, blocker, and delivery summary synchronized by Workbench. */
    'weave/work-task': WorkTaskProjection
    /** One unresolved Workbench command. Null clears it after an authoritative response. */
    'weave/work-task-action': { readonly pendingAction: WorkTaskPendingAction | null }
  }
}

declare module '@deepseek-ai/dsh-session-projection/types' {
  interface SessionProjectionStateMap { workTask: WorkTaskState }
  interface SessionProjectionMap { workTask: WorkTaskProjection | null }
}

const statusSchema = z.enum(['preparing', 'queued', 'running', 'waiting', 'stopping', 'completed', 'failed', 'stopped'])
const runtimeSchema = z.object({ name: z.string(), detail: z.string(), status: statusSchema }).strict()
const memberStatusSchema = z.enum(['pending', 'running', 'partially-completed', 'completed', 'failed', 'stopped', 'not-recorded'])
const memberInputSchema = z.object({
  name: z.string(), expectedType: z.string(), source: z.string(), nodeId: z.string(), path: z.string(), summary: z.string().default(''),
}).strict()
const memberStageSchema = z.object({
  nodeId: z.string(), name: z.string(), status: memberStatusSchema,
  inputs: z.array(memberInputSchema), outputRefs: z.array(z.string()),
  startedAt: z.string().default(''), completedAt: z.string().default(''),
  durationMs: z.number().int().nonnegative().default(0), toolCalls: z.number().int().nonnegative().default(0),
  tools: z.array(z.object({ callId: z.string(), name: z.string(), status: z.enum(['running', 'ok', 'error']),
    startedAt: z.string(), completedAt: z.string(), input: z.string().default(''), output: z.string().default('') }).strict()).default([]),
}).strict()
const memberSchema = z.object({
  agentId: z.string(), name: z.string(), duty: z.string(), role: z.enum(['lead', 'worker']),
  status: memberStatusSchema, runtime: z.string(), stages: z.array(memberStageSchema),
}).strict()
const deliverableSchema = z.object({
  id: z.string(), title: z.string(), kind: z.enum(['final', 'stage']),
  contentType: z.string(), preview: z.string(), content: z.string().default(''), truncated: z.boolean().default(false), createdAt: z.string(),
}).strict()
const attemptSchema = z.object({
  clientRequestId: z.string(), runId: z.string(), brief: z.string(), status: statusSchema,
  completedStages: z.number().int().nonnegative(), totalStages: z.number().int().nonnegative(), latestStage: z.string(),
  deliverableCount: z.number().int().nonnegative(), deliverables: z.array(deliverableSchema),
  createdAt: z.number().nonnegative(), updatedAt: z.number().nonnegative(),
}).strict()
const pendingActionSchema = z.object({
  kind: z.enum(['stop', 'rerun', 'correction-request', 'correction-confirm']), targetRunId: z.string(), idempotencyKey: z.string(),
  clientRequestId: z.string(), brief: z.string(), requestedAt: z.number().nonnegative(),
  targetKind: z.enum(['team', 'member', '']).default(''), targetMemberId: z.string().default(''),
  correctionId: z.string().default(''), disposition: z.enum(['apply', 'discard', '']).default(''),
  instruction: z.string().default(''),
}).strict()
const correctionSchema = z.object({
  correctionId: z.string(), targetKind: z.enum(['team', 'member']), targetMemberId: z.string(),
  instruction: z.string(), status: z.enum(['requested', 'ready', 'confirmed', 'discarded', 'applied']),
  safeNodeId: z.string(), restartNodeId: z.string(), affectedNodeIds: z.array(z.string()),
  preservedNodeIds: z.array(z.string()), requestedAt: z.string(),
}).strict()
const completenessSchema = z.record(z.string(), z.enum(['complete', 'partial', 'unavailable']))
const taskSchema = z.object({
  brief: z.string().default(''),
  clientRequestId: z.string(), runId: z.string(), teamId: z.string(), teamName: z.string(),
  workflowName: z.string(), status: statusSchema,
  completedStages: z.number().int().nonnegative(), totalStages: z.number().int().nonnegative(), latestStage: z.string(),
  members: z.array(memberSchema).default([]), corrections: z.array(correctionSchema).default([]),
  runtimes: z.array(runtimeSchema), humanTaskCount: z.number().int().nonnegative(),
  deliverableCount: z.number().int().nonnegative(),
  deliverables: z.array(deliverableSchema).default([]),
  blocker: z.enum(['none', 'queued', 'runtime-missing', 'failed']),
  attempts: z.array(attemptSchema).default([]), pendingAction: pendingActionSchema.nullable().default(null),
  actionError: z.string().default(''),
  completeness: completenessSchema.default({}),
  startedAt: z.string().default(''), finishedAt: z.string().default(''),
  tokensIn: z.number().int().nonnegative().default(0), tokensOut: z.number().int().nonnegative().default(0),
  costUSD: z.number().nonnegative().default(0),
  outcome: z.enum(['unrated', 'adopted', 'needs-revision']).default('unrated'), outcomeNote: z.string().default(''),
  observedAt: z.number().nonnegative().default(0),
  updatedAt: z.number().nonnegative(),
}).strict()
const stateSchema = z.object({
  task: taskSchema.nullable(),
  pendingCalls: z.record(z.string(), z.object({ name: z.string(), args: z.unknown() })),
  teams: z.record(z.string(), z.string()),
}).strict()

function object(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined
}

function parsed(value: string): unknown {
  try { return JSON.parse(value) as unknown } catch { return value }
}

function deepValue(value: unknown, keys: ReadonlySet<string>, depth = 0): unknown {
  if (depth > 5) return undefined
  const item = object(value)
  if (item !== undefined) {
    for (const [key, candidate] of Object.entries(item)) if (keys.has(key)) return candidate
    for (const candidate of Object.values(item)) {
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

function text(value: unknown, keys: readonly string[]): string {
  const found = deepValue(value, new Set(keys))
  return typeof found === 'string' ? found.trim() : ''
}

function preferredText(value: unknown, keys: readonly string[]): string {
  const item = object(value)
  if (item !== undefined) {
    for (const key of keys) {
      const candidate = item[key]
      if (typeof candidate === 'string' && candidate.trim() !== '') return candidate.trim()
    }
  }
  return text(value, keys)
}

function count(value: unknown, keys: readonly string[]): number {
  const found = deepValue(value, new Set(keys))
  if (typeof found === 'number' && Number.isFinite(found)) return Math.max(0, Math.floor(found))
  if (typeof found === 'string' && /^\d+$/.test(found)) return Number(found)
  return 0
}

function finiteNumber(value: unknown, keys: readonly string[]): number | undefined {
  const found = deepValue(value, new Set(keys))
  if (typeof found === 'number' && Number.isFinite(found)) return Math.max(0, found)
  if (typeof found === 'string' && /^\d+(?:\.\d+)?$/.test(found)) return Number(found)
  return undefined
}

function status(value: unknown): WorkTaskStatus {
  const raw = text(value, ['status', 'state', 'run_status']).toLowerCase().replaceAll('-', '_')
  if (['completed', 'complete', 'succeeded', 'success', 'done'].includes(raw)) return 'completed'
  if (['cancelled', 'canceled', 'abandoned', 'stopped'].includes(raw)) return 'stopped'
  if (['failed', 'error'].includes(raw)) return 'failed'
  if (raw === 'cancel_requested') return 'stopping'
  if (['parked', 'waiting', 'yielded', 'waiting_for_human', 'needs_input', 'blocked', 'paused'].includes(raw)) return 'waiting'
  if (['queued', 'pending', 'admitting'].includes(raw)) return 'queued'
  if (['running', 'active', 'in_progress', 'working'].includes(raw)) return 'running'
  return 'preparing'
}

function terminalStatus(value: WorkTaskStatus): boolean {
  return value === 'completed' || value === 'failed' || value === 'stopped'
}

function resultValue(event: Extract<SessionEvent, { type: 'tool/result' }>): unknown {
  const content = event.data.message.content[0].content
  const joined = content.map(block => block.type === 'text' ? block.text : '').filter(Boolean).join('\n')
  return parsed(joined)
}

function listedTeams(value: unknown): Record<string, string> {
  const nested = deepValue(value, new Set(['teams', 'items']))
  const items = Array.isArray(value) ? value : Array.isArray(nested) ? nested : []
  return Object.fromEntries(items.flatMap((candidate): [string, string][] => {
    const id = text(candidate, ['team_id', 'teamId', 'id'])
    return id === '' ? [] : [[id, text(candidate, ['name']) || id]]
  }))
}

function runtimeList(value: unknown, taskStatus: WorkTaskStatus): WorkTaskRuntime[] {
  const source = deepValue(value, new Set(['runtime_assignment', 'runtimes', 'agents', 'workers', 'executors']))
  if (source === undefined || source === null) return []
  const entries = Array.isArray(source)
    ? source.map((item, index) => [String(index + 1), item] as const)
    : Object.entries(object(source) ?? {})
  return entries.flatMap(([fallback, candidate]): WorkTaskRuntime[] => {
    const item = object(candidate)
    const name = item === undefined ? fallback : preferredText(item, ['name', 'runtime_name', 'role', 'agent_name', 'id']) || fallback
    const detail = item === undefined ? String(candidate ?? '') : [
      text(item, ['engine']), text(item, ['provider']), text(item, ['model', 'model_name']), text(item, ['mode']),
    ].filter(Boolean).join(' · ')
    const candidateStatus = status(candidate)
    return name === '' ? [] : [{ name, detail, status: candidateStatus === 'preparing' ? taskStatus : candidateStatus }]
  })
}

function memberStatus(value: unknown): WorkTaskMemberStatus {
  const raw = text(value, ['status', 'state']).toLowerCase().replaceAll('_', '-')
  if (raw === 'completed' || raw === 'finished') return 'completed'
  if (raw === 'partially-completed') return 'partially-completed'
  if (raw === 'running' || raw === 'active') return 'running'
  if (raw === 'failed') return 'failed'
  if (raw === 'stopped' || raw === 'cancelled' || raw === 'abandoned') return 'stopped'
  if (raw === 'not-recorded' || raw === 'known') return 'not-recorded'
  return 'pending'
}

function memberInputs(value: unknown): WorkTaskMemberInput[] {
  if (!Array.isArray(value)) return []
  return value.flatMap((candidate): WorkTaskMemberInput[] => {
    const item = object(candidate)
    if (item === undefined) return []
    const name = text(item, ['name'])
    if (name === '') return []
    return [{
      name, expectedType: text(item, ['expected_type', 'expectedType']),
      source: text(item, ['source']), nodeId: text(item, ['node_id', 'nodeId']),
      path: text(item, ['path']), summary: text(item, ['summary']),
    }]
  })
}

function memberStages(value: unknown): WorkTaskMemberStage[] {
  if (!Array.isArray(value)) return []
  return value.flatMap((candidate): WorkTaskMemberStage[] => {
    const item = object(candidate)
    if (item === undefined) return []
    const nodeId = text(item, ['node_id', 'nodeId'])
    if (nodeId === '') return []
    const rawOutputs = item.output_refs ?? item.outputRefs
    const rawTools = Array.isArray(item.tools) ? item.tools : []
    return [{
      nodeId, name: text(item, ['name', 'label']) || nodeId, status: memberStatus(item),
      inputs: memberInputs(item.inputs),
      outputRefs: Array.isArray(rawOutputs) ? rawOutputs.filter((entry): entry is string => typeof entry === 'string') : [],
      startedAt: text(item, ['started_at', 'startedAt']), completedAt: text(item, ['completed_at', 'completedAt']),
      durationMs: count(item, ['duration_ms', 'durationMs']), toolCalls: count(item, ['tool_calls', 'toolCalls']),
      tools: rawTools.flatMap((candidate): WorkTaskMemberTool[] => {
        const tool = object(candidate)
        if (tool === undefined) return []
        const name = text(tool, ['name'])
        const rawStatus = text(tool, ['status'])
        if (name === '' || !['running', 'ok', 'error'].includes(rawStatus)) return []
        return [{ callId: text(tool, ['call_id', 'callId']), name, status: rawStatus as WorkTaskMemberTool['status'],
          startedAt: text(tool, ['started_at', 'startedAt']), completedAt: text(tool, ['completed_at', 'completedAt']),
          input: text(tool, ['input']), output: text(tool, ['output']) }]
      }),
    }]
  })
}

function correctionList(value: unknown): WorkTaskCorrection[] {
  const source = deepValue(value, new Set(['corrections']))
  if (!Array.isArray(source)) return []
  return source.flatMap((candidate): WorkTaskCorrection[] => {
    const item = object(candidate)
    if (item === undefined) return []
    const correctionId = text(item, ['correction_id', 'correctionId'])
    const targetKind = text(item, ['target_kind', 'targetKind'])
    const rawStatus = text(item, ['status'])
    if (correctionId === '' || (targetKind !== 'team' && targetKind !== 'member')
			|| !['requested', 'ready', 'confirmed', 'discarded', 'applied'].includes(rawStatus)) return []
    const strings = (raw: unknown): string[] => Array.isArray(raw)
      ? raw.filter((entry): entry is string => typeof entry === 'string') : []
    return [{
      correctionId, targetKind, targetMemberId: text(item, ['target_member_id', 'targetMemberId']),
      instruction: text(item, ['instruction']), status: rawStatus as WorkTaskCorrection['status'],
      safeNodeId: text(item, ['safe_node_id', 'safeNodeId']), restartNodeId: text(item, ['restart_node_id', 'restartNodeId']),
      affectedNodeIds: strings(item.affected_node_ids ?? item.affectedNodeIds),
      preservedNodeIds: strings(item.preserved_node_ids ?? item.preservedNodeIds),
      requestedAt: text(item, ['requested_at', 'requestedAt']),
    }]
  })
}

function memberList(value: unknown): WorkTaskMember[] {
  const source = deepValue(value, new Set(['members']))
  if (!Array.isArray(source)) return []
  return source.flatMap((candidate): WorkTaskMember[] => {
    const item = object(candidate)
    if (item === undefined) return []
    const agentId = text(item, ['agent_id', 'agentId'])
    if (agentId === '') return []
    const runtime = object(item.runtime)
    const runtimeDetail = runtime === undefined ? '' : [
      preferredText(runtime, ['name', 'runtime_name', 'runtime_id', 'runtimeId']), text(runtime, ['engine']),
      [text(runtime, ['provider']), text(runtime, ['model'])].filter(Boolean).join('/'),
    ].filter(Boolean).join(' · ')
    return [{
      agentId, name: text(item, ['name', 'display_name']) || agentId,
      duty: text(item, ['duty']), role: text(item, ['role']) === 'lead' ? 'lead' : 'worker',
      status: memberStatus(item), runtime: runtimeDetail, stages: memberStages(item.stages),
    }]
  })
}

function matchingCount(value: unknown, runId: string, collectionKeys: readonly string[]): number {
  if (runId === '') return 0
  const nested = deepValue(value, new Set(collectionKeys))
  const candidates = Array.isArray(value) ? value : Array.isArray(nested) ? nested : []
  return candidates.filter(candidate => text(candidate, ['run_id', 'runId', 'run_snapshot_id']) === runId).length
}

function collectionCount(value: unknown, collectionKeys: readonly string[]): number {
  const nested = deepValue(value, new Set(collectionKeys))
  return Array.isArray(value) ? value.length : Array.isArray(nested) ? nested.length : 0
}

function latestRecordedStage(value: unknown): string {
  const nested = deepValue(value, new Set(['stages', 'workflow_stages', 'steps']))
  if (!Array.isArray(nested)) return ''
  for (let index = nested.length - 1; index >= 0; index--) {
    const name = text(nested[index], ['name', 'title', 'stage_name', 'label'])
    if (name !== '') return name
  }
  return ''
}

function deliverableList(value: unknown, runId: string): WorkTaskDeliverable[] {
  if (runId === '') return []
  const nested = deepValue(value, new Set(['deliverables', 'items']))
  const candidates = Array.isArray(value) ? value : Array.isArray(nested) ? nested : []
  const seen = new Set<string>()
  return candidates.flatMap((candidate): WorkTaskDeliverable[] => {
    const item = object(candidate)
    if (item === undefined || text(item, ['run_id', 'runId']) !== runId) return []
    const id = text(item, ['id', 'deliverable_id'])
    if (id === '' || seen.has(id)) return []
    seen.add(id)
    const rawMetadata = item.metadata
    const metadata = typeof rawMetadata === 'string' ? parsed(rawMetadata) : rawMetadata
    const publishedWorkflow = text(metadata, ['source']) === 'published_workflow'
    const filename = text(metadata, ['filename'])
    const nodeType = text(metadata, ['node_type'])
    const rawKind = text(metadata, ['artifact_kind'])
    const kind = rawKind === 'final' && !(publishedWorkflow && filename === '' && nodeType === 'deliver') ? 'final' : 'stage'
    const content = typeof item.content === 'string' ? item.content : ''
    const contentLimit = 256 * 1024
    return [{
      id,
      title: text(item, ['title', 'name']) || id,
      kind,
      contentType: text(item, ['content_type', 'contentType']) || 'text/plain',
      preview: content.trim().slice(0, 6_000),
      content: content.slice(0, contentLimit),
      truncated: content.length > contentLimit,
      createdAt: text(item, ['created_at', 'createdAt']),
    }]
  })
}

function completeness(value: unknown): Readonly<Record<string, 'complete' | 'partial' | 'unavailable'>> {
  const raw = object(deepValue(value, new Set(['completeness'])))
  if (raw === undefined) return {}
  return Object.fromEntries(Object.entries(raw).flatMap(([key, candidate]) =>
    candidate === 'complete' || candidate === 'partial' || candidate === 'unavailable'
      ? [[key, candidate]]
      : []))
}

function observedAt(value: unknown, fallback: number): number {
  const raw = text(value, ['observed_at', 'observedAt'])
  const parsedTime = raw === '' ? Number.NaN : Date.parse(raw)
  return Number.isFinite(parsedTime) ? parsedTime : fallback
}

function syncAttempt(task: Omit<WorkTaskProjection, 'attempts'>, previous: WorkTaskProjection | null, createdAt: number): WorkTaskAttempt[] {
  if (task.runId === '' && task.clientRequestId === '') return previous?.attempts ?? []
  const attempt: WorkTaskAttempt = {
    clientRequestId: task.clientRequestId,
    runId: task.runId,
    brief: task.brief,
    status: task.status,
    completedStages: task.completedStages,
    totalStages: task.totalStages,
    latestStage: task.latestStage,
    deliverableCount: task.deliverableCount,
    deliverables: task.deliverables,
    createdAt,
    updatedAt: task.updatedAt,
  }
  const attempts = [...(previous?.attempts ?? [])]
  const index = attempts.findIndex(candidate =>
    (task.runId !== '' && candidate.runId === task.runId)
    || (task.clientRequestId !== '' && candidate.clientRequestId === task.clientRequestId))
  if (index < 0) attempts.push(attempt)
  else attempts[index] = { ...attempt, createdAt: attempts[index]?.createdAt ?? createdAt }
  return attempts
}

function resyncAttempts(task: WorkTaskProjection, previous: WorkTaskProjection | null): WorkTaskProjection {
  const { attempts: _attempts, ...base } = task
  return { ...task, attempts: syncAttempt(base, previous, task.updatedAt) }
}

function snapshot(
  previous: WorkTaskProjection | null,
  value: unknown,
  now: number,
  seed: Partial<WorkTaskProjection> = {},
): WorkTaskProjection {
  const nextStatus = status(value)
  const runtimeMissing = text(value, ['classification']) === 'terminal_missing'
  const nextRuntimes = runtimeList(value, nextStatus)
  const rawMembers = deepValue(value, new Set(['members']))
  const rawCorrections = deepValue(value, new Set(['corrections']))
  const nextMembers = memberList(value)
  const memberStages = nextMembers.flatMap(member => member.stages)
  const activeMemberStage = nextMembers.flatMap(member => member.stages)
    .find(stage => stage.status === 'running')?.name ?? ''
  const nextRunId = text(value, ['run_id', 'runId']) || seed.runId || previous?.runId || ''
  const sameRun = nextRunId !== '' && previous?.runId === nextRunId
  const completedStages = memberStages.length > 0
    ? memberStages.filter(stage => stage.status === 'completed').length
    : count(value, ['completed_stages', 'stages_completed', 'completed_count'])
      || (sameRun ? previous?.completedStages ?? 0 : 0)
  const totalStages = memberStages.length > 0
    ? memberStages.length
    : count(value, ['total_stages', 'stages_total', 'stage_count'])
      || (sameRun ? previous?.totalStages ?? 0 : 0)
  const retainedRuntimes = nextRuntimes.length === 0 ? (sameRun ? previous?.runtimes ?? [] : []) : nextRuntimes
  const observed = observedAt(value, now)
  const base: Omit<WorkTaskProjection, 'attempts'> = {
    brief: seed.brief ?? previous?.brief ?? '',
    clientRequestId: text(value, ['client_request_id', 'clientRequestId']) || seed.clientRequestId || previous?.clientRequestId || '',
    runId: nextRunId,
    teamId: seed.teamId || previous?.teamId || '', teamName: seed.teamName || previous?.teamName || '',
    workflowName: seed.workflowName || previous?.workflowName || '',
    status: runtimeMissing ? 'waiting' : nextStatus === 'preparing' ? previous?.status ?? 'preparing' : nextStatus,
    completedStages,
    totalStages,
    latestStage: activeMemberStage || text(value, ['latest_stage', 'current_stage'])
		|| latestRecordedStage(value) || (sameRun ? previous?.latestStage ?? '' : ''),
    members: rawMembers === undefined ? (sameRun ? previous?.members ?? [] : []) : nextMembers,
    corrections: rawCorrections === undefined ? (sameRun ? previous?.corrections ?? [] : []) : correctionList(value),
    runtimes: terminalStatus(nextStatus)
      ? retainedRuntimes.map(runtime => ({ ...runtime, status: nextStatus }))
      : retainedRuntimes,
    humanTaskCount: seed.humanTaskCount ?? (sameRun ? previous?.humanTaskCount ?? 0 : 0),
    deliverableCount: seed.deliverableCount ?? (sameRun ? previous?.deliverableCount ?? 0 : 0),
    deliverables: seed.deliverables ?? (sameRun ? previous?.deliverables ?? [] : []),
    blocker: runtimeMissing ? 'runtime-missing' : nextStatus === 'failed' ? 'failed' : nextStatus === 'queued' ? 'queued' : 'none',
    pendingAction: seed.pendingAction === undefined ? previous?.pendingAction ?? null : seed.pendingAction,
    actionError: seed.actionError ?? previous?.actionError ?? '',
    completeness: Object.keys(completeness(value)).length === 0 ? (sameRun ? previous?.completeness ?? {} : {}) : completeness(value),
    startedAt: text(value, ['started_at', 'startedAt']) || (sameRun ? previous?.startedAt ?? '' : ''),
    finishedAt: text(value, ['ended_at', 'endedAt', 'terminal_at', 'terminalAt', 'finished_at', 'finishedAt']) || (sameRun ? previous?.finishedAt ?? '' : ''),
    tokensIn: Math.floor(finiteNumber(value, ['tokens_in', 'tokensIn', 'input_tokens']) ?? (sameRun ? previous?.tokensIn ?? 0 : 0)),
    tokensOut: Math.floor(finiteNumber(value, ['tokens_out', 'tokensOut', 'output_tokens']) ?? (sameRun ? previous?.tokensOut ?? 0 : 0)),
    costUSD: finiteNumber(value, ['cost_usd', 'costUSD']) ?? (sameRun ? previous?.costUSD ?? 0 : 0),
    outcome: sameRun ? previous?.outcome ?? 'unrated' : 'unrated',
    outcomeNote: sameRun ? previous?.outcomeNote ?? '' : '',
    observedAt: observed,
    updatedAt: now,
  }
  return { ...base, attempts: syncAttempt(base, previous, now) }
}

/**
 * Pure durable fold shared by live updates, cold lists, and restored sessions.
 * @param state - current task projection state and pending Weave-call correlations.
 * @param event - next committed Session event.
 * @returns updated state, or the original reference for an unrelated event.
 */
export function applyWorkTaskProjection(state: WorkTaskState, event: SessionEvent): WorkTaskState {
  if (event.type === 'weave/work-task') return { ...state, task: event.data }
  if (event.type === 'weave/work-task-action') {
    return state.task === null
      ? state
      : { ...state, task: { ...state.task, pendingAction: event.data.pendingAction, actionError: '' } }
  }
  if (event.type === 'tool/call' && event.data.name.startsWith('mcp__weave__')) {
    const pending = { name: event.data.name, args: parsed(event.data.arguments) }
    return { ...state, pendingCalls: { ...state.pendingCalls, [event.data.callId]: pending } }
  }
  if (event.type !== 'tool/result') return state
  const callId = event.data.message.source.callId
  const call = state.pendingCalls[callId]
  if (call === undefined) return state
  const pendingCalls = Object.fromEntries(Object.entries(state.pendingCalls).filter(([id]) => id !== callId))
  if (event.data.message.content[0].isError) return { ...state, pendingCalls }
  const value = resultValue(event)
  if (call.name === 'mcp__weave__team_list') return { ...state, pendingCalls, teams: { ...state.teams, ...listedTeams(value) } }
  if (call.name === 'mcp__weave__team_dispatch') {
    const teamId = text(call.args, ['team_id', 'teamId', 'team'])
    const task = snapshot(state.task, value, event.time, {
      brief: text(call.args, ['task']),
      teamId, teamName: state.teams[teamId] ?? teamId,
      workflowName: text(call.args, ['workflow_name', 'workflow_id', 'workflow']),
      clientRequestId: text(value, ['client_request_id', 'clientRequestId']) || text(call.args, ['client_request_id', 'clientRequestId']),
    })
    return { ...state, pendingCalls, task }
  }
  if (state.task === null) return { ...state, pendingCalls }
  if (call.name === 'mcp__weave__dispatch_status'
    || call.name === 'mcp__weave__team_run_status'
    || call.name === 'mcp__weave__team_run_activity') {
    return { ...state, pendingCalls, task: snapshot(state.task, value, event.time) }
  }
  return { ...state, pendingCalls }
}

/** Projection definition registered with the Session projection registry. */
export const workTaskProjectionDefinition = {
  key: 'workTask', stateVersion: 8, stateSchema,
  init: (): WorkTaskState => ({ task: null, pendingCalls: {}, teams: {} }),
  apply: applyWorkTaskProjection,
  wire: { viewSchema: taskSchema.nullable(), view: (state: WorkTaskState) => state.task },
} satisfies ProjectionDefinition<'workTask', WorkTaskState>

/** Host-only WorkTask synchronization settings. */
export interface Config {
  /** Weave HTTP API origin; defaults to `WEAVE_API_URL` and then the local development endpoint. */
  readonly apiUrl?: string
  /** Business API credential; defaults to host-only `WEAVE_API_KEY`. */
  readonly apiKey?: string
  /** Delay between non-terminal status reads in milliseconds. */
  readonly pollIntervalMs?: number
}
export const name = 'workbench-work-task'
export const inject = ['sessions', 'sessionProjections', 'commands', 'systemPrompt', 'connection']

/** Product policy that remains visible when a per-session agent preset shadows the deployment persona. */
export const workbenchTeamRoutingSection = {
  name: 'workbench:team-routing',
  order: FIRST_PARTY_SECTION_ORDER.TEAM_POLICY + 10,
  text: 'For substantive business work in Weave Workbench, first list the available Weave teams and match the request against each team\'s stated purpose, responsibilities, success criteria, default workflow availability, and health. If a suitable active team exists, dispatch its default workflow with wait=false. A successful Weave dispatch is already the durable task: do not create or update a DSH goal for it. After dispatch, make at most one activity or status call to confirm the handoff, then return the team, run ID, and current status immediately. Do not poll the run in the foreground, and do not save a duplicate foreground deliverable; Workbench monitors the run and projects Weave\'s deliverables in the background. Ask the user only when a human task requires input. When the run is terminal or the user later asks for the result, read the final Weave deliverable and answer with a short user-facing completion summary: what was finished, the main findings or decisions, the files the user can open, and any user action still needed. Keep internal run IDs, deliverable IDs, runtime IDs, host paths, hashes, validation command names, and engine details out of the main answer unless the user explicitly asks for technical details. If no suitable team exists, say so plainly and collaborate with the user on a team definition. Never select free collaboration unless the user explicitly requests it. If Weave tools are unavailable, report that the Weave connection is not configured instead of pretending that team work was performed.',
} as const

/** Register the durable projection and keep non-terminal Weave runs synchronized outside the conversation turn. */
export function apply(ctx: Context, config: Config = {}): void {
  ctx.effect(() => ctx.systemPrompt.section(workbenchTeamRoutingSection), 'workbench team-routing policy')
  ctx.sessionProjections.register(workTaskProjectionDefinition)
  const apiUrl = (config.apiUrl ?? process.env.WEAVE_API_URL ?? 'http://127.0.0.1:18080').replace(/\/$/, '')
  const apiKey = (config.apiKey ?? process.env.WEAVE_API_KEY ?? '').trim()
  const connection = Reflect.get(ctx, 'connection') as {
    readonly fetch: { register(route: { readonly path: string; readonly methods: readonly ('GET' | 'HEAD')[]; readonly fetch: (request: Request) => Promise<Response> }): () => Promise<void> }
  }
  const head = async (request: Request, response: Response): Promise<Response> => {
    if (request.method === 'GET') return response
    await response.body?.cancel()
    return new Response(null, { status: response.status, headers: response.headers })
  }
  connection.fetch.register({
    path: '/api/weave.status', methods: ['GET', 'HEAD'],
    fetch: async request => head(request, Response.json(await inspectWeaveReadiness(apiUrl, apiKey), {
      headers: { 'Cache-Control': 'no-store' },
    })),
  })
  connection.fetch.register({
    path: '/api/weave.pilot-report', methods: ['GET', 'HEAD'],
    fetch: async (request) => {
      const report = buildPilotReport(ctx.sessions, ctx.sessionProjections)
      const date = report.generatedAt.slice(0, 10)
      return head(request, new Response(JSON.stringify(report, null, 2), {
        headers: {
          'Cache-Control': 'no-store',
          'Content-Type': 'application/json; charset=utf-8',
          'Content-Disposition': `attachment; filename="weave-pilot-report-${date}.json"`,
        },
      }))
    },
  })
  const pollIntervalMs = Math.max(500, config.pollIntervalMs ?? 2_000)
  const timers = new Map<string, ReturnType<typeof setTimeout>>()
  const liveSessions = new Map<string, Session>()
  const terminalChecked = new Set<string>()
  const sessionKey = (session: Session): string => String(session.id)
  const terminal = (task: WorkTaskProjection): boolean => terminalStatus(task.status)
  const stop = (session: Session): void => {
    const key = sessionKey(session)
    const timer = timers.get(key)
    if (timer !== undefined) clearTimeout(timer)
    timers.delete(key)
    liveSessions.delete(key)
  }
  const schedule = (session: Session, delay = 0): void => {
    const key = sessionKey(session)
    liveSessions.set(key, session)
    if (apiKey === '' || timers.has(key)) return
    const current = ctx.sessionProjections.stateOf(session, 'workTask')?.task
    if (current === null || current === undefined || (current.clientRequestId === '' && current.runId === '')) return
    if (!terminal(current)) terminalChecked.delete(key)
    if (terminal(current) && current.pendingAction === null && terminalChecked.has(key)) return
    timers.set(key, setTimeout(() => {
      timers.delete(key)
      const latest = liveSessions.get(key)
      if (latest !== undefined) void poll(latest)
    }, delay))
  }
  const poll = async (session: Session): Promise<void> => {
    const current = ctx.sessionProjections.stateOf(session, 'workTask')?.task
    if (current === null || current === undefined) return
    try {
      const request = (path: string, init: RequestInit = {}): Promise<Response> => {
        const headers = new Headers(init.headers)
        headers.set('Authorization', `Bearer ${apiKey}`)
        if (!headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
        return fetch(`${apiUrl}${path}`, { ...init, headers, signal: AbortSignal.timeout(15_000) })
      }
      if (current.pendingAction !== null) {
        const action = current.pendingAction
        let response: Response
        if (action.kind === 'stop') {
          response = await request(`/v1/runs/${encodeURIComponent(action.targetRunId)}/stop`, {
            method: 'POST', body: JSON.stringify({ reason: 'workbench_user_requested', idempotency_key: action.idempotencyKey }),
          })
        } else if (action.kind === 'rerun') {
          response = await request(`/v1/teams/${encodeURIComponent(current.teamId)}/dispatch`, {
            method: 'POST', body: JSON.stringify({ task: action.brief, mode: 'workflow', client_request_id: action.clientRequestId,
              ...(current.workflowName === '' ? {} : { workflow_id: current.workflowName }) }),
          })
        } else if (action.kind === 'correction-request') {
          response = await request(`/v1/runs/${encodeURIComponent(action.targetRunId)}/corrections`, {
            method: 'POST', body: JSON.stringify({ target_kind: action.targetKind,
              ...(action.targetMemberId === '' ? {} : { target_member_id: action.targetMemberId }),
              instruction: action.instruction, idempotency_key: action.idempotencyKey }),
          })
        } else {
          response = await request(`/v1/runs/${encodeURIComponent(action.targetRunId)}/corrections/${encodeURIComponent(action.correctionId)}/confirm`, {
            method: 'POST', body: JSON.stringify({ disposition: action.disposition, idempotency_key: action.idempotencyKey }),
          })
        }
        if (response.ok) {
          const value = await response.json() as unknown
          let next: WorkTaskProjection
          if (action.kind === 'rerun') {
            next = snapshot(current, value, Date.now(), { brief: action.brief, clientRequestId: action.clientRequestId,
              runId: text(value, ['run_id', 'runId']), pendingAction: null, actionError: '' })
          } else if (action.kind === 'correction-request') {
            const correction = correctionList({ corrections: [value] })[0]
            const corrections = correction === undefined
              ? current.corrections
              : [correction, ...current.corrections.filter(item => item.correctionId !== correction.correctionId)]
            next = { ...current, pendingAction: null, actionError: '',
              corrections,
              updatedAt: Date.now() }
          } else if (action.kind === 'correction-confirm') {
            next = { ...current, pendingAction: null, actionError: '', updatedAt: Date.now() }
          } else {
            next = snapshot(current, value, Date.now(), { pendingAction: null, actionError: '' })
          }
          session.append('weave/work-task', next)
          await ctx.sessions.flush(session)
          if (action.kind === 'rerun') terminalChecked.delete(sessionKey(session))
          schedule(session, 0)
          return
        }
        if (response.status >= 400 && response.status < 500) {
          let message = `Weave rejected the ${action.kind} command (${response.status}).`
          try {
            const value = object(await response.json() as unknown)
            if (typeof value?.error === 'string' && value.error.trim() !== '') message = value.error
          } catch { /* The status code remains authoritative. */ }
          const next = { ...current, pendingAction: null, actionError: message, updatedAt: Date.now() }
          session.append('weave/work-task', next)
          await ctx.sessions.flush(session)
          schedule(session, 0)
          return
        }
      }
      const response = current.runId !== ''
        ? await request(`/v1/runs/${encodeURIComponent(current.runId)}/activity`)
        : await request(`/v1/chat-requests/${encodeURIComponent(current.clientRequestId)}`)
      if (response.ok) {
        const activity = await response.json() as unknown
        let next = snapshot(current, activity, Date.now())
        if (next.status === 'waiting' && next.runId !== '') {
          const humanResponse = await request('/v1/human-tasks?limit=100')
          if (humanResponse.ok) {
            next = { ...next, humanTaskCount: matchingCount(await humanResponse.json() as unknown, next.runId, ['items', 'tasks']) }
          }
        }
        const reportedDeliverables = collectionCount(activity, ['deliverables', 'items'])
        if (next.runId !== '' && (terminal(next) || reportedDeliverables !== next.deliverableCount)) {
          const deliverableResponse = await request(`/v1/deliverables?run_id=${encodeURIComponent(next.runId)}&limit=100`)
          if (deliverableResponse.ok) {
            const value = await deliverableResponse.json() as unknown
            const deliverables = deliverableList(value, next.runId)
            next = { ...next, deliverableCount: deliverables.length, deliverables }
          }
        }
        next = resyncAttempts(next, current)
        const materiallyChanged = JSON.stringify({ ...next, observedAt: 0, updatedAt: 0 })
          !== JSON.stringify({ ...current, observedAt: 0, updatedAt: 0 })
        if (materiallyChanged || next.observedAt - current.observedAt >= 30_000) {
          next = { ...next, updatedAt: Date.now() }
          session.append('weave/work-task', next)
          await ctx.sessions.flush(session)
        }
        if (terminal(next)) { terminalChecked.add(sessionKey(session)); stop(session); return }
      }
    } catch { /* Transient connection loss must not become a false task failure. */ }
    schedule(session, pollIntervalMs)
  }
  ctx.on('session/created', (session) => { queueMicrotask(() => { schedule(session) }) })
  ctx.on('session/event', (session, event) => {
    if (event.type === 'tool/result' || event.type === 'weave/work-task' || event.type === 'weave/work-task-action') queueMicrotask(() => { schedule(session) })
  })
  // The plugin may mount after persistence has restored live Sessions, so
  // session/created alone is insufficient to resume background ownership.
  for (const session of ctx.sessions.list()) queueMicrotask(() => { schedule(session) })
  ctx.effect(() => ctx.commands.register({
    name: 'weave-stop',
    description: 'stop the exact current Weave run without invoking the model',
    recordInput: false,
    handler: async ({ agent, rawInput }) => {
      const current = ctx.sessionProjections.stateOf(agent.session, 'workTask')?.task
      let request: { runId?: unknown }
      try { request = JSON.parse(rawInput.trim()) as { runId?: unknown } } catch { return { kind: 'error', text: 'Invalid Weave stop request.' } }
      if (current === null || current === undefined || typeof request.runId !== 'string' || request.runId !== current.runId || terminal(current)) {
        return { kind: 'error', text: 'The selected Weave run is no longer stoppable.' }
      }
      if (current.pendingAction !== null) return { kind: 'error', text: 'Another Weave action is already pending.' }
      const pendingAction: WorkTaskPendingAction = {
        kind: 'stop', targetRunId: current.runId,
        idempotencyKey: `workbench-stop:${randomUUID()}`,
        clientRequestId: '', brief: '', requestedAt: Date.now(),
        targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: '',
      }
      agent.session.append('weave/work-task-action', { pendingAction })
      await ctx.sessions.flush(agent.session)
      schedule(agent.session, 0)
      return { kind: 'success', text: 'Weave stop request recorded.' }
    },
  }), 'workbench: exact run stop command')
  ctx.effect(() => ctx.commands.register({
    name: 'weave-rerun',
    description: 'start a new Weave run from a fully revised brief without invoking the model',
    recordInput: false,
    handler: async ({ agent, rawInput }) => {
      const current = ctx.sessionProjections.stateOf(agent.session, 'workTask')?.task
      let request: { runId?: unknown; brief?: unknown }
      try { request = JSON.parse(rawInput.trim()) as { runId?: unknown; brief?: unknown } } catch { return { kind: 'error', text: 'Invalid Weave rerun request.' } }
      const brief = typeof request.brief === 'string' ? request.brief.trim() : ''
      if (current === null || current === undefined || !terminal(current)
        || typeof request.runId !== 'string' || request.runId !== current.runId || brief === '') {
        return { kind: 'error', text: 'A terminal current run and a complete revised brief are required.' }
      }
      if (current.pendingAction !== null) return { kind: 'error', text: 'Another Weave action is already pending.' }
      const pendingAction: WorkTaskPendingAction = {
        kind: 'rerun', targetRunId: current.runId, idempotencyKey: '',
        clientRequestId: randomUUID(), brief, requestedAt: Date.now(),
        targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: '',
      }
      agent.session.append('weave/work-task-action', { pendingAction })
      await ctx.sessions.flush(agent.session)
      schedule(agent.session, 0)
      return { kind: 'success', text: 'Revised Weave run request recorded.' }
    },
  }), 'workbench: revised run command')
  ctx.effect(() => ctx.commands.register({
    name: 'weave-correct',
    description: 'request a durable member or whole-team correction at the next Weave safe point',
    recordInput: false,
    handler: async ({ agent, rawInput }) => {
      const current = ctx.sessionProjections.stateOf(agent.session, 'workTask')?.task
      let request: { runId?: unknown; targetKind?: unknown; targetMemberId?: unknown; instruction?: unknown }
      try { request = JSON.parse(rawInput.trim()) as typeof request } catch { return { kind: 'error', text: 'Invalid Weave correction request.' } }
      const targetKind = request.targetKind === 'team' || request.targetKind === 'member' ? request.targetKind : ''
      const targetMemberId = typeof request.targetMemberId === 'string' ? request.targetMemberId.trim() : ''
      const instruction = typeof request.instruction === 'string' ? request.instruction.trim() : ''
      if (current === null || current === undefined || typeof request.runId !== 'string' || request.runId !== current.runId
				|| terminal(current) || current.status === 'stopping' || targetKind === '' || instruction === ''
				|| (targetKind === 'member' && !current.members.some(member => member.agentId === targetMemberId))) {
        return { kind: 'error', text: 'A live run, valid target, and correction instruction are required.' }
      }
      if (current.pendingAction !== null || current.corrections.some(item => ['requested', 'ready', 'confirmed'].includes(item.status))) {
        return { kind: 'error', text: 'Another Weave action or correction is already active.' }
      }
      const pendingAction: WorkTaskPendingAction = {
        kind: 'correction-request', targetRunId: current.runId,
        idempotencyKey: `workbench-correction:${randomUUID()}`, clientRequestId: '', brief: '', requestedAt: Date.now(),
        targetKind, targetMemberId: targetKind === 'member' ? targetMemberId : '', correctionId: '', disposition: '', instruction,
      }
      agent.session.append('weave/work-task-action', { pendingAction })
      await ctx.sessions.flush(agent.session)
      schedule(agent.session, 0)
      return { kind: 'success', text: 'Weave correction request recorded.' }
    },
  }), 'workbench: correction request command')
  ctx.effect(() => ctx.commands.register({
    name: 'weave-confirm-correction',
    description: 'apply or discard a ready Weave correction plan without invoking the model',
    recordInput: false,
    handler: async ({ agent, rawInput }) => {
      const current = ctx.sessionProjections.stateOf(agent.session, 'workTask')?.task
      let request: { runId?: unknown; correctionId?: unknown; disposition?: unknown }
      try { request = JSON.parse(rawInput.trim()) as typeof request } catch { return { kind: 'error', text: 'Invalid Weave correction confirmation.' } }
      const correctionId = typeof request.correctionId === 'string' ? request.correctionId.trim() : ''
      const disposition = request.disposition === 'apply' || request.disposition === 'discard' ? request.disposition : ''
      const ready = current?.corrections.find(item => item.correctionId === correctionId && item.status === 'ready')
      if (current === null || current === undefined || typeof request.runId !== 'string' || request.runId !== current.runId
				|| ready === undefined || disposition === '') {
        return { kind: 'error', text: 'A ready correction impact plan is required.' }
      }
      if (current.pendingAction !== null) return { kind: 'error', text: 'Another Weave action is already pending.' }
      const pendingAction: WorkTaskPendingAction = {
        kind: 'correction-confirm', targetRunId: current.runId,
        idempotencyKey: `workbench-correction-confirm:${randomUUID()}`, clientRequestId: '', brief: '', requestedAt: Date.now(),
        targetKind: '', targetMemberId: '', correctionId, disposition, instruction: '',
      }
      agent.session.append('weave/work-task-action', { pendingAction })
      await ctx.sessions.flush(agent.session)
      schedule(agent.session, 0)
      return { kind: 'success', text: 'Weave correction confirmation recorded.' }
    },
  }), 'workbench: correction confirmation command')
  ctx.effect(() => ctx.commands.register({
    name: 'weave-assess',
    description: 'record whether the final Weave delivery is usable without invoking the model',
    recordInput: false,
    handler: async ({ agent, rawInput }) => {
      const current = ctx.sessionProjections.stateOf(agent.session, 'workTask')?.task
      let request: { runId?: unknown; outcome?: unknown; note?: unknown }
      try { request = JSON.parse(rawInput.trim()) as typeof request } catch { return { kind: 'error', text: 'Invalid Weave outcome assessment.' } }
      const outcome = request.outcome === 'adopted' || request.outcome === 'needs-revision' ? request.outcome : ''
      const note = typeof request.note === 'string' ? request.note.trim().slice(0, 2_000) : ''
      if (current === null || current === undefined || current.status !== 'completed'
        || request.runId !== current.runId || outcome === '') {
        return { kind: 'error', text: 'A completed current run and a valid outcome are required.' }
      }
      agent.session.append('weave/work-task', { ...current, outcome, outcomeNote: note, updatedAt: Date.now() })
      await ctx.sessions.flush(agent.session)
      return { kind: 'success', text: 'Weave delivery outcome recorded.' }
    },
  }), 'workbench: delivery outcome command')
  ctx.effect(() => () => {
    for (const timer of timers.values()) clearTimeout(timer)
    timers.clear()
    liveSessions.clear()
    terminalChecked.clear()
  }, 'workbench work-task pollers')
}
