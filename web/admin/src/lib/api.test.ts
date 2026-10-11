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

  it('opens the console session with the Forge session an approval released', async () => {
    const fetchMock = vi.fn(async () => new Response(JSON.stringify({ name: '小周', role: 'developer', source: 'forge' }), { status: 200 }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(signInWithForge('forge-session')).resolves.toMatchObject({ name: '小周' })
    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe('/v1/admin/session')
    expect(JSON.parse(String(init.body))).toEqual({ forge_token: 'forge-session' })
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('explains each way Forge can decline to confirm', () => {
    expect(errorMessage(new ApiError(404, 'forge_approval_unavailable'))).toContain('还没有启用浏览器确认登录')
    expect(errorMessage(new ApiError(400, 'forge_client_refused'))).toContain('不接受 Weave 管理端登录')
    expect(errorMessage(new ApiError(400, 'forge_approval_denied'))).toBe('已在 Forge 页面拒绝')
    expect(errorMessage(new ApiError(400, 'forge_approval_expired'))).toBe('确认已过期，请重新开始')
  })
})
