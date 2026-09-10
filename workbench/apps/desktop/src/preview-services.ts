/** Self-contained loopback fixtures; no Weave API, account, or execution endpoint is exposed. */
import { createServer, type Server } from 'node:http'
import { randomUUID } from 'node:crypto'
import { FilePreferences } from './preferences.js'

/** Non-sensitive service descriptor shown in the preview's connection settings. */
export interface PreviewService {
  readonly name: string
  readonly origin: string
  readonly instanceId: string
}

/** Owned fixture listeners and stable identities for restart validation. */
export interface PreviewServices {
  readonly services: readonly PreviewService[]
  /** Close every owned listener and connection before desktop shutdown completes. */
  close(): Promise<void>
}

type SavedService = { name: string; port: number; instanceId: string }

function parseConfig(json: string): SavedService[] {
  const data: unknown = JSON.parse(json)
  if (!Array.isArray(data) || data.length !== 2) throw new Error('Invalid preview service configuration')
  return data.map((item: unknown, i) => {
    if (typeof item !== 'object' || item === null || !('name' in item) || typeof item.name !== 'string' || item.name !== ['A', 'B'][i]
      || !('port' in item) || typeof item.port !== 'number' || !Number.isInteger(item.port) || item.port < 1 || item.port > 65535
      || !('instanceId' in item) || typeof item.instanceId !== 'string' || !item.instanceId.trim()) {
      throw new Error('Invalid preview service configuration')
    }
    return { name: item.name, port: item.port, instanceId: item.instanceId }
  })
}


/**
 * Start two local test services; saved ports are reused or fail explicitly if occupied.
 * @param configPath - private fixture-only metadata file, separate from connection preferences.
 * @returns listeners owned exclusively by the current application instance.
 */
export async function startPreviewServices(configPath: string): Promise<PreviewServices> {
  const store = new FilePreferences(configPath)
  const saved = store.read()
  const settings = saved === null
    ? ['A', 'B'].map(name => ({ name, port: 0, instanceId: randomUUID() }))
    : parseConfig(saved)
  const servers: Server[] = []
  const services: PreviewService[] = []
  const close = async () => {
    await Promise.all(servers.map(server => new Promise<void>((resolve, reject) => {
      server.close((error) => { if (error) reject(error); else resolve() })
      server.closeAllConnections()
    })))
  }
  try {
    for (const setting of settings) {
      let delay = 0
      let unavailable = false
      let downloadFailure = false
      let instanceId = setting.instanceId
      const server = createServer((request, response) => {
        const route = new URL(request.url ?? '/', 'http://fixture')
        response.setHeader('Cache-Control', 'no-store')
        response.setHeader('X-Content-Type-Options', 'nosniff')
        if (route.pathname === '/fixture/control' && request.method === 'POST') {
          // Loopback test controls never appear in the remote product protocol.
          delay = route.searchParams.get('delay') === '1' ? 900 : 0
          unavailable = route.searchParams.get('unavailable') === '1'
          downloadFailure = route.searchParams.get('downloadFailure') === '1'
          if (route.searchParams.get('rotate') === '1') instanceId = randomUUID()
          response.end('ok')
          return
        }
        if (unavailable) { request.socket.destroy(); return }
        if (route.pathname === '/fixture/describe' || route.pathname === '/fixture/authenticate') {
          const timer = setTimeout(() => {
            if (response.destroyed) return
            response.setHeader('Content-Type', 'application/json')
            if (route.pathname.endsWith('authenticate')) response.setHeader('Set-Cookie', `preview=${setting.name}; HttpOnly; SameSite=Strict; Path=/`)
            response.end(JSON.stringify({ fixture: true, origin: services.find(value => value.name === setting.name)?.origin,
              instanceId, authenticated: route.pathname.endsWith('authenticate') }))
          }, delay)
          response.once('close', () => { clearTimeout(timer) })
          return
        }
        if (route.pathname === '/fixture/download') {
          response.setHeader('Content-Type', 'text/plain; charset=utf-8')
          response.setHeader('Content-Disposition', `attachment; filename="connection-check-${setting.name}.txt"`)
          if (downloadFailure) {
            response.setHeader('Content-Length', '100000')
            response.write('incomplete')
            const timer = setTimeout(() => { response.destroy() }, 150)
            response.once('close', () => { clearTimeout(timer) })
            return
          }
          response.end(`Workbench desktop preview\nService ${setting.name}\nFixture only; no business task was created.\n`)
          return
        }
        response.writeHead(404, { 'Content-Type': 'text/plain' })
        response.end('This preview service serves no application page')
      })
      await new Promise<void>((resolve, reject) => {
        server.once('error', reject)
        server.listen(setting.port, '127.0.0.1', () => { server.removeListener('error', reject); resolve() })
      })
      servers.push(server)
      const address = server.address()
      if (!address || typeof address === 'string') throw new Error('Fixture did not bind a TCP listener')
      setting.port = address.port
      services.push({ name: setting.name, origin: `http://127.0.0.1:${address.port}`, instanceId })
    }
    if (saved === null) store.write(JSON.stringify(settings))
    return { services, close }
  } catch (error) { await close(); throw error }
}
