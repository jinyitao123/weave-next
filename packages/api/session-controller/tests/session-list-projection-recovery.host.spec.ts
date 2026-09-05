/** A version-invalid cold task remains recoverable without activating its Session. */
import { Context } from '@deepseek-ai/cordis'
import SessionStore, { type Session, type SessionEvent, type SessionHeader, type SessionId } from '@deepseek-ai/dsh-session'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { describe, expect, it, vi } from 'vitest'
import { z } from 'zod'
import { createSessionTestRemote, testSessionPersistence } from './test-remote.ts'

declare module '@deepseek-ai/dsh-session-projection/types' {
  interface SessionProjectionMap { listRecoveryTask: string | null }
  interface SessionProjectionStateMap { listRecoveryTask: string | null }
}

const sid = (value: string): SessionId => value as SessionId

async function bench() {
  const ctx = new Context()
  await ctx.plugin(SessionStore)
  const root = mkdtempSync(join(tmpdir(), 'dsh-cold-projection-'))
  const small = join(root, 'small.log')
  const large = join(root, 'large.log')
  writeFileSync(small, 'x'.repeat(1024))
  writeFileSync(large, 'x'.repeat(1025))
  const headers: SessionHeader[] = ['saved', 'ordinary', 'large'].map(id => ({
    version: 0, id: sid(id), createdAt: 100, cwd: '/project',
  }))
  const inspect = vi.fn(async (id: SessionId) => ({
    meta: headers.find(header => header.id === id)!,
    events: [
      { type: 'turn/start', seq: 0, time: 101, data: { turn: 1 } },
      ...(id === sid('ordinary') ? [] : [{
        type: 'session/title', seq: 1, time: 102,
        data: { title: 'Saved report', source: { kind: 'fallback' }, messageSeqs: [] },
      }]),
    ] as SessionEvent[],
  }))
  ctx.provide('sessionPersistence', testSessionPersistence(ctx, {
    list: () => Promise.resolve(headers),
    locate: (header: SessionHeader) => ({ kind: 'jsonl', path: header.id === sid('large') ? large : small }),
    inspect,
  }) as never)
  const remote = createSessionTestRemote(ctx, { defaultModelSelection: () => ({ provider: 'p', model: 'm' }), cwd: '/tmp' })
  const register = (version: number, list = true) => ctx.sessionProjections.register({
    key: 'listRecoveryTask', stateVersion: version, stateSchema: z.string().nullable(),
    init: () => null,
    apply: (state, event) => event.type === 'session/title' ? event.data.title : state,
    wire: { list, viewSchema: z.string().nullable(), view: state => state },
  })
  let dispose = register(2)
  ctx.provide('sessionProjectionCache', {
    cachedSnapshot: () => ({ asOfSeq: 1, values: {
      ...ctx.sessionProjections.viewCheckpoint({ listRecoveryTask: { ver: 1, seq: 1, val: 'Old summary' } }),
      sessionListMetadata: { blank: false, lastPromptAt: 101 },
    } }),
    hydratePrepared: (session: Session, _header: SessionHeader, events: readonly SessionEvent[]) =>
      ctx.sessionProjections.hydrate(session, {}, events, 0),
  } as never)
  const list = async () => {
    const result = await remote.list({})
    if (!result.ok) throw new Error('Session listing failed')
    return Object.fromEntries(result.value.items.map(item => [item.sessionId, item]))
  }
  return {
    ctx, inspect, list, small,
    setVersion: (version: number, requested = true) => { dispose(); dispose = register(version, requested) },
  }
}

describe('cold list projection recovery', () => {
  it('recovers a version-invalid task, preserves a computed null, and exposes an oversized unknown', async () => {
    const b = await bench()
    const rows = await b.list()
    expect(rows.saved?.projections?.values.listRecoveryTask).toBe('Saved report')
    expect(rows.saved?.projectionUnavailableKeys).toBeUndefined()
    expect(rows.ordinary?.projections?.values.listRecoveryTask).toBeNull()
    expect(rows.ordinary?.projectionUnavailableKeys).toBeUndefined()
    expect(rows.large?.projectionUnavailableKeys).toEqual(['listRecoveryTask'])
    expect(rows.large?.projections?.values.listRecoveryTask).toBeUndefined()
    expect(b.inspect.mock.calls.map(([id]) => id).sort()).toEqual(['ordinary', 'saved'])
    expect(b.ctx.sessions.list()).toHaveLength(0)
  })

  it('reuses unchanged observations, invalidates on a projection version change, and honors opt-out', async () => {
    const b = await bench()
    await b.list()
    await b.list()
    expect(b.inspect).toHaveBeenCalledTimes(2)
    b.setVersion(3)
    await b.list()
    expect(b.inspect).toHaveBeenCalledTimes(4)
    b.setVersion(4, false)
    const rows = await b.list()
    expect(rows.large?.projectionUnavailableKeys).toBeUndefined()
    expect(b.inspect).toHaveBeenCalledTimes(4)
    expect(b.ctx.sessions.list()).toHaveLength(0)
  })
})
