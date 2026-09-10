/** Loopback-only identity fixture; it neither authenticates Weave users nor executes tasks. */
import { createServer } from 'node:http'
import { serviceInstanceId, serviceOrigin } from '../src/connection-selection.ts'
import type { ConnectionAdapters, ServiceIdentity } from '../src/connection-selection.ts'

/**
 * Create a deferred result for deterministic race injection.
 * @returns a promise and the callbacks that settle it.
 */
export function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

/**
 * Start a new loopback server on an OS-selected port.
 * @param instance - test identity, unrelated to any production account or instance.
 * @returns identity, controlled response holds, observed calls, and awaited teardown.
 */
export async function startFixture(instance: string) {
  const calls: string[] = []
  let hold: ReturnType<typeof deferred<undefined>> | null = null
  let arrived = deferred<undefined>()
  let instanceId = instance
  const server = createServer((request, response) => {
    calls.push(request.url ?? '')
    if (request.url !== '/fixture/describe' && request.url !== '/fixture/authenticate') {
      response.writeHead(404).end()
      return
    }
    const responseInstance = instanceId
    if (hold) {
      const pending = hold
      arrived.resolve(undefined)
      void pending.promise.then(send).catch((error: unknown) => {
        response.destroy(error instanceof Error ? error : new Error('Fixture response failed'))
      })
      return
    }
    send()
    function send() {
      response.setHeader('content-type', 'application/json')
      response.end(JSON.stringify({ fixture: true, origin, instanceId: responseInstance }))
    }
  })
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  const address = server.address()
  if (!address || typeof address === 'string') throw new Error('Expected a loopback TCP listener')
  const origin = serviceOrigin(`http://127.0.0.1:${address.port}`, true)
  return {
    origin,
    calls,
    changeInstance(value: string) { instanceId = value },
    holdResponses() { hold = deferred<undefined>(); arrived = deferred<undefined>(); return arrived.promise },
    release() { hold?.resolve(undefined); hold = null },
    async close() {
      hold?.resolve(undefined); hold = null
      await new Promise<void>((resolve, reject) => {
        server.close((error) => { if (error) reject(error); else resolve() })
        server.closeAllConnections()
      })
    },
  }
}

async function readIdentity(url: string, signal: AbortSignal): Promise<ServiceIdentity> {
  const response = await fetch(url, { signal, redirect: 'error' })
  if (!response.ok) throw new Error(`Fixture HTTP ${response.status}`)
  const value: unknown = await response.json()
  if (typeof value !== 'object' || value === null || !('fixture' in value) || value.fixture !== true
    || !('origin' in value) || typeof value.origin !== 'string'
    || !('instanceId' in value) || typeof value.instanceId !== 'string') throw new Error('Invalid fixture identity')
  return { origin: serviceOrigin(value.origin, true), expectedInstanceId: serviceInstanceId(value.instanceId) }
}

/** Test adapter only; /fixture routes are not proposed Weave endpoints. */
export const fixtureAdapters: ConnectionAdapters = {
  describe: (origin, signal) => readIdentity(`${origin}/fixture/describe`, signal),
  authenticate: (identity, signal) => readIdentity(`${identity.origin}/fixture/authenticate`, signal),
}
