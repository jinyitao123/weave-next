import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { AdminSession } from '../lib/api'
import { TeamsPage } from './TeamsPage'

const developer: AdminSession = { name: '开发者', role: 'developer', source: 'forge' }
const pkg = { format: 'weave-team-package', version: 1, team: { name: '合同复核团队', objective: '复核合同条款' }, members: [], workflows: [] }

type Call = { url: string; method: string; body?: Record<string, unknown> }
function serve(imported: unknown) {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? 'GET'
    calls.push({ url, method, body: init.body ? JSON.parse(String(init.body)) : undefined })
    if (url.endsWith('/development/teams/import')) return new Response(JSON.stringify(imported), { status: 201 })
    return new Response('[]', { status: 200 })
  }))
  return calls
}

const choose = (text: string) => {
  const input = document.querySelector('input[type="file"]') as HTMLInputElement
  fireEvent.change(input, { target: { files: [new File([text], 'team.weave-team.json', { type: 'application/json' })] } })
}

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('team import', () => {
  it('refuses a file that is not a team package before sending it', async () => {
    const calls = serve({})
    render(<TeamsPage session={developer} navigate={() => {}} />)
    fireEvent.click(await screen.findByRole('button', { name: '导入团队' }))
    choose('{"name":"别的东西"}')
    expect(await screen.findByText('这个文件不是团队导出文件')).toBeTruthy()
    expect((screen.getByRole('button', { name: '导入' }) as HTMLButtonElement).disabled).toBe(true)
    expect(calls.some((call) => call.method === 'POST')).toBe(false)
  })

  it('sends the package once and shows what was adjusted before opening the team', async () => {
    const calls = serve({ team_id: 'team-new', name: '合同复核团队', revision: 2, adjustments: ['“审核员”的模型“source-only-model”在本工作区不可用，已清空'] })
    const navigate = vi.fn()
    render(<TeamsPage session={developer} navigate={navigate} />)
    fireEvent.click(await screen.findByRole('button', { name: '导入团队' }))
    choose(JSON.stringify(pkg))
    expect(await screen.findByText('（team.weave-team.json）')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '导入' }))
    expect(await screen.findByText('“审核员”的模型“source-only-model”在本工作区不可用，已清空')).toBeTruthy()
    const post = calls.find((call) => call.method === 'POST')
    expect(post?.body?.package).toEqual(pkg)
    expect(typeof post?.body?.request_id).toBe('string')
    expect(navigate).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '打开团队' }))
    await waitFor(() => expect(navigate).toHaveBeenCalledWith('/teams/team-new'))
  })
})
