import { z } from 'zod'

const id = z.string().min(1).max(200)
const action = z.discriminatedUnion('action', [
  z.object({ action: z.literal('create'), name: z.string().trim().min(1).max(160) }).strict(),
  z.object({ action: z.literal('enable'), app_id: id, enabled: z.boolean() }).strict(),
  z.object({ action: z.literal('issue'), app_id: id, name: z.string().trim().min(1).max(160), scopes: z.array(z.enum(['invoke', 'read', 'cancel'])).min(1).max(3) }).strict(),
  z.object({ action: z.literal('revoke'), app_id: id, credential_id: id }).strict(),
  z.object({ action: z.literal('grant'), app_id: id, capability_id: id, revision: z.number().int().positive().max(Number.MAX_SAFE_INTEGER), enabled: z.boolean() }).strict(),
])

/**
 * Forward application management through the private Host credential.
 * @param apiUrl - Fixed Weave endpoint.
 * @param apiKey - Host credential, never returned to the browser.
 * @param request - Browser operation.
 * @param fetcher - HTTP transport.
 * @returns Management metadata, or a newly issued application key once.
 */
export async function handleCapabilityAppsRequest(apiUrl: string, apiKey: string, request: Request, fetcher: typeof fetch = fetch): Promise<Response> {
  const fail = (status: number, code: string) => Response.json({ code }, { status, headers: { 'Cache-Control': 'no-store' } })
  if (apiKey === '') return fail(503, 'weave_disconnected')
  let body: object | undefined
  if (request.method !== 'GET') {
    if (request.method !== 'POST') return fail(405, 'method_not_allowed')
    let value: unknown
    try { value = await request.json() } catch { return fail(400, 'invalid_input') }
    const parsed = action.safeParse(value)
    if (!parsed.success) return fail(400, 'invalid_input')
    body = parsed.data
  }
  try {
    const response = await fetcher(apiUrl.replace(/\/$/u, '') + '/v1/capability-apps' + (body === undefined ? '' : '/actions'), {
      method: body === undefined ? 'GET' : 'POST', headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(15_000)]), redirect: 'error',
    })
    if (response.status === 204) return new Response(null, { status: 204, headers: { 'Cache-Control': 'no-store' } })
    return Response.json(await response.json() as unknown, { status: response.status, headers: { 'Cache-Control': 'no-store' } })
  } catch { return fail(502, 'weave_unreachable') }
}
