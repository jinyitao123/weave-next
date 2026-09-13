import { z } from 'zod'

const id = z.string().min(1).max(200)
const action = z.discriminatedUnion('action', [
  z.object({ action: z.literal('quota'), max_steps_per_invocation: z.number().int().min(1).max(1000), max_active_invocations: z.number().int().min(1).max(1000) }).strict(),
  z.object({ action: z.literal('cancel'), invocation_id: id }).strict(),
  z.object({ action: z.literal('resume'), invocation_id: id, step_id: id, approved: z.boolean() }).strict(),
])

type JsonObject = Record<string, unknown>

function object(value: unknown): JsonObject | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? value as JsonObject : undefined
}

function documentText(value: unknown): string {
  const result = object(object(value)?.invocation)?.result
  const resultObject = object(result)
  const markdown = resultObject?.document_markdown ?? resultObject?.document ?? resultObject?.content
  if (typeof markdown === 'string' && markdown.trim() !== '') return markdown
  if (typeof result === 'string' && result.trim() !== '') return result
  return result === undefined ? '' : `${JSON.stringify(result, null, 2)}\n`
}

/** Proxy capability operations through the trusted Workbench Host. */
export async function handleCapabilityOperationsRequest(
  apiUrl: string,
  apiKey: string,
  request: Request,
  fetcher: typeof fetch = fetch,
): Promise<Response> {
  const fail = (status: number, code: string): Response => Response.json({ code }, { status, headers: { 'Cache-Control': 'no-store' } })
  if (apiKey === '') return fail(503, 'weave_disconnected')
  const call = async (path: string, init: RequestInit = {}): Promise<Response> => fetcher(`${apiUrl.replace(/\/$/u, '')}${path}`, {
    ...init,
    headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
    signal: AbortSignal.any([request.signal, AbortSignal.timeout(30_000)]),
    redirect: 'error',
  })
  try {
    if (request.method === 'GET') {
      const url = new URL(request.url)
      const invocationId = url.searchParams.get('invocation_id')?.trim() ?? ''
      if (invocationId !== '') {
        const detail = await call(`/v1/invocations/${encodeURIComponent(invocationId)}/events`)
        const value = await detail.json() as unknown
        if (url.searchParams.get('download') === '1' && detail.ok) {
          const text = documentText(value)
          if (text === '') return fail(404, 'document_unavailable')
          return new Response(text, { headers: {
            'Cache-Control': 'no-store',
            'Content-Type': 'text/markdown; charset=utf-8',
            'Content-Disposition': `attachment; filename="capability-${encodeURIComponent(invocationId)}.md"`,
          } })
        }
        return Response.json(value, { status: detail.status, headers: { 'Cache-Control': 'no-store' } })
      }
      const [history, quota] = await Promise.all([call('/v1/capability-invocations'), call('/v1/capability-quotas')])
      const historyValue = await history.json() as JsonObject
      const quotaValue = await quota.json() as JsonObject
      return Response.json({ ...historyValue, quota: quotaValue }, {
        status: history.ok && quota.ok ? 200 : Math.max(history.status, quota.status),
        headers: { 'Cache-Control': 'no-store' },
      })
    }
    if (request.method !== 'POST') return fail(405, 'method_not_allowed')
    let value: unknown
    try { value = await request.json() } catch { return fail(400, 'invalid_input') }
    const parsed = action.safeParse(value)
    if (!parsed.success) return fail(400, 'invalid_input')
    const input = parsed.data
    let response: Response
    if (input.action === 'quota') {
      response = await call('/v1/capability-quotas', { method: 'PUT', body: JSON.stringify(input) })
    } else if (input.action === 'cancel') {
      response = await call(`/v1/invocations/${encodeURIComponent(input.invocation_id)}/cancel`, { method: 'POST', body: '{}' })
    } else {
      response = await call(`/v1/invocations/${encodeURIComponent(input.invocation_id)}/resume`, {
        method: 'POST', body: JSON.stringify({ step_id: input.step_id, response: { approved: input.approved } }),
      })
    }
    return Response.json(await response.json() as unknown, { status: response.status, headers: { 'Cache-Control': 'no-store' } })
  } catch {
    return fail(502, 'weave_unreachable')
  }
}
