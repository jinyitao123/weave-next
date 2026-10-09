import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api, errorMessage, signInWithForge } from './api'

afterEach(() => vi.unstubAllGlobals())

describe('api', () => {
  it('sends the console header and maps server errors', async () => {
    const fetchMock = vi.fn(async () => new Response(JSON.stringify({ error: 'invalid api key' }), { status: 401 }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(api('/v1/admin/session', { method: 'POST', body: '{}' })).rejects.toMatchObject({ status: 401, code: 'invalid api key' })
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    const headers = new Headers(init.headers)
    expect(headers.get('X-Weave-Admin')).toBe('1')
    expect(headers.get('Content-Type')).toBe('application/json')
    expect(init.credentials).toBe('same-origin')
  })

  it('never shows raw server text for unknown errors', () => {
    expect(errorMessage(new ApiError(500, 'pq: relation missing'))).toBe('服务暂时不可用')
    expect(errorMessage(new ApiError(400, 'something internal'))).toBe('请求未完成')
    expect(errorMessage(new Error('boom'))).toBe('请求未完成')
  })

  it.each([200, 403])('waits for native Forge session release after exchange status %i', async (exchangeStatus) => {
    const calls: Array<{ url: string; init?: RequestInit }> = []
    let releaseForgeSession!: (response: Response) => void
    const releaseResponse = new Promise<Response>((resolve) => { releaseForgeSession = resolve })
    vi.stubGlobal('fetch', vi.fn(async (url: string | URL, init?: RequestInit) => {
      calls.push({ url: String(url), init })
      if (String(url).endsWith('/api/v1/auth/sign-in/email')) return new Response(JSON.stringify({ token: 'forge-session' }), { status: 200 })
      if (String(url) === '/v1/admin/session') return new Response(JSON.stringify(exchangeStatus === 200
        ? { name: '小周', role: 'developer', source: 'forge' }
        : { error: 'account binding failed' }), { status: exchangeStatus })
      return releaseResponse
    }))
    let settled = false
    const result = signInWithForge('https://forge.example', ' dev@example.com ', 'secret').then(
      (session) => { settled = true; return { session } },
      (error: unknown) => { settled = true; return { error } },
    )
    await vi.waitFor(() => expect(calls).toHaveLength(3))
    expect(settled).toBe(false)
    expect(calls.map((call) => call.url)).toEqual([
      'https://forge.example/api/v1/auth/sign-in/email',
      '/v1/admin/session',
      'https://forge.example/api/v1/auth/sign-out',
    ])
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ email: 'dev@example.com', password: 'secret' })
    expect(calls[0].init?.credentials).toBe('omit')
    expect(JSON.parse(String(calls[1].init?.body))).toEqual({ forge_token: 'forge-session' })
    const release = calls[2].init!
    expect(release.method).toBe('POST')
    const releaseHeaders = new Headers(release.headers)
    expect(releaseHeaders.get('Authorization')).toBe('Bearer forge-session')
    expect(releaseHeaders.get('Content-Type')).toBe('application/json')
    expect(JSON.parse(String(release.body))).toEqual({})
    expect(release.credentials).toBe('omit')
    expect(release.signal).toBeInstanceOf(AbortSignal)
    releaseForgeSession(new Response(null, { status: 200 }))
    await expect(result).resolves.toMatchObject(exchangeStatus === 200
      ? { session: { name: '小周' } }
      : { error: { status: 403, code: 'account binding failed' } })
  })

  it('reports rejected Forge credentials without contacting Weave', async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 401 }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(signInWithForge('https://forge.example', 'a@b.c', 'x')).rejects.toMatchObject({ code: 'forge_credentials_rejected' })
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})
