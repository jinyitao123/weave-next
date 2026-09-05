/** Exact-run access to complete, remotely retained Weave deliverable content. */
import type { WorkTaskProjection } from './index.ts'

/**
 * Forward a known task deliverable without exposing the Host's business credential.
 * @param apiUrl - Configured Weave origin.
 * @param apiKey - Host-only business API key.
 * @param request - Browser-authenticated request carrying the current run and deliverable IDs.
 * @param task - The selected Session's authoritative projection.
 * @param fetcher - HTTP transport used for the complete content response.
 * @returns Full content for a bound deliverable, or a bounded error response.
 */
export async function handleWeaveDeliverableRequest(
  apiUrl: string,
  apiKey: string,
  request: Request,
  task: WorkTaskProjection | null | undefined,
  fetcher: typeof fetch = fetch,
): Promise<Response> {
  const query = new URL(request.url).searchParams
  const id = query.get('id') ?? ''
  const item = task?.deliverables.find(deliverable => deliverable.id === id)
  if (task == null || task.runId !== query.get('runId') || item === undefined) {
    return new Response('This deliverable is not bound to the current task.', { status: 404 })
  }
  if (apiKey === '') return new Response('Weave is unavailable.', { status: 503 })
  const preview = query.get('mode') === 'preview'
  if (preview && item.contentType !== 'image/svg+xml') return new Response('No image preview is available.', { status: 415 })
  try {
    const response = await fetcher(`${apiUrl}/v1/deliverables/${encodeURIComponent(id)}/content`, {
      headers: { Authorization: `Bearer ${apiKey}` }, signal: AbortSignal.timeout(15_000), redirect: 'error',
    })
    if (!response.ok) {
      await response.body?.cancel()
      return new Response('The complete deliverable could not be loaded.', { status: response.status === 404 ? 404 : 502 })
    }
    const disposition = response.headers.get('Content-Disposition') ?? ''
    const headers = new Headers({
      'Content-Type': preview ? 'image/svg+xml' : response.headers.get('Content-Type') ?? 'application/octet-stream',
      'Content-Disposition': preview ? 'inline' : /^attachment(?:;|$)/i.test(disposition) ? disposition : 'attachment',
      'Cache-Control': 'no-store',
      'X-Content-Type-Options': 'nosniff',
      'Content-Security-Policy': "default-src 'none'; sandbox",
    })
    if (request.method === 'HEAD') {
      await response.body?.cancel()
      return new Response(null, { headers })
    }
    return new Response(response.body, { headers })
  } catch { return new Response('The complete deliverable could not be loaded.', { status: 502 }) }
}
