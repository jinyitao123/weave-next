import { afterEach, describe, expect, it, vi } from 'vitest'
import { mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { ConnectionSelection, serviceInstanceId, serviceOrigin } from '../src/connection-selection.ts'
import type { ConnectionAdapters, ConnectionPreferenceStore, ServiceIdentity } from '../src/connection-selection.ts'
import { deferred, fixtureAdapters, startFixture } from './fixture-service.ts'

const A = serviceOrigin('https://a.example')
const B = serviceOrigin('https://b.example')
const instance = serviceInstanceId('fixture-instance')
const identity = (origin = A, id = instance): ServiceIdentity => ({ origin, expectedInstanceId: id })
const success: ConnectionAdapters = {
  describe: async origin => identity(origin),
  authenticate: async service => service,
}
function memoryStore(saved: string | null = null) {
  let value = saved
  return { read: () => value, write: vi.fn((next: string) => { value = next }) }
}
function setup(store = memoryStore()) {
  return { store, selection: new ConnectionSelection(store, { defaultOrigin: A }) }
}
const cleanup: (() => Promise<void> | void)[] = []
afterEach(async () => { for (const release of cleanup.splice(0).reverse()) await release() })

describe('connection selection', () => {
  it('uses the default only when no preference exists and requires authentication before readiness', async () => {
    const { store, selection } = setup()
    expect(selection.selected).toEqual({ origin: A, expectedInstanceId: null })
    expect(() => selection.capture()).toThrow('No active')
    expect(store.write).not.toHaveBeenCalled()
    const scope = await selection.select(A, success)
    expect(selection.isCurrent(scope)).toBe(true)
    expect(Object.isFrozen(scope)).toBe(true)
    expect(JSON.parse(store.read()!)).toEqual({ version: 1, ...identity() })
  })

  it('retains a user-selected service across restart and an installer default change', async () => {
    const { store, selection } = setup()
    await selection.select(B, success)
    const restarted = new ConnectionSelection(store, { defaultOrigin: 'https://new-default.example' })
    expect(restarted.selected).toEqual(identity(B))
    expect(() => restarted.capture()).toThrow('No active')
    expect(selection.selected).toEqual(restarted.selected)
  })

  it('round-trips the preference through a private file independent of the owner lifetime', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'weave-desktop-preference-'))
    cleanup.push(() => { rmSync(dir, { recursive: true }) })
    const path = join(dir, 'connection.json')
    let exists = false
    const store: ConnectionPreferenceStore = {
      read: () => exists ? readFileSync(path, 'utf8') : null,
      write: (value) => { writeFileSync(`${path}.new`, value, { mode: 0o600 }); renameSync(`${path}.new`, path); exists = true },
    }
    const first = new ConnectionSelection(store, { defaultOrigin: A })
    await first.select(B, success)
    first.close()
    expect(new ConnectionSelection(store, { defaultOrigin: A }).selected.origin).toBe(B)
  })

  it.each(['{', '{"version":2}', JSON.stringify({ version: 1, ...identity(), token: 'fixture-secret' })])(
    'rejects invalid saved data without silently selecting the default (%s)', (saved) => {
      expect(() => setup(memoryStore(saved))).toThrow()
    },
  )

  it.each(['http://example.com', 'https://user:password@example.com', 'https://example.com/path',
    'https://example.com?token=x', 'https://example.com/#fragment', 'file:///tmp/a'])('rejects unsafe origins (%s)', (origin) => {
    expect(() => serviceOrigin(origin, true)).toThrow()
  })

  it('allows loopback HTTP only when explicitly configured', () => {
    expect(() => serviceOrigin('http://127.0.0.1:3131')).toThrow()
    expect(serviceOrigin('http://127.0.0.1:3131', true)).toBe('http://127.0.0.1:3131')
    expect(serviceOrigin('https://EXAMPLE.com:443/')).toBe('https://example.com')
  })

  it.each(['describe', 'authenticate'] as const)('preserves the selected service when %s fails', async (method) => {
    const { selection, store } = setup()
    const old = await selection.select(A, success)
    const saved = store.read()
    await expect(selection.select(B, { ...success, [method]: async () => { throw new Error('fixture offline') } })).rejects.toThrow('offline')
    expect(selection.selected).toEqual(identity())
    expect(selection.capture().origin).toBe(A)
    expect(selection.isCurrent(old)).toBe(false)
    expect(store.read()).toBe(saved)
  })

  it('rejects a changed instance before authenticating the saved origin', async () => {
    const { selection } = setup()
    await selection.select(A, success)
    const authenticate = vi.fn(async (service: ServiceIdentity) => service)
    await expect(selection.select(A, { describe: async () => identity(A, serviceInstanceId('replacement')), authenticate })).rejects.toThrow('instance changed')
    expect(authenticate).not.toHaveBeenCalled()
    expect(() => selection.capture()).toThrow('No active')
    expect(selection.selected.expectedInstanceId).toBe(instance)
  })

  it.each(['origin', 'instance'] as const)('rejects a different authenticated %s without storing it', async (mismatch) => {
    const { selection, store } = setup()
    const authenticated = mismatch === 'origin' ? identity(B) : identity(A, serviceInstanceId('replacement'))
    await expect(selection.select(A, { ...success, authenticate: async () => authenticated })).rejects.toThrow('different service')
    expect(store.read()).toBeNull()
    expect(() => selection.capture()).toThrow('No active')
  })

  it('drops readiness if authentication discovers replacement at the active origin', async () => {
    const { selection } = setup()
    const old = await selection.select(A, success)
    await expect(selection.select(A, { ...success, authenticate: async () => identity(A, serviceInstanceId('replacement')) })).rejects.toThrow('different service')
    expect(() => selection.capture()).toThrow('No active')
    expect(selection.isCurrent(old)).toBe(false)
    expect(selection.selected.expectedInstanceId).toBe(instance)
  })

  it('does not activate a service when the atomic preference write fails', async () => {
    const { selection, store } = setup()
    await selection.select(A, success)
    store.write.mockImplementationOnce(() => { throw new Error('fixture disk full') })
    await expect(selection.select(B, success)).rejects.toThrow('disk full')
    expect(selection.selected.origin).toBe(A)
    expect(selection.capture().origin).toBe(A)
    expect(JSON.parse(store.read()!)).toMatchObject({ origin: A })
  })

  it('cancels a stalled adapter promptly and ignores its late authentication result', async () => {
    const { selection, store } = setup()
    await selection.select(A, success)
    const arrived = deferred<undefined>()
    const pending = deferred<ServiceIdentity>()
    const attempt = selection.select(B, { ...success, authenticate: () => { arrived.resolve(undefined); return pending.promise } })
    const result = expect(attempt).rejects.toThrow('cancelled')
    await arrived.promise
    expect(() => selection.capture()).toThrow('No active')
    selection.cancel()
    await result
    pending.resolve(identity(B))
    await pending.promise
    expect(selection.selected.origin).toBe(A)
    expect(JSON.parse(store.read()!)).toMatchObject({ origin: A })
  })

  it('ignores an older selection failure after a newer selection is active', async () => {
    const { selection } = setup()
    const pending = deferred<ServiceIdentity>()
    const first = selection.select(A, { ...success, describe: () => pending.promise })
    const firstResult = expect(first).rejects.toThrow('cancelled')
    const scope = await selection.select(B, success)
    pending.reject(new Error('late network failure'))
    await firstResult
    expect(selection.switching).toBe(false)
    expect(selection.isCurrent(scope)).toBe(true)
  })

  it('rejects stale responses across A→B→A', async () => {
    const { selection } = setup()
    const firstA = await selection.select(A, success)
    await selection.select(B, success)
    const secondA = await selection.select(A, success)
    expect(selection.isCurrent(firstA)).toBe(false)
    expect(selection.isCurrent(secondA)).toBe(true)
  })

  it('invalidates scopes across owner replacement, disconnect and close', async () => {
    const { selection, store } = setup()
    const first = await selection.select(A, success)
    const second = new ConnectionSelection(store, { defaultOrigin: A })
    await second.select(A, success)
    expect(second.isCurrent(first)).toBe(false)
    selection.disconnect()
    expect(selection.isCurrent(first)).toBe(false)
    expect(selection.selected.origin).toBe(A)
    selection.close()
    await expect(selection.select(A, success)).rejects.toThrow('closed')
  })
})

describe('isolated HTTP fixtures', () => {
  it('handles real connection loss, late replies and instance changes without calling business routes', async () => {
    const a = await startFixture('fixture-A')
    const b = await startFixture('fixture-B')
    cleanup.push(() => a.close())
    const closeB = () => b.close()
    cleanup.push(closeB)
    const store = memoryStore()
    const selection = new ConnectionSelection(store, { defaultOrigin: a.origin, allowLoopbackHttp: true })
    cleanup.push(() => { selection.close() })
    const old = await selection.select(a.origin, fixtureAdapters)
    const arrived = b.holdResponses()
    const pending = selection.select(b.origin, fixtureAdapters)
    const result = expect(pending).rejects.toThrow('cancelled')
    await arrived
    selection.cancel()
    await result
    b.release()
    expect(selection.selected.origin).toBe(a.origin)
    expect(selection.isCurrent(old)).toBe(false)
    await selection.select(b.origin, fixtureAdapters)
    b.changeInstance('fixture-B-reinitialized')
    await expect(selection.select(b.origin, fixtureAdapters)).rejects.toThrow('instance changed')
    expect(selection.selected.expectedInstanceId).toBe('fixture-B')
    expect(a.calls.concat(b.calls).every(path => path.startsWith('/fixture/'))).toBe(true)
    await b.close()
    cleanup.splice(cleanup.indexOf(closeB), 1)
    await expect(selection.select(b.origin, fixtureAdapters)).rejects.toThrow()
    expect(selection.selected.expectedInstanceId).toBe('fixture-B')
  })
})
