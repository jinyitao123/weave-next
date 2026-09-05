/** Projection-backed snapshots retain journal cursors without repeating domain state. */

import { describe, expect, it } from 'vitest'
import { Context } from '@deepseek-ai/cordis'
import { z } from 'zod'
import AgentRegistry from '@deepseek-ai/dsh-agent'
import SessionStore, { type Session, type SessionEvent } from '@deepseek-ai/dsh-session'
import { createUserMessage, createMessage, createToolResultMessage, ToolCallId } from '@deepseek-ai/dsh-llm'
import type { ProjectionDefinition } from '@deepseek-ai/dsh-session-projection'
import type SessionController from '../src/index.ts'
import type { SessionFollowFrame, SessionProjectionReceipt } from '../src/types.ts'
import { createSessionTestController } from './test-remote.ts'

type HistoryState = { note: string; observation: number } | null

declare module '@deepseek-ai/dsh-session-projection/types' {
  interface SessionProjectionStateMap {
    'test/history-state': HistoryState
    'test/other-history': HistoryState
  }
  interface SessionProjectionMap {
    'test/history-state': HistoryState
    'test/other-history': HistoryState
  }
}

const KEY = 'test/history-state'
const EVENT = 'weave/work-task'
const stateSchema = z.object({ note: z.string(), observation: z.number() }).nullable()
const definition = {
  key: KEY,
  stateSchema,
  init: () => null,
  apply: (state, event) => event.type === EVENT ? stateSchema.parse(event.data) : state,
  wire: { viewSchema: stateSchema, view: state => state },
  stateVersion: 1,
} satisfies ProjectionDefinition<typeof KEY>

function snapshotEvent(session: Session, observation: number, note = 'state'): SessionEvent {
  return (session.append as unknown as (type: string, data: unknown) => SessionEvent)(EVENT, { note, observation })
}

function user(session: Session, text: string): SessionEvent {
  return session.append('user/message', createUserMessage({
    content: [{ type: 'text', text }], source: { kind: 'user' },
  }), { surfaceOp: 'append' })
}

async function harness() {
  const ctx = new Context()
  await ctx.plugin(SessionStore)
  await ctx.plugin(AgentRegistry)
  const controller = createSessionTestController(ctx, {
    defaultModelSelection: () => ({ provider: 'p', model: 'm' }), cwd: '/workspace',
  })
  const session = ctx.sessions.create(undefined, { meta: { cwd: '/workspace' } })
  return { ctx, controller, session }
}

async function opening(controller: SessionController, session: Session, maxMessages?: number) {
  const abort = new AbortController()
  const iterator = controller.follow({ address: { kind: 'session', sessionId: session.id },
    ...(maxMessages === undefined ? {} : { maxMessages }),
  }, abort.signal)[Symbol.asyncIterator]()
  const first = await iterator.next()
  if (first.done || first.value.type !== 'snapshot') throw new Error('snapshot missing')
  return { frame: first.value, iterator, close: async () => { abort.abort(); await iterator.return?.() } }
}

function receipt(frame: Extract<SessionFollowFrame, { type: 'snapshot' }>): SessionProjectionReceipt {
  return { asOfSeq: frame.projections.asOfSeq, keys: Object.keys(frame.projections.values) }
}

describe('projection-backed Session history', () => {
  it('opens twelve thousand large snapshots with the current state, full dialogue, tool facts, and one range', async () => {
    const { ctx, controller, session } = await harness()
    ctx.sessionProjections.register(definition)
    controller.registerHistoryProjection({ key: KEY, eventTypes: [EVENT] })
    const prompt = user(session, 'Review the existing knowledge base')
    const call = session.append('tool/call', {
      turn: 1, step: 1, callId: ToolCallId('call'), name: 'review', arguments: '{"target":"existing"}',
    })
    const result = session.append('tool/result', {
      turn: 1, step: 1,
      message: createToolResultMessage({ callId: ToolCallId('call'), content: [{ type: 'text', text: 'Review started' }], isError: false }),
      meta: { receipt: 'accepted' },
    }, { surfaceOp: 'append' })
    const answer = session.append('assistant/message', {
      turn: 1, step: 1,
      message: createMessage({ role: 'assistant', content: [{ type: 'text', text: 'Your review is running' }],
        source: { kind: 'model', provider: 'p', model: 'm' } }),
    }, { surfaceOp: 'append' })
    const note = '材料'.repeat(8_000)
    const first = snapshotEvent(session, 0, note)
    for (let i = 1; i < 12_000; i++) snapshotEvent(session, i, note)
    const cursor = session.seq - 1
    const logLength = session.events.length
    const followed = await opening(controller, session)
    try {
      expect(followed.frame.projections).toMatchObject({ asOfSeq: cursor, values: { [KEY]: { observation: 11_999, note } } })
      expect(followed.frame.records).toEqual([
        ...[prompt, call, result, answer].map(event => ({ type: 'event', event })),
        { type: 'projection', event: { type: 'history/projection', seq: first.seq, time: first.time,
          data: { key: KEY, throughSeq: cursor } } },
      ])
      // Measure the complete UTF-8 snapshot, including metadata and the retained projection.
      expect(Buffer.byteLength(JSON.stringify(followed.frame))).toBeLessThan(52_000)
      expect(followed.frame.hasMore).toBe(false)
      expect(session.events).toHaveLength(logLength)
      expect(session.events.at(-1)?.data).toEqual({ observation: 11_999, note })
      const live = snapshotEvent(session, 12_000, note)
      await expect(followed.iterator.next()).resolves.toMatchObject({ value: { type: 'event', event: live } })
    } finally {
      await followed.close()
      await ctx.fiber.dispose()
    }
  })

  it('keeps action and unregistered events between ranges and does not replace surface messages', async () => {
    const { ctx, controller, session } = await harness()
    ctx.sessionProjections.register(definition)
    controller.registerHistoryProjection({ key: KEY, eventTypes: [EVENT, 'user/message'] })
    const first = snapshotEvent(session, 0)
    const prompt = user(session, 'Keep this prompt')
    const second = snapshotEvent(session, 1)
    const action = session.append('weave/work-task-action', { pendingAction: null })
    const third = snapshotEvent(session, 2)
    const end = session.append('turn/end', { turn: 1, reason: { kind: 'completed' } })
    const followed = await opening(controller, session)
    try {
      expect(followed.frame.records.map(record => record.type)).toEqual(['projection', 'event', 'projection', 'event', 'projection', 'event'])
      expect(followed.frame.records.filter(record => record.type === 'event').map(record => record.event)).toEqual([prompt, action, end])
      expect(followed.frame.records.filter(record => record.type === 'projection').map(record => [record.event.seq, record.event.data.throughSeq]))
        .toEqual([first, second, third].map(event => [event.seq, event.seq]))
    } finally {
      await followed.close()
      await ctx.fiber.dispose()
    }
  })

  it('pages older snapshots only through the accepted baseline and keeps the raw page addressable', async () => {
    const { ctx, controller, session } = await harness()
    ctx.sessionProjections.register(definition)
    controller.registerHistoryProjection({ key: KEY, eventTypes: [EVENT] })
    const older = user(session, 'Older prompt')
    const oldSnapshot = snapshotEvent(session, 0)
    const recent = user(session, 'Recent prompt')
    const recentSnapshot = snapshotEvent(session, 1)
    const followed = await opening(controller, session, 1)
    const projectionBaseline = receipt(followed.frame)
    try {
      expect(followed.frame.hasMore).toBe(true)
      expect(followed.frame.records[0]?.event).toEqual(recent)
      const request = { address: { kind: 'session' as const, sessionId: session.id }, throughSeq: followed.frame.cursor, beforeSeq: recent.seq, projectionBaseline }
      const page = await controller.page(request, new AbortController().signal)
      expect(page.records).toMatchObject([
        { type: 'event', event: older },
        { type: 'projection', event: { seq: oldSnapshot.seq, data: { throughSeq: oldSnapshot.seq } } },
      ])
      expect(page.hasMore).toBe(false)
      const appended = snapshotEvent(session, 2)
      const repair = await controller.page({
        address: request.address, projectionBaseline, throughSeq: appended.seq,
      }, new AbortController().signal)
      expect(repair.records.at(-1)).toEqual({ type: 'event', event: appended })
      const raw = await controller.page({ address: request.address, throughSeq: recentSnapshot.seq }, new AbortController().signal)
      expect(raw.records.map(record => record.event)).toEqual([older, oldSnapshot, recent, recentSnapshot])
    } finally {
      await followed.close()
      await ctx.fiber.dispose()
    }
  })

  it('requires a registered available projection and releases declarations with their owning fiber', async () => {
    const { ctx, controller, session } = await harness()
    const event = snapshotEvent(session, 1)
    const fiber = await ctx.plugin({
      name: 'history-owner',
      apply: (scoped: Context) => {
        scoped.sessionController.registerHistoryProjection({ key: KEY, eventTypes: [EVENT] })
      },
    })
    let followed = await opening(controller, session)
    expect(followed.frame.records).toEqual([{ type: 'event', event }])
    await followed.close()
    const disposeProjection = ctx.sessionProjections.register(definition)
    followed = await opening(controller, session)
    expect(followed.frame.records[0]?.type).toBe('projection')
    await followed.close()
    await fiber.dispose()
    followed = await opening(controller, session)
    expect(followed.frame.records).toEqual([{ type: 'event', event }])
    await followed.close()
    controller.registerHistoryProjection({ key: KEY, eventTypes: [EVENT] })
    disposeProjection()
    followed = await opening(controller, session)
    expect(followed.frame.records).toEqual([{ type: 'event', event }])
    await followed.close()
    await ctx.fiber.dispose()
  })

  it('shares matching declarations and rejects conflicting owners without partial registration', async () => {
    const { ctx, controller, session } = await harness()
    ctx.sessionProjections.register(definition)
    const first = controller.registerHistoryProjection({ key: KEY, eventTypes: [EVENT] })
    const second = controller.registerHistoryProjection({ key: KEY, eventTypes: [EVENT] })
    expect(() => controller.registerHistoryProjection({ key: 'test/other-history', eventTypes: [EVENT] })).toThrow('already owned')
    snapshotEvent(session, 0)
    first()
    let followed = await opening(controller, session)
    expect(followed.frame.records[0]?.type).toBe('projection')
    await followed.close()
    second()
    followed = await opening(controller, session)
    expect(followed.frame.records[0]?.type).toBe('event')
    await followed.close()
    await ctx.fiber.dispose()
  })

  it.each([-2, 1.5, Number.NaN, 4])('rejects a page receipt outside its history cut (%s)', async (asOfSeq) => {
    const { ctx, controller, session } = await harness()
    user(session, 'prompt')
    await expect(controller.page({ address: { kind: 'session', sessionId: session.id }, throughSeq: 0,
      projectionBaseline: { asOfSeq, keys: [KEY] },
    }, new AbortController().signal)).rejects.toThrow('projection baseline')
    await ctx.fiber.dispose()
  })
})
