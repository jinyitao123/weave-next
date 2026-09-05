import { describe, expect, it, vi } from 'vitest'
import { handleWeaveRuntimeRequest, resolveRuntimeServerUrl } from '../src/runtime-control.ts'

describe('runtime server URL resolution', () => {
  const interfaces = {
    en0: [{ address: '192.168.1.9', netmask: '255.255.255.0', family: 'IPv4' as const, mac: '', internal: false, cidr: '192.168.1.9/24' }],
    vpn0: [{ address: '198.18.0.1', netmask: '255.254.0.0', family: 'IPv4' as const, mac: '', internal: false, cidr: '198.18.0.1/15' }],
  }

  it('uses a configured public URL before the Host API address', () => {
    expect(resolveRuntimeServerUrl('http://weave:8080', 'https://weave.example.com/', interfaces))
      .toBe('https://weave.example.com')
  })

  it('replaces loopback and container-only hosts with the preferred LAN IPv4', () => {
    expect(resolveRuntimeServerUrl('http://127.0.0.1:18081', undefined, interfaces)).toBe('http://192.168.1.9:18081')
    expect(resolveRuntimeServerUrl('http://weave:8080', undefined, interfaces)).toBe('http://192.168.1.9:8080')
  })

  it('keeps an already reachable API URL', () => {
    expect(resolveRuntimeServerUrl('https://weave.example.com', undefined, interfaces)).toBe('https://weave.example.com')
    expect(resolveRuntimeServerUrl('http://10.0.0.8:8080', undefined, interfaces)).toBe('http://10.0.0.8:8080')
  })
})

describe('Workbench runtime management proxy', () => {
  it('returns product runtime facts without credentials or backend failures', async () => {
    const fetcher = vi.fn<typeof fetch>(async (_input, init) => {
      expect(new Headers(init?.headers).get('Authorization')).toBe('Bearer host-secret')
      return Response.json({ runtimes: [{
        id: 'runtime-1', name: '分析节点', engines: ['codex'], health_status: 'healthy',
        engine_capabilities: { codex: {
          engine: 'codex', binary_path: '/private/bin/codex', binary_version: '0.91.0',
          auth_mode: 'chatgpt', protocol_version: '1', endpoint_class: 'openai',
        } },
        total_slots: 3, active_slots: 1, pool_id: 'fallback', enabled: true, online: true,
        last_heartbeat_at: '2026-09-03T10:00:00Z', created_at: '2026-09-01T10:00:00Z',
        binary_path: '/private/bin', last_failure_reason: 'private failure',
      }] })
    })
    const response = await handleWeaveRuntimeRequest(
      'http://weave.test', 'host-secret', new Request('http://host/api/weave.runtimes'), fetcher,
    )
    expect(response.status).toBe(200)
    const text = await response.text()
    expect(JSON.parse(text)).toMatchObject({ runtimes: [{
      name: '分析节点', engines: ['codex'], activeSlots: 1,
      engineCapabilities: [{ engine: 'codex', binaryVersion: '0.91.0', authMode: 'chatgpt' }],
    }] })
    expect(text).not.toContain('host-secret')
    expect(text).not.toContain('/private/bin')
    expect(text).not.toContain('private failure')
  })

  it('forwards create and configure actions while keeping the host key private', async () => {
    const fetcher = vi.fn<typeof fetch>(async (input, init) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      if (url.endsWith('/v1/runtimes') && init?.method === 'POST') {
        expect(init.body).toBe(JSON.stringify({ name: '新节点' }))
        return Response.json({ id: 'runtime-2', name: '新节点', token: 'rtk_once' }, { status: 201 })
      }
      expect(url).toContain('/v1/runtimes/runtime-2')
      expect(init?.method).toBe('PUT')
      return new Response(null, { status: 204 })
    })
    const created = await handleWeaveRuntimeRequest('http://weave.test', 'host-secret', new Request('http://host/api/weave.runtimes', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'create', name: '新节点' }),
    }), fetcher, 'https://weave.example.com')
    expect(await created.json()).toEqual({ id: 'runtime-2', name: '新节点', token: 'rtk_once', serverUrl: 'https://weave.example.com' })
    const configured = await handleWeaveRuntimeRequest('http://weave.test', 'host-secret', new Request('http://host/api/weave.runtimes', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action: 'configure', id: 'runtime-2', name: '主分析节点', poolId: 'fallback' }),
    }), fetcher)
    expect(configured.status).toBe(204)
    expect(fetcher).toHaveBeenCalledTimes(2)
  })

  it('does not expose a raw Weave error or operate without a host key', async () => {
    const fetcher = vi.fn<typeof fetch>(async () => Response.json({ error: 'database internals' }, { status: 500 }))
    const absent = await handleWeaveRuntimeRequest('http://weave.test', '', new Request('http://host/api/weave.runtimes'), fetcher)
    expect(absent.status).toBe(503)
    expect(fetcher).not.toHaveBeenCalled()
    const failed = await handleWeaveRuntimeRequest('http://weave.test', 'key', new Request('http://host/api/weave.runtimes'), fetcher)
    expect(await failed.text()).not.toContain('database internals')
  })
})
