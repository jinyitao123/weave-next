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

  it('exchanges the Forge token once and then releases the Forge session', async () => {
    const calls: Array<{ url: string; init?: RequestInit }> = []
    vi.stubGlobal('fetch', vi.fn(async (url: string | URL, init?: RequestInit) => {
      calls.push({ url: String(url), init })
      if (String(url).endsWith('/api/v1/auth/sign-in/email')) return new Response(JSON.stringify({ token: 'forge-session' }), { status: 200 })
      if (String(url) === '/v1/admin/session') return new Response(JSON.stringify({ name: '小周', role: 'developer', source: 'forge' }), { status: 200 })
      return new Response(null, { status: 200 })
    }))
    const session = await signInWithForge('https://forge.example', ' dev@example.com ', 'secret')
    expect(session.name).toBe('小周')
    expect(calls.map((call) => call.url)).toEqual([
      'https://forge.example/api/v1/auth/sign-in/email',
      '/v1/admin/session',
      'https://forge.example/api/v1/auth/sign-out',
    ])
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ email: 'dev@example.com', password: 'secret' })
    expect(calls[0].init?.credentials).toBe('omit')
    expect(JSON.parse(String(calls[1].init?.body))).toEqual({ forge_token: 'forge-session' })
  })

  it('reports rejected Forge credentials without contacting Weave', async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 401 }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(signInWithForge('https://forge.example', 'a@b.c', 'x')).rejects.toMatchObject({ code: 'forge_credentials_rejected' })
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
})
