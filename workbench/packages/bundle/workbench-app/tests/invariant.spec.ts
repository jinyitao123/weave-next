import { createHash } from 'node:crypto'
import { Context } from '@deepseek-ai/cordis'
import InvariantRegistry, { InvariantError } from '@deepseek-ai/dsh-invariants'
import { createUserMessage } from '@deepseek-ai/dsh-llm'
import SessionStore, { Session, SessionId } from '@deepseek-ai/dsh-session'
import { afterEach, describe, expect, it } from 'vitest'
import { prepareDispatchInput, type DispatchInputRecord } from '../src/dispatch-input.ts'
import * as WorkbenchInvariant from '../src/invariant.ts'

const contexts: Context[] = []

async function host(): Promise<Context> {
  const ctx = new Context()
  contexts.push(ctx)
  await ctx.plugin(SessionStore)
  await ctx.plugin(InvariantRegistry, { enabled: true })
  return ctx
}

function input(session: Session): DispatchInputRecord {
  session.append('user/message', createUserMessage({
    content: [{ type: 'text', text: '  核对 INV-440\r\n保留  空白与尾换行。\n' }], source: { kind: 'user' },
  }), { surfaceOp: 'append' })
  return prepareDispatchInput(session, { team_id: 'orders' })
}

function accepted(record: DispatchInputRecord): DispatchInputRecord {
  const revision = { input_revision_id: '10000000-0000-4000-8000-000000000001',
    client_request_id: '20000000-0000-4000-8000-000000000001',
    task_sha256: createHash('sha256').update(record.task, 'utf8').digest('hex') }
  return { ...record, state: 'accepted', revision, result: { run_id: 'run-1', ...revision } }
}

afterEach(async () => {
  for (const ctx of contexts.splice(0).reverse()) await ctx.fiber.dispose()
})

describe('Workbench dispatch-input invariants', () => {
  it('accepts original source bytes and the matching admitted receipt', async () => {
    const ctx = await host()
    await ctx.plugin(WorkbenchInvariant)
    const session = ctx.sessions.create(SessionId('dispatch-invariant-valid'))
    const pending = input(session)
    expect(() => {
      session.append('weave/dispatch-input', pending)
      session.append('weave/dispatch-input', accepted(pending))
    }).not.toThrow()
  })

  it.each([
    { label: 'a source identity from another message', code: 'dispatch_input_source_identity_mismatch',
      change: (record: DispatchInputRecord): DispatchInputRecord => ({ ...record,
        sourceMessages: [{ ...record.sourceMessages[0]!, message_id: 'another-message' }] }) },
    { label: 'rewritten task bytes', code: 'dispatch_input_task_source_mismatch',
      change: (record: DispatchInputRecord): DispatchInputRecord => ({ ...record, task: record.task.trim() }) },
    { label: 'an accepted receipt for another revision', code: 'dispatch_revision_identity_mismatch',
      change: (record: DispatchInputRecord): DispatchInputRecord => ({ ...accepted(record),
        result: { ...accepted(record).result, input_revision_id: '10000000-0000-4000-8000-000000000002' } }) },
  ])('rejects $label before commit and attributes the event', async ({ change, code }) => {
    const ctx = await host()
    await ctx.plugin(WorkbenchInvariant)
    const session = ctx.sessions.create(SessionId('dispatch-invariant-invalid'))
    const record = input(session)
    const seq = session.seq
    expect(() => session.append('weave/dispatch-input', change(record))).toThrow(new InvariantError(
      '@deepseek-ai/dsh-workbench-app',
      `session "${session.id}" event ${seq} violates the durable dispatch input: ${code}`,
    ))
    expect(session.seq).toBe(seq)
    expect(() => session.append('weave/dispatch-input', record)).not.toThrow()
  })

  it('checks older malformed records when the companion loads into an existing session', async () => {
    const ctx = await host()
    const session = ctx.sessions.create(SessionId('dispatch-invariant-existing'))
    const record = input(session)
    const seq = session.seq
    session.append('weave/dispatch-input', { ...record, task: 'old task' })
    session.append('weave/dispatch-input', record)
    await expect(Promise.resolve(ctx.plugin(WorkbenchInvariant))).rejects.toThrow(
      `session "${session.id}" event ${seq} violates the durable dispatch input: dispatch_input_task_source_mismatch`,
    )
  })

  it('rejects restored records that reference a user message recorded in the future', async () => {
    const ctx = await host()
    await ctx.plugin(WorkbenchInvariant)
    const source = Session.create(SessionId('dispatch-invariant-future-source'))
    const record = input(source)
    const futureMessage = source.events[0]!
    const dispatch = source.append('weave/dispatch-input', record)
    const seed = [
      { ...dispatch, seq: 0, data: { ...record, sourceThroughSeq: 1,
        sourceMessages: [{ ...record.sourceMessages[0]!, event_seq: 1 }] } },
      { ...futureMessage, seq: 1 },
    ]
    expect(() => ctx.sessions.create(SessionId('dispatch-invariant-restored'), { seed })).toThrow(
      'event 0 violates the durable dispatch input: dispatch_input_source_identity_mismatch',
    )
    expect(ctx.sessions.list()).toHaveLength(0)
  })

  it('removes the session listeners when the companion is disposed', async () => {
    const ctx = await host()
    const companion = await ctx.plugin(WorkbenchInvariant)
    const session = ctx.sessions.create(SessionId('dispatch-invariant-dispose'))
    const record = input(session)
    await companion.dispose()
    expect(() => session.append('weave/dispatch-input', { ...record, task: 'unchecked after disposal' })).not.toThrow()
  })
})
