import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { FeishuTeamAccessPanel } from './FeishuTeamAccess'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('team Feishu access', () => {
  it('starts closed and saves the chosen workflow and notifications on the access revision', async () => {
    const access = { enabled: false, workflowId: null, notify: { result: true, revisionRequired: true, humanReview: true, failure: true, cancelled: true }, revision: 0 }
    const puts: unknown[] = []
    vi.stubGlobal('fetch', vi.fn(async (_url: string, init: RequestInit = {}) => {
      if (init.method === 'PUT') {
        const body = JSON.parse(String(init.body))
        puts.push(body)
        return new Response(JSON.stringify({ ...body, revision: 1 }), { status: 200 })
      }
      return new Response(JSON.stringify(access), { status: 200 })
    }))
    render(<FeishuTeamAccessPanel teamId="team" workflows={[{ id: 'flow', name: '合同审核' }]} />)
    const enabled = await screen.findByRole('switch', { name: '可在飞书中使用' })
    expect(enabled.getAttribute('aria-checked')).toBe('false')
    expect((screen.getByRole('button', { name: '保存' }) as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(enabled)
    fireEvent.click(screen.getByRole('button', { name: '飞书发起时使用的流程' }))
    fireEvent.mouseDown(screen.getByRole('option', { name: '合同审核' }))
    fireEvent.click(screen.getByRole('switch', { name: '需要人工确认' }))
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(puts).toHaveLength(1))
    expect(puts[0]).toEqual({ expectedRevision: 0, enabled: true, workflowId: 'flow', notify: { result: true, revisionRequired: true, humanReview: false, failure: true, cancelled: true } })
    await waitFor(() => expect((screen.getByRole('button', { name: '保存' }) as HTMLButtonElement).disabled).toBe(true))
  })
})
