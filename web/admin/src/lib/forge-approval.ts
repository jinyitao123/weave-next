import { ApiError } from './api'

// The console proves a Forge account without seeing its password: Forge issues
// a confirmation code, the person approves it on a Forge page in their own
// browser, and Forge releases a short session to the console. That session is
// handed to one call and then released.

/** The public name Forge knows the console by; a Forge must list it to accept console sign-in. */
export const CONSOLE_CLIENT_ID = 'weave-console'

export interface PendingApproval {
  userCode: string
  /** The Forge page that approves this code. */
  page: string
}

export interface ApprovalOptions {
  signal: AbortSignal
  /** Called once Forge has issued the code and the page that approves it. */
  onPending(pending: PendingApproval): void
  /** Waits between polls; replaced in tests. */
  wait?(milliseconds: number, signal: AbortSignal): Promise<void>
}

const cancelled = () => new ApiError(0, 'forge_approval_cancelled')

/** Resolves after the given time, or rejects as cancelled when the signal fires first. */
export function waitFor(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(cancelled()); return }
    const stop = () => { clearTimeout(timer); reject(cancelled()) }
    const timer = setTimeout(() => { signal.removeEventListener('abort', stop); resolve() }, milliseconds)
    signal.addEventListener('abort', stop, { once: true })
  })
}

async function forge(forgeOrigin: string, path: string, body: unknown, signal: AbortSignal, token?: string): Promise<Response> {
  try {
    return await fetch(new URL(path, forgeOrigin), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) },
      body: JSON.stringify(body),
      credentials: 'omit',
      signal,
    })
  } catch {
    throw signal.aborted ? cancelled() : new ApiError(0, 'forge_unreachable')
  }
}

const release = (forgeOrigin: string, token: string) =>
  forge(forgeOrigin, '/api/v1/auth/sign-out', {}, AbortSignal.timeout(10_000), token).then(() => undefined, () => undefined)

const text = (value: unknown) => typeof value === 'string' && value.trim() ? value : undefined

/** Runs one approval on Forge and hands the session it releases to `use`. */
export async function withForgeApproval<T>(forgeOrigin: string, options: ApprovalOptions, use: (forgeToken: string) => Promise<T>): Promise<T> {
  const { signal } = options
  const started = await forge(forgeOrigin, '/api/v1/auth/device/code', { client_id: CONSOLE_CLIENT_ID }, signal)
  const code = (await started.json().catch(() => undefined)) as Record<string, unknown> | undefined
  if (!started.ok) {
    if (started.status === 404) throw new ApiError(404, 'forge_approval_unavailable')
    if (code?.error === 'invalid_client') throw new ApiError(started.status, 'forge_client_refused')
    throw new ApiError(started.status, 'forge_unavailable')
  }
  const deviceCode = text(code?.device_code), userCode = text(code?.user_code)
  const address = text(code?.verification_uri_complete) ?? text(code?.verification_uri)
  if (!deviceCode || !userCode || !address) throw new ApiError(502, 'forge_response_unrecognized')
  let page: URL
  try { page = new URL(address) } catch { throw new ApiError(502, 'forge_response_unrecognized') }
  // Only this Forge may ask the person to approve a sign-in to it.
  if (page.origin !== new URL(forgeOrigin).origin) throw new ApiError(502, 'forge_response_unrecognized')
  const deadline = Date.now() + (typeof code?.expires_in === 'number' && code.expires_in > 0 ? code.expires_in : 600) * 1000
  let interval = typeof code?.interval === 'number' && code.interval >= 0 ? code.interval : 5
  options.onPending({ userCode, page: page.toString() })

  let forgeToken: string | undefined
  while (!forgeToken) {
    await (options.wait ?? waitFor)(interval * 1000, signal)
    if (signal.aborted) throw cancelled()
    if (Date.now() >= deadline) throw new ApiError(400, 'forge_approval_expired')
    const polled = await forge(forgeOrigin, '/api/v1/auth/device/token', { grant_type: 'urn:ietf:params:oauth:grant-type:device_code', device_code: deviceCode, client_id: CONSOLE_CLIENT_ID }, signal)
    const answer = (await polled.json().catch(() => undefined)) as Record<string, unknown> | undefined
    const token = polled.ok ? text(answer?.access_token) : undefined
    if (signal.aborted) {
      // Released in the moment the person gave up: it has no owner here.
      if (token) await release(forgeOrigin, token)
      throw cancelled()
    }
    if (polled.ok) {
      if (!token) throw new ApiError(502, 'forge_response_unrecognized')
      forgeToken = token
    } else if (answer?.error === 'slow_down' || polled.status === 429) interval += 5
    else if (answer?.error === 'access_denied') throw new ApiError(polled.status, 'forge_approval_denied')
    else if (answer?.error === 'expired_token') throw new ApiError(polled.status, 'forge_approval_expired')
    else if (answer?.error !== 'authorization_pending') throw new ApiError(polled.status, 'forge_unavailable')
  }
  try {
    return await use(forgeToken)
  } finally {
    await release(forgeOrigin, forgeToken)
  }
}
