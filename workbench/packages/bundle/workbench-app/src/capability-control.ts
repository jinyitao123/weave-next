import { z } from 'zod'

const id = z.string().min(1).max(200)
const revision = z.number().int().positive().max(Number.MAX_SAFE_INTEGER)
const command = z.discriminatedUnion('action', [
  z.object({ action: z.literal('save'), definition: z.record(z.string(), z.unknown()) }).strict(),
  z.object({ action: z.literal('publish'), id, revision }).strict(),
  z.object({ action: z.literal('invoke'), id, revision, requestId: id, input: z.record(z.string(), z.unknown()) }).strict(),
  z.object({ action: z.literal('status'), id }).strict(),
  z.object({ action: z.literal('cancel'), id }).strict(),
])

/**
 * Proxy capability operations through the configured Workbench Host credential.
 * @param apiUrl - Fixed Weave server URL.
 * @param apiKey - Host-only credential.
 * @param request - Browser operation; arbitrary upstream paths are not accepted.
 * @param fetcher - HTTP transport.
 * @returns Weave response without credentials or upstream headers.
 */
export async function handleCapabilityRequest(apiUrl: string, apiKey: string, request: Request, fetcher: typeof fetch = fetch): Promise<Response> {
  const reply = (status: number, code: string) => Response.json({ code }, { status, headers: { 'Cache-Control': 'no-store' } })
  if (apiKey === '') return reply(503, 'weave_disconnected')
  let path = '/v1/capabilities/drafts'
  let method = 'GET'
  let body: object | undefined
  if (request.method !== 'GET') {
    if (request.method !== 'POST') return reply(405, 'method_not_allowed')
    let raw: unknown
    try { raw = await request.json() } catch { return reply(400, 'invalid_input') }
    const parsed = command.safeParse(raw)
    if (!parsed.success) return reply(400, 'invalid_input')
    const op = parsed.data
    switch (op.action) {
      case 'save': method = 'POST'; body = { definition: op.definition }; break
      case 'publish': method = 'POST'; path = `/v1/capabilities/${encodeURIComponent(op.id)}/versions/${op.revision}/publish`; body = {}; break
      case 'invoke': method = 'POST'; path = `/v1/capabilities/${encodeURIComponent(op.id)}/versions/${op.revision}/invocations`; body = { request_id: op.requestId, input: op.input }; break
      case 'status': path = `/v1/invocations/${encodeURIComponent(op.id)}`; break
      case 'cancel': method = 'POST'; path = `/v1/invocations/${encodeURIComponent(op.id)}/cancel`; body = {}; break
    }
  }
  try {
    const response = await fetcher(apiUrl.replace(/\/$/u, '') + path, {
      method, headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(15_000)]), redirect: 'error',
    })
    const data: unknown = await response.json()
    return Response.json(data, { status: response.status, headers: { 'Cache-Control': 'no-store' } })
  } catch { return reply(502, 'weave_unreachable') }
}
