import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError } from '../lib/api'
import { Shell } from './Shell'

vi.mock('../lib/api', async (original) => ({ ...await original<typeof import('../lib/api')>(), api: vi.fn() }))

afterEach(() => { cleanup(); vi.resetAllMocks(); window.history.replaceState(null, '', '/admin/') })

function openRun(trial: boolean) {
  window.history.replaceState(null, '', '/admin/tasks/local-run')
  vi.mocked(api).mockImplementation(async (path) => {
    if (path === '/v1/admin/tasks') return { tasks: [] }
    if (path === '/v1/admin/tasks/local-run') {
      if (trial) throw new ApiError(404, 'not_found')
      return { run_id: 'local-run', team_id: 'local-team', title: '正常任务', team_name: '本地团队', workflow_version: 1, created_at: new Date().toISOString() }
    }
    if (path === '/v1/runs/local-run/activity') return {
      run_id: 'local-run', team_id: 'local-team', development_trial: trial,
      status: 'succeeded', members: [], runtimes: [], completed_stages: 0, total_stages: 0,
    }
    if (path === '/v1/teams/local-team/development') return {
      revision: 1, published_revision: 0, trials: [], document: { name: '本地团队', objective: '', audience: [], members: [], workflows: [] },
    }
    if (path === '/v1/runtimes') return { runtimes: [] }
    if (path === '/v1/development/business-capabilities') return { available: false, capabilities: [] }
    throw new ApiError(404, 'not_found')
  })
  render(<Shell session={{ name: '开发者', role: 'developer', source: 'forge' }} onSignOut={vi.fn()} />)
}

describe('result return navigation', () => {
  it('returns a directly opened trial result to its team with the trial tab selected', async () => {
    openRun(true)
    fireEvent.click(await screen.findByRole('button', { name: '返回团队试跑' }))
    expect(window.location.pathname).toBe('/admin/teams/local-team/trial')
    await screen.findByRole('region', { name: '发起试跑' })
    expect(screen.getByRole('tab', { name: '试跑' }).getAttribute('aria-selected')).toBe('true')
    fireEvent.click(screen.getByRole('button', { name: '返回团队列表' }))
    expect(window.location.pathname).toBe('/admin/teams')
  })

  it('keeps ordinary task results returning to the run list', async () => {
    openRun(false)
    await screen.findByRole('heading', { name: '正常任务' })
    await waitFor(() => expect(screen.getByRole('button', { name: '返回运行列表' }).hasAttribute('disabled')).toBe(false))
    fireEvent.click(screen.getByRole('button', { name: '返回运行列表' }))
    expect(window.location.pathname).toBe('/admin/runs')
    await screen.findByRole('heading', { name: '运行' })
  })

  it('lands on teams and keeps the code pages folded until they are needed', async () => {
    vi.mocked(api).mockImplementation(async (path) => {
      if (path === '/v1/admin/tasks') return { tasks: [] }
      if (path === '/v1/runtimes') return { runtimes: [] }
      if (path === '/v1/environments') return { environments: [] }
      if (String(path).startsWith('/v1/teams')) return []
      throw new ApiError(404, 'not_found')
    })
    render(<Shell session={{ name: '开发者', role: 'developer', source: 'forge' }} onSignOut={vi.fn()} />)
    await screen.findByRole('heading', { name: '团队' })
    const nav = screen.getByRole('navigation', { name: '主导航' })
    expect(Array.from(nav.querySelectorAll('.nav__item')).map((item) => item.textContent)).toEqual(['团队', '运行', '集成'])
    fireEvent.click(screen.getByRole('button', { name: '代码任务' }))
    expect(Array.from(nav.querySelectorAll('.nav__item')).map((item) => item.textContent)).toEqual(['团队', '运行', '集成', '代码仓库', '节点'])
  })
})
