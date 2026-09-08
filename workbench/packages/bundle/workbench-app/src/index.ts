/** Host-side durable Weave work-task projection and status synchronizer. */

import { handleWeaveDeliverableRequest } from './deliverable-content.ts'
import { randomUUID } from 'node:crypto'
import { isDeepStrictEqual } from 'node:util'
import type { Context } from '@deepseek-ai/cordis'
import type {} from '@deepseek-ai/dsh-commands'
import type {} from '@deepseek-ai/dsh-api-session-controller'
import { FIRST_PARTY_SECTION_ORDER } from '@deepseek-ai/dsh-system-prompt'
import { z } from 'zod'
import { SessionId, type Session, type SessionEvent } from '@deepseek-ai/dsh-session'
import type { ProjectionDefinition } from '@deepseek-ai/dsh-session-projection'
import { buildPilotReport } from './pilot-report.ts'
import { inspectWeaveReadiness } from './readiness.ts'
import { handleWeaveRuntimeRequest, resolveRuntimeServerUrl } from './runtime-control.ts'
import { handleAgentExecutionRequest } from './agent-execution-control.ts'
import { DispatchResponseError, installDispatchInputTool, latestDispatchInput, type DispatchInputFacts } from './dispatch-input.ts'

export { buildPilotReport } from './pilot-report.ts'
export type { PilotReport, PilotReportTask } from './pilot-report.ts'
export { inspectWeaveReadiness } from './readiness.ts'
export type { WeaveReadiness, WeaveReadinessCheck, WeaveReadinessTone } from './readiness.ts'
export { handleWeaveRuntimeRequest, resolveRuntimeServerUrl } from './runtime-control.ts'
export type { WeaveRuntimeEngineView, WeaveRuntimeList, WeaveRuntimeView } from './runtime-control.ts'

/** User-facing lifecycle of one Weave-dispatched task. */
export type WorkTaskStatus = 'preparing' | 'queued' | 'running' | 'waiting' | 'stopping' | 'completed' | 'failed' | 'stopped'

/** Saved Weave checks for one immutable delivery revision. */
export interface WorkTaskDelivery {
  readonly revisionId: string
  readonly contractDigest: string
  readonly verificationId: string
  readonly verificationStatus: 'pending' | 'passed' | 'failed' | 'unknown'
  readonly reason: string
  readonly checks: { readonly checkId: string; readonly status: 'pending' | 'passed' | 'failed' | 'unknown'; readonly reason: string }[]
  readonly checkCounts: Readonly<Record<string, number>>
  readonly available: boolean
  readonly evidenceCompleteness: 'complete' | 'unavailable'
}

/** Last user-authored assessment, retained when delivery evidence cannot be read. */
export interface WorkTaskAssessment {
  readonly runId: string
  readonly revisionId: string
  readonly outcome: 'adopted' | 'needs-revision'
  readonly note: string
  readonly assessedAt: number
}

/** Durable product view of one Weave dispatch owned by a Workbench session. */
export interface WorkTaskProjection {
  /** Preparation remains visible until dispatch; a transport error does not prove creation failed. */
  readonly preparation?: { readonly callId: string; readonly buildId: string; readonly updatedAt?: number | undefined; readonly state: 'submitting' | 'building' | 'ready' | 'failed' | 'unknown'; readonly error: string; readonly steps?: { readonly id: string; readonly label: string; readonly status: string; readonly attempt: number }[] | undefined } | undefined
  /** Host-derived meaning shared by every task surface. */
  readonly displayState?: string | undefined
  readonly brief: string
  readonly clientRequestId: string
  readonly runId: string
  readonly teamId: string
  readonly teamName: string
  readonly workflowName: string
  readonly status: WorkTaskStatus
  readonly waitKind: '' | 'timer' | 'fanout' | 'human' | 'correction' | 'runtime'
  readonly waitNodeId: string
  readonly completedStages: number
  readonly totalStages: number
  readonly latestStage: string
  readonly members: WorkTaskMember[]
  readonly corrections: WorkTaskCorrection[]
  readonly runtimes: WorkTaskRuntime[]
  readonly humanTaskCount: number
  readonly humanTask: WorkTaskHumanTask | null
  readonly deliverableCount: number
  readonly deliverables: WorkTaskDeliverable[]
  readonly blocker: 'none' | 'queued' | 'runtime-missing' | 'failed'
  readonly attempts: WorkTaskAttempt[]
  readonly pendingAction: WorkTaskPendingAction | null
  readonly actionError: string
  readonly actionHistory: WorkTaskActionReceipt[]
  readonly completeness: Readonly<Record<string, 'complete' | 'partial' | 'unavailable'>>
  readonly startedAt: string
  readonly finishedAt: string
  readonly tokensIn: number
  readonly tokensOut: number
  readonly costUSD: number
  readonly outcome: 'unrated' | 'adopted' | 'needs-revision'
  readonly outcomeNote: string
  /** Absent in historical session records that predate delivery verification. */
  readonly delivery?: WorkTaskDelivery | undefined
  readonly outcomeRevisionId?: string | undefined
  readonly outcomeAssessedAt?: number | undefined
  readonly latestAssessment?: WorkTaskAssessment | undefined
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
  /** Last change to the retained attempt fields; observation-only refreshes preserve it. */
  readonly updatedAt: number
}

/** A response from Weave to a durably recorded user action; acceptance does not imply execution completion. */
export interface WorkTaskActionReceipt {
  readonly id: string
  readonly action: WorkTaskPendingAction
  readonly outcome: 'accepted' | 'rejected'
  readonly resolvedAt: number
}

/** One exact human wait and the response shape declared by the workflow. */
export interface WorkTaskHumanTask {
  readonly interactionId: string
  readonly nodeId: string
  readonly title: string
  readonly instructions: string
  readonly resumeSchema: Readonly<Record<string, unknown>>
}

/** The sole unresolved local network action, persisted before it is sent. */
export interface WorkTaskPendingAction {
  readonly kind: 'stop' | 'rerun' | 'stage-retry' | 'correction-request' | 'correction-confirm' | 'human-complete'
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
  readonly nodeId: string
  readonly authorizedTotalRounds?: number | undefined
  readonly humanPayload?: unknown
  readonly humanInteractionId?: string | undefined
}

/** One runtime assignment reported by Weave for the task. */
export interface WorkTaskRuntime {
  readonly name: string
  readonly detail: string
  readonly status: WorkTaskStatus
}

/** User-facing lifecycle state for one workflow member or stage. */
export type WorkTaskMemberStatus = 'waiting' | 'pending' | 'running' | 'partially-completed' | 'completed' | 'failed' | 'stopped' | 'not-recorded'

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
 readonly budgetPause?: { readonly reason: string; readonly roundsUsed: number; readonly authorizedTotalRounds: number } | undefined
  readonly memberRunId?: string | undefined
  readonly checkpointSavedAt?: string | undefined
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
  readonly failureClass: 'work' | 'verification' | 'infrastructure' | 'cancelled' | ''
  readonly failureReason: string
  readonly retryable: boolean
  readonly currentTaskId: string
  readonly publicUpdatesState: 'live' | 'complete' | 'partial' | 'unavailable'
  readonly publicUpdatesTruncated: boolean
  readonly publicUpdates: WorkTaskPublicUpdate[]
}

/** One public runtime message; never private reasoning or final delivery evidence. */
export interface WorkTaskPublicUpdate {
  readonly eventId: string
  readonly taskId: string
  readonly seq: number
  readonly text: string
  readonly occurredAt: string
  readonly truncated: boolean
}

/** One bounded tool call observation recorded by Weave. */
export interface WorkTaskMemberTool {
  readonly callId: string
  readonly taskId: string
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
  readonly updateMode: 'live' | 'on_completion'
  readonly stages: WorkTaskMemberStage[]
}

/** One exact-run output available from the Workbench task surface. */
export interface WorkTaskDeliverable {
  readonly id: string
  readonly title: string
  readonly kind: 'final' | 'summary' | 'stage'
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
const memberStatusSchema = z.enum(['waiting', 'pending', 'running', 'partially-completed', 'completed', 'failed', 'stopped', 'not-recorded'])
const memberInputSchema = z.object({
  name: z.string(), expectedType: z.string(), source: z.string(), nodeId: z.string(), path: z.string(), summary: z.string().default(''),
}).strict()
const memberStageSchema = z.object({
 budgetPause: z.object({reason:z.string(),roundsUsed:z.number().int().nonnegative(),authorizedTotalRounds:z.number().int().positive()}).strict().optional(),
  nodeId: z.string(), name: z.string(), status: memberStatusSchema,
  inputs: z.array(memberInputSchema), outputRefs: z.array(z.string()),
  startedAt: z.string().default(''), completedAt: z.string().default(''),
  durationMs: z.number().int().nonnegative().default(0), toolCalls: z.number().int().nonnegative().default(0),
  tools: z.array(z.object({ callId: z.string(), taskId: z.string().default(''), name: z.string(), status: z.enum(['running', 'ok', 'error']),
    startedAt: z.string(), completedAt: z.string(), input: z.string().default(''), output: z.string().default('') }).strict()).default([]),
  failureClass: z.enum(['work', 'verification', 'infrastructure', 'cancelled', '']).default(''),
  failureReason: z.string().default(''), retryable: z.boolean().default(false),
  memberRunId: z.string().optional(), checkpointSavedAt: z.string().optional(),
  currentTaskId: z.string().default(''), publicUpdatesState: z.enum(['live', 'complete', 'partial', 'unavailable']).default('unavailable'),
  publicUpdatesTruncated: z.boolean().default(false),
  publicUpdates: z.array(z.object({
    eventId: z.string(), taskId: z.string(), seq: z.number().int().positive(), text: z.string(),
    occurredAt: z.string(), truncated: z.boolean(),
  }).strict()).default([]),
}).strict()
const memberSchema = z.object({
  agentId: z.string(), name: z.string(), duty: z.string(), role: z.enum(['lead', 'worker']),
  status: memberStatusSchema, runtime: z.string(), updateMode: z.enum(['live', 'on_completion']).default('on_completion'), stages: z.array(memberStageSchema),
}).strict()
const deliverableSchema = z.object({
  id: z.string(), title: z.string(), kind: z.enum(['final', 'summary', 'stage']),
  contentType: z.string(), preview: z.string(), content: z.string().default(''), truncated: z.boolean().default(false), createdAt: z.string(),
}).strict()
const attemptSchema = z.object({
  clientRequestId: z.string(), runId: z.string(), brief: z.string(), status: statusSchema,
  completedStages: z.number().int().nonnegative(), totalStages: z.number().int().nonnegative(), latestStage: z.string(),
  deliverableCount: z.number().int().nonnegative(), deliverables: z.array(deliverableSchema),
  createdAt: z.number().nonnegative(), updatedAt: z.number().nonnegative(),
}).strict()

const browserTaskActionSchema = z.discriminatedUnion('action', [
  z.object({ action: z.literal('stop'), sessionId: z.string(), runId: z.string() }).strict(),
  z.object({ action: z.literal('rerun'), sessionId: z.string(), runId: z.string(), brief: z.string().max(100_000).refine(value => value.trim().length > 0) }).strict(),
  z.object({ action: z.literal('stage-retry'), sessionId: z.string(), runId: z.string(), nodeId: z.string(), authorizedTotalRounds: z.number().int().positive().max(Number.MAX_SAFE_INTEGER).optional() }).strict(),
  z.object({ action: z.literal('correction-request'), sessionId: z.string(), runId: z.string(), targetKind: z.enum(['team', 'member']), targetMemberId: z.string(), instruction: z.string().trim().min(1).max(20_000) }).strict(),
  z.object({ action: z.literal('correction-confirm'), sessionId: z.string(), runId: z.string(), correctionId: z.string(), disposition: z.enum(['apply', 'discard']) }).strict(),
  z.object({ action: z.literal('human-complete'), sessionId: z.string(), runId: z.string(), interactionId: z.string(), payload: z.unknown().refine(value => value !== undefined) }).strict(),
  z.object({ action: z.literal('assess'), sessionId: z.string(), runId: z.string(), deliveryRevisionId: z.string().min(1), outcome: z.enum(['adopted', 'needs-revision']), note: z.string().max(2_000) }).strict(),
  z.object({ action: z.literal('recheck'), sessionId: z.string(), runId: z.string(), deliveryRevisionId: z.string().min(1), contractDigest: z.string().min(1) }).strict(),
])
const pendingActionSchema = z.object({
 authorizedTotalRounds: z.number().int().positive().max(Number.MAX_SAFE_INTEGER).optional(),
  kind: z.enum(['stop', 'rerun', 'stage-retry', 'correction-request', 'correction-confirm', 'human-complete']), targetRunId: z.string(), idempotencyKey: z.string(),
  clientRequestId: z.string(), brief: z.string(), requestedAt: z.number().nonnegative(),
  targetKind: z.enum(['team', 'member', '']).default(''), targetMemberId: z.string().default(''),
  correctionId: z.string().default(''), disposition: z.enum(['apply', 'discard', '']).default(''),
  instruction: z.string().default(''),
  nodeId: z.string().default(''), humanPayload: z.unknown().optional(), humanInteractionId: z.string().optional(),
}).strict()
const correctionSchema = z.object({
  correctionId: z.string(), targetKind: z.enum(['team', 'member']), targetMemberId: z.string(),
  instruction: z.string(), status: z.enum(['requested', 'ready', 'confirmed', 'discarded', 'applied']),
  safeNodeId: z.string(), restartNodeId: z.string(), affectedNodeIds: z.array(z.string()),
  preservedNodeIds: z.array(z.string()), requestedAt: z.string(),
}).strict()
const completenessSchema = z.record(z.string(), z.enum(['complete', 'partial', 'unavailable']))
const verificationStatusSchema = z.enum(['pending', 'passed', 'failed', 'unknown'])
const deliverySchema = z.object({
  revisionId: z.string(), contractDigest: z.string(), verificationId: z.string(), verificationStatus: verificationStatusSchema,
  reason: z.string(), checks: z.array(z.object({ checkId: z.string(), status: verificationStatusSchema, reason: z.string() }).strict()),
  checkCounts: z.record(z.string(), z.number().int().nonnegative()), available: z.boolean(), evidenceCompleteness: z.enum(['complete', 'unavailable']),
}).strict()
const wireDeliverySchema = z.object({
  revision_id: z.string().default(''), contract_digest: z.string().default(''), verification_id: z.string().default(''), verification_status: verificationStatusSchema,
  reason: z.string().default(''), checks: z.array(z.object({ check_id: z.string(), status: verificationStatusSchema, reason: z.string() })),
  check_counts: z.record(z.string(), z.number().int().nonnegative()), available: z.boolean(), evidence_completeness: z.enum(['complete', 'unavailable']),
})

function readDelivery(value: unknown): WorkTaskDelivery {
  const parsed = wireDeliverySchema.safeParse(value)
  if (!parsed.success) return { revisionId: '', contractDigest: '', verificationId: '', verificationStatus: 'unknown',
    reason: 'delivery_verification_unavailable', checks: [], checkCounts: {}, available: false, evidenceCompleteness: 'unavailable' }
  const item = parsed.data
  return { revisionId: item.revision_id, contractDigest: item.contract_digest, verificationId: item.verification_id,
    verificationStatus: item.verification_status, reason: item.reason,
    checks: item.checks.map(check => ({ checkId: check.check_id, status: check.status, reason: check.reason })),
    checkCounts: item.check_counts, available: item.available, evidenceCompleteness: item.evidence_completeness }
}

function assessmentForDelivery(previous: WorkTaskProjection | null, runId: string, delivery: WorkTaskDelivery | undefined):
Pick<WorkTaskProjection, 'outcome' | 'outcomeNote' | 'outcomeRevisionId' | 'outcomeAssessedAt' | 'latestAssessment'> {
  const receipt = previous?.runId === runId ? previous.latestAssessment
    ?? (previous.outcome !== 'unrated' && previous.outcomeRevisionId !== undefined && previous.outcomeRevisionId !== ''
      ? { runId, revisionId: previous.outcomeRevisionId, outcome: previous.outcome,
        note: previous.outcomeNote, assessedAt: previous.outcomeAssessedAt ?? 0 }
      : undefined) : undefined
  const sameRevision = receipt?.runId === runId && delivery !== undefined && delivery.revisionId !== '' && receipt.revisionId === delivery.revisionId
  return { ...(receipt?.runId === runId ? { latestAssessment: receipt } : {}), ...(sameRevision
    ? { outcome: receipt.outcome, outcomeNote: receipt.note, outcomeRevisionId: receipt.revisionId, outcomeAssessedAt: receipt.assessedAt }
    : { outcome: 'unrated' as const, outcomeNote: '', outcomeRevisionId: '', outcomeAssessedAt: 0 }) }
}

const taskSchema = z.object({
  preparation: z.object({ callId: z.string(), buildId: z.string(), updatedAt: z.number().nonnegative().optional(), state: z.enum(['submitting', 'building', 'ready', 'failed', 'unknown']), error: z.string(), steps: z.array(z.object({ id: z.string(), label: z.string(), status: z.string(), attempt: z.number() }).strict()).optional() }).strict().optional(),
  displayState: z.string().optional(),
  brief: z.string().default(''),
  clientRequestId: z.string(), runId: z.string(), teamId: z.string(), teamName: z.string(),
  workflowName: z.string(), status: statusSchema,
  waitKind: z.enum(['', 'timer', 'fanout', 'human', 'correction', 'runtime']).default(''), waitNodeId: z.string().default(''),
  completedStages: z.number().int().nonnegative(), totalStages: z.number().int().nonnegative(), latestStage: z.string(),
  members: z.array(memberSchema).default([]), corrections: z.array(correctionSchema).default([]),
  runtimes: z.array(runtimeSchema), humanTaskCount: z.number().int().nonnegative(),
  humanTask: z.object({
    interactionId: z.string(), nodeId: z.string(), title: z.string(), instructions: z.string(),
    resumeSchema: z.record(z.string(), z.unknown()),
  }).strict().nullable().default(null),
  deliverableCount: z.number().int().nonnegative(),
  deliverables: z.array(deliverableSchema).default([]),
  blocker: z.enum(['none', 'queued', 'runtime-missing', 'failed']),
  attempts: z.array(attemptSchema).default([]), pendingAction: pendingActionSchema.nullable().default(null),
  actionError: z.string().default(''),
  actionHistory: z.array(z.object({ id: z.string(), action: pendingActionSchema, outcome: z.enum(['accepted', 'rejected']), resolvedAt: z.number().nonnegative() }).strict()).default([]),
  completeness: completenessSchema.default({}),
  startedAt: z.string().default(''), finishedAt: z.string().default(''),
  tokensIn: z.number().int().nonnegative().default(0), tokensOut: z.number().int().nonnegative().default(0),
  costUSD: z.number().nonnegative().default(0),
  outcome: z.enum(['unrated', 'adopted', 'needs-revision']).default('unrated'), outcomeNote: z.string().default(''),
  delivery: deliverySchema.optional(), outcomeRevisionId: z.string().optional(), outcomeAssessedAt: z.number().nonnegative().optional(),
  latestAssessment: z.object({ runId: z.string().min(1), revisionId: z.string().min(1), outcome: z.enum(['adopted', 'needs-revision']), note: z.string(), assessedAt: z.number().nonnegative() }).strict().optional(),
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

function ownText(value: unknown, keys: readonly string[]): string {
  const item = object(value)
  for (const key of keys) {
    const candidate = item?.[key]
    if (typeof candidate === 'string' && candidate.trim() !== '') return candidate.trim()
  }
  return ''
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
  if (['cancelled', 'canceled', 'stopped'].includes(raw)) return 'stopped'
  if (['failed', 'error', 'abandoned'].includes(raw)) return 'failed'
  if (raw === 'cancel_requested') return 'stopping'
  if (['parked', 'waiting', 'yielded', 'waiting_for_human', 'needs_input', 'blocked', 'paused'].includes(raw)) return 'waiting'
  if (['queued', 'pending', 'admitting'].includes(raw)) return 'queued'
  if (['running', 'active', 'in_progress', 'working'].includes(raw)) return 'running'
  return 'preparing'
}

function terminalStatus(value: WorkTaskStatus): boolean {
  return value === 'completed' || value === 'failed' || value === 'stopped'
}

function waitKind(value: unknown): WorkTaskProjection['waitKind'] {
  const raw = object(value)?.wait_kind
  return raw === 'timer' || raw === 'fanout' || raw === 'human' || raw === 'correction' || raw === 'runtime' ? raw : ''
}

function canRetryStage(task: WorkTaskProjection, nodeId: string): boolean {
  return task.status === 'waiting'
    && (task.waitKind === 'fanout' || (task.waitKind === 'runtime' && task.waitNodeId === nodeId))
    && task.members.some(member => member.stages.some(stage => stage.nodeId === nodeId
      && ((stage.status === 'failed' && stage.failureClass === 'infrastructure') || (stage.status === 'waiting' && stage.budgetPause !== undefined)) && stage.retryable))
}

function resultValue(event: Extract<SessionEvent, { type: 'tool/result' }>): unknown {
  const content = event.data.message.content[0].content
  const joined = content.map(block => block.type === 'text' ? block.text : '').filter(Boolean).join('\n')
  return parsed(joined)
}

function teamDisplayName(value: string): string {
  const name = value.trim()
  if (/^[0-9a-f]{8}-[0-9a-f-]{27,}$/iu.test(name)) return ''
  if (!/^[a-z0-9_-]+$/u.test(name) || (!name.includes('-') && !name.includes('_'))) return name
  return name.split(/[-_]+/u).filter(Boolean).map(word => word.charAt(0).toUpperCase() + word.slice(1)).join(' ')
}

function listedTeams(value: unknown): Record<string, string> {
  const nested = deepValue(value, new Set(['teams', 'items']))
  const items = Array.isArray(value) ? value : Array.isArray(nested) ? nested : []
  return Object.fromEntries(items.flatMap((candidate): [string, string][] => {
    const id = text(candidate, ['team_id', 'teamId', 'id'])
    return id === '' ? [] : [[id, teamDisplayName(preferredText(candidate, ['display_name', 'displayName', 'name']))]]
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
  if (raw === 'waiting') return 'waiting'
  if (raw === 'running' || raw === 'active') return 'running'
  if (raw === 'failed') return 'failed'
  if (raw === 'stopped' || raw === 'cancelled') return 'stopped'
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

function publicUpdates(value: unknown): WorkTaskPublicUpdate[] {
  if (!Array.isArray(value)) return []
  const updates = new Map<string, WorkTaskPublicUpdate>()
  for (const candidate of value) {
    const item = object(candidate)
    if (item === undefined || (item.kind !== 'text' && item.kind !== 'attempt')) continue
    const taskId = text(item, ['task_id'])
    const eventId = text(item, ['event_id'])
    const seq = count(item, ['seq'])
    if (taskId === '' || eventId === '' || (!Number.isSafeInteger(seq) || seq <= 0) || typeof item.text !== 'string') continue
    updates.set(item.kind === 'attempt' ? eventId : `${taskId}:${seq}`, { eventId, taskId, seq, text: item.text,
      occurredAt: text(item, ['occurred_at']), truncated: item.truncated === true })
  }
  return [...updates.values()]
}

function memberBudgetPause(value: unknown): { budgetPause?: { reason: string; roundsUsed: number; authorizedTotalRounds: number } } {
 const item = value as Record<string, unknown> | null
 if (typeof item !== 'object' || item === null) return {}
 const used = item.rounds_used ?? item.roundsUsed
 const ceiling = item.authorized_total_rounds ?? item.authorizedTotalRounds
 if (typeof item.reason !== 'string' || typeof used !== 'number' || typeof ceiling !== 'number'
   || !Number.isSafeInteger(used) || !Number.isSafeInteger(ceiling) || used < 0 || ceiling < used) return {}
 return { budgetPause: { reason: item.reason, roundsUsed: used, authorizedTotalRounds: ceiling } }
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
      failureClass: (['work', 'verification', 'infrastructure', 'cancelled'].includes(text(item, ['failure_class', 'failureClass']))
        ? text(item, ['failure_class', 'failureClass']) : '') as WorkTaskMemberStage['failureClass'],
      failureReason: text(item, ['failure_reason', 'failureReason']), retryable: item.retryable === true,
      ...memberBudgetPause(item.budget_pause ?? item.budgetPause),
      ...(text(item, ['member_run_id', 'memberRunId']) === '' ? {} : { memberRunId: text(item, ['member_run_id', 'memberRunId']) }),
      ...(text(item, ['checkpoint_saved_at', 'checkpointSavedAt']) === '' ? {} : { checkpointSavedAt: text(item, ['checkpoint_saved_at', 'checkpointSavedAt']) }),
      publicUpdates: publicUpdates(item.public_updates), publicUpdatesTruncated: item.public_updates_truncated === true,
      currentTaskId: text(item, ['current_task_id']),
      publicUpdatesState: ['live', 'complete', 'partial'].includes(String(item.public_updates_state)) ? item.public_updates_state as WorkTaskMemberStage['publicUpdatesState'] : 'unavailable',
      tools: rawTools.flatMap((candidate): WorkTaskMemberTool[] => {
        const tool = object(candidate)
        if (tool === undefined) return []
        const name = text(tool, ['name'])
        const rawStatus = text(tool, ['status'])
        if (name === '' || !['running', 'ok', 'error'].includes(rawStatus)) return []
        return [{ callId: text(tool, ['call_id', 'callId']), taskId: text(tool, ['task_id']), name, status: rawStatus as WorkTaskMemberTool['status'],
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
      text(runtime, ['configured_endpoint']),
      Array.isArray(runtime.reported_models) && runtime.reported_models.length > 0
        ? `回执模型 ${runtime.reported_models.filter((model): model is string => typeof model === 'string').join(', ')}`
        : text(runtime, ['configured_model']) === '' ? '' : `节点默认 ${text(runtime, ['configured_model'])}`,
    ].filter(Boolean).join(' · ')
    return [{
      agentId, name: text(item, ['name', 'display_name']) || agentId,
      duty: text(item, ['duty']), role: text(item, ['role']) === 'lead' ? 'lead' : 'worker',
      status: memberStatus(item), runtime: runtimeDetail, updateMode: runtime?.update_mode === 'live' ? 'live' : 'on_completion', stages: memberStages(item.stages),
    }]
  })
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
    const kind = rawKind !== 'final' || publishedWorkflow && nodeType !== '' && nodeType !== 'deliver'
      ? 'stage'
      : publishedWorkflow && filename === '' && nodeType === 'deliver'
        ? 'summary'
        : 'final'
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

function reconcileDeliverableKinds(items: WorkTaskDeliverable[], value: unknown): WorkTaskDeliverable[] {
  const refs = object(value)?.deliverables
  if (!Array.isArray(refs)) return items
  const stages = new Set(refs.flatMap((ref) => {
    const item = object(ref)
    return item?.kind === 'stage' && typeof item.id === 'string' ? [item.id] : []
  }))
  return items.map(item => stages.has(item.id) && item.kind !== 'stage' ? { ...item, kind: 'stage' } : item)
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
  const prior = attempts[index]
  if (prior === undefined) attempts.push(attempt)
  else {
    const next = { ...attempt, createdAt: prior.createdAt }
    // Observation time alone does not change the retained run attempt.
    attempts[index] = isDeepStrictEqual({ ...next, updatedAt: 0 }, { ...prior, updatedAt: 0 })
      ? prior : next
  }
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
  const rawHumanTasks = object(value)?.human_tasks
  const nextMembers = memberList(value)
  const memberStages = nextMembers.flatMap(member => member.stages)
  const activeMemberStage = nextMembers.flatMap(member => member.stages)
    .find(stage => stage.status === 'running')?.name ?? ''
  const nextRunId = text(value, ['run_id', 'runId']) || seed.runId || previous?.runId || ''
  const sameRun = nextRunId !== '' && previous?.runId === nextRunId
  const completedStages = memberStages.length > 0
    ? memberStages.filter(stage => stage.status === 'completed').length
    : count(value, ['completed_stages', 'stages_completed', 'completed_count'])
      || (sameRun ? previous.completedStages : 0)
  const totalStages = memberStages.length > 0
    ? memberStages.length
    : count(value, ['total_stages', 'stages_total', 'stage_count'])
      || (sameRun ? previous.totalStages : 0)
  const retainedRuntimes = nextRuntimes.length === 0 ? (sameRun ? previous.runtimes : []) : nextRuntimes
  const observed = observedAt(value, now)
  const delivery = Object.hasOwn(object(value) ?? {}, 'delivery') ? readDelivery(object(value)?.delivery)
    : sameRun ? previous.delivery : undefined
  const base: Omit<WorkTaskProjection, 'attempts'> = {
    brief: seed.brief ?? previous?.brief ?? '',
    clientRequestId: text(value, ['client_request_id', 'clientRequestId']) || seed.clientRequestId || previous?.clientRequestId || '',
    runId: nextRunId,
    teamId: ownText(value, ['team_id', 'teamId']) || seed.teamId || previous?.teamId || '',
    teamName: teamDisplayName(seed.teamName || previous?.teamName || ''),
    workflowName: ownText(value, ['workflow_name', 'workflow_id']) || seed.workflowName || previous?.workflowName || '',
    status: runtimeMissing ? 'waiting' : nextStatus === 'preparing' ? previous?.status ?? 'preparing' : nextStatus,
    waitKind: nextStatus === 'waiting' ? waitKind(value) : '',
    waitNodeId: nextStatus === 'waiting' ? text(value, ['wait_node_id']) : '',
    completedStages,
    totalStages,
    latestStage: activeMemberStage || text(value, ['latest_stage', 'current_stage'])
      || latestRecordedStage(value) || (sameRun ? previous.latestStage : ''),
    members: rawMembers === undefined ? (sameRun ? previous.members : []) : nextMembers,
    corrections: rawCorrections === undefined ? (sameRun ? previous.corrections : []) : correctionList(value),
    runtimes: terminalStatus(nextStatus)
      ? retainedRuntimes.map(runtime => ({ ...runtime, status: nextStatus }))
      : retainedRuntimes,
    humanTaskCount: nextStatus !== 'waiting' ? 0 : Array.isArray(rawHumanTasks) ? rawHumanTasks.length
      : seed.humanTaskCount ?? (sameRun ? previous.humanTaskCount : 0),
    humanTask: nextStatus === 'waiting' && waitKind(value) === 'human' && sameRun
      && previous.waitNodeId === text(value, ['wait_node_id']) ? previous.humanTask : null,
    deliverableCount: seed.deliverableCount ?? (sameRun ? previous.deliverableCount : 0),
    deliverables: reconcileDeliverableKinds(seed.deliverables ?? (sameRun ? previous.deliverables : []), value),
    blocker: runtimeMissing ? 'runtime-missing' : nextStatus === 'failed' ? 'failed' : nextStatus === 'queued' ? 'queued' : 'none',
    pendingAction: seed.pendingAction === undefined ? previous?.pendingAction ?? null : seed.pendingAction,
    actionError: text(value, ['status', 'state', 'run_status']) === 'abandoned' && object(value)?.stop_unconfirmed === true ? 'stop_unconfirmed'
      : seed.actionError ?? (previous?.actionError === 'stop_unconfirmed' ? '' : previous?.actionError ?? ''),
    actionHistory: previous?.actionHistory ?? [],
    completeness: Object.keys(completeness(value)).length === 0 ? (sameRun ? previous.completeness : {}) : completeness(value),
    startedAt: text(value, ['started_at', 'startedAt']) || (sameRun ? previous.startedAt : ''),
    finishedAt: text(value, ['ended_at', 'endedAt', 'terminal_at', 'terminalAt', 'finished_at', 'finishedAt']) || (sameRun ? previous.finishedAt : ''),
    tokensIn: Math.floor(finiteNumber(value, ['tokens_in', 'tokensIn', 'input_tokens']) ?? (sameRun ? previous.tokensIn : 0)),
    tokensOut: Math.floor(finiteNumber(value, ['tokens_out', 'tokensOut', 'output_tokens']) ?? (sameRun ? previous.tokensOut : 0)),
    costUSD: finiteNumber(value, ['cost_usd', 'costUSD']) ?? (sameRun ? previous.costUSD : 0),
    ...(delivery === undefined ? {} : { delivery }),
    ...assessmentForDelivery(previous, nextRunId, delivery),
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
  if (event.type === 'weave/dispatch-input') {
    const input = event.data
    // The exact-run action poller owns rerun projection and its action receipt together.
    if (input.rerun !== null) return state
    if (input.state === 'registered') return state
    if (input.state === 'accepted') {
      if (input.result === null) throw new Error('accepted dispatch input requires its run receipt')
      return { ...state, task: snapshot(state.task, input.result, event.time, {
        brief: input.task, teamId: input.facts.team_id, teamName: state.teams[input.facts.team_id] ?? input.facts.team_id,
        workflowName: input.facts.workflow_id ?? '', clientRequestId: input.revision?.client_request_id ?? '',
      }) }
    }
    if (input.state === 'rejected' || input.state === 'closed') return state.task === null ? state : { ...state, task: {
      ...state.task, status: 'failed', actionError: input.errorCode, updatedAt: event.time,
    } }
    return { ...state, task: { ...snapshot(null, {}, event.time, {
      brief: input.task, teamId: input.facts.team_id, teamName: state.teams[input.facts.team_id] ?? input.facts.team_id,
      workflowName: input.facts.workflow_id ?? '',
    }), attempts: state.task?.attempts ?? [] } }
  }
  if (event.type === 'weave/work-task') {
    const incoming = taskSchema.parse(event.data)
    if (state.task !== null && state.task.runId !== '' && incoming.runId !== '' && incoming.runId !== state.task.runId
      && state.task.attempts.some(attempt => attempt.runId === incoming.runId)) return state
    if (state.task !== null && incoming.runId === state.task.runId && incoming.observedAt < state.task.observedAt) return state
    const receipts = new Map<string, WorkTaskActionReceipt>()
    for (const receipt of [...(state.task?.actionHistory ?? []), ...incoming.actionHistory]) {
      if (!receipts.has(receipt.id)) receipts.set(receipt.id, receipt)
    }
    const { latestAssessment: _assessment, ...incomingFacts } = incoming
    return { ...state, task: { ...incomingFacts, ...assessmentForDelivery(incoming, incoming.runId, incoming.delivery),
      teamName: state.teams[incoming.teamId] || incoming.teamName,
      actionHistory: [...receipts.values()] } }
  }
  if (event.type === 'weave/work-task-action') {
    return state.task === null
      ? state
      : { ...state, task: { ...state.task, pendingAction: event.data.pendingAction, actionError: '' } }
  }
  if (event.type === 'tool/call' && event.data.name.startsWith('mcp__weave__')) {
    const pending = { name: event.data.name, args: parsed(event.data.arguments) }
    const creating = pending.name === 'mcp__weave__team_create' && (state.task === null || state.task.runId === '' || terminalStatus(state.task.status))
    const task = creating ? { ...snapshot(null, {}, event.time, {
      brief: text(object(pending.args)?.definition, ['purpose']),
      teamName: text(object(pending.args)?.definition, ['display_name']),
    }), attempts: state.task?.attempts ?? [], preparation: { callId: event.data.callId, buildId: '', state: 'submitting' as const, error: '' } } : state.task
    return { ...state, task, pendingCalls: { ...state.pendingCalls, [event.data.callId]: pending } }
  }
  if (event.type !== 'tool/result') return state
  const callId = event.data.message.source.callId
  const call = state.pendingCalls[callId]
  if (call === undefined) return state
  const pendingCalls = Object.fromEntries(Object.entries(state.pendingCalls).filter(([id]) => id !== callId))
  const value = resultValue(event)
  if (event.data.message.content[0].isError) {
    if (call.name !== 'mcp__weave__team_create' || state.task?.preparation?.callId !== callId) return { ...state, pendingCalls }
    const code = text(value, ['code', 'error']) || (typeof value === 'string' ? value : '')
    const failed = code.includes('template_build_failed')
    return { ...state, pendingCalls, task: { ...state.task, status: failed ? 'failed' : 'preparing',
      preparation: { ...state.task.preparation, state: failed ? 'failed' : 'unknown', error: code }, updatedAt: event.time } }
  }
  if (call.name === 'mcp__weave__team_list' || call.name === 'mcp__weave__team_status') {
    const team = object(value)?.team
    const teams = { ...state.teams, ...listedTeams(call.name === 'mcp__weave__team_status' ? [team] : value) }
    const refreshedName = state.task === null ? '' : teams[state.task.teamId] ?? ''
    const task = state.task === null || refreshedName === '' || refreshedName === state.task.teamName
      ? state.task
      : { ...state.task, teamName: refreshedName, updatedAt: event.time }
    return { ...state, pendingCalls, teams, task }
  }
  if (call.name === 'mcp__weave__team_create') {
    const teamId = text(value, ['team_id', 'teamId', 'id'])
    const definition = object(call.args)?.definition
    const displayName = teamDisplayName(preferredText(definition, ['display_name', 'displayName', 'name']))
    const teams = teamId === '' || displayName === '' ? state.teams : { ...state.teams, [teamId]: displayName }
    const preparation = state.task?.preparation
    const buildId = text(value, ['build_id', 'build_run_id'])
    const ready = ['ready', 'active', 'completed'].includes(ownText(value, ['status']))
    const task = state.task === null || preparation?.callId !== callId ? state.task : {
      ...state.task, teamId, teamName: displayName || state.task.teamName,
      preparation: { ...preparation, buildId, state: ready ? 'ready' as const : buildId !== '' ? 'building' as const : 'unknown' as const }, updatedAt: event.time,
    }
    return { ...state, pendingCalls, teams, task }
  }
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
  if (call.name === 'mcp__weave__build_status' && text(call.args, ['build_id']) === state.task.preparation?.buildId) {
    return { ...state, pendingCalls, task: buildSnapshot(state.task, value, event.time) }
  }
  if (call.name === 'mcp__weave__dispatch_status'
    || call.name === 'mcp__weave__team_run_status'
    || call.name === 'mcp__weave__team_run_activity') {
    const current = state.task
    const targetRequests = [ownText(call.args, ['client_request_id']), ownText(value, ['client_request_id'])].filter(Boolean)
    if (call.name === 'mcp__weave__dispatch_status' && targetRequests.some(id => id !== current.clientRequestId)) return { ...state, pendingCalls }
    const targetRuns = [ownText(call.args, ['run_id']), ownText(value, ['run_id'])].filter(Boolean)
    if (targetRuns.some(id => id !== current.runId)) return { ...state, pendingCalls }
    const next = snapshot(state.task, value, event.time)
    if (next.observedAt < state.task.observedAt || terminalStatus(state.task.status) && !terminalStatus(next.status) && (ownText(value, ['observed_at', 'observedAt']) === '' || next.observedAt <= state.task.observedAt)) return { ...state, pendingCalls }
    return { ...state, pendingCalls, task: next }
  }
  return { ...state, pendingCalls }
}

function displayState(task: WorkTaskProjection): string {
  if (task.pendingAction?.kind === 'stop' || task.status === 'stopping') return 'stopping'
  if (task.actionError === 'stop_unconfirmed') return 'stopUnconfirmed'
  if (task.runId === '' && task.preparation !== undefined) return `build${task.preparation.state.charAt(0).toUpperCase()}${task.preparation.state.slice(1)}`
  if (task.status === 'completed') return task.deliverables.some(item => item.kind === 'final' || item.kind === 'summary') ? 'completed' : 'outputsMissing'
  if (task.humanTask !== null || task.humanTaskCount > 0) return 'human'
  if (task.corrections.some(item => item.status === 'ready')) return 'correction'
  if (task.status !== 'waiting') return task.status
  const interrupted = task.members.flatMap(member => member.stages).filter(stage => stage.status === 'failed' && stage.failureClass === 'infrastructure')
  if (interrupted.some(stage => !stage.retryable && stage.failureReason === 'Reconnect the runtime and wait for the previous execution to confirm it has stopped.')) return 'runtimeStop'
  if (interrupted.some(stage => stage.retryable)) return 'retryable'
  if (task.waitKind === 'fanout' && task.members.some(member => member.status === 'running')) return 'parallel'
  return task.waitKind || 'waiting'
}

function buildSnapshot(task: WorkTaskProjection, value: unknown, now: number): WorkTaskProjection {
  const preparation = task.preparation
  if (preparation === undefined || ownText(value, ['build_run_id', 'build_id']) !== preparation.buildId) return task
  const rawStatus = ownText(value, ['run_status', 'status'])
  const state = rawStatus === 'passed' ? 'ready' : ['blocked', 'cancelled', 'failed'].includes(rawStatus) ? 'failed' : 'building'
  const sourceTime = Date.parse(ownText(value, ['updated_at']))
  const sourceUpdatedAt = Number.isFinite(sourceTime) ? sourceTime : 0
  const previousUpdatedAt = preparation.updatedAt ?? 0
  if (sourceUpdatedAt > 0 && sourceUpdatedAt < previousUpdatedAt) return task
  if (['ready', 'failed'].includes(preparation.state) && state === 'building'
    && (sourceUpdatedAt <= previousUpdatedAt || previousUpdatedAt === 0)) return task
  const rawSteps = object(value)?.steps
  const steps = Array.isArray(rawSteps) ? rawSteps.flatMap((item) => {
    const id = ownText(item, ['operation_id'])
    const label = ownText(item, ['display_label', 'target_name'])
    return id === '' || label === '' ? [] : [{ id, label, status: ownText(item, ['status']), attempt: count(item, ['attempt']) }]
  }) : []
  return { ...task, status: state === 'failed' ? 'failed' : 'preparing',
    teamId: ownText(object(value)?.final_ref, ['team_id']) || task.teamId,
    preparation: { ...preparation, state, steps, updatedAt: sourceUpdatedAt || previousUpdatedAt }, observedAt: now, updatedAt: now }
}

/** Projection definition registered with the Session projection registry. */
export const workTaskProjectionDefinition = {
  key: 'workTask', stateVersion: 14, stateSchema,
  init: (): WorkTaskState => ({ task: null, pendingCalls: {}, teams: {} }),
  apply: applyWorkTaskProjection,
  wire: {
    list: true, viewSchema: taskSchema.nullable(),
    view: (state: WorkTaskState) => state.task === null ? null : { ...state.task, displayState: displayState(state.task) },
  },
} satisfies ProjectionDefinition<'workTask', WorkTaskState>

/** Host-only WorkTask synchronization settings. */
export interface Config {
  /** Weave HTTP API origin; defaults to `WEAVE_API_URL` and then the local development endpoint. */
  readonly apiUrl?: string
  /** Business API credential; defaults to host-only `WEAVE_API_KEY`. */
  readonly apiKey?: string
  /** Public or otherwise externally routable Weave URL advertised to new runtime nodes; defaults to `WEAVE_RUNTIME_SERVER_URL`. */
  readonly runtimeServerUrl?: string
  /** Delay between non-terminal status reads in milliseconds. */
  readonly pollIntervalMs?: number
}
export const name = 'workbench-work-task'
export const inject = ['sessions', 'sessionProjections', 'sessionController', 'commands', 'systemPrompt', 'connection', 'tools']

function rerunFacts(session: Session, task: WorkTaskProjection): DispatchInputFacts {
  const accepted = session.events.findLast(event => event.type === 'weave/dispatch-input'
    && event.data.state === 'accepted' && event.data.result?.run_id === task.runId)
  if (accepted?.type !== 'weave/dispatch-input') return { team_id: task.teamId,
    ...(task.workflowName === '' ? {} : { workflow_id: task.workflowName }) }
  const input = accepted.data
  const workflowId = text(input.result, ['workflow_id'])
  const workflowVersion = finiteNumber(input.result, ['workflow_version'])
  const projectId = text(input.result, ['project_id'])
  return { ...input.facts,
    ...(workflowId === '' ? {} : { workflow_id: workflowId }),
    ...(workflowVersion === undefined ? {} : { workflow_version: workflowVersion }),
    ...(projectId === '' ? {} : { project_id: projectId }) }
}

/** Product policy that remains visible when a per-session agent preset shadows the deployment persona. */
export const workbenchTeamRoutingSection = {
  name: 'workbench:team-routing',
  order: FIRST_PARTY_SECTION_ORDER.TEAM_POLICY + 10,
  text: 'For substantive business work in Weave Workbench, first list the available Weave teams and match the request against each team\'s stated purpose, responsibilities, success criteria, default workflow availability, and health. If a suitable active team exists, first ensure the user has confirmed the selected team, task scope, and expected deliverables. Summarize only the unconfirmed choices and request their confirmation; when these choices are already explicitly confirmed in the conversation, call weave_dispatch with the agreed team and workflow without asking again; Workbench attaches the original user inputs automatically. If no suitable team exists, say so plainly and collaborate with the user on a team definition. Once the user confirms the team definition, task scope, and expected deliverables, create the team with its agreed workflow and call weave_dispatch without repeating confirmation only when that confirmation includes dispatch. If the user explicitly asks to create first and confirm dispatch separately, create the team, show the final task summary, and wait for that separate dispatch decision. Selecting a team card only selects a proposal; it does not authorize dispatch. Use the existing structured question or plan-review interaction to show the goal, expected outputs, actual members and workflow, missing materials, budget limits, and exactly which actions the decision authorizes. Preserve the displayed proposal and the user response in the conversation. Never treat your own claim of confirmation as a user decision. Do not replace a confirmed team or expand its task silently. If required materials are missing, obtain them before dispatch unless the user explicitly authorizes an assessment limited to those gaps. Reuse the same idempotency key for repeated identical creation. Use weave_dispatch for initial dispatch; it owns the original input and idempotency key. Never copy a task body into an MCP dispatch call. Retry an unresolved weave_dispatch with the same team and workflow. After a creation transport error, verify the existing request outcome before creating another team. A build_id belongs to the current creation only; Workbench monitors its progress in the background. A failed creation stays the current issue until it is resolved or the user changes the proposal. Internal construction and evaluation steps are not additional user approval gates; do not perform the requested research or production work in the foreground. A successful Weave dispatch is already the durable task: do not create or update a DSH goal for it. After dispatch, make at most one activity or status call to confirm the handoff, then return the selected team and a short human-facing state such as queued, underway, waiting for input, completed, or needs attention. Do not include internal identifiers, raw workflow versions, orchestration phases, or backend enums unless the user explicitly asks for technical details. Do not poll the run in the foreground, and do not save a duplicate foreground deliverable; Workbench monitors the run and projects Weave\'s deliverables in the background. For follow-up questions and adjustments to an existing task, keep the current team and exact run; do not repeat team matching or dispatch a new task. A member reference provides context, not permission to modify work. Discuss ordinary questions. When the user explicitly requests a modification, submit the existing correction request for those targets, then present the computed impact for confirmation before applying it. Do not add a separate confirmation before submitting that explicitly requested correction, and never describe an accepted request as an applied change. When the run is terminal or the user later asks for the result, check whether an exact-run final deliverable exists. If execution ended with only stage records or no final deliverable, say that the final output is not yet confirmed and do not claim delivery is complete. Read any available final Weave deliverable and answer with a short user-facing completion summary: what was finished, the main findings or decisions, the files the user can open, and any user action still needed. Keep internal run IDs, deliverable IDs, runtime IDs, host paths, hashes, validation command names, and engine details out of the main answer unless the user explicitly asks for technical details. Never select free collaboration unless the user explicitly requests it. If Weave tools are unavailable, report that the Weave connection is not configured instead of pretending that team work was performed.',
} as const

/** Register the durable projection and keep non-terminal Weave runs synchronized outside the conversation turn. */
export function apply(ctx: Context, config: Config = {}): void {
  ctx.effect(() => ctx.systemPrompt.section(workbenchTeamRoutingSection), 'workbench team-routing policy')
  ctx.sessionProjections.register(workTaskProjectionDefinition)
  ctx.sessionController.registerHistoryProjection({ key: 'workTask', eventTypes: ['weave/work-task'] })
  const apiUrl = (config.apiUrl ?? process.env.WEAVE_API_URL ?? 'http://127.0.0.1:18080').replace(/\/$/, '')
  const apiKey = (config.apiKey ?? process.env.WEAVE_API_KEY ?? '').trim()
  const dispatchInputs = apiKey === '' ? undefined : installDispatchInputTool(ctx, { apiUrl, apiKey }, (session) => {
    const task = ctx.sessionProjections.stateOf(session, 'workTask')?.task
    if (task === null || task === undefined || task.runId === '') return
    if (task.pendingAction !== null) throw new Error('dispatch_input_pending: finish the current task action first')
    const latest = latestDispatchInput(session)
    const replay = latest?.state === 'accepted' && latest.result?.run_id === task.runId
      && !session.events.some(event => event.type === 'user/message' && event.data.source.kind === 'user' && event.seq > latest.sourceThroughSeq)
    if (!terminalStatus(task.status) && !replay) throw new Error('dispatch_current_run_active: use the current run for progress, correction or stop before starting a new task')
  })
  const runtimeServerUrl = resolveRuntimeServerUrl(apiUrl, config.runtimeServerUrl ?? process.env.WEAVE_RUNTIME_SERVER_URL)
  const connection = Reflect.get(ctx, 'connection') as {
    readonly fetch: { register(route: { readonly path: string; readonly methods: readonly ('GET' | 'HEAD' | 'POST' | 'PUT' | 'DELETE')[]; readonly fetch: (request: Request) => Promise<Response> }): () => Promise<void> }
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
  connection.fetch.register({
    path: '/api/weave.agent-execution', methods: ['GET', 'PUT'],
    fetch: request => handleAgentExecutionRequest(apiUrl, apiKey, request),
  })
  connection.fetch.register({
    path: '/api/weave.runtimes', methods: ['GET', 'HEAD', 'POST', 'PUT', 'DELETE'],
    fetch: request => handleWeaveRuntimeRequest(apiUrl, apiKey, request, fetch, runtimeServerUrl),
  })
  connection.fetch.register({
    path: '/api/weave.deliverable', methods: ['GET', 'HEAD'],
    fetch: (request) => {
      const sessionId = new URL(request.url).searchParams.get('sessionId') ?? ''
      const session = ctx.sessions.get(SessionId(sessionId))
      const task = session === undefined ? null : ctx.sessionProjections.stateOf(session, 'workTask')?.task
      return handleWeaveDeliverableRequest(apiUrl, apiKey, request, task)
    },
  })
  const pollIntervalMs = Math.max(500, config.pollIntervalMs ?? 2_000)
  const timers = new Map<string, ReturnType<typeof setTimeout>>()
  const liveSessions = new Map<string, Session>()
  interface PollOperation { readonly controller: AbortController; nextDelay: number | undefined; publishing: boolean }
  const operations = new Map<string, PollOperation>()
  let disposed = false
  const terminalChecked = new Set<string>()
  const sessionKey = (session: Session): string => String(session.id)
  const terminal = (task: WorkTaskProjection): boolean => terminalStatus(task.status)
  const recordAssessment = async (session: Session, runId: unknown, revisionId: unknown,
    outcome: 'adopted' | 'needs-revision', note: string): Promise<string | null> => {
    const current = ctx.sessionProjections.stateOf(session, 'workTask')?.task
    const matches = (task: WorkTaskProjection | null | undefined): task is WorkTaskProjection => task != null
      && task.status === 'completed' && task.runId === runId && typeof revisionId === 'string' && revisionId !== ''
      && task.delivery?.revisionId === revisionId && task.delivery.available
      && task.deliverables.some(item => item.kind === 'final' || item.kind === 'summary')
    if (!matches(current)) return '交付版本已经变化，或尚未收到可评价的成果，请刷新后重试。'
    let detail: unknown
    try {
      const response = await fetch(`${apiUrl}/v1/runs/${encodeURIComponent(current.runId)}/delivery`, {
        headers: { Authorization: `Bearer ${apiKey}` }, signal: AbortSignal.timeout(10_000),
      })
      if (!response.ok) { await response.body?.cancel(); return '暂时无法核对交付版本，请稍后重试。' }
      detail = await response.json() as unknown
    } catch { return '暂时无法核对交付版本，请稍后重试。' }
    const delivery = readDelivery(object(detail)?.delivery)
    const latest = ctx.sessionProjections.stateOf(session, 'workTask')?.task
    if (disposed || object(detail)?.run_id !== runId || object(detail)?.status !== 'succeeded'
      || delivery.revisionId !== revisionId || !delivery.available || !matches(latest)) {
      return '交付版本已经变化，请刷新后重新评价。'
    }
    const now = Date.now()
    session.append('weave/work-task', { ...latest, outcome, outcomeNote: note,
      outcomeRevisionId: delivery.revisionId, outcomeAssessedAt: now,
      latestAssessment: { runId: latest.runId, revisionId: delivery.revisionId, outcome, note, assessedAt: now }, updatedAt: now })
    await ctx.sessions.flush(session)
    terminalChecked.delete(sessionKey(session))
    schedule(session, 0)
    return null
  }
  const stop = (session: Session): void => {
    const key = sessionKey(session)
    const timer = timers.get(key)
    if (timer !== undefined) clearTimeout(timer)
    timers.delete(key)
    liveSessions.delete(key)
  }
  const schedule = (session: Session, delay = 0): void => {
    if (disposed) return
    const key = sessionKey(session)
    liveSessions.set(key, session)
    if (apiKey === '') return
    const operation = operations.get(key)
    if (operation !== undefined) { operation.nextDelay = Math.min(operation.nextDelay ?? delay, delay); return }
    if (timers.has(key)) return
    const current = ctx.sessionProjections.stateOf(session, 'workTask')?.task
    if (current === null || current === undefined) return
    const building = current.runId === '' && current.preparation?.buildId !== '' && current.preparation?.state === 'building'
    if (current.clientRequestId === '' && current.runId === '' && !building) return
    if (!terminal(current)) terminalChecked.delete(key)
    if (terminal(current) && current.pendingAction === null && terminalChecked.has(key)) return
    timers.set(key, setTimeout(() => {
      timers.delete(key)
      const latest = liveSessions.get(key)
      if (latest !== undefined) {
        const operation: PollOperation = { controller: new AbortController(), nextDelay: undefined, publishing: false }
        operations.set(key, operation)
        void poll(latest, operation).finally(() => {
          if (operations.get(key) !== operation) return
          operations.delete(key)
          if (!operation.controller.signal.aborted && liveSessions.has(key)) schedule(latest, operation.nextDelay ?? pollIntervalMs)
        })
      }
    }, delay))
  }
  connection.fetch.register({
    path: '/api/weave.task-action', methods: ['POST'],
    fetch: async (request) => {
      let body: unknown
      try { body = await request.json() as unknown } catch {
        return Response.json({ error: '操作信息不完整，请重试。' }, { status: 400 })
      }
      const parsed = browserTaskActionSchema.safeParse(body)
      if (!parsed.success) return Response.json({ error: '操作信息不完整，请重试。' }, { status: 400 })
      const input = parsed.data
      const session = ctx.sessions.get(SessionId(input.sessionId))
      const current = session === undefined ? null : ctx.sessionProjections.stateOf(session, 'workTask')?.task
      if (session === undefined || current === null || current === undefined || current.runId !== input.runId) {
        return Response.json({ error: '当前任务状态已经变化，请刷新后重试。' }, { status: 409 })
      }
      if (input.action === 'assess') {
        const error = await recordAssessment(session, input.runId, input.deliveryRevisionId, input.outcome, input.note.trim())
        if (error !== null) return Response.json({ error }, { status: 409 })
        return new Response(null, { status: 204 })
      }
      if (input.action === 'recheck') {
        if (current.delivery?.revisionId !== input.deliveryRevisionId
          || current.delivery.contractDigest !== input.contractDigest || !current.delivery.available) {
          return Response.json({ error: '交付版本已经变化，请刷新后重新核验。' }, { status: 409 })
        }
        try {
          const response = await fetch(`${apiUrl}/v1/runs/${encodeURIComponent(current.runId)}/delivery/recheck`, {
            method: 'POST', headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
            body: JSON.stringify({ revision_id: input.deliveryRevisionId, contract_digest: input.contractDigest }),
            signal: AbortSignal.any([request.signal, AbortSignal.timeout(50_000)]),
          })
          if (!response.ok) {
            await response.body?.cancel()
            return Response.json({ error: response.status === 409 ? '交付版本已经变化，请刷新后重新核验。'
              : response.status === 422 ? '这份历史成果缺少重查所需的证据，暂时无法重新核验。' : '核验请求未完成，请稍后重试。' }, { status: response.status === 409 ? 409 : 503 })
          }
          const result = object(await response.json() as unknown)
          if (!disposed) { terminalChecked.delete(sessionKey(session)); schedule(session, 0) }
          if (result?.current !== true) return Response.json({ error: '成果或核验记录已经变化，本次结果保留在历史中，请查看最新记录。' }, { status: 409 })
          return new Response(null, { status: 204 })
        } catch { return Response.json({ error: '核验结果暂时无法确认，请刷新后查看最新记录。' }, { status: 503 }) }
      }
      if (current.pendingAction !== null) return Response.json({ error: '已有一个操作正在处理中，请稍后再试。' }, { status: 409 })
      let pendingAction: WorkTaskPendingAction
      if (input.action === 'stop') {
        if (terminal(current) || current.status === 'stopping') return Response.json({ error: '这次运行已经无法停止。' }, { status: 409 })
        pendingAction = { kind: 'stop', targetRunId: current.runId, idempotencyKey: `workbench-stop:${randomUUID()}`,
          clientRequestId: '', brief: '', requestedAt: Date.now(), targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: '', nodeId: '' }
      } else if (input.action === 'rerun') {
        if (!terminal(current)) return Response.json({ error: '当前任务结束后才能重新运行。' }, { status: 409 })
        pendingAction = { kind: 'rerun', targetRunId: current.runId, idempotencyKey: '', clientRequestId: randomUUID(), brief: input.brief,
          requestedAt: Date.now(), targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: '', nodeId: '' }
      } else if (input.action === 'stage-retry') {
        const retryable = canRetryStage(current, input.nodeId)
        if (!retryable) return Response.json({ error: '这个阶段当前不能单独重试。' }, { status: 409 })
        const budget = current.members.flatMap(member => member.stages).find(stage => stage.nodeId === input.nodeId)?.budgetPause
        if ((budget?.reason === 'total_limit' && (input.authorizedTotalRounds ?? 0) <= budget.authorizedTotalRounds)
          || (input.authorizedTotalRounds !== undefined && (budget === undefined || input.authorizedTotalRounds < budget.authorizedTotalRounds))) {
          return Response.json({ error: '请输入高于已用额度的新累计轮次上限。' }, { status: 409 })
        }
        pendingAction = { kind: 'stage-retry', authorizedTotalRounds: input.authorizedTotalRounds, targetRunId: current.runId, idempotencyKey: `workbench-stage-retry:${randomUUID()}`, clientRequestId: '', brief: '', requestedAt: Date.now(),
          targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: current.members.flatMap(member => member.stages).find(stage => stage.nodeId === input.nodeId)?.name ?? '', nodeId: input.nodeId }
      } else if (input.action === 'human-complete') {
        if (current.status !== 'waiting' || current.waitKind !== 'human' || current.humanTask?.interactionId !== input.interactionId) {
          return Response.json({ error: '这个问题已经变化，请查看最新问题后回答。' }, { status: 409 })
        }
        const encodedPayload = JSON.stringify(input.payload)
        if (Buffer.byteLength(encodedPayload, 'utf8') > 64_000) return Response.json({ error: '回答内容过长，请精简后再提交。' }, { status: 400 })
        pendingAction = { kind: 'human-complete', targetRunId: current.runId, idempotencyKey: `workbench-human:${randomUUID()}`,
          clientRequestId: '', brief: '', requestedAt: Date.now(), targetKind: '', targetMemberId: '', correctionId: '', disposition: '',
          instruction: current.humanTask.title, nodeId: current.humanTask.nodeId,
          humanPayload: input.payload, humanInteractionId: input.interactionId }
      } else if (input.action === 'correction-request') {
        const validMember = input.targetKind === 'team' || current.members.some(member => member.agentId === input.targetMemberId)
        const correctionActive = current.corrections.some(item => ['requested', 'ready', 'confirmed'].includes(item.status))
        if (terminal(current) || current.status === 'stopping' || !validMember || correctionActive) {
          return Response.json({ error: '当前状态无法开始这次纠偏，请刷新后重试。' }, { status: 409 })
        }
        pendingAction = { kind: 'correction-request', targetRunId: current.runId, idempotencyKey: `workbench-correction:${randomUUID()}`,
          clientRequestId: '', brief: '', requestedAt: Date.now(), targetKind: input.targetKind,
          targetMemberId: input.targetKind === 'member' ? input.targetMemberId : '', correctionId: '', disposition: '', instruction: input.instruction, nodeId: '' }
      } else {
        const ready = current.corrections.some(item => item.correctionId === input.correctionId && item.status === 'ready')
        if (!ready) return Response.json({ error: '纠偏范围已经变化，请刷新后重新确认。' }, { status: 409 })
        pendingAction = { kind: 'correction-confirm', targetRunId: current.runId, idempotencyKey: `workbench-correction-confirm:${randomUUID()}`,
          clientRequestId: '', brief: '', requestedAt: Date.now(), targetKind: '', targetMemberId: '', correctionId: input.correctionId,
          disposition: input.disposition, instruction: '', nodeId: '' }
      }
      session.append('weave/work-task-action', { pendingAction })
      await ctx.sessions.flush(session)
      schedule(session, 0)
      return new Response(null, { status: 204 })
    },
  })
  const poll = async (session: Session, operation: PollOperation): Promise<void> => {
    const current = ctx.sessionProjections.stateOf(session, 'workTask')?.task
    if (current === null || current === undefined) return
    const latestForRun = (): WorkTaskProjection | undefined => {
      const latest = ctx.sessionProjections.stateOf(session, 'workTask')?.task
      return !operation.controller.signal.aborted && latest != null && latest.runId === current.runId
        && latest.clientRequestId === current.clientRequestId ? latest : undefined
    }
    const sameAction = (latest: WorkTaskProjection | undefined, action: WorkTaskPendingAction): latest is WorkTaskProjection =>
      latest !== undefined && latest.pendingAction?.kind === action.kind
      && latest.pendingAction.idempotencyKey === action.idempotencyKey
      && latest.pendingAction.clientRequestId === action.clientRequestId && latest.pendingAction.requestedAt === action.requestedAt
    const receipt = (latest: WorkTaskProjection, action: WorkTaskPendingAction, outcome: WorkTaskActionReceipt['outcome']): WorkTaskActionReceipt[] => {
      const id = action.idempotencyKey || action.clientRequestId
      return latest.actionHistory.some(item => item.id === id)
        ? latest.actionHistory : [...latest.actionHistory, { id, action, outcome, resolvedAt: Date.now() }]
    }
    try {
      const request = (path: string, init: RequestInit = {}): Promise<Response> => {
        const headers = new Headers(init.headers)
        headers.set('Authorization', `Bearer ${apiKey}`)
        if (!headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
        return fetch(`${apiUrl}${path}`, { ...init, headers, signal: AbortSignal.any([operation.controller.signal, AbortSignal.timeout(15_000)]) })
      }
      if (current.runId === '' && current.preparation?.state === 'building' && current.preparation.buildId !== '') {
        const response = await request(`/v1/internal/team-build-runs/${encodeURIComponent(current.preparation.buildId)}/progress`)
        if (response.ok) {
          const value = await response.json() as unknown
          const latest = latestForRun()
          if (latest?.preparation?.callId === current.preparation.callId && latest.preparation.state === 'building') {
            const next = buildSnapshot(latest, value, Date.now())
            if (next !== latest) {
              operation.publishing = true
              try { session.append('weave/work-task', next) } finally { operation.publishing = false }
              await ctx.sessions.flush(session)
            }
          }
        } else await response.body?.cancel()
        return
      }
      if (current.pendingAction !== null) {
        const action = current.pendingAction
        if (action.kind === 'human-complete' && !action.humanInteractionId) {
          session.append('weave/work-task', { ...current, pendingAction: null, actionError: 'action_rejected', updatedAt: Date.now() })
          await ctx.sessions.flush(session)
          schedule(session, 0)
          return
        }
        if (action.kind === 'stage-retry' && action.idempotencyKey === '') {
          session.append('weave/work-task-action', { pendingAction: { ...action, idempotencyKey: `workbench-stage-retry:${randomUUID()}` } })
          await ctx.sessions.flush(session)
          schedule(session, 0)
          return
        }
        let response: Response
        if (action.kind === 'stop') {
          response = await request(`/v1/runs/${encodeURIComponent(action.targetRunId)}/stop`, {
            method: 'POST', body: JSON.stringify({ reason: 'workbench_user_requested', idempotency_key: action.idempotencyKey }),
          })
        } else if (action.kind === 'rerun') {
          if (dispatchInputs === undefined) throw new Error('dispatch connection is unavailable')
          try {
            response = Response.json(await dispatchInputs.rerun(session, rerunFacts(session, current), action.clientRequestId,
              AbortSignal.any([operation.controller.signal, AbortSignal.timeout(15_000)])))
          } catch (error) {
            if (!(error instanceof DispatchResponseError) || !error.rejected) throw error
            response = Response.json({ code: error.code }, { status: 409 })
          }
        } else if (action.kind === 'stage-retry') {
          response = await request(`/v1/runs/${encodeURIComponent(action.targetRunId)}/stages/${encodeURIComponent(action.nodeId)}/retry`, {
            method: 'POST', body: JSON.stringify({ idempotency_key: action.idempotencyKey, authorized_total_rounds: action.authorizedTotalRounds }),
          })
        } else if (action.kind === 'human-complete') {
          response = await request(`/v1/human-tasks/${encodeURIComponent(action.targetRunId)}/complete`, {
            method: 'POST', body: JSON.stringify({ payload: action.humanPayload, interaction_id: action.humanInteractionId, idempotency_key: action.idempotencyKey }),
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
          const latest = latestForRun()
          if (!sameAction(latest, action)) return
          let next: WorkTaskProjection
          if (action.kind === 'rerun') {
            next = snapshot(latest, value, Date.now(), { brief: action.brief, clientRequestId: text(value, ['client_request_id']),
              runId: text(value, ['run_id', 'runId']), pendingAction: null, actionError: '' })
          } else if (action.kind === 'correction-request') {
            const correction = correctionList({ corrections: [value] })[0]
            const corrections = correction === undefined
              ? latest.corrections
              : [correction, ...latest.corrections.filter(item => item.correctionId !== correction.correctionId)]
            next = { ...latest, pendingAction: null, actionError: '',
              corrections,
              updatedAt: Date.now() }
          } else if (action.kind === 'correction-confirm' || action.kind === 'stage-retry' || action.kind === 'human-complete') {
            next = { ...latest, pendingAction: null, actionError: '', updatedAt: Date.now() }
          } else {
            next = snapshot(latest, value, Date.now(), { pendingAction: null, actionError: '' })
          }
          next = { ...next, ...assessmentForDelivery(latest, next.runId, next.delivery), actionHistory: receipt(latest, action, 'accepted') }
          session.append('weave/work-task', next)
          await ctx.sessions.flush(session)
          if (action.kind === 'rerun') terminalChecked.delete(sessionKey(session))
          schedule(session, 0)
          return
        }
        if (response.status >= 400 && response.status < 500) {
          await response.body?.cancel()
          const latest = latestForRun()
          if (!sameAction(latest, action)) return
          const next = { ...latest, pendingAction: null, actionError: 'action_rejected', actionHistory: receipt(latest, action, 'rejected'), updatedAt: Date.now() }
          session.append('weave/work-task', next)
          await ctx.sessions.flush(session)
          schedule(session, 0)
          return
        }
        await response.body?.cancel()
      }
      const response = current.runId !== ''
        ? await request(`/v1/runs/${encodeURIComponent(current.runId)}/activity`)
        : await request(`/v1/chat-requests/${encodeURIComponent(current.clientRequestId)}`)
      if (response.ok) {
        const activity = await response.json() as unknown
        if (current.runId !== '' && object(activity)?.run_id !== current.runId) return
        let next = snapshot(current, activity, Date.now())
        if (current.runId !== '' && !Object.hasOwn(object(activity) ?? {}, 'delivery')) next = { ...next,
          delivery: { ...readDelivery(undefined), reason: 'delivery_verification_unsupported', evidenceCompleteness: 'complete' } }
        let supplementalReadsComplete = true
        const readSupplement = async (path: string): Promise<unknown> => {
          try {
            const response = await request(path)
            if (response.ok) return await response.json() as unknown
            await response.body?.cancel()
          } catch { /* A failed detail read keeps the observed activity and remains eligible for retry. */ }
          supplementalReadsComplete = false
          return undefined
        }
        if (terminal(next) && next.delivery?.verificationStatus === 'pending') {
          const detail = await readSupplement(`/v1/runs/${encodeURIComponent(next.runId)}/delivery`)
          const delivery = readDelivery(object(detail)?.run_id === next.runId ? object(detail)?.delivery : undefined)
          next = { ...next, delivery: delivery.verificationStatus === 'pending'
            ? { ...delivery, verificationStatus: 'unknown', reason: 'delivery_report_missing' } : delivery }
        }
        if (next.teamName === '' && next.teamId !== '') {
          const detail = await readSupplement(`/v1/teams/${encodeURIComponent(next.teamId)}?include=summary`)
          const team = object(object(detail)?.team)
          if (team?.id === next.teamId) {
            next = { ...next, teamName: teamDisplayName(preferredText(team, ['display_name', 'displayName', 'name'])) }
          } else { supplementalReadsComplete = false }
        }
        if (next.status === 'waiting' && next.waitKind === 'human' && next.runId !== '') {
          const humanResponse = await request(`/v1/human-tasks/${encodeURIComponent(next.runId)}`)
          if (humanResponse.ok) {
            const detail = object(await humanResponse.json() as unknown)
            const resumeSchema = object(detail?.resume_schema)
            if (detail?.run_id === next.runId && typeof detail.interaction_id === 'string' && detail.interaction_id !== '' && typeof detail.node_id === 'string' && detail.node_id !== '' && resumeSchema !== undefined) {
              next = { ...next, waitNodeId: detail.node_id, humanTaskCount: 1, humanTask: {
                interactionId: detail.interaction_id, nodeId: detail.node_id,
                title: typeof detail.title === 'string' ? detail.title : '', instructions: typeof detail.instructions === 'string' ? detail.instructions : '', resumeSchema,
              } }
            }
          } else { await humanResponse.body?.cancel() }
        }
        const reportedDeliverables = collectionCount(activity, ['deliverables', 'items'])
        if (next.runId !== '' && (terminal(next) || reportedDeliverables !== next.deliverableCount)) {
          const value = await readSupplement(`/v1/deliverables?run_id=${encodeURIComponent(next.runId)}&limit=100`)
          if (Array.isArray(value) || Array.isArray(object(value)?.deliverables) || Array.isArray(object(value)?.items)) {
            const deliverables = deliverableList(value, next.runId)
            next = { ...next, deliverableCount: deliverables.length, deliverables }
          } else { supplementalReadsComplete = false }
        }
        const latest = latestForRun()
        if (latest === undefined || next.observedAt < latest.observedAt) return
        next = resyncAttempts({ ...next, pendingAction: latest.pendingAction,
          actionError: latest.actionError !== current.actionError ? latest.actionError : next.actionError,
          ...assessmentForDelivery(latest, next.runId, next.delivery), actionHistory: latest.actionHistory }, latest)
        const materiallyChanged = !isDeepStrictEqual({ ...next, observedAt: 0, updatedAt: 0 },
          { ...latest, observedAt: 0, updatedAt: 0 })
        if (materiallyChanged || next.observedAt - latest.observedAt >= 30_000) {
          next = { ...next, updatedAt: Date.now() }
          operation.publishing = true
          try { session.append('weave/work-task', next) } finally { operation.publishing = false }
          await ctx.sessions.flush(session)
        }
        const settled = latestForRun()
        if (settled !== undefined && terminal(settled) && settled.pendingAction === null
          && operation.nextDelay !== 0 && settled.delivery?.evidenceCompleteness !== 'unavailable'
          && supplementalReadsComplete && Array.isArray(object(activity)?.members)
          && settled.completeness.members !== 'unavailable') {
          terminalChecked.add(sessionKey(session)); stop(session); return
        }
      } else { await response.body?.cancel() }
    } catch { /* Transient connection loss must not become a false task failure. */ }
    schedule(session, pollIntervalMs)
  }
  ctx.on('session/created', (session) => { queueMicrotask(() => { schedule(session) }) })
  ctx.on('session/event', (session, event) => {
    // The active poll owns its next interval; its durable snapshot cannot trigger another immediate read.
    if (event.type === 'weave/work-task' && operations.get(sessionKey(session))?.publishing === true) return
    if (event.type === 'tool/result' || event.type === 'weave/work-task' || event.type === 'weave/work-task-action'
      || event.type === 'weave/dispatch-input') queueMicrotask(() => { schedule(session) })
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
        targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: '', nodeId: '',
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
      const brief = typeof request.brief === 'string' ? request.brief : ''
      if (current === null || current === undefined || !terminal(current)
        || typeof request.runId !== 'string' || request.runId !== current.runId || brief.trim() === '' || brief.length > 100_000) {
        return { kind: 'error', text: 'A terminal current run and a complete revised brief are required.' }
      }
      if (current.pendingAction !== null) return { kind: 'error', text: 'Another Weave action is already pending.' }
      const pendingAction: WorkTaskPendingAction = {
        kind: 'rerun', targetRunId: current.runId, idempotencyKey: '',
        clientRequestId: randomUUID(), brief, requestedAt: Date.now(),
        targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: '', nodeId: '',
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
        targetKind, targetMemberId: targetKind === 'member' ? targetMemberId : '', correctionId: '', disposition: '', instruction, nodeId: '',
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
        targetKind: '', targetMemberId: '', correctionId, disposition, instruction: '', nodeId: '',
      }
      agent.session.append('weave/work-task-action', { pendingAction })
      await ctx.sessions.flush(agent.session)
      schedule(agent.session, 0)
      return { kind: 'success', text: 'Weave correction confirmation recorded.' }
    },
  }), 'workbench: correction confirmation command')
  ctx.effect(() => ctx.commands.register({
    name: 'weave-retry-stage',
    description: 'retry one exact infrastructure-failed Weave stage without invoking the model',
    recordInput: false,
    handler: async ({ agent, rawInput }) => {
      const current = ctx.sessionProjections.stateOf(agent.session, 'workTask')?.task
      let request: { runId?: unknown; nodeId?: unknown }
      try { request = JSON.parse(rawInput.trim()) as typeof request } catch { return { kind: 'error', text: 'Invalid Weave stage retry request.' } }
      const nodeId = typeof request.nodeId === 'string' ? request.nodeId.trim() : ''
      const retryable = current !== null && current !== undefined && canRetryStage(current, nodeId)
      if (current === null || current === undefined || request.runId !== current.runId || nodeId === '' || !retryable) {
        return { kind: 'error', text: 'A retryable infrastructure-failed stage is required.' }
      }
      if (current.pendingAction !== null) return { kind: 'error', text: 'Another Weave action is already pending.' }
      const pendingAction: WorkTaskPendingAction = {
        kind: 'stage-retry', targetRunId: current.runId, idempotencyKey: `workbench-stage-retry:${randomUUID()}`, clientRequestId: '', brief: '', requestedAt: Date.now(),
        targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: '', nodeId,
      }
      agent.session.append('weave/work-task-action', { pendingAction })
      await ctx.sessions.flush(agent.session)
      schedule(agent.session, 0)
      return { kind: 'success', text: 'Weave stage retry request recorded.' }
    },
  }), 'workbench: exact failed stage retry command')
  ctx.effect(() => ctx.commands.register({
    name: 'weave-assess',
    description: 'record whether the final Weave delivery is usable without invoking the model',
    recordInput: false,
    handler: async ({ agent, rawInput }) => {
      let request: { runId?: unknown; deliveryRevisionId?: unknown; outcome?: unknown; note?: unknown }
      try { request = JSON.parse(rawInput.trim()) as typeof request } catch { return { kind: 'error', text: 'Invalid Weave outcome assessment.' } }
      const outcome = request.outcome === 'adopted' || request.outcome === 'needs-revision' ? request.outcome : ''
      const note = typeof request.note === 'string' ? request.note.trim().slice(0, 2_000) : ''
      if (outcome === '') return { kind: 'error', text: 'Invalid Weave outcome assessment.' }
      const error = await recordAssessment(agent.session, request.runId, request.deliveryRevisionId, outcome, note)
      if (error !== null) return { kind: 'error', text: error }
      return { kind: 'success', text: 'Weave delivery outcome recorded.' }
    },
  }), 'workbench: delivery outcome command')
  ctx.effect(() => () => {
    disposed = true
    for (const operation of operations.values()) operation.controller.abort()
    operations.clear()
    for (const timer of timers.values()) clearTimeout(timer)
    timers.clear()
    liveSessions.clear()
    terminalChecked.clear()
  }, 'workbench work-task pollers')
}
