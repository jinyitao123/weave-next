// @vitest-environment jsdom

import { cleanup, fireEvent, render, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { CapabilityCenter } from '../src/client/CapabilityCenter.tsx'
import { zh } from '../src/client/locales.ts'

const t = makeTranslate(zh, commonZh)

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

const snapshot = {
  apps: [{ id: 'app-1', name: '订单业务服务', description: '检查订单材料', enabled: true, max_concurrent_invocations: 3 }],
  credentials: [],
  capabilities: [{ id: 'cap-1', key: 'order_material_check', name: '订单材料检查', description: '返回缺件清单', enabled: true }],
  releases: [{
    id: 'release-1', capability_id: 'cap-1', version: 1, workflow_id: 'workflow-1', workflow_version: 2,
    artifact_content_hash: 'sha256:published', enabled: true,
    execution_limits: { queue_timeout_seconds: 60, execution_timeout_seconds: 300 },
    result_policy: { exposed_fields: ['summary', 'issues'] },
  }],
  grants: [],
  invocations: [{
    invocation: {
      app_id: 'app-1', invocation_id: 'invocation-1', request_id: 'order-2026-001',
      capability_id: 'cap-1', release_version: 1,
      accepted_at: '2026-09-12T09:00:00.000Z', deadline_at: '2026-09-12T09:05:00.000Z',
    },
    execution_status: 'running', result_availability: 'available',
    result: { summary: '材料齐全', issues: [] }, cancellation_status: 'not_requested',
  }],
  publishable_workflows: [{
    workflow_id: 'workflow-1', workflow_name: '材料检查流程', workflow_version: 2,
    team_id: 'team-1', team_name: '订单审核团队',
  }],
}

describe('published capability center', () => {
  it('completes the administrator lifecycle and presents only the returned business result', async () => {
    const actions: Record<string, unknown>[] = []
    const fetcher = vi.fn<typeof fetch>(async (_input, init) => {
      if (init?.method === 'POST') {
        const action = JSON.parse(String(init.body)) as Record<string, unknown>
        actions.push(action)
        if (action.action === 'create_credential') {
          return Response.json({ secret: 'wv_app_once_only' }, { status: 201 })
        }
        return new Response(null, { status: 204 })
      }
      return Response.json(snapshot)
    })
    vi.stubGlobal('fetch', fetcher)
    const view = render(<CapabilityCenter t={t} />)

    expect(await view.findByText('1 个调用应用，1 项能力')).toBeTruthy()
    fireEvent.change(view.getByLabelText('应用名称'), { target: { value: '售后业务服务' } })
    fireEvent.change(view.getByLabelText('用途说明'), { target: { value: '检查售后材料' } })
    fireEvent.click(view.getByRole('button', { name: '创建应用' }))
    await waitFor(() => { expect(actions.some(item => item.action === 'create_app')).toBe(true) })

    fireEvent.change(view.getByLabelText('能力名称'), { target: { value: '售后材料检查' } })
    fireEvent.change(view.getByLabelText('稳定标识'), { target: { value: 'after_sales_material_check' } })
    fireEvent.change(view.getByLabelText('结果用途'), { target: { value: '返回缺件' } })
    fireEvent.click(view.getByRole('button', { name: '创建能力' }))
    await waitFor(() => { expect(actions.some(item => item.action === 'create_capability')).toBe(true) })

    fireEvent.click(view.getByText('订单业务服务'))
    fireEvent.click(view.getByRole('button', { name: '签发新密钥' }))
    fireEvent.change(view.getByLabelText('密钥名称'), { target: { value: '九月轮换' } })
    fireEvent.click(view.getByRole('button', { name: '签发并显示一次' }))
    expect(await view.findByText('wv_app_once_only')).toBeTruthy()
    fireEvent.click(view.getByRole('button', { name: '我已保存' }))
    expect(view.queryByText('wv_app_once_only')).toBeNull()

    fireEvent.click(view.getByText('订单材料检查'))
    fireEvent.click(view.getByRole('button', { name: '发布新版本' }))
    fireEvent.change(view.getByLabelText('已发布工作流'), { target: { value: 'workflow-1:2' } })
    fireEvent.change(view.getByLabelText(/^返回给应用的字段/u), { target: { value: 'summary, issues' } })
    fireEvent.click(view.getByRole('button', { name: '发布不可变版本' }))
    await waitFor(() => { expect(actions.some(item => item.action === 'publish_release')).toBe(true) })

    fireEvent.click(view.getByRole('button', { name: '授权应用' }))
    const grantDialog = view.getByRole('dialog', { name: '授权调用这个版本' })
    fireEvent.change(within(grantDialog).getByLabelText('调用应用'), { target: { value: 'app-1' } })
    fireEvent.click(within(grantDialog).getByRole('button', { name: '确认授权' }))
    await waitFor(() => { expect(actions.some(item => item.action === 'grant_release')).toBe(true) })

    fireEvent.click(view.getByText('订单材料检查 · v1'))
    expect(view.getByText(/材料齐全/u)).toBeTruthy()
    expect(view.container.textContent).not.toContain('server_private')
    fireEvent.click(view.getByRole('button', { name: '请求停止' }))
    fireEvent.change(view.getByLabelText('停止原因'), { target: { value: '业务请求已撤回' } })
    fireEvent.click(view.getByRole('button', { name: '确认请求停止' }))
    await waitFor(() => { expect(actions.some(item => item.action === 'cancel_invocation')).toBe(true) })

    expect(actions).toEqual(expect.arrayContaining([
      expect.objectContaining({ action: 'create_app', name: '售后业务服务', max_concurrent: 2 }),
      expect.objectContaining({ action: 'create_capability', key: 'after_sales_material_check' }),
      expect.objectContaining({ action: 'create_credential', app_id: 'app-1', scopes: ['invoke', 'read', 'cancel'] }),
      expect.objectContaining({ action: 'publish_release', capability_id: 'cap-1', release_version: 2, workflow_version: 2 }),
      expect.objectContaining({ action: 'grant_release', app_id: 'app-1', capability_id: 'cap-1', release_version: 1 }),
      expect.objectContaining({ action: 'cancel_invocation', invocation_id: 'invocation-1', reason: '业务请求已撤回' }),
    ]))
  })
})
