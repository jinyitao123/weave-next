import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from './api'
import { APPROVE_ANSWER, APPROVE_REQUEST, approvesThroughForgePage, embedded, withForgePageApproval } from './forge-embed'

const FORGE = 'https://forge.example'
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

// The page around the console: it receives the code and may answer about it.
function insideForge() {
  const posted: Array<{ message: { type: string; userCode: string }; target: string }> = []
  const page = { postMessage: (message: { type: string; userCode: string }, target: string) => { posted.push({ message, target }) } }
  vi.spyOn(window, 'parent', 'get').mockReturnValue(page as unknown as Window)
  const answer = (data: unknown, from: { origin?: string; source?: unknown } = {}) => {
    const event = new MessageEvent('message', { data })
    Object.defineProperty(event, 'origin', { value: from.origin ?? FORGE })
    Object.defineProperty(event, 'source', { value: 'source' in from ? from.source : page })
    window.dispatchEvent(event)
  }
  return { posted, answer }
}

function forge() {
  const paths: string[] = []
  vi.stubGlobal('fetch', vi.fn(async (input: URL | RequestInfo) => {
    const url = new URL(String(input))
    paths.push(url.pathname)
    if (url.pathname === '/api/v1/auth/device/code') return json({ device_code: 'device-code', user_code: 'WXYZ2345', verification_uri_complete: `${FORGE}/_console/auth/device?user_code=WXYZ2345`, expires_in: 1800, interval: 5 })
    if (url.pathname === '/api/v1/auth/device/token') return json({ access_token: 'forge-session', token_type: 'Bearer' })
    return new Response(null, { status: 200 })
  }))
  return paths
}

afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks() })

describe('signing in through the Forge page around the console', () => {
  it('is only attempted when the console is shown inside another page', () => {
    expect(embedded()).toBe(false)
    insideForge()
    expect(embedded()).toBe(true)
  })

  it('sends that Forge the code, and goes on at once when the page says it approved', async () => {
    const paths = forge()
    const { posted, answer } = insideForge()
    const entering = withForgePageApproval(FORGE, new AbortController().signal, async (token) => `used ${token}`)
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    // Addressed to this Forge only, and carrying nothing but the code.
    expect(posted[0]).toEqual({ message: { type: APPROVE_REQUEST, userCode: 'WXYZ2345' }, target: FORGE })
    answer({ type: APPROVE_ANSWER, userCode: 'WXYZ2345', approved: true })
    await expect(entering).resolves.toBe('used forge-session')
    expect(paths).toEqual(['/api/v1/auth/device/code', '/api/v1/auth/device/token', '/api/v1/auth/sign-out'])
    expect(approvesThroughForgePage()).toBe(true)
  })

  it('takes no answer from another site, another window or about another code, and gives up when none comes', async () => {
    forge()
    const { posted, answer } = insideForge()
    const entering = withForgePageApproval(FORGE, new AbortController().signal, async () => 'used', 60)
    const refused = expect(entering).rejects.toMatchObject({ code: 'forge_page_unavailable' })
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    answer({ type: APPROVE_ANSWER, userCode: 'WXYZ2345', approved: true }, { origin: 'https://elsewhere.example' })
    answer({ type: APPROVE_ANSWER, userCode: 'WXYZ2345', approved: true }, { source: window })
    answer({ type: APPROVE_ANSWER, userCode: 'OTHER', approved: true })
    answer({ type: 'something-else', userCode: 'WXYZ2345', approved: true })
    await refused
    expect(approvesThroughForgePage()).toBe(false)
  })

  it('stops when the page says the person is not approved', async () => {
    const paths = forge()
    const { posted, answer } = insideForge()
    const entering = withForgePageApproval(FORGE, new AbortController().signal, async () => 'used')
    const refused = expect(entering).rejects.toBeInstanceOf(ApiError)
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    answer({ type: APPROVE_ANSWER, userCode: 'WXYZ2345', approved: false })
    await refused
    await expect(entering).rejects.toMatchObject({ code: 'forge_approval_denied' })
    expect(paths).not.toContain('/api/v1/auth/device/token')
  })

  it('does not mark the surrounding page approved if identity exchange fails', async () => {
    const paths = forge()
    const { posted, answer } = insideForge()
    const entering = withForgePageApproval(FORGE, new AbortController().signal, async () => { throw new ApiError(403, 'forbidden') })
    const refused = expect(entering).rejects.toMatchObject({ code: 'forbidden' })
    await vi.waitFor(() => expect(posted).toHaveLength(1))
    answer({ type: APPROVE_ANSWER, userCode: 'WXYZ2345', approved: true })
    await refused
    expect(approvesThroughForgePage()).toBe(false)
    expect(paths.at(-1)).toBe('/api/v1/auth/sign-out')
  })

  it('does not issue a code after the caller has already cancelled', async () => {
    const paths = forge()
    insideForge()
    await expect(withForgePageApproval(FORGE, AbortSignal.abort(), async () => 'used')).rejects.toMatchObject({ code: 'forge_approval_cancelled' })
    expect(paths).toEqual([])
    expect(approvesThroughForgePage()).toBe(false)
  })
})
