// @vitest-environment jsdom

import { cleanup, fireEvent, render, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { RuntimeCenter, RuntimeSidebarEntry } from '../src/client/RuntimeCenter.tsx'
import { WorkTaskPanel } from '../src/client/WorkTaskPanel.tsx'
import { zh } from '../src/client/locales.ts'

const t = makeTranslate(zh, commonZh)

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

function runtimeResponse(): Response {
  return Response.json({ runtimes: [{
    id: 'runtime-secret-id', name: 'analysis-codex-node', engines: ['codex'], healthStatus: 'healthy',
    engineCapabilities: [{ engine: 'codex', binaryVersion: '0.91.0', authMode: 'chatgpt' }],
    totalSlots: 3, activeSlots: 1, poolId: 'private-pool', enabled: true, online: true,
    lastHeartbeatAt: new Date().toISOString(), createdAt: new Date().toISOString(),
  }] })
}

describe('Weave runtime center', () => {
  it('opens runtime management from the global DSH sidebar', async () => {
    vi.stubGlobal('fetch', vi.fn<typeof fetch>(() => Promise.resolve(runtimeResponse())))
    const props = { wide: true, t } as unknown as Parameters<typeof RuntimeSidebarEntry>[0]
    const view = render(<RuntimeSidebarEntry {...props} />)
    expect(await view.findByText('1/1 个可用')).toBeTruthy()
    fireEvent.click(view.getByRole('button', { name: /运行节点/u }))
    expect(await view.findByRole('dialog', { name: '运行节点' })).toBeTruthy()
    expect(await view.findByText('Analysis Codex Node')).toBeTruthy()
    fireEvent.click(view.getByRole('button', { name: '关闭运行节点' }))
    expect(view.queryByRole('dialog', { name: '运行节点' })).toBeNull()
  })

  it('shows runtime capacity and management without exposing identifiers', async () => {
    const fetcher = vi.fn<typeof fetch>(() => Promise.resolve(runtimeResponse()))
    vi.stubGlobal('fetch', fetcher)
    const view = render(<RuntimeCenter t={t} />)

    expect(await view.findByText('Analysis Codex Node')).toBeTruthy()
    expect(view.container.textContent).toContain('Codex')
    expect(view.container.textContent).toContain('ChatGPT 账号')
    expect(view.container.textContent).toContain('0.91.0')
    expect(view.container.textContent).toContain('1/3 个位置占用')
    expect(view.container.textContent).not.toContain('runtime-secret-id')
    expect(view.container.textContent).not.toContain('private-pool')
    fireEvent.click(view.getByText('Analysis Codex Node'))
    expect(view.getByRole('button', { name: '管理' })).toBeTruthy()
    expect(view.getByRole('button', { name: '移除' })).toBeTruthy()
    expect(view.container.textContent).toContain('Private Pool')
  })

  it('creates a node and presents its one-time token only after creation', async () => {
    const fetcher = vi.fn<typeof fetch>(async (_input, init) => {
      if (init?.method === 'POST') return Response.json({ id: 'runtime-2', name: '办公室 Mac', token: 'rtk_once_only' }, { status: 201 })
      return runtimeResponse()
    })
    vi.stubGlobal('fetch', fetcher)
    const view = render(<RuntimeCenter t={t} />)
    await view.findByText('Analysis Codex Node')
    fireEvent.click(view.getByRole('button', { name: '添加节点' }))
    fireEvent.change(view.getByLabelText('节点名称'), { target: { value: '办公室 Mac' } })
    fireEvent.click(view.getByRole('button', { name: '创建并获取令牌' }))

    expect(await view.findByText('rtk_once_only')).toBeTruthy()
    await waitFor(() => { expect(fetcher).toHaveBeenCalledTimes(3) })
    const createInit = fetcher.mock.calls.find(([, init]) => init?.method === 'POST')?.[1]
    expect(typeof createInit?.body === 'string' ? JSON.parse(createInit.body) : null)
      .toEqual({ action: 'create', name: '办公室 Mac' })
  })
})

describe('Weave work scene presentation', () => {
  it('keeps technical identifiers and raw enums out of the primary task view', async () => {
    vi.stubGlobal('fetch', vi.fn<typeof fetch>(() => Promise.resolve(Response.json({ runtimes: [] }))))
    const projection = {
      brief: '完成一次跨主题风险分析', clientRequestId: 'request-secret', runId: 'run-secret',
      teamId: 'team-secret', teamName: 'daily-intelligence', workflowName: 'daily-intelligence-v1-workflow', status: 'completed',
      completedStages: 1, totalStages: 1, latestStage: 'edit', stages: [], teamCandidates: [], corrections: [], humanTaskCount: 0,
      members: [{ agentId: 'member-secret', name: '汇总员', duty: '形成最终成果', role: 'lead', status: 'completed', runtime: 'daily-codex-runtime · codex', stages: [{
        nodeId: 'edit', name: 'edit', status: 'completed', inputs: [{ name: 'source_bundle', expectedType: 'json', source: 'upstream_node', nodeId: 'research', path: '/private/source.json', summary: '前序研究结果' }],
        outputRefs: ['deliverable-secret'], startedAt: '', completedAt: '', durationMs: 500, toolCalls: 1,
        tools: [{ callId: 'call-secret', name: 'exec_command', status: 'ok', startedAt: '', completedAt: '', input: 'npm test', output: 'PASS' }],
        failureClass: '', failureReason: '', retryable: false,
      }] }],
      runtimes: [{ name: 'daily-codex-runtime', detail: 'codex · openai · gpt-5.6', status: 'completed' }],
      deliverableCount: 1, deliverables: [{ id: 'deliverable-secret', title: '风险信号包', kind: 'final', contentType: 'text/html', preview: '<h1>完成</h1>', content: '<h1>完成</h1>', truncated: false, createdAt: new Date().toISOString() }],
      blocker: 'none', attempts: [], pendingAction: null, actionError: '', completeness: { run: 'complete' },
      startedAt: '', finishedAt: '', tokensIn: 0, tokensOut: 0, costUSD: 0, outcome: 'unrated', outcomeNote: '', observedAt: Date.now(), updatedAt: Date.now(),
    }
    const props = {
      useChat: (select: (value: unknown) => unknown) => select({ nodes: { values: () => [] } }),
      useProjection: () => projection,
      useSessions: (select: (value: unknown) => unknown) => select({ byId: { session: { title: '风险分析任务' } } }),
      sessionId: 'session', openDetails: vi.fn(), t,
    }
    const componentProps = props as unknown as Parameters<typeof WorkTaskPanel>[0]
    const view = render(<WorkTaskPanel {...componentProps} />)
    expect(view.container.textContent).toContain('Daily Intelligence')
    expect(view.container.textContent).toContain('汇总交付')
    expect(view.container.textContent).toContain('网页')
    expect(view.container.textContent).toContain('Daily Codex Runtime')
    expect(view.container.textContent).not.toContain('text/html')
    expect(view.container.textContent).not.toContain('exec_command')
    expect(view.container.textContent).not.toContain('/private/source.json')
    const diagnostics = Array.from(view.container.querySelectorAll('details')).find(item => item.querySelector('summary')?.textContent === '运行识别信息')
    expect(diagnostics?.open).toBe(false)
    expect(diagnostics?.textContent).toContain('run-secret')
    expect(diagnostics?.textContent).toContain('daily-intelligence-v1-workflow')
  })
})
