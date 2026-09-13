import { describe, expect, it, vi } from 'vitest'
import { handleWeaveCapabilityRequest } from '../src/capability-control.ts'

describe('published capability management proxy', () => {
  it('keeps the host credential private while returning the management snapshot', async () => {
    const fetcher = vi.fn<typeof fetch>(() => Promise.resolve(Response.json({ apps: [], credentials: [], capabilities: [], releases: [], grants: [], invocations: [], publishable_workflows: [] })))
    const response = await handleWeaveCapabilityRequest(
      'http://weave.test', 'host-secret', new Request('http://host/api/weave.capabilities'), fetcher,
    )
    expect(response.status).toBe(200)
    const value = await response.json()
    expect(value).toMatchObject({ apps: [], invocations: [] })
    expect(fetcher).toHaveBeenCalledWith('http://weave.test/v1/capability-management', expect.objectContaining({
      headers: expect.any(Headers),
    }))
    const init = fetcher.mock.calls[0]?.[1]
    expect((init?.headers as Headers).get('Authorization')).toBe('Bearer host-secret')
    expect(JSON.stringify(value)).not.toContain('host-secret')
  })

  it('forwards only recognized actions and preserves a one-time service credential', async () => {
    const fetcher = vi.fn<typeof fetch>(() => Promise.resolve(Response.json({
      credential: { id: 'cred-1', app_id: 'app-1', name: '新密钥' }, secret: 'wv_app_once',
    }, { status: 201 })))
    const response = await handleWeaveCapabilityRequest(
      'http://weave.test', 'host-secret', new Request('http://host/api/weave.capabilities', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({
          action: 'create_credential', app_id: 'app-1', name: '新密钥', scopes: ['invoke', 'read', 'cancel'],
        }),
      }), fetcher,
    )
    expect(response.status).toBe(201)
    expect(await response.json()).toMatchObject({ secret: 'wv_app_once' })
    const [, init] = fetcher.mock.calls[0] ?? []
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({
      action: 'create_credential', app_id: 'app-1', name: '新密钥', scopes: ['invoke', 'read', 'cancel'],
    })

    const invalid = await handleWeaveCapabilityRequest(
      'http://weave.test', 'host-secret', new Request('http://host/api/weave.capabilities', {
        method: 'POST', body: JSON.stringify({ action: 'delete_everything' }),
      }), fetcher,
    )
    expect(invalid.status).toBe(400)
    expect(fetcher).toHaveBeenCalledTimes(1)
  })

  it('fails closed when the host is disconnected or Weave rejects the action', async () => {
    const disconnected = await handleWeaveCapabilityRequest(
      'http://weave.test', '', new Request('http://host/api/weave.capabilities'), vi.fn(),
    )
    expect(disconnected.status).toBe(503)

    const rejected = await handleWeaveCapabilityRequest(
      'http://weave.test', 'host-secret', new Request('http://host/api/weave.capabilities', {
        method: 'POST', body: JSON.stringify({
          action: 'create_app', name: '订单服务', description: '', max_concurrent: 2,
        }),
      }), () => Promise.resolve(Response.json({ code: 'capability_management_conflict' }, { status: 409 })),
    )
    expect(rejected.status).toBe(409)
    expect(await rejected.json()).toEqual({ code: 'capability_conflict' })
  })
})
