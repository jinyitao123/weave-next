/** Workbench-owned dispatch inputs, frozen before any network request. */

import { createHash, randomUUID } from 'node:crypto'
import { isDeepStrictEqual } from 'node:util'
import type { Context } from '@deepseek-ai/cordis'
import { snapshotJsonValue, type JsonValue, type Session, type SessionEvent } from '@deepseek-ai/dsh-session'
import { defineTool } from '@deepseek-ai/dsh-tools'
import { z } from 'zod'

/** Team and published workflow selected for the current user inputs. */
export interface DispatchInputFacts {
  readonly team_id: string
  readonly workflow_id?: string
  readonly workflow_version?: number
  readonly project_id?: string
}

/** One exact, identified user message included in a dispatch. */
export interface DispatchSourceMessage {
  readonly message_id: string
  readonly event_seq: number
  readonly sha256: string
  readonly content: JsonValue
}

/** Host input and network outcome retained together for restart-safe retries. */
export interface DispatchInputRecord {
  readonly registrationId: string
  readonly expectedRevisionId: string
  readonly requestedTeamRef?: string
  readonly facts: DispatchInputFacts
  readonly sourceThroughSeq: number
  readonly sourceMessages: readonly DispatchSourceMessage[]
  readonly task: string
  readonly state: 'pending' | 'registered' | 'accepted' | 'rejected' | 'closed'
  readonly rerun: { readonly targetRunId: string; readonly actionId: string } | null
  readonly revision: { readonly input_revision_id: string; readonly client_request_id: string; readonly task_sha256: string } | null
  readonly result: Readonly<Record<string, JsonValue>> | null
  readonly errorCode: string
}

declare module '@deepseek-ai/dsh-session/types' {
  interface SessionEventMap {
    /** Exact source messages and one immutable dispatch request; accepted advances its source watermark. */
    'weave/dispatch-input': DispatchInputRecord
  }
}

const factsSchema = z.object({
  team_id: z.string().min(1), workflow_id: z.string().min(1).optional(),
  workflow_version: z.number().int().positive().optional(), project_id: z.string().min(1).optional(),
}).strict()
const revisionSchema = z.object({
  input_revision_id: z.uuid(), client_request_id: z.uuid(), task_sha256: z.string().regex(/^[a-f0-9]{64}$/),
})

// Registration checks the existing request before these selection errors. They
// prove this request has no revision; HTTP status alone cannot prove admission.
const unregisteredSelectionErrors = new Set([
  'team_not_found', 'team_not_active', 'no_default_workflow', 'default_workflow_unavailable',
  'team_reference_ambiguous', 'workflow_team_mismatch', 'workflow_not_published', 'dispatch_delivery_contract_invalid',
])

function digest(text: string): string { return createHash('sha256').update(text, 'utf8').digest('hex') }

function parseFacts(value: unknown): DispatchInputFacts {
  const facts = factsSchema.parse(value)
  return { team_id: facts.team_id,
    ...(facts.workflow_id === undefined ? {} : { workflow_id: facts.workflow_id }),
    ...(facts.workflow_version === undefined ? {} : { workflow_version: facts.workflow_version }),
    ...(facts.project_id === undefined ? {} : { project_id: facts.project_id }) }
}

function freezeSource(messageId: string, seq: number, content: unknown): DispatchSourceMessage {
  const snapshot = snapshotJsonValue(content)
  if (snapshot === undefined) throw new Error('dispatch_input_source_invalid')
  return { message_id: messageId, event_seq: seq, sha256: digest(JSON.stringify(snapshot)), content: snapshot as JsonValue }
}

function ownedEvents(session: Session): readonly SessionEvent[] {
  return session.events.slice(session.header.seedLength ?? 0)
}

function requireResolvedForkInput(session: Session): void {
  const inherited = session.events.slice(0, session.header.seedLength ?? 0)
    .findLast(event => event.type === 'weave/dispatch-input')
  if (inherited?.data.state === 'pending' || inherited?.data.state === 'registered') {
    throw new DispatchResponseError('dispatch_inherited_input_pending: resolve the original session dispatch, then fork from its resolved history; an independent task can use a new empty session', true)
  }
}

/**
 * Read the last request owned by this Session, excluding inherited fork history.
 * @param session - the authoritative supervising session.
 * @returns its most recent request, if one has been frozen in this Session.
 */
export function latestDispatchInput(session: Session): DispatchInputRecord | undefined {
  const latest = ownedEvents(session).findLast(event => event.type === 'weave/dispatch-input')
  return latest === undefined ? undefined : decodeDispatchInput(latest.data, session.events.filter(event => event.seq < latest.seq))
}

/**
 * Validate a persisted input against the exact source history preceding its event.
 * @param value - candidate dispatch input read from a durable record.
 * @param sourceEvents - original Session events before this input was recorded.
 * @returns the original record reference after source bytes and receipt identities are verified.
 * @throws when a source, digest, task body or admitted receipt differs from its recorded identity.
 */
export function decodeDispatchInput(value: unknown, sourceEvents: readonly SessionEvent[]): DispatchInputRecord {
  const record = recordSchema.parse(value)
  let priorSeq = -1
  for (const message of record.sourceMessages) {
    if (message.event_seq <= priorSeq) throw new Error('dispatch_input_source_order_invalid')
    priorSeq = message.event_seq
    if (digest(JSON.stringify(message.content)) !== message.sha256) throw new Error('dispatch_input_source_digest_mismatch')
    const event = sourceEvents.find(candidate => candidate.seq === message.event_seq)
    const original = record.rerun === null
      ? event?.type === 'user/message' && event.data.source.kind === 'user' && event.data.id === message.message_id ? event.data.content : undefined
      : event?.type === 'weave/work-task-action' && event.data.pendingAction?.kind === 'rerun'
        && event.data.pendingAction.clientRequestId === record.rerun.actionId
        && event.data.pendingAction.targetRunId === record.rerun.targetRunId
        && message.message_id === `workbench-rerun:${record.rerun.actionId}`
        ? [{ type: 'text', text: event.data.pendingAction.brief }] : undefined
    if (original === undefined || digest(JSON.stringify(original)) !== message.sha256) throw new Error('dispatch_input_source_identity_mismatch')
  }
  if (taskText(record.sourceMessages) !== record.task) throw new Error('dispatch_input_task_source_mismatch')
  if (record.revision !== null && digest(record.task) !== record.revision.task_sha256) throw new Error('dispatch_input_digest_mismatch')
  if (record.state === 'accepted' && record.result !== null && record.revision !== null) validateRunReceipt(record.result, record.revision)
  return value as DispatchInputRecord
}

const recordSchema = z.object({
  registrationId: z.uuid(), expectedRevisionId: z.union([z.literal(''), z.uuid()]),
  requestedTeamRef: z.string().min(1).optional(), facts: factsSchema,
  sourceThroughSeq: z.number().int().nonnegative(),
  sourceMessages: z.array(z.object({ message_id: z.string().min(1), event_seq: z.number().int().nonnegative(),
    sha256: z.string().regex(/^[a-f0-9]{64}$/), content: z.json() }).strict()).min(1),
  task: z.string().min(1), state: z.enum(['pending', 'registered', 'accepted', 'rejected', 'closed']),
  rerun: z.object({ targetRunId: z.string().min(1), actionId: z.string().min(1) }).strict().nullable(),
  revision: revisionSchema.strict().nullable(), result: z.record(z.string(), z.json()).nullable(), errorCode: z.string(),
}).strict().superRefine((record, ctx) => {
  if (['registered', 'accepted', 'closed'].includes(record.state) && record.revision === null) ctx.addIssue({ code: 'custom', message: 'resolved input requires a revision' })
  if (record.state === 'accepted' && (record.result === null || typeof record.result.run_id !== 'string')) ctx.addIssue({ code: 'custom', message: 'accepted input requires a run receipt' })
  if (record.sourceMessages.at(-1)?.event_seq !== record.sourceThroughSeq) ctx.addIssue({ code: 'custom', message: 'source watermark must identify the frozen upper bound' })
})

function legacyWatermark(events: readonly SessionEvent[]): number {
  const calls = new Map<string, number>()
  let through = -1
  for (const event of events) {
    if (event.type === 'tool/call' && event.data.name === 'mcp__weave__team_dispatch') calls.set(event.data.callId, event.seq)
    if (event.type !== 'tool/result') continue
    const call = calls.get(event.data.message.source.callId)
    const result = event.data.message.content[0]
    if (call === undefined || result.isError) continue
    for (const block of result.content) {
      if (block.type !== 'text') continue
      let value: unknown
      try { value = JSON.parse(block.text) as unknown } catch { continue /* Legacy non-JSON results do not prove admission. */ }
      if (typeof value === 'object' && value !== null && 'run_id' in value && typeof value.run_id === 'string' && value.run_id !== '') {
        through = Math.max(through, call)
      }
    }
  }
  return through
}

function sourceText(content: JsonValue): string {
  if (!Array.isArray(content)) throw new Error('dispatch_input_source_invalid')
  return content.map((block) => {
    if (typeof block === 'object' && block !== null && !Array.isArray(block) && block.type === 'text' && typeof block.text === 'string') return block.text
    return JSON.stringify(block)
  }).join('')
}

function taskText(messages: readonly DispatchSourceMessage[]): string {
  const first = messages[0]
  if (first === undefined) throw new Error('dispatch_input_source_missing')
  return messages.length === 1 ? sourceText(first.content)
    : messages.map((message, index) => `[User input ${index + 1}]\n${sourceText(message.content)}`).join('\n\n')
}

/**
 * Freeze all unconsumed user inputs; model text cannot replace any of them.
 * @param session - supervising session containing the original message blocks.
 * @param facts - the selected team and published workflow.
 * @returns a new request, an unresolved request to retry, or the last accepted request when no new input exists.
 */
export function prepareDispatchInput(session: Session, facts: DispatchInputFacts): DispatchInputRecord {
  requireResolvedForkInput(session)
  const latest = latestDispatchInput(session)
  if (latest?.state === 'pending' || latest?.state === 'registered') {
    if (latest.rerun !== null) throw new Error('dispatch_input_pending: a rerun is already pending')
    if (!isDeepStrictEqual(latest.facts, facts)) throw new Error('dispatch_input_pending: resolve the existing dispatch before changing its team or workflow')
    return latest
  }
  let watermark = legacyWatermark(session.events)
  let expectedRevisionId = ''
  for (const event of session.events) {
    if (event.type !== 'weave/dispatch-input') continue
    if (event.data.state === 'accepted' && event.data.rerun === null) watermark = Math.max(watermark, event.data.sourceThroughSeq)
  }
  for (const event of ownedEvents(session)) {
    if (event.type === 'weave/dispatch-input' && event.data.revision !== null) expectedRevisionId = event.data.revision.input_revision_id
  }
  const messages: DispatchSourceMessage[] = session.events.flatMap((event) => {
    if (event.type !== 'user/message' || event.data.source.kind !== 'user' || event.seq <= watermark) return []
    return [freezeSource(event.data.id, event.seq, event.data.content)]
  })
  const last = messages.at(-1)
  if (last === undefined) {
    if (latest?.state === 'accepted' && latest.rerun === null && isDeepStrictEqual(latest.facts, facts)) return latest
    throw new Error('dispatch_input_missing: no new user input is available; use the existing task recovery or rerun action')
  }
  const task = taskText(messages)
  if (task.trim() === '') throw new Error('dispatch_input_empty')
  return { registrationId: randomUUID(), expectedRevisionId, requestedTeamRef: facts.team_id, facts, sourceThroughSeq: last.event_seq,
    sourceMessages: messages, task, state: 'pending', rerun: null, revision: null, result: null, errorCode: '' }
}

function prepareRerun(session: Session, facts: DispatchInputFacts, actionId: string): DispatchInputRecord {
  requireResolvedForkInput(session)
  const action = ownedEvents(session).findLast(event => event.type === 'weave/work-task-action'
    && event.data.pendingAction?.kind === 'rerun' && event.data.pendingAction.clientRequestId === actionId)
  if (action?.type !== 'weave/work-task-action' || action.data.pendingAction?.kind !== 'rerun') throw new DispatchResponseError('dispatch_rerun_source_missing: rerun actions must originate in this session', true)
  const latest = latestDispatchInput(session)
  if (latest?.rerun?.actionId === actionId) {
    if (!isDeepStrictEqual(latest.facts, facts)) throw new Error('dispatch_input_pending: the rerun already has a fixed team and workflow')
    return latest
  }
  if (latest?.state === 'pending' || latest?.state === 'registered') throw new Error('dispatch_input_pending')
  const pending = action.data.pendingAction
  const source = freezeSource(`workbench-rerun:${actionId}`, action.seq, [{ type: 'text', text: pending.brief }])
  const revision = ownedEvents(session).findLast(event => event.type === 'weave/dispatch-input' && event.data.revision !== null)
  const expectedRevisionId = revision?.type === 'weave/dispatch-input' ? revision.data.revision?.input_revision_id ?? '' : ''
  return { registrationId: randomUUID(), expectedRevisionId, requestedTeamRef: facts.team_id,
    facts, sourceThroughSeq: action.seq, sourceMessages: [source], task: pending.brief, state: 'pending',
    rerun: { targetRunId: pending.targetRunId, actionId }, revision: null, result: null, errorCode: '' }
}

/** Trusted Host connection used only by the registered Workbench dispatch tool. */
export interface DispatchInputConnection {
  readonly apiUrl: string
  readonly apiKey: string
}

/** Transport result with an explicit distinction between rejection and unknown admission. */
export class DispatchResponseError extends Error {
  constructor(readonly code: string, readonly rejected: boolean) { super(code) }
}

/** Host-only entry for revised briefs already recorded by a user action. */
export interface DispatchInputService {
  /**
   * Dispatch one durable rerun action; repeated calls reuse its revision and request identity.
   * @param session - supervising session with the user's recorded rerun action.
   * @param facts - team and workflow fixed for that action.
   * @param actionId - exact pending action identity, never a model task body.
   * @param signal - caller cancellation; unknown outcomes remain durable for reconciliation.
   * @returns the matching server run receipt after local persistence succeeds.
   */
  rerun(session: Session, facts: DispatchInputFacts, actionId: string, signal: AbortSignal): Promise<Record<string, JsonValue>>
}

function validateRunReceipt(result: Readonly<Record<string, JsonValue>>, revision: NonNullable<DispatchInputRecord['revision']>): void {
  if (typeof result.run_id !== 'string' || result.run_id === '') throw new DispatchResponseError('dispatch_run_identity_missing', false)
  if (result.client_request_id !== revision.client_request_id) throw new DispatchResponseError('dispatch_request_identity_mismatch', false)
  if (result.input_revision_id !== revision.input_revision_id) throw new DispatchResponseError('dispatch_revision_identity_mismatch', false)
}

async function post(
  connection: DispatchInputConnection, path: string, body: unknown, signal: AbortSignal,
): Promise<Record<string, JsonValue>> {
  const response = await fetch(`${connection.apiUrl}${path}`, { method: 'POST', signal,
    headers: { Authorization: `Bearer ${connection.apiKey}`, 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
  let value: unknown
  try { value = await response.json() as unknown } catch {
    throw new DispatchResponseError(`dispatch_response_invalid_${response.status}`, false)
  }
  if (!response.ok) {
    const candidate = typeof value === 'object' && value !== null && 'code' in value ? value.code : undefined
    const code = typeof candidate === 'string' && /^[a-z0-9_]+$/.test(candidate) ? candidate : `dispatch_http_${response.status}`
    throw new DispatchResponseError(code, false)
  }
  if (typeof value !== 'object' || value === null || Array.isArray(value)) throw new DispatchResponseError('dispatch_response_invalid', false)
  return snapshotJsonValue(value) as Record<string, JsonValue>
}

function teamRecord(value: unknown): Record<string, unknown> | undefined {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined
  const item = value as Record<string, unknown>
  const nested = item.team
  return typeof nested === 'object' && nested !== null && !Array.isArray(nested)
    ? nested as Record<string, unknown> : item
}

async function resolveTeamReference(
  connection: DispatchInputConnection, reference: string, signal: AbortSignal,
): Promise<string> {
  const response = await fetch(`${connection.apiUrl}/v1/teams?status=active&include=summary`, {
    signal, headers: { Authorization: `Bearer ${connection.apiKey}`, Accept: 'application/json' },
  })
  let value: unknown
  try { value = await response.json() as unknown } catch {
    throw new DispatchResponseError(`team_resolution_response_invalid_${response.status}`, false)
  }
  if (!response.ok) throw new DispatchResponseError(`team_resolution_http_${response.status}`, false)
  const entries = Array.isArray(value) ? value
    : typeof value === 'object' && value !== null && Array.isArray((value as Record<string, unknown>).teams)
      ? (value as { teams: unknown[] }).teams : undefined
  if (entries === undefined) throw new DispatchResponseError('team_resolution_response_invalid', false)
  const teams = entries.flatMap((entry) => {
    const team = teamRecord(entry)
    if (team === undefined) return []
    const id = team.id
    if (typeof id !== 'string' || id === '') return []
    return [{ id, name: typeof team.name === 'string' ? team.name : '',
      displayName: typeof team.display_name === 'string' ? team.display_name : '', status: team.status }]
  }).filter(team => team.status === undefined || team.status === 'active')
  const exactID = teams.find(team => team.id === reference)
  if (exactID !== undefined) return exactID.id
  const matches = teams.filter(team => team.name === reference || team.displayName === reference)
  if (matches.length === 0) throw new DispatchResponseError('team_not_found', true)
  if (matches.length > 1) throw new DispatchResponseError('team_reference_ambiguous', true)
  const match = matches[0]
  if (match === undefined) throw new DispatchResponseError('team_not_found', true)
  return match.id
}

/**
 * Register the task-text-free product tool and retain its request before transport.
 * @param ctx - Host services owning tools and the durable session log.
 * @param connection - trusted Weave connection; credentials never enter tool arguments or events.
 * @param canDispatch - product check for the supervising session's current run and pending actions.
 * @returns the same durable dispatch path for explicit user reruns.
 */
export function installDispatchInputTool(
  ctx: Context, connection: DispatchInputConnection, canDispatch?: (session: Session) => void,
): DispatchInputService {
  const controller = new AbortController()
  const operations = new Map<Session, { readonly key: string; readonly promise: Promise<Record<string, JsonValue>> }>()
  const save = async (session: Session, record: DispatchInputRecord): Promise<void> => {
    session.append('weave/dispatch-input', record)
    await ctx.sessions.flush(session)
  }
  const retryFacts = (session: Session, facts: DispatchInputFacts): DispatchInputFacts => {
    const latest = latestDispatchInput(session)
    if (latest === undefined || !['pending', 'registered', 'accepted'].includes(latest.state)
      || facts.team_id !== (latest.requestedTeamRef ?? latest.facts.team_id)) return facts
    const requested = { ...facts, team_id: latest.facts.team_id }
    return isDeepStrictEqual(requested, latest.facts) ? latest.facts : facts
  }
  const dispatch = async (
    session: Session, facts: DispatchInputFacts, signal: AbortSignal, actionId: string,
  ): Promise<Record<string, JsonValue>> => {
    let record = actionId === '' ? prepareDispatchInput(session, facts) : prepareRerun(session, facts, actionId)
    if (record !== latestDispatchInput(session)) await save(session, record)
    else await ctx.sessions.flush(session)
    if (record.state === 'accepted' && record.result !== null) return { ...record.result }
    if (record.state === 'closed' || record.state === 'rejected') throw new DispatchResponseError(record.errorCode || 'dispatch_input_closed', true)
    const accept = async (
      result: Record<string, JsonValue>, revision: NonNullable<DispatchInputRecord['revision']>,
    ): Promise<Record<string, JsonValue>> => {
      validateRunReceipt(result, revision)
      const accepted = { ...result, task_sha256: revision.task_sha256 }
      record = { ...record, state: 'accepted', result: accepted }
      await save(session, record)
      return accepted
    }
    try {
      if (record.revision === null) {
        const register = () => post(connection, '/v1/workbench/dispatch-inputs', {
          registration_id: record.registrationId, workbench_session_id: String(session.id), expected_revision_id: record.expectedRevisionId,
          source_messages: record.sourceMessages.map(({ message_id, event_seq, sha256 }) => ({ message_id, event_seq, sha256 })),
          task: record.task, mode: 'workflow', ...record.facts,
          ...(record.rerun === null ? {} : { revision_context: {
            parent_input_revision_id: record.expectedRevisionId, parent_run_id: record.rerun.targetRunId,
          } }),
        }, signal)
        let registered: Record<string, JsonValue>
        try { registered = await register() } catch (error) {
          if (!(error instanceof DispatchResponseError) || error.code !== 'team_not_found') throw error
          const teamID = await resolveTeamReference(connection, record.facts.team_id, signal)
          if (teamID === record.facts.team_id) throw error
          record = { ...record, facts: { ...record.facts, team_id: teamID } }
          await save(session, record)
          registered = await register()
        }
        const revision = revisionSchema.parse(registered)
        if (revision.task_sha256 !== digest(record.task)) throw new DispatchResponseError('dispatch_input_digest_mismatch', false)
        record = { ...record, state: 'registered', revision: {
          input_revision_id: revision.input_revision_id, client_request_id: revision.client_request_id, task_sha256: revision.task_sha256,
        } }
        await save(session, record)
      }
      const revision = record.revision
      if (revision === null) throw new Error('dispatch_input_revision_missing')
      const reconcile = session.events.some(event => event.type === 'user/message'
        && event.data.source.kind === 'user' && event.seq > record.sourceThroughSeq)
      if (reconcile) {
        const result = await post(connection, `/v1/workbench/dispatch-inputs/${revision.input_revision_id}/reconcile`, {}, signal)
        const receipt = revisionSchema.parse(result.receipt)
        if (!isDeepStrictEqual(receipt, revision)) {
          throw new DispatchResponseError('dispatch_reconcile_identity_mismatch', false)
        }
        if (result.state === 'accepted' && typeof result.result === 'object' && result.result !== null && !Array.isArray(result.result)) return await accept(result.result, revision)
        if (result.state !== 'closed') throw new DispatchResponseError('dispatch_reconcile_invalid', false)
        record = { ...record, state: 'closed', errorCode: 'dispatch_input_superseded' }
        await save(session, record)
        throw new DispatchResponseError('dispatch_input_superseded: the earlier request was closed without starting it; dispatch the current user inputs again', true)
      }
      const result = await post(connection, `/v1/teams/${encodeURIComponent(record.facts.team_id)}/dispatch`, {
        input_revision_id: revision.input_revision_id, client_request_id: revision.client_request_id,
      }, signal)
      return await accept(result, revision)
    } catch (error) {
      if (record.revision === null && error instanceof DispatchResponseError && unregisteredSelectionErrors.has(error.code)) {
        await save(session, { ...record, state: 'rejected', errorCode: error.code })
        throw new DispatchResponseError(error.code, true)
      }
      throw error
    }
  }
  const execute = async (session: Session, facts: DispatchInputFacts, signal: AbortSignal, actionId = ''): Promise<Record<string, JsonValue>> => {
    const key = JSON.stringify({ facts, actionId })
    const existing = operations.get(session)
    if (existing !== undefined) {
      if (existing.key !== key) throw new Error('dispatch_input_pending')
      return existing.promise
    }
    const promise = dispatch(session, facts, AbortSignal.any([signal, controller.signal]), actionId)
    const operation = { key, promise }
    operations.set(session, operation)
    try { return await promise } finally { if (operations.get(session) === operation) operations.delete(session) }
  }
  ctx.tools.register(defineTool({
    name: 'weave_dispatch',
    description: 'Dispatch the current user task to the agreed Weave team and published workflow. team_id may be the exact visible team name, machine name, or stable ID; Workbench resolves names only against active teams and rejects ambiguity. Use only when the user has authorized the team, task scope and expected outputs; existing explicit authorization is sufficient. Original user inputs are attached automatically and cannot be replaced with a rewritten task. Do not use for progress questions, recovery or rerunning an existing task. Repeating the same unresolved request is safe.',
    parameters: {
      team_id: { type: 'string', required: true, description: 'The agreed active team by visible name, machine name, or stable ID.' },
      workflow_id: { type: 'string', description: 'The agreed published workflow; omit to use the team default.' },
      workflow_version: { type: 'integer', description: 'The agreed published version, when explicitly pinned.' },
      project_id: { type: 'string', description: 'The current Weave project, when one is selected.' },
    },
    output: { schema: { type: 'object', additionalProperties: true, properties: {} },
      render: (_args, value) => [{ type: 'text', text: JSON.stringify(value) }] },
    async execute(input, exec) {
      if (exec.agent === undefined) throw new Error('weave_dispatch requires its supervising Workbench session')
      const session = exec.agent.session
      canDispatch?.(session)
      return execute(session, retryFacts(session, parseFacts(input)), exec.signal)
    },
  }))
  ctx.effect(() => async () => {
    controller.abort()
    await Promise.allSettled([...operations.values()].map(operation => operation.promise))
  }, 'workbench dispatch inputs')
  return { rerun: (session, facts, actionId, signal) => execute(session, facts, signal, actionId) }
}
