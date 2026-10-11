import { afterEach, describe, expect, it, vi } from 'vitest'
import { withForgeApproval, type PendingApproval } from './forge-approval'

const FORGE = 'https://forge.example'
const PAGE = `${FORGE}/_console/auth/device?user_code=WXYZ2345`
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
const issued = () => json({ device_code: 'device-code', user_code: 'WXYZ2345', verification_uri: `${FORGE}/_console/auth/device`, verification_uri_complete: PAGE, expires_in: 1800, interval: 5 })
const waiting = () => json({ error: 'authorization_pending' }, 400)
const released = () => json({ access_token: 'forge-session', token_type: 'Bearer' })

type Call = { path: string; init: RequestInit }

function forge(code: () => Response, polls: Array<() => Response | Promise<Response>>) {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: URL | RequestInfo, init: RequestInit = {}) => {
    const url = new URL(String(input))
    calls.push({ path: url.pathname, init })
    if (url.pathname === '/api/v1/auth/device/code') return code()
    if (url.pathname === '/api/v1/auth/device/token') return (polls.length > 1 ? polls.shift()! : polls[0]!)()
    return new Response(null, { status: 200 })
  }))
  return calls
}

const immediately = async () => undefined

afterEach(() => vi.unstubAllGlobals())

describe('approving on Forge', () => {
  it('shows the code, waits at the pace Forge sets, hands over the session and then releases it', async () => {
    const calls = forge(issued, [waiting, () => json({ error: 'slow_down' }, 400), released])
    const shown: PendingApproval[] = [], waits: number[] = []
    const use = vi.fn(async (token: string) => `used ${token}`)

    await expect(withForgeApproval(FORGE, {
      signal: new AbortController().signal, onPending: (pending) => shown.push(pending), wait: async (milliseconds) => { waits.push(milliseconds) },
    }, use)).resolves.toBe('used forge-session')

    expect(shown).toEqual([{ userCode: 'WXYZ2345', page: PAGE }])
    expect(waits).toEqual([5000, 5000, 10000])
    expect(calls.map((call) => call.path)).toEqual(['/api/v1/auth/device/code', '/api/v1/auth/device/token', '/api/v1/auth/device/token', '/api/v1/auth/device/token', '/api/v1/auth/sign-out'])
    expect(JSON.parse(String(calls[0].init.body))).toEqual({ client_id: 'weave-console' })
    expect(JSON.parse(String(calls[1].init.body))).toEqual({ grant_type: 'urn:ietf:params:oauth:grant-type:device_code', device_code: 'device-code', client_id: 'weave-console' })
    // Forge's own cookies never travel with the console's requests.
    expect(calls.every((call) => call.init.credentials === 'omit')).toBe(true)
    expect(new Headers(calls[4].init.headers).get('Authorization')).toBe('Bearer forge-session')
  })

  it('releases the session even when the call it was handed to fails', async () => {
    const calls = forge(issued, [released])
    await expect(withForgeApproval(FORGE, { signal: new AbortController().signal, onPending: () => undefined, wait: immediately }, async () => { throw new Error('refused') })).rejects.toThrow('refused')
    expect(calls.at(-1)?.path).toBe('/api/v1/auth/sign-out')
  })

  it.each([
    [() => json({}, 404), 'forge_approval_unavailable'],
    [() => json({ error: 'invalid_client' }, 400), 'forge_client_refused'],
    [() => json({}, 500), 'forge_unavailable'],
    [() => json({ device_code: 'device-code', user_code: 'WXYZ2345', verification_uri: 'https://elsewhere.example/auth/device', expires_in: 1800, interval: 5 }), 'forge_response_unrecognized'],
  ])('does not start an approval Forge cannot or should not carry out', async (code, expected) => {
    const calls = forge(code, [released])
    const onPending = vi.fn()
    await expect(withForgeApproval(FORGE, { signal: new AbortController().signal, onPending, wait: immediately }, async () => undefined)).rejects.toMatchObject({ code: expected })
    expect(onPending).not.toHaveBeenCalled()
    expect(calls.map((call) => call.path)).toEqual(['/api/v1/auth/device/code'])
  })

  it.each([
    ['access_denied', 'forge_approval_denied'],
    ['expired_token', 'forge_approval_expired'],
    ['server_error', 'forge_unavailable'],
  ])('stops waiting when Forge answers %s', async (error, expected) => {
    forge(issued, [waiting, () => json({ error }, 400)])
    const use = vi.fn()
    await expect(withForgeApproval(FORGE, { signal: new AbortController().signal, onPending: () => undefined, wait: immediately }, use)).rejects.toMatchObject({ code: expected })
    expect(use).not.toHaveBeenCalled()
  })

  it('says Forge is unreachable when the browser cannot call it', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    await expect(withForgeApproval(FORGE, { signal: new AbortController().signal, onPending: () => undefined, wait: immediately }, async () => undefined)).rejects.toMatchObject({ code: 'forge_unreachable' })
  })

  it('gives up when cancelled, and hands back a session released in that moment', async () => {
    let answer!: (response: Response) => void
    const calls = forge(issued, [() => new Promise<Response>((resolve) => { answer = resolve })])
    const controller = new AbortController(), use = vi.fn()
    const approval = withForgeApproval(FORGE, { signal: controller.signal, onPending: () => undefined, wait: immediately }, use)
    const refused = expect(approval).rejects.toMatchObject({ code: 'forge_approval_cancelled' })
    await vi.waitFor(() => expect(calls.some((call) => call.path === '/api/v1/auth/device/token')).toBe(true))
    controller.abort()
    answer(released())
    await refused
    expect(use).not.toHaveBeenCalled()
    expect(calls.at(-1)?.path).toBe('/api/v1/auth/sign-out')
    expect(new Headers(calls.at(-1)?.init.headers).get('Authorization')).toBe('Bearer forge-session')
  })
})
