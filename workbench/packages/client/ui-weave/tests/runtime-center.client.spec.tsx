// @vitest-environment jsdom

import { useSyncExternalStore } from 'react'
import { createWorkTaskViewStore } from '../src/client/view-store.ts'
import { cleanup, fireEvent, render, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { RuntimeCenter, RuntimeSettingsSection } from '../src/client/RuntimeCenter.tsx'
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
  it('renders runtime management as a Settings section', async () => {
    vi.stubGlobal('fetch', vi.fn<typeof fetch>(() => Promise.resolve(runtimeResponse())))
    const props = { close: vi.fn(), t } as unknown as Parameters<typeof RuntimeSettingsSection>[0]
    const view = render(<RuntimeSettingsSection {...props} />)
    expect(await view.findByText('1/1 个可用')).toBeTruthy()
    expect(await view.findByText('Analysis Codex Node')).toBeTruthy()
    expect(view.getByRole('region', { name: '运行节点' })).toBeTruthy()
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
    const writeText = vi.fn<(value: string) => Promise<void>>(() => Promise.resolve())
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    const fetcher = vi.fn<typeof fetch>(async (_input, init) => {
      if (init?.method === 'POST') return Response.json({
        id: 'runtime-2', name: '办公室 Mac', token: 'rtk_once_only', serverUrl: 'https://weave.example.com',
      }, { status: 201 })
      return runtimeResponse()
    })
    vi.stubGlobal('fetch', fetcher)
    const view = render(<RuntimeCenter t={t} />)
    await view.findByText('Analysis Codex Node')
    fireEvent.click(view.getByRole('button', { name: '添加节点' }))
    fireEvent.change(view.getByLabelText('节点名称'), { target: { value: '办公室 Mac' } })
    fireEvent.click(view.getByRole('button', { name: '创建并获取令牌' }))

    expect(await view.findByText('rtk_once_only')).toBeTruthy()
    expect(view.container.textContent).toContain("weave runtime --server 'https://weave.example.com' --runtime-token 'rtk_once_only'")
    expect(view.container.textContent).not.toContain('<WEAVE')
    fireEvent.click(view.getByRole('button', { name: '复制连接命令' }))
    await waitFor(() => { expect(writeText).toHaveBeenCalledWith("weave runtime --server 'https://weave.example.com' --runtime-token 'rtk_once_only'") })
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
      members: [{ agentId: 'member-secret', name: '汇总员', duty: '形成最终成果', role: 'lead', status: 'completed', runtime: 'daily-codex-runtime · codex', updateMode: 'on_completion', stages: [{
        nodeId: 'edit', name: 'edit', status: 'completed', inputs: [{ name: 'source_bundle', expectedType: 'json', source: 'upstream_node', nodeId: 'research', path: '/private/source.json', summary: '前序研究结果' }],
        outputRefs: ['deliverable-secret'], startedAt: '', completedAt: '', durationMs: 500, toolCalls: 1,
        tools: [{ callId: 'call-secret', name: 'exec_command', status: 'ok', startedAt: '', completedAt: '', input: 'npm test', output: 'PASS' }],
        failureClass: '', failureReason: '', retryable: false,
        currentTaskId: '', publicUpdatesState: 'unavailable', publicUpdatesTruncated: false, publicUpdates: [],
      }] }],
      runtimes: [{ name: 'daily-codex-runtime', detail: 'codex · openai · gpt-5.6', status: 'completed' }],
      deliverableCount: 1, deliverables: [{ id: 'deliverable-secret', title: '风险信号包', kind: 'final', contentType: 'text/html', preview: '<h1>完成</h1>', content: '<h1>完成</h1>', truncated: false, createdAt: new Date().toISOString() }],
      blocker: 'none', attempts: [], pendingAction: null, actionError: '', completeness: { run: 'complete' },
      startedAt: '', finishedAt: '', tokensIn: 0, tokensOut: 0, costUSD: 0, outcome: 'unrated', outcomeNote: '', observedAt: Date.now(), updatedAt: Date.now(),
    }
    const taskView = createWorkTaskViewStore().create()
    const props = {
      useStore: (select: (value: unknown) => unknown) => select(useSyncExternalStore(
        listener => taskView.subscribe(listener), () => taskView.getSnapshot(),
      )), actions: taskView.actions,
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
    expect(view.container.textContent).not.toContain('npm test')
    const diagnostics = Array.from(view.container.querySelectorAll('details')).find(item => item.querySelector('summary')?.textContent === '运行识别信息')
    expect(diagnostics?.open).toBe(false)
    expect(diagnostics?.textContent).toContain('run-secret')
    expect(diagnostics?.textContent).toContain('daily-intelligence-v1-workflow')

    fireEvent.click(view.getByRole('tab', { name: '进展' }))
    fireEvent.click(view.getByRole('button', { name: '查看 汇总员 的工作' }))
    expect(view.getByRole('button', { name: '返回团队总览' })).toBeTruthy()
    expect(view.getByRole('region', { name: '执行记录' })).toBeTruthy()
    expect(view.container.textContent).toContain('npm test')
    expect(view.container.textContent).toContain('PASS')
    expect(view.container.textContent).not.toContain('exec_command')
    expect(view.container.textContent).not.toContain('/private/source.json')

    fireEvent.click(view.getByRole('button', { name: '返回团队总览' }))
    expect(view.container.textContent).toContain('团队成员')
    expect(view.container.textContent).not.toContain('npm test')
  })

  it('offers exact current-stage recovery when an external runtime disconnects', async () => {
    const retryStage = vi.fn<(runId: string, nodeId: string) => Promise<string | null>>(() => Promise.resolve(null))
    const projection = {
      brief: '完成多团队工程论证', clientRequestId: 'request-1', runId: 'run-1', teamId: 'team-1',
      teamName: '工程论证团队', workflowName: 'research-workflow', status: 'waiting', completedStages: 3,
      waitKind: 'runtime', waitNodeId: 'physics-review',
      totalStages: 5, latestStage: 'physics-review', stages: [], teamCandidates: [], corrections: [], humanTaskCount: 0,
      members: [{ agentId: 'member-1', name: '物理复核员', duty: '复核关键约束', role: 'worker', status: 'failed', runtime: 'remote-codex · codex', updateMode: 'on_completion', stages: [{
        nodeId: 'physics-review', name: '物理约束复核', status: 'failed', inputs: [], outputRefs: [],
        startedAt: '2026-09-05T08:00:00.000Z', completedAt: '2026-09-05T08:03:00.000Z', durationMs: 180_000,
        currentTaskId: '', publicUpdatesState: 'unavailable', publicUpdatesTruncated: false, publicUpdates: [],
        toolCalls: 2, tools: [], failureClass: 'infrastructure', failureReason: 'stream disconnected', retryable: true,
      }] }],
      runtimes: [{ name: 'remote-codex', detail: 'codex · openai', status: 'waiting' }], deliverableCount: 2,
      deliverables: [{ id: 'prior-output', title: '已完成的证据包', kind: 'stage', contentType: 'text/markdown', preview: '保留', content: '保留', truncated: false, createdAt: '2026-09-05T08:01:00.000Z' }],
      blocker: 'none', attempts: [], pendingAction: null, actionError: '', completeness: { run: 'complete' },
      startedAt: '', finishedAt: '', tokensIn: 0, tokensOut: 0, costUSD: 0, outcome: 'unrated', outcomeNote: '', observedAt: Date.now(), updatedAt: Date.now(),
    }
    const taskView = createWorkTaskViewStore().create()
    const props = {
      useStore: (select: (value: unknown) => unknown) => select(useSyncExternalStore(
        listener => taskView.subscribe(listener), () => taskView.getSnapshot(),
      )), actions: taskView.actions,
      useChat: (select: (value: unknown) => unknown) => select({ nodes: { values: () => [] } }),
      useProjection: () => projection,
      useSessions: (select: (value: unknown) => unknown) => select({ byId: { session: { title: '工程论证任务' } } }),
      sessionId: 'session', openDetails: vi.fn(), retryStage, t,
    }
    const view = render(<WorkTaskPanel {...props as unknown as Parameters<typeof WorkTaskPanel>[0]} />)

    expect(view.getByRole('region', { name: '运行节点断开，工作已保留' })).toBeTruthy()
    expect(view.container.textContent).toContain('无需重新执行整个团队')
    expect(view.container.textContent).toContain('其他已完成阶段和产物会保留')
    fireEvent.click(view.getByRole('button', { name: '从当前阶段重试' }))
    fireEvent.click(view.getByRole('button', { name: '确认重试' }))
    await waitFor(() => { expect(retryStage).toHaveBeenCalledWith('run-1', 'physics-review') })
  })

  it('does not offer stage recovery for professional-work failures', () => {
    const projection = {
      brief: '完成分析', clientRequestId: 'request-2', runId: 'run-2', teamId: 'team-2', teamName: '分析团队',
      workflowName: 'analysis-workflow', status: 'failed', completedStages: 0, totalStages: 1, latestStage: 'analysis',
      stages: [], teamCandidates: [], corrections: [], humanTaskCount: 0,
      members: [{ agentId: 'member-2', name: '分析员', duty: '', role: 'worker', status: 'failed', runtime: 'local', stages: [{
        nodeId: 'analysis', name: '分析', status: 'failed', inputs: [], outputRefs: [], startedAt: '', completedAt: '', durationMs: 0,
        toolCalls: 0, tools: [], failureClass: 'work', failureReason: 'insufficient evidence', retryable: true,
      }] }], runtimes: [], deliverableCount: 0, deliverables: [], blocker: 'failed', attempts: [], pendingAction: null,
      actionError: '', completeness: { run: 'complete' }, startedAt: '', finishedAt: '', tokensIn: 0, tokensOut: 0,
      costUSD: 0, outcome: 'unrated', outcomeNote: '', observedAt: Date.now(), updatedAt: Date.now(),
    }
    const taskView = createWorkTaskViewStore().create()
    const props = {
      useStore: (select: (value: unknown) => unknown) => select(useSyncExternalStore(
        listener => taskView.subscribe(listener), () => taskView.getSnapshot(),
      )), actions: taskView.actions,
      useChat: (select: (value: unknown) => unknown) => select({ nodes: { values: () => [] } }), useProjection: () => projection,
      useSessions: (select: (value: unknown) => unknown) => select({ byId: { session: { title: '分析任务' } } }),
      sessionId: 'session', openDetails: vi.fn(), retryStage: vi.fn(), t,
    }
    const view = render(<WorkTaskPanel {...props as unknown as Parameters<typeof WorkTaskPanel>[0]} />)
    expect(view.queryByRole('button', { name: '从当前阶段重试' })).toBeNull()
  })
})
