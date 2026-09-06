import { z } from 'zod'

type Fetch = (input: string | URL, init?: RequestInit) => Promise<Response>
const mutation = z.object({
  name: z.string().regex(/^[a-z0-9][a-z0-9_-]*$/u), expectedVersion: z.number().int().positive(),
  engine: z.enum(['claude', 'codex', 'opencode']), runtimeId: z.string().min(1).max(200),
  model: z.string().trim().max(200), fallbackModels: z.array(z.string().trim().min(1).max(200)).max(2),
  fallbackRetries: z.number().int().min(0).max(2),
}).strict()
function object(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? value as Record<string, unknown> : undefined
}
function text(value: unknown): string { return typeof value === 'string' ? value : '' }
function failure(status: number): Response {
  return Response.json({ code: status === 409 ? 'execution_conflict' : status === 422 ? 'execution_invalid' : status === 401 || status === 403 ? 'runtime_forbidden' : 'execution_unavailable' }, { status: status >= 400 && status < 600 ? status : 502 })
}

/**
 * Proxy execution settings without exposing provider credentials.
 * @param apiUrl Trusted Weave service address.
 * @param apiKey Host credential for runtime administration.
 * @param request Browser-authenticated settings request.
 * @param fetcher HTTP transport.
 * @returns Editable settings or a safe mutation result.
 */
export async function handleAgentExecutionRequest(
  apiUrl: string, apiKey: string, request: Request, fetcher: Fetch = fetch,
): Promise<Response> {
  if (apiKey === '') return Response.json({ code: 'weave_disconnected' }, { status: 503 })
  const common = { headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' }, signal: AbortSignal.timeout(60_000) }
  try {
    if (request.method === 'GET') {
      const response = await fetcher(`${apiUrl}/v1/agent-execution-settings`, common)
      if (!response.ok) return failure(response.status)
      const body = await response.json() as unknown
      const items = Array.isArray(body) ? body : object(body)?.agents
      const agents = (Array.isArray(items) ? items : []).flatMap((value) => {
        const item = object(value)
        if (item === undefined || item.visibility === 'platform' || text(item.name) === '' || typeof item.version !== 'number') return []
        return [{ name: text(item.name), displayName: text(item.display_name) || text(item.name), version: item.version,
          engine: text(item.engine) || 'loom', runtimeId: text(item.runtime_id), model: text(item.model),
          fallbackModels: Array.isArray(item.fallback_models) ? item.fallback_models.filter((model): model is string => typeof model === 'string') : [],
          fallbackRetries: typeof item.fallback_retries === 'number' ? item.fallback_retries : 0 }]
      })
      return Response.json({ agents }, { headers: { 'Cache-Control': 'no-store' } })
    }
    if (request.method !== 'PUT') return new Response(null, { status: 405 })
    const parsed = mutation.safeParse(await request.json())
    if (!parsed.success) return Response.json({ code: 'invalid_input' }, { status: 400 })
    const input = parsed.data
    const response = await fetcher(`${apiUrl}/v1/agents/${encodeURIComponent(input.name)}/execution`, { ...common, method: 'PUT', body: JSON.stringify({
      expected_version: input.expectedVersion, engine: input.engine, runtime_id: input.runtimeId, model: input.model,
      fallback_models: input.fallbackModels, fallback_retries: input.fallbackRetries,
    }) })
    if (!response.ok) return failure(response.status)
    return Response.json({ saved: true })
  } catch { return Response.json({ code: 'weave_unreachable' }, { status: 502 }) }
}
