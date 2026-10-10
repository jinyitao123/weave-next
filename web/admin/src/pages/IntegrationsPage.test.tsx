import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { AdminSession } from '../lib/api'
import type { FeishuApp } from '../lib/feishu'
import { IntegrationsPage } from './IntegrationsPage'

const admin: AdminSession = { name: '管理员', role: 'admin', source: 'forge' }
const stored: FeishuApp = { source: 'workspace', appId: 'cli_app', tenantKey: 'tenant', secrets: { appSecret: true, verificationToken: true, encryptKey: true },
  callbackPath: '/v1/integrations/feishu/events/0123456789abcdef0123456789abcdef', revision: 3, boundEmployees: 2 }
const none: FeishuApp = { source: 'none', secrets: { appSecret: false, verificationToken: false, encryptKey: false }, revision: 0, boundEmployees: 0 }

type Call = { url: string; method: string; body?: Record<string, unknown> }
function serve(app: FeishuApp, check?: { status: number; body: unknown }) {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? 'GET'
    calls.push({ url, method, body: init.body ? JSON.parse(String(init.body)) : undefined })
    if (url.endsWith('/check')) return new Response(JSON.stringify(check?.body ?? { ok: true }), { status: check?.status ?? 200 })
    return new Response(JSON.stringify(app), { status: 200 })
  }))
  return calls
}

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('integrations page', () => {
  it('asks for every secret before the first save', async () => {
    const calls = serve(none)
    render(<IntegrationsPage session={admin} />)
    fireEvent.change(await screen.findByRole('textbox', { name: '应用 ID' }), { target: { value: 'cli_app' } })
    fireEvent.change(screen.getByRole('textbox', { name: '租户标识' }), { target: { value: 'tenant' } })
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText('首次保存或更换应用时需要填写全部三项密钥')).toBeTruthy()
    expect(calls.some((call) => call.method === 'PUT')).toBe(false)
  })

  it('keeps saved secrets when they are left empty and shows the callback address', async () => {
    const calls = serve(stored)
    render(<IntegrationsPage session={admin} />)
    expect(await screen.findByText(`${window.location.origin}${stored.callbackPath}`)).toBeTruthy()
    expect((screen.getByLabelText('App Secret') as HTMLInputElement).placeholder).toBe('已保存，留空保持不变')
    fireEvent.change(screen.getByRole('textbox', { name: '租户标识' }), { target: { value: 'tenant-2' } })
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(calls.find((call) => call.method === 'PUT')).toBeTruthy())
    expect(calls.find((call) => call.method === 'PUT')?.body).toEqual({ expectedRevision: 3, appId: 'cli_app', tenantKey: 'tenant-2' })
  })

  it('reports an unreachable provider from the check', async () => {
    serve(stored, { status: 503, body: { ok: false, reason: 'unreachable' } })
    render(<IntegrationsPage session={admin} />)
    fireEvent.click(await screen.findByRole('button', { name: '检查连接' }))
    expect(await screen.findByText('无法连接飞书')).toBeTruthy()
  })

  it('is read-only for developers', async () => {
    serve(stored)
    render(<IntegrationsPage session={{ ...admin, role: 'developer' }} />)
    expect((await screen.findByRole('textbox', { name: '应用 ID' }) as HTMLInputElement).disabled).toBe(true)
    expect(screen.queryByRole('button', { name: '保存' })).toBeNull()
    expect(screen.queryByRole('button', { name: '删除' })).toBeNull()
  })
})
