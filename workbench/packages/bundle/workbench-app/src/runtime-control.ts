/** Host-only runtime management proxy. Weave credentials never cross this boundary. */

import { isIP } from 'node:net'
import { networkInterfaces } from 'node:os'
import { z } from 'zod'

/** Secretless identity facts for one engine observed on a registered runtime. */
export interface WeaveRuntimeEngineView {
  readonly engine: string
  readonly binaryVersion: string
  readonly authMode: 'chatgpt' | 'oauth' | 'provider' | 'unknown'
}

/** Secretless scheduling facts for one registered Weave runtime. */
export interface WeaveRuntimeView {
  readonly id: string
  readonly name: string
  readonly engines: readonly string[]
  readonly engineCapabilities: readonly WeaveRuntimeEngineView[]
  readonly healthStatus: 'healthy' | 'busy' | 'degraded' | 'quarantined' | 'offline'
  readonly totalSlots: number
  readonly activeSlots: number
  readonly poolId: string
  readonly enabled: boolean
  readonly online: boolean
  readonly lastHeartbeatAt: string
  readonly createdAt: string
}

/** Browser-safe runtime collection returned by the Workbench Host. */
export interface WeaveRuntimeList {
  readonly runtimes: readonly WeaveRuntimeView[]
}

const mutationSchema = z.discriminatedUnion('action', [
  z.object({ action: z.literal('create'), name: z.string().trim().min(1).max(120) }).strict(),
  z.object({
    action: z.literal('configure'), id: z.string().trim().min(1).max(200),
    name: z.string().trim().min(1).max(120), poolId: z.string().trim().max(80),
  }).strict(),
  z.object({ action: z.literal('delete'), id: z.string().trim().min(1).max(200) }).strict(),
])

type Fetch = (input: string | URL, init?: RequestInit) => Promise<Response>

function normalizedServiceUrl(value: string, name: string): URL {
  const url = new URL(value.trim())
  if (!['http:', 'https:'].includes(url.protocol) || url.username !== '' || url.password !== '' || url.search !== '' || url.hash !== '') {
    throw new Error(`${name} must be an absolute HTTP(S) URL without credentials, query, or fragment`)
  }
  url.pathname = url.pathname.replace(/\/+$/u, '')
  return url
}

function urlText(url: URL): string {
  return url.href.replace(/\/$/u, '')
}

function privateIPv4Rank(address: string): number {
  const octets = address.split('.').map(Number)
  if (octets.length !== 4 || octets.some(value => !Number.isInteger(value) || value < 0 || value > 255)) return 4
  const first = octets[0] ?? -1
  const second = octets[1] ?? -1
  if (first === 10 || (first === 172 && second >= 16 && second <= 31) || (first === 192 && second === 168)) return 0
  if (first === 100 && second >= 64 && second <= 127) return 1
  if (first === 169 && second === 254) return 4
  if (first === 198 && (second === 18 || second === 19)) return 4
  return 2
}

function localIPv4(interfaces: ReturnType<typeof networkInterfaces>): string | undefined {
  return Object.values(interfaces).flatMap(items => items ?? [])
    .filter(item => item.family === 'IPv4' && !item.internal && privateIPv4Rank(item.address) < 4)
    .sort((left, right) => privateIPv4Rank(left.address) - privateIPv4Rank(right.address))[0]?.address
}

function needsReachableHost(hostname: string): boolean {
  const host = hostname.replace(/^\[|\]$/gu, '').toLowerCase()
  if (host === 'localhost' || host.endsWith('.localhost') || host === '0.0.0.0' || host === '::' || host === '::1') return true
  if (isIP(host) === 4) return host.startsWith('127.')
  return isIP(host) === 0 && !host.includes('.')
}

/**
 * Resolve the Weave URL placed in one-time runtime connection commands.
 * @param apiUrl - Weave URL used by the Workbench Host.
 * @param configuredUrl - Optional public or otherwise externally routable deployment URL.
 * @param interfaces - Host network interfaces, replaceable for deterministic tests.
 * @returns A concrete HTTP(S) URL, preferring deployment configuration and then a reachable LAN address.
 */
export function resolveRuntimeServerUrl(
  apiUrl: string,
  configuredUrl?: string,
  interfaces: ReturnType<typeof networkInterfaces> = networkInterfaces(),
): string {
  if (configuredUrl?.trim()) return urlText(normalizedServiceUrl(configuredUrl, 'runtimeServerUrl'))
  const api = normalizedServiceUrl(apiUrl, 'apiUrl')
  if (!needsReachableHost(api.hostname)) return urlText(api)
  const address = localIPv4(interfaces)
  if (address === undefined) return urlText(api)
  api.hostname = address
  return urlText(api)
}

function object(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined
}

function string(value: unknown): string { return typeof value === 'string' ? value : '' }
function count(value: unknown): number { return typeof value === 'number' && Number.isFinite(value) ? Math.max(0, Math.floor(value)) : 0 }

function healthStatus(value: unknown): WeaveRuntimeView['healthStatus'] {
  return value === 'healthy' || value === 'busy' || value === 'degraded' || value === 'quarantined'
    ? value
    : 'offline'
}

function authMode(value: unknown): WeaveRuntimeEngineView['authMode'] {
  return value === 'chatgpt' || value === 'oauth' || value === 'provider' ? value : 'unknown'
}

function engineCapabilities(value: unknown, engines: readonly string[]): readonly WeaveRuntimeEngineView[] {
  const items = object(value)
  if (items === undefined) return []
  return engines.flatMap((engine): WeaveRuntimeEngineView[] => {
    const capability = object(items[engine])
    if (capability === undefined) return []
    return [{ engine, binaryVersion: string(capability.binary_version), authMode: authMode(capability.auth_mode) }]
  })
}

function runtimeView(value: unknown): WeaveRuntimeView | null {
  const item = object(value)
  if (item === undefined || string(item.id) === '' || string(item.name) === '') return null
  const engines = Array.isArray(item.engines) ? item.engines.filter((engine): engine is string => typeof engine === 'string') : []
  return {
    id: string(item.id), name: string(item.name),
    engines, engineCapabilities: engineCapabilities(item.engine_capabilities, engines),
    healthStatus: healthStatus(item.health_status), totalSlots: count(item.total_slots), activeSlots: count(item.active_slots),
    poolId: string(item.pool_id), enabled: item.enabled !== false, online: item.online === true,
    lastHeartbeatAt: string(item.last_heartbeat_at), createdAt: string(item.created_at),
  }
}

function list(value: unknown): readonly unknown[] {
  if (Array.isArray(value)) return value
  const runtimes = object(value)?.runtimes
  return Array.isArray(runtimes) ? runtimes : []
}

function headers(apiKey: string): Headers {
  return new Headers({ Accept: 'application/json', Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' })
}

async function payload(response: Response): Promise<unknown> {
  try { return await response.json() as unknown } catch { return undefined }
}

function failure(status: number): Response {
  const code = status === 401 || status === 403
    ? 'runtime_forbidden'
    : status === 404
      ? 'runtime_missing'
      : 'runtime_unavailable'
  return Response.json({ code }, { status: status >= 400 && status < 600 ? status : 502 })
}

/**
 * Serve one authenticated browser runtime-management request without exposing the Weave API key.
 * @param apiUrl - Base URL of the Weave service owned by the Host.
 * @param apiKey - Host-only bearer credential for Weave.
 * @param request - Authenticated browser request to proxy.
 * @param fetcher - HTTP implementation, replaceable in tests.
 * @param runtimeServerUrl - Concrete URL that a runtime node can use to reach Weave.
 * @returns A browser-safe runtime response with no Weave credential or backend diagnostic text.
 */
export async function handleWeaveRuntimeRequest(
  apiUrl: string,
  apiKey: string,
  request: Request,
  fetcher: Fetch = fetch,
  runtimeServerUrl: string = resolveRuntimeServerUrl(apiUrl),
): Promise<Response> {
  if (apiKey === '') return Response.json({ code: 'weave_disconnected' }, { status: 503 })
  const common = { headers: headers(apiKey), signal: AbortSignal.timeout(15_000) }
  try {
    if (request.method === 'GET' || request.method === 'HEAD') {
      const response = await fetcher(`${apiUrl}/v1/runtimes`, common)
      if (!response.ok) return failure(response.status)
      const runtimes = list(await payload(response)).map(runtimeView).filter((item): item is WeaveRuntimeView => item !== null)
      const result = Response.json({ runtimes } satisfies WeaveRuntimeList, { headers: { 'Cache-Control': 'no-store' } })
      if (request.method === 'GET') return result
      await result.body?.cancel()
      return new Response(null, { status: result.status, headers: result.headers })
    }
    const parsed = mutationSchema.safeParse(await request.json())
    if (!parsed.success) return Response.json({ code: 'invalid_input' }, { status: 400 })
    const input = parsed.data
    const path = input.action === 'create' ? '/v1/runtimes' : `/v1/runtimes/${encodeURIComponent(input.id)}`
    const method = input.action === 'create' ? 'POST' : input.action === 'configure' ? 'PUT' : 'DELETE'
    const body = input.action === 'create'
      ? JSON.stringify({ name: input.name })
      : input.action === 'configure'
        ? JSON.stringify({ name: input.name, pool_id: input.poolId })
        : undefined
    const response = await fetcher(`${apiUrl}${path}`, { ...common, method, ...(body === undefined ? {} : { body }) })
    if (!response.ok) return failure(response.status)
    if (input.action !== 'create') return new Response(null, { status: 204 })
    const created = object(await payload(response))
    if (created === undefined || string(created.id) === '' || string(created.name) === '' || string(created.token) === '') {
      return Response.json({ code: 'runtime_token_missing' }, { status: 502 })
    }
    const result = { id: string(created.id), name: string(created.name), token: string(created.token), serverUrl: runtimeServerUrl }
    return Response.json(result, {
      status: 201, headers: { 'Cache-Control': 'no-store' },
    })
  } catch {
    return Response.json({ code: 'weave_unreachable' }, { status: 502 })
  }
}
