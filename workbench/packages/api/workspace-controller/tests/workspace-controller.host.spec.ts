import { existsSync, mkdirSync, mkdtempSync, realpathSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Context } from '@deepseek-ai/cordis'
import SessionStore, { SessionId } from '@deepseek-ai/dsh-session'
import Storage from '@deepseek-ai/dsh-storage'
import { DomainFacility } from '@deepseek-ai/dsh-storage-domain'
import { TypertRemoteFailure } from '@deepseek-ai/dsh-typert-protocol'
import WorkspaceRegistry from '@deepseek-ai/dsh-workspace'
import type { WorkspaceId } from '@deepseek-ai/dsh-workspace/types'
import WorkspaceController from '../src/index.ts'
import { WorkspaceFeed } from '../src/feed.ts'
import type { WorkspaceFollowFrame } from '../src/types.ts'
import { MemoryStorageBackend } from '../../../storage/storage-domain/tests/helpers/memory-backend.ts'

const roots: Context[] = []

afterEach(async () => {
  await Promise.all(roots.splice(0).map(ctx => ctx.fiber.dispose()))
})

interface Deferred<T> {
  readonly promise: Promise<T>
  resolve(value: T): void
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((settle) => { resolve = settle })
  return { promise, resolve }
}

async function harness() {
  const root = realpathSync.native(mkdtempSync(join(tmpdir(), 'dsh-workspace-controller-')))
  const ctx = new Context()
  roots.push(ctx)
  await ctx.plugin(SessionStore)
  await ctx.plugin(Storage)
  ctx.storage.backend.register('memory', new MemoryStorageBackend())
  const storageDomain = new DomainFacility(ctx, { backend: 'memory', routes: {} })
  ctx.storage.mount('domain', storageDomain)
  ctx.provide('storageDomain', storageDomain)
  ctx.provide('sessionPersistence', { list: () => Promise.resolve([]) } as never)
  await ctx.plugin(WorkspaceRegistry)
  const dispose = (): void => {}
  ctx.provide('typert', {
    lookups: { configure: () => dispose },
    contexts: { configureHost: () => dispose },
  } as never)
  const controller = new WorkspaceController(ctx)
  return { controller, ctx, root, storageDomain }
}

function stageDir(root: string, name: string): string {
  const path = join(root, name)
  mkdirSync(path, { recursive: true })
  return path
}

async function nextFrame(
  iterator: AsyncIterator<WorkspaceFollowFrame>,
): Promise<WorkspaceFollowFrame> {
  const next = await iterator.next()
  if (next.done === true) throw new Error('Workspace stream ended before the expected frame')
  return next.value
}

describe('WorkspaceController commands', () => {
  it('serializes concurrent path adoption and preserves an existing title', async () => {
    const { controller, root } = await harness()
    const path = stageDir(root, 'alpha')
    const results = await Promise.all([
      controller.create({ path }),
      controller.create({ path }),
    ])
    const created = results.find(result => result.created)
    const resolved = results.find(result => !result.created)
    expect(created).toMatchObject({ workspace: { path, title: 'alpha' } })
    expect(resolved?.workspace.workspaceId).toBe(created?.workspace.workspaceId)

    const workspaceId = created?.workspace.workspaceId
    if (workspaceId === undefined) throw new Error('fixture did not create a Workspace')
    await controller.rename({ workspaceId, title: 'renamed' })
    await expect(controller.create({ path })).resolves.toMatchObject({
      created: false,
      workspace: { workspaceId, title: 'renamed' },
    })
  })

  it('maps invalid paths, blank names, conflicts, and unknown ids to stable failures', async () => {
    const { controller, root } = await harness()
    const first = await controller.create({ path: stageDir(root, 'first') })
    const second = await controller.create({ path: stageDir(root, 'second') })

    await expect(controller.create({ path: join(root, 'missing') })).rejects.toMatchObject({
      failure: { code: 'workspace-invalid-path', details: { path: join(root, 'missing') } },
    })
    expect(existsSync(join(root, 'missing'))).toBe(false)
    await expect(controller.rename({ workspaceId: first.workspace.workspaceId, title: '  ' }))
      .rejects.toMatchObject({ failure: { code: 'bad-request' } })
    await controller.rename({ workspaceId: first.workspace.workspaceId, title: 'occupied' })
    await expect(controller.rename({ workspaceId: second.workspace.workspaceId, title: ' occupied ' }))
      .rejects.toMatchObject({ failure: { code: 'workspace-name-conflict' } })
    await expect(controller.delete({ workspaceId: 'missing' as WorkspaceId }))
      .rejects.toMatchObject({ failure: { code: 'workspace-not-found' } })
  })

  it('preserves Remote failures and propagates unexpected registry failures', async () => {
    const { controller, ctx, root } = await harness()
    const remoteFailure = new TypertRemoteFailure({
      code: 'fixture-failure',
      message: 'already mapped',
      details: {},
    })
    const resolveByPath = vi.spyOn(ctx.workspaceRegistry, 'resolveByPath')
      .mockRejectedValueOnce(remoteFailure)
      .mockRejectedValueOnce('plain failure')
    await expect(controller.create({ path: stageDir(root, 'remote-failure') }))
      .rejects.toBe(remoteFailure)
    const plainFailure = controller.create({ path: stageDir(root, 'plain-failure') })
    await expect(plainFailure).rejects.toMatchObject({
      failure: { code: 'workspace-invalid-path' },
    })
    await expect(plainFailure).rejects.toThrow('plain failure')
    resolveByPath.mockRestore()

    const created = await controller.create({ path: stageDir(root, 'created') })
    const workspace = ctx.workspaceRegistry.get(created.workspace.workspaceId)
    if (workspace === undefined) throw new Error('fixture Workspace disappeared')

    const orderFailure = new Error('order storage failed')
    vi.spyOn(ctx.workspaceRegistry, 'insertBefore').mockRejectedValueOnce(orderFailure)
    await expect(controller.insertBefore({ workspaceId: created.workspace.workspaceId }))
      .rejects.toBe(orderFailure)

    const moveFailure = new Error('membership storage failed')
    vi.spyOn(workspace, 'insertSessionBefore').mockRejectedValueOnce(moveFailure)
    await expect(controller.insertSessionBefore({
      workspaceId: created.workspace.workspaceId,
      sessionId: SessionId('session'),
    })).rejects.toBe(moveFailure)

    const archiveFailure = new Error('archive storage failed')
    vi.spyOn(ctx.workspaceRegistry, 'archiveSession').mockRejectedValueOnce(archiveFailure)
    await expect(controller.archiveSession({ sessionId: SessionId('session') }))
      .rejects.toBe(archiveFailure)
  })

  it('resolves queued Workspace identities when their operation starts', async () => {
    const { controller, ctx, root } = await harness()
    const target = await controller.create({ path: stageDir(root, 'target') })
    const blockerPath = stageDir(root, 'blocker')
    const gate = deferred<undefined>()
    const originalResolveByPath = ctx.workspaceRegistry.resolveByPath.bind(ctx.workspaceRegistry)
    const resolveByPath = vi.spyOn(ctx.workspaceRegistry, 'resolveByPath')
    resolveByPath.mockImplementationOnce(async (path) => {
      await gate.promise
      return originalResolveByPath(path)
    })

    const blocker = controller.create({ path: blockerPath })
    const deletion = controller.delete({ workspaceId: target.workspace.workspaceId })
    const staleRename = controller.rename({
      workspaceId: target.workspace.workspaceId,
      title: 'must-not-land',
    })
    gate.resolve(undefined)
    await blocker
    await expect(deletion).resolves.toEqual({ deleted: true })
    await expect(staleRename).rejects.toMatchObject({ failure: { code: 'workspace-not-found' } })
  })

  it('reorders Workspaces and Sessions and archives only known Sessions', async () => {
    const { controller, ctx, root } = await harness()
    const first = await controller.create({ path: stageDir(root, 'first') })
    const second = await controller.create({ path: stageDir(root, 'second') })
    await expect(controller.insertBefore({
      workspaceId: first.workspace.workspaceId,
      beforeWorkspaceId: second.workspace.workspaceId,
    })).resolves.toEqual({
      workspaceIds: [first.workspace.workspaceId, second.workspace.workspaceId],
    })
    await expect(controller.insertBefore({ workspaceId: 'missing' as WorkspaceId }))
      .rejects.toMatchObject({ failure: { code: 'workspace-not-found' } })

    const session = ctx.sessions.create(SessionId('session-one'), {
      meta: { cwd: first.workspace.path },
    })
    const workspace = ctx.workspaceRegistry.get(first.workspace.workspaceId)
    if (workspace === undefined) throw new Error('fixture Workspace disappeared')
    await workspace.attachSession(session.id)
    await expect(controller.insertSessionBefore({
      workspaceId: first.workspace.workspaceId,
      sessionId: session.id,
    })).resolves.toMatchObject({ workspace: { sessionIds: [session.id] } })
    await expect(controller.insertSessionBefore({
      workspaceId: first.workspace.workspaceId,
      sessionId: SessionId('missing-session'),
    })).rejects.toMatchObject({ failure: { code: 'workspace-move-invalid' } })
    await expect(controller.insertSessionBefore({
      workspaceId: first.workspace.workspaceId,
      sessionId: session.id,
      beforeSessionId: SessionId('missing-anchor'),
    })).rejects.toMatchObject({
      failure: {
        code: 'workspace-move-invalid',
        details: { beforeSessionId: 'missing-anchor' },
      },
    })
    await expect(controller.insertSessionBefore({
      workspaceId: 'missing' as WorkspaceId,
      sessionId: session.id,
    })).rejects.toMatchObject({ failure: { code: 'workspace-not-found' } })

    await expect(controller.archiveSession({ sessionId: session.id }))
      .resolves.toEqual({ archivedSessionIds: [session.id] })
    await expect(controller.archiveSession({ sessionId: SessionId('unknown') }))
      .rejects.toMatchObject({ failure: { code: 'session-not-found' } })
  })
})

describe('WorkspaceController follow', () => {
  it('seeds a new feed from existing rows and rejects an inconsistent registry commit', async () => {
    const { ctx, root } = await harness()
    const existing = await ctx.workspaceRegistry.create(stageDir(root, 'existing'))
    const feed = new WorkspaceFeed(ctx)
    expect(feed.baseline()).toMatchObject({
      items: [{ workspaceId: existing.id }],
    })

    expect(() => {
      ctx.emit('domain/changed', {
        domain: 'workspace',
        table: '',
        key: '',
        operation: 'put',
        value: {
          initialized: true,
          workspaceIds: ['missing'],
          archivedSessionIds: [],
        },
      })
    }).toThrow('references missing Workspace "missing"')
  })

  it('starts with a complete baseline and emits committed increments in domain order', async () => {
    const { controller, ctx, root } = await harness()
    const abort = new AbortController()
    const iterator = controller.follow(abort.signal)[Symbol.asyncIterator]()
    await expect(nextFrame(iterator)).resolves.toEqual({
      type: 'baseline',
      value: { items: [], archivedSessionIds: [] },
    })

    const first = await controller.create({ path: stageDir(root, 'first') })
    await expect(nextFrame(iterator)).resolves.toMatchObject({
      type: 'upsert', workspace: { workspaceId: first.workspace.workspaceId },
    })
    await expect(nextFrame(iterator)).resolves.toEqual({
      type: 'order', workspaceIds: [first.workspace.workspaceId],
    })
    await controller.rename({ workspaceId: first.workspace.workspaceId, title: 'renamed' })
    await expect(nextFrame(iterator)).resolves.toMatchObject({
      type: 'upsert', workspace: { title: 'renamed' },
    })

    const second = await controller.create({ path: stageDir(root, 'second') })
    await expect(nextFrame(iterator)).resolves.toMatchObject({
      type: 'upsert', workspace: { workspaceId: second.workspace.workspaceId },
    })
    await expect(nextFrame(iterator)).resolves.toEqual({
      type: 'order', workspaceIds: [second.workspace.workspaceId, first.workspace.workspaceId],
    })
    await controller.insertBefore({
      workspaceId: first.workspace.workspaceId,
      beforeWorkspaceId: second.workspace.workspaceId,
    })
    await expect(nextFrame(iterator)).resolves.toEqual({
      type: 'order',
      workspaceIds: [first.workspace.workspaceId, second.workspace.workspaceId],
    })

    const session = ctx.sessions.create(SessionId('archived'), {
      meta: { cwd: first.workspace.path },
    })
    await controller.archiveSession({ sessionId: session.id })
    await expect(nextFrame(iterator)).resolves.toEqual({
      type: 'archived', archivedSessionIds: [session.id],
    })
    await controller.delete({ workspaceId: second.workspace.workspaceId })
    await expect(nextFrame(iterator)).resolves.toEqual({
      type: 'order', workspaceIds: [first.workspace.workspaceId],
    })
    await expect(nextFrame(iterator)).resolves.toEqual({
      type: 'remove', workspaceId: second.workspace.workspaceId,
    })

    abort.abort()
    await expect(iterator.next()).resolves.toEqual({ done: true, value: undefined })
  })

  it('ignores unrelated domain writes and closes active followers on disposal', async () => {
    const { controller, ctx, root } = await harness()
    const abort = new AbortController()
    const iterator = controller.follow(abort.signal)[Symbol.asyncIterator]()
    await nextFrame(iterator)
    ctx.emit('domain/changed', {
      domain: 'other', table: 'records', key: 'x', operation: 'put', value: {},
    })
    ctx.emit('domain/changed', {
      domain: 'workspace', table: '', key: '', operation: 'deleted',
    })
    ctx.emit('domain/changed', {
      domain: 'workspace', table: 'other', key: 'x', operation: 'put', value: {},
    })
    ctx.emit('domain/changed', {
      domain: 'workspace', table: 'workspaces', key: 'unknown', operation: 'deleted',
    })
    const pending = iterator.next()
    const created = await controller.create({ path: stageDir(root, 'visible') })
    await expect(pending).resolves.toMatchObject({ value: { type: 'upsert' } })
    await expect(iterator.next()).resolves.toEqual({
      done: false,
      value: { type: 'order', workspaceIds: [created.workspace.workspaceId] },
    })

    const closing = iterator.next()
    await ctx.fiber.dispose()
    roots.splice(roots.indexOf(ctx), 1)
    await expect(closing).resolves.toEqual({ done: true, value: undefined })
  })
})

describe('WorkspaceController request-bound Session visibility', () => {
  async function sharedWorkspace() {
    const fixture = await harness()
    const created = await fixture.controller.create({ path: stageDir(fixture.root, 'shared') })
    const workspace = fixture.ctx.workspaceRegistry.get(created.workspace.workspaceId)!
    const owners = new Map<string, string>()
    for (const [id, owner] of [['alice-one', 'alice'], ['bob-one', 'bob'], ['alice-two', 'alice'], ['bob-two', 'bob']].reverse()) {
      const session = fixture.ctx.sessions.create(SessionId(id!), { meta: { cwd: created.workspace.path } })
      owners.set(session.id, owner!)
      await workspace.attachSession(session.id)
    }
    let actor = 'alice'
    const policy = () => {
      const captured = actor
      return async (id: SessionId) => owners.get(id) === captured
    }
    fixture.controller.setSessionVisibility(policy)
    return { ...fixture, workspace, as: (value: string) => { actor = value }, policy }
  }

  it('filters baseline and later grouping/archive frames for each captured caller', async () => {
    const { controller, workspace, as, ctx } = await sharedWorkspace()
    const aliceAbort = new AbortController()
    const bobAbort = new AbortController()
    const aliceFrames = controller.follow(aliceAbort.signal)[Symbol.asyncIterator]()
    as('bob')
    const bobFrames = controller.follow(bobAbort.signal)[Symbol.asyncIterator]()
    const aliceBaseline = await nextFrame(aliceFrames)
    const bobBaseline = await nextFrame(bobFrames)
    expect(JSON.stringify(aliceBaseline)).not.toContain('bob-')
    expect(JSON.stringify(bobBaseline)).not.toContain('alice-')
    expect(aliceBaseline).toMatchObject({ type: 'baseline', value: { items: [{ sessionIds: ['alice-one', 'alice-two'] }] } })
    expect(bobBaseline).toMatchObject({ type: 'baseline', value: { items: [{ sessionIds: ['bob-one', 'bob-two'] }] } })

    await workspace.setTitle('Shared project')
    expect(await nextFrame(aliceFrames)).toMatchObject({ type: 'upsert', workspace: { sessionIds: ['alice-one', 'alice-two'] } })
    expect(await nextFrame(bobFrames)).toMatchObject({ type: 'upsert', workspace: { sessionIds: ['bob-one', 'bob-two'] } })
    await ctx.workspaceRegistry.archiveSession(SessionId('alice-one'))
    expect(await nextFrame(aliceFrames)).toEqual({ type: 'archived', archivedSessionIds: ['alice-one'] })
    expect(await nextFrame(bobFrames)).toEqual({ type: 'archived', archivedSessionIds: [] })
    await ctx.workspaceRegistry.archiveSession(SessionId('bob-one'))
    expect(await nextFrame(aliceFrames)).toEqual({ type: 'archived', archivedSessionIds: ['alice-one'] })
    expect(await nextFrame(bobFrames)).toEqual({ type: 'archived', archivedSessionIds: ['bob-one'] })
    aliceAbort.abort(); bobAbort.abort()
    await aliceFrames.return?.(); await bobFrames.return?.()
  })

  it('filters idempotent creation, rename, move, and archive receipts without changing stored membership', async () => {
    const { controller, workspace, as, ctx } = await sharedWorkspace()
    expect((await controller.create({ path: workspace.path })).workspace.sessionIds).toEqual(['alice-one', 'alice-two'])
    expect((await controller.rename({ workspaceId: workspace.id, title: 'Renamed' })).workspace.sessionIds).toEqual(['alice-one', 'alice-two'])
    expect((await controller.insertSessionBefore({ workspaceId: workspace.id, sessionId: SessionId('alice-two'), beforeSessionId: SessionId('alice-one') })).workspace.sessionIds).toEqual(['alice-two', 'alice-one'])
    await ctx.workspaceRegistry.archiveSession(SessionId('bob-one'))
    expect(await controller.archiveSession({ sessionId: SessionId('alice-one') })).toEqual({ archivedSessionIds: ['alice-one'] })
    as('bob')
    expect((await controller.create({ path: workspace.path })).workspace.sessionIds).toEqual(['bob-one', 'bob-two'])
    expect(await controller.archiveSession({ sessionId: SessionId('bob-two') })).toEqual({ archivedSessionIds: ['bob-one', 'bob-two'] })
    expect(workspace.sessionIds).toHaveLength(4)
  })

  it('refuses foreign move targets, anchors, and archive targets before any registry mutation', async () => {
    const { controller, workspace, ctx } = await sharedWorkspace()
    const originalOrder = [...workspace.sessionIds]
    const archive = vi.spyOn(ctx.workspaceRegistry, 'archiveSession')
    const move = vi.spyOn(workspace, 'insertSessionBefore')
    for (const sessionId of ['bob-one', 'unknown']) {
      await expect(controller.archiveSession({ sessionId: SessionId(sessionId) })).rejects.toMatchObject({ failure: { code: 'session-not-found', message: 'Session is unavailable' } })
      await expect(controller.insertSessionBefore({ workspaceId: workspace.id, sessionId: SessionId(sessionId) })).rejects.toMatchObject({ failure: { code: 'session-not-found', message: 'Session is unavailable' } })
      await expect(controller.insertSessionBefore({ workspaceId: workspace.id, sessionId: SessionId('alice-one'), beforeSessionId: SessionId(sessionId) })).rejects.toMatchObject({ failure: { code: 'session-not-found', message: 'Session is unavailable' } })
    }
    expect(archive).not.toHaveBeenCalled()
    expect(move).not.toHaveBeenCalled()
    expect(workspace.sessionIds).toEqual(originalOrder)
  })

  it('captures visibility before an asynchronous command and safely replaces policy registrations', async () => {
    const { controller, workspace, as, ctx, policy } = await sharedWorkspace()
    const gate = deferred<undefined>()
    const resolve = ctx.workspaceRegistry.resolveByPath.bind(ctx.workspaceRegistry)
    vi.spyOn(ctx.workspaceRegistry, 'resolveByPath').mockImplementationOnce(async path => { await gate.promise; return resolve(path) })
    const response = controller.create({ path: workspace.path })
    as('bob')
    gate.resolve(undefined)
    expect((await response).workspace.sessionIds).toEqual(['alice-one', 'alice-two'])
    const old = controller.setSessionVisibility(() => async () => false)
    const current = controller.setSessionVisibility(policy)
    old()
    expect((await controller.create({ path: workspace.path })).workspace.sessionIds).toEqual(['bob-one', 'bob-two'])
    current()
    expect((await controller.create({ path: workspace.path })).workspace.sessionIds).toHaveLength(4)
  })
})
