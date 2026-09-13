/** Host-only published-capability management proxy. Weave credentials never reach the browser. */

import { z } from 'zod'

const id = z.string().trim().min(1).max(200)
const name = z.string().trim().min(1).max(120)
const description = z.string().max(2_000)
const positive = z.number().int().positive()
const limits = z.object({
  max_input_bytes: positive,
  max_output_bytes: positive,
  max_nesting_depth: positive,
  max_object_fields: positive,
  max_array_items: positive,
  max_string_bytes: positive,
  queue_timeout_seconds: positive,
  execution_timeout_seconds: positive,
  max_output_tokens: positive,
}).strict()
const resultPolicy = z.object({
  exposed_fields: z.array(z.string().trim().min(1).max(128)).min(1).max(64),
  include_usage: z.boolean(),
  include_contract_evidence: z.boolean(),
}).strict()

const actionSchema = z.discriminatedUnion('action', [
  z.object({ action: z.literal('create_app'), name, description, max_concurrent: positive.max(1_024) }).strict(),
  z.object({ action: z.literal('set_app_enabled'), app_id: id, enabled: z.boolean() }).strict(),
  z.object({ action: z.literal('create_credential'), app_id: id, name, scopes: z.array(z.enum(['invoke', 'read', 'cancel'])).min(1).max(3) }).strict(),
  z.object({ action: z.literal('revoke_credential'), credential_id: id }).strict(),
  z.object({ action: z.literal('create_capability'), key: z.string().regex(/^[a-z][a-z0-9_]{0,62}$/u), name, description }).strict(),
  z.object({ action: z.literal('set_capability_enabled'), capability_id: id, enabled: z.boolean() }).strict(),
  z.object({
    action: z.literal('publish_release'), capability_id: id, release_version: positive,
    workflow_id: id, workflow_version: positive, execution_limits: limits, result_policy: resultPolicy,
  }).strict(),
  z.object({ action: z.literal('set_release_enabled'), capability_id: id, release_version: positive, enabled: z.boolean() }).strict(),
  z.object({ action: z.literal('grant_release'), app_id: id, capability_id: id, release_version: positive, max_concurrent: positive.max(1_024) }).strict(),
  z.object({ action: z.literal('revoke_grant'), grant_id: id }).strict(),
  z.object({ action: z.literal('cancel_invocation'), invocation_id: id, reason: z.string().trim().min(1).max(256) }).strict(),
])

type Fetch = (input: string | URL, init?: RequestInit) => Promise<Response>

function browserFailure(status: number, value?: unknown): Response {
  const backendCode = typeof value === 'object' && value !== null && 'code' in value && typeof value.code === 'string'
    ? value.code
    : ''
  const code = status === 401 || status === 403
    ? 'capability_forbidden'
    : backendCode === 'capability_management_invalid'
      ? 'invalid_input'
      : backendCode === 'capability_management_missing'
        ? 'capability_missing'
        : backendCode === 'capability_management_conflict'
          ? 'capability_conflict'
          : backendCode === 'capability_release_unavailable'
            ? 'capability_release_unavailable'
            : 'capability_unavailable'
  return Response.json({ code }, { status: status >= 400 && status < 600 ? status : 502 })
}

/**
 * Serve one browser capability-management request through the Host credential.
 * @param apiUrl - Base URL of the Weave service.
 * @param apiKey - Host-only administrator credential.
 * @param request - Browser request already admitted by the Workbench connection.
 * @param fetcher - HTTP implementation, replaceable in tests.
 * @returns The bounded management snapshot, one-time credential, or mutation receipt.
 */
export async function handleWeaveCapabilityRequest(
  apiUrl: string,
  apiKey: string,
  request: Request,
  fetcher: Fetch = fetch,
): Promise<Response> {
  if (apiKey === '') return Response.json({ code: 'weave_disconnected' }, { status: 503 })
  const headers = new Headers({ Accept: 'application/json', Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' })
  try {
    if (request.method === 'GET' || request.method === 'HEAD') {
      const response = await fetcher(`${apiUrl}/v1/capability-management`, {
        headers, signal: AbortSignal.timeout(15_000),
      })
      const value = await response.json() as unknown
      if (!response.ok) return browserFailure(response.status, value)
      const result = Response.json(value, { headers: { 'Cache-Control': 'no-store' } })
      if (request.method === 'GET') return result
      await result.body?.cancel()
      return new Response(null, { status: result.status, headers: result.headers })
    }
    const parsed = actionSchema.safeParse(await request.json())
    if (!parsed.success) return Response.json({ code: 'invalid_input' }, { status: 400 })
    const response = await fetcher(`${apiUrl}/v1/capability-management/actions`, {
      method: 'POST', headers, body: JSON.stringify(parsed.data), signal: AbortSignal.timeout(15_000),
    })
    if (response.status === 204) return new Response(null, { status: 204 })
    const value = await response.json() as unknown
    if (!response.ok) return browserFailure(response.status, value)
    return Response.json(value, { status: response.status, headers: { 'Cache-Control': 'no-store' } })
  } catch {
    return Response.json({ code: 'weave_unreachable' }, { status: 502 })
  }
}
