// @vitest-environment jsdom

import { useSyncExternalStore } from 'react'
import { cleanup, fireEvent, render, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import type { ChatConversationViewNode } from '@deepseek-ai/dsh-client-ui-chat/client'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { WorkTaskConversationCard, WorkTaskHeader, WorkTaskPanel } from '../src/client/WorkTaskPanel.tsx'
import { workTaskModel, type WorkTaskProjection } from '../src/client/work-task-model.ts'
import { createWorkTaskViewStore } from '../src/client/view-store.ts'
import { zh } from '../src/client/locales.ts'

const t = makeTranslate(zh, commonZh)
let viewStore = createWorkTaskViewStore().create('test')
beforeEach(() => { Element.prototype.scrollIntoView = vi.fn(); localStorage.clear(); viewStore = createWorkTaskViewStore().create('test') })
afterEach(cleanup)

function task(overrides: Partial<WorkTaskProjection> = {}): WorkTaskProjection {
  return {
    ...workTaskModel([]), runId: 'run-1', clientRequestId: 'request-1', teamId: 'team-1', teamName: '研究团队',
    updatedAt: 1, observedAt: Date.now(), status: 'waiting', waitKind: 'runtime', waitNodeId: 'review',
    members: [{ agentId: 'reviewer', name: '复核员', duty: '', role: 'worker', status: 'failed', runtime: 'Codex', updateMode: 'on_completion', stages: [{
      nodeId: 'review', name: '复核', status: 'failed', inputs: [], outputRefs: [], startedAt: '', completedAt: '',
      durationMs: 0, toolCalls: 0, tools: [], failureClass: 'infrastructure', failureReason: 'connection interrupted', retryable: true, publicUpdates: [], publicUpdatesTruncated: false, publicUpdatesState: 'unavailable', currentTaskId: '',
    }] }],
    ...overrides,
  }
}

function props(projection: WorkTaskProjection): Parameters<typeof WorkTaskPanel>[0] {
  return {
    useStore: (select: (value: unknown) => unknown) => select(useSyncExternalStore(
      listener => viewStore.subscribe(listener), () => viewStore.getSnapshot(),
    )), actions: viewStore.actions,
    useChat: (select: (value: unknown) => unknown) => select({ nodes: { values: () => [] } }),
    useProjection: () => projection,
    useSessions: (select: (value: unknown) => unknown) => select({ byId: { session: { title: '研究任务' } } }),
    sessionId: 'session', openDetails: vi.fn(), retryStage: vi.fn(), t,
  } as unknown as Parameters<typeof WorkTaskPanel>[0]
}

function recordedTool(name: string, value: unknown, seq: number): ChatConversationViewNode {
  return {
    key: `tool:${seq}`, id: `${seq}`, target: 'chat', anchorSeq: seq,
    location: { kind: 'session' }, visibility: 'visible', kind: 'tool-call',
    data: { root: {
      kind: 'tool-result', seq, time: seq, callId: `call-${seq}`,
      call: { name, argsRaw: '{}' }, callTime: seq - 0.5,
      content: [{ type: 'text', text: JSON.stringify(value) }], isError: false, subCalls: [],
    } },
  }
}

describe('Workbench recovery and delivery facts', () => {
  it.each(['fanout', 'runtime'] as const)('explains a %s interruption until the runtime acknowledges stopping, then offers retry', async (waitKind) => {
    const member = task().members[0]!
    const pending = task({ waitKind, members: [{ ...member, stages: [{ ...member.stages[0]!, retryable: false,
      failureReason: 'Reconnect the runtime and wait for the previous execution to confirm it has stopped.',
    }] }] })
    const ready = task({ waitKind, members: [{ ...member, stages: [{ ...member.stages[0]!, retryable: true,
      failureReason: 'The execution environment stopped before this stage could finish.',
    }] }] })
    const help = '请先恢复运行节点的连接，等待原执行确认停止；确认后会显示重试入口，已完成的工作会保留。'
    const retryStage = vi.fn().mockResolvedValue(null)
    const card = render(<WorkTaskConversationCard
      {...props(pending) as unknown as Parameters<typeof WorkTaskConversationCard>[0]} retryStage={retryStage} />)
    expect(card.getByText('等待运行节点确认停止')).toBeTruthy()
    expect(card.getByText('复核员 · 复核')).toBeTruthy()
    expect(card.getByText(help)).toBeTruthy()
    expect(card.container.querySelector('[data-executing]')).toBeNull()
    expect(card.container.querySelector('[data-weave-task-receipt]')?.textContent).toMatchInlineSnapshot('"等待运行节点确认停止复核员 · 复核请先恢复运行节点的连接，等待原执行确认停止；确认后会显示重试入口，已完成的工作会保留。查看进展与成果"')
    expect(card.queryByRole('button', { name: /重试/u })).toBeNull()
    expect(card.queryByText(/当前状态不支持单独恢复/u)).toBeNull()
    card.rerender(<WorkTaskConversationCard
      {...props(ready) as unknown as Parameters<typeof WorkTaskConversationCard>[0]} retryStage={retryStage} />)
    expect(card.queryByText(help)).toBeNull()
    fireEvent.click(card.getByRole('button', { name: '从当前阶段重试' }))
    expect(retryStage).not.toHaveBeenCalled()
    fireEvent.click(card.getByRole('button', { name: '确认重试' }))
    await waitFor(() => { expect(retryStage).toHaveBeenCalledWith('run-1', 'review') })
    card.unmount()

    const scene = render(<WorkTaskPanel {...props(pending)} />)
    expect(scene.getByText('等待运行节点确认停止')).toBeTruthy()
    expect(scene.getByText(help)).toBeTruthy()
    fireEvent.click(scene.getByRole('button', { name: '查看 复核员 的工作' }))
    expect(scene.getByRole('region', { name: '执行记录' }).textContent).toContain(help)
    expect(scene.queryByRole('button', { name: /重试/u })).toBeNull()
    expect(scene.queryByText(/当前状态不支持单独恢复/u)).toBeNull()
    scene.rerender(<WorkTaskPanel {...props(ready)} />)
    expect(scene.queryByText(help)).toBeNull()
    expect(scene.getByRole('button', { name: '只重试这个阶段' })).toBeTruthy()
  })

  it.each([
    { status: 'running' as const }, { waitKind: 'human' as const }, { waitNodeId: 'another-stage' },
  ])('does not turn an old stop-confirmation reason into the current wait: %j', (overrides) => {
    const member = task().members[0]!
    const projection = task({ ...overrides, members: [{ ...member, stages: [{ ...member.stages[0]!, retryable: false,
      failureReason: 'Reconnect the runtime and wait for the previous execution to confirm it has stopped.',
    }] }] })
    const card = render(<WorkTaskConversationCard {...props(projection) as unknown as Parameters<typeof WorkTaskConversationCard>[0]} />)
    expect(card.queryByText('等待运行节点确认停止')).toBeNull()
    expect(card.queryByText(/请先恢复运行节点的连接/u)).toBeNull()
    expect(card.queryByRole('button', { name: /重试/u })).toBeNull()
  })

  it.each(['running', 'waiting'] as const)('marks observed member activity while %s without inventing public output', (status) => {
    const member = task().members[0]!
    const stage = { ...member.stages[0]!, status: 'running' as const, failureClass: '' as const,
      failureReason: '', retryable: false }
    const projection = task({ status, waitKind: status === 'waiting' ? 'fanout' : '',
      members: [{ ...member, status: 'running', stages: [stage] }] })
    const header = render(<WorkTaskHeader {...props(projection)} />)
    expect(header.container.querySelector('[data-executing]')).toBeTruthy()
    header.unmount()
    const card = render(<WorkTaskConversationCard {...props(projection) as unknown as Parameters<typeof WorkTaskConversationCard>[0]} />)
    expect(card.getByText('复核员 正在执行')).toBeTruthy()
    expect(card.container.querySelector('[data-executing]')).toBeTruthy()
    expect(card.queryByRole('log')).toBeNull()
    card.unmount()
    const scene = render(<WorkTaskPanel {...props(projection)} />)
    const memberButton = scene.getByRole('button', { name: '查看 复核员 的工作' })
    expect(memberButton.querySelector('[data-executing]')).toBeTruthy()
    fireEvent.click(memberButton)
    expect(scene.queryByRole('log')).toBeNull()
    const next = task({ ...projection, members: [{ ...projection.members[0]!, stages: [{ ...stage, publicUpdates: [{
      eventId: 'event-1', taskId: 'task-1', seq: 1, occurredAt: '2026-09-05T15:00:00Z', text: '已取得第一份核对材料。', truncated: false,
    }] }] }] })
    scene.rerender(<WorkTaskPanel {...props(next)} />)
    expect(within(scene.getByRole('log')).getByText('已取得第一份核对材料。')).toBeTruthy()
    expect(scene.getByRole('log').closest('li')?.getAttribute('data-executing')).toBe('true')
  })

  it.each([
    { status: 'failed' as const }, { status: 'completed' as const }, { status: 'stopping' as const },
    { status: 'stopped' as const }, { waitKind: 'human' as const }, { waitKind: 'runtime' as const },
    { observedAt: Date.now() - 60_000 },
  ])('does not animate retained member activity outside a current execution: %j', (overrides) => {
    const member = task().members[0]!
    const projection = task({ waitKind: 'fanout', ...overrides,
      members: [{ ...member, status: 'running', stages: [{ ...member.stages[0]!, status: 'running' }] }] })
    const header = render(<WorkTaskHeader {...props(projection)} />)
    expect(header.container.querySelector('[data-executing]')).toBeNull()
    header.unmount()
    const scene = render(<WorkTaskPanel {...props(projection)} />)
    expect(scene.container.querySelector('[data-executing]')).toBeNull()
    expect(scene.queryByText('复核员 正在执行')).toBeNull()
  })

  it('opens a failed run at its member progress and keeps the reported cause in the conversation', () => {
    const member = task().members[0]!
    const projection = task({ status: 'failed', teamName: '', waitKind: '', members: [{ ...member, stages: [{
      ...member.stages[0]!, retryable: false, failureClass: 'work',
      failureReason: 'the referenced result file was not saved as a deliverable',
    }] }] })
    const view = render(<WorkTaskPanel {...props(projection)} />)
    expect(view.getByRole('tab', { name: '进展' }).getAttribute('aria-selected')).toBe('true')
    expect(view.getByRole('button', { name: '查看 复核员 的工作' })).toBeTruthy()
    expect(view.getByText('团队名称暂未取得')).toBeTruthy()
    expect(view.queryByText('正在匹配合适团队')).toBeNull()
    expect(view.getByText('本阶段提到的结果文件尚未被保存为可领取的成果。')).toBeTruthy()
    expect(view.getByText('the referenced result file was not saved as a deliverable')).toBeTruthy()
    view.unmount()
    const card = render(<WorkTaskConversationCard {...props(projection) as unknown as Parameters<typeof WorkTaskConversationCard>[0]} />)
    expect(card.container.querySelector('[data-weave-task-receipt]')?.textContent).toMatchInlineSnapshot('"运行失败复核员 · 复核本阶段提到的结果文件尚未被保存为可领取的成果。查看具体原因the referenced result file was not saved as a deliverable查看进展与成果"')
    expect(card.queryByText('正在匹配合适团队')).toBeNull()
  })

  it.each(['running', 'stopping', 'stopped', 'failed', 'completed'] as const)('hides stale retry controls while %s', (status) => {
    const view = render(<WorkTaskPanel {...props(task({ status }))} />)
    expect(view.queryByRole('button', { name: '从当前阶段重试' })).toBeNull()
    fireEvent.click(view.getByRole('tab', { name: '进展' }))
    fireEvent.click(view.getByRole('button', { name: '查看 复核员 的工作' }))
    expect(view.queryByRole('button', { name: '只重试这个阶段' })).toBeNull()
  })

  it('offers only the node named by the current runtime wait', () => {
    const view = render(<WorkTaskPanel {...props(task({ waitNodeId: 'another-stage' }))} />)
    expect(view.queryByRole('button', { name: '从当前阶段重试' })).toBeNull()
    view.rerender(<WorkTaskPanel {...props(task())} />)
    expect(view.getByRole('button', { name: '从当前阶段重试' })).toBeTruthy()
  })

  it.each([
    ['human', '需要你回答', '回答团队的问题后继续执行；提交前也可以在主对话讨论。'],
    ['correction', '等待确认修改范围', '查看下方的修改范围，确认应用或放弃本次修改后继续。'],
    ['timer', '等待约定时间', '到达约定时间后会自动继续；已有成果仍可查看。'],
    ['fanout', '等待团队阶段完成', '团队汇总正在等待分工阶段；可恢复的中断阶段会在下方提供重试。'],
  ] as const)('explains a %s wait without displaying an unrelated retry', (waitKind, title, help) => {
    const view = render(<WorkTaskPanel {...props(task({ waitKind, members: [] }))} />)
    expect(view.container.querySelector('[data-weave-work-task] > header')?.textContent).toContain(title)
    expect(view.getByText(help)).toBeTruthy()
    expect(view.queryByRole('button', { name: '从当前阶段重试' })).toBeNull()
  })

  it('keeps completed execution separate from final delivery', () => {
    const projection = task({ status: 'completed', members: [], deliverableCount: 1, deliverables: [{
      id: 'stage-output', title: '已完成研究记录', kind: 'stage', contentType: 'text/plain',
      content: '已保存', preview: '已保存', truncated: false, createdAt: '',
    }] })
    const view = render(<WorkTaskPanel {...props(projection)} />)
    expect(view.getByText('执行已结束，最终成果待核实')).toBeTruthy()
    expect(view.getByText('已完成研究记录')).toBeTruthy()
    expect(view.queryByText(/份最终产物已就绪/u)).toBeNull()
    expect(view.container.querySelector('[data-weave-work-task] > header p')?.textContent).toMatchInlineSnapshot(
      '"执行已结束，但尚未取得最终成果。阶段记录可供查看，不能作为已交付的最终结果。"',
    )
    view.unmount()
    const header = render(<WorkTaskHeader {...props(projection)} />)
    expect(header.getByText('执行已结束，最终成果待核实')).toBeTruthy()
  })

  it('recognizes a filename-free final delivery returned by the real workflow API', async () => {
    const recorded = workTaskModel([
      recordedTool('mcp__weave__team_run_activity', { run_id: 'run-1', status: 'succeeded' }, 1),
      recordedTool('mcp__weave__deliverable_list', { deliverables: [{
        id: 'delivery-1', run_id: 'run-1', title: '最终产物 · Deliver', content_type: 'text/markdown',
        content: '验收结论已生成。', created_at: '2026-09-05T11:27:11.24615+08:00',
        metadata: { source: 'published_workflow', node_id: 'deliver', filename: '',
          node_type: 'deliver', node_label: 'Deliver', artifact_kind: 'final' },
      }] }, 2),
    ])
    expect(recorded.deliverables[0]?.kind).toBe('summary')
    const projection = task(recorded)
    const view = render(<WorkTaskPanel {...props(projection)} />)
    expect(view.queryByText('执行已结束，最终成果待核实')).toBeNull()
    expect(view.getByText('已完成')).toBeTruthy()
    expect(view.queryByText(/文件包尚未同步/u)).toBeNull()
    expect(view.getByText('最终交付结论已就绪').textContent).toMatchInlineSnapshot('"最终交付结论已就绪"')
    fireEvent.click(view.getByText('汇总交付 · 最终成果'))
    expect(await view.findByText('验收结论已生成。')).toBeTruthy()
    view.unmount()
    const header = render(<WorkTaskHeader {...props(projection)} />)
    expect(header.getByText('已完成')).toBeTruthy()
    expect(header.queryByText('执行已结束，最终成果待核实')).toBeNull()
  })
  it('keeps missing delivery neutral and offers review before another run', async () => {
    const requestDelivery = vi.fn(() => Promise.resolve())
    const projection = task({ status: 'completed', members: [] })
    const view = render(<WorkTaskPanel {...props(projection)} requestDelivery={requestDelivery} rerun={vi.fn()} assessOutcome={vi.fn()} />)
    expect(view.container.querySelector('header [data-status]')?.getAttribute('data-status')).toBe('attention')
    expect(view.queryByText('这份交付可以采用吗')).toBeNull()
    expect(view.getByRole('button', { name: '修订并重新运行' }).closest('details')?.open).toBe(false)
    fireEvent.click(view.getByRole('button', { name: '核对并补齐交付' }))
    await waitFor(() => { expect(requestDelivery).toHaveBeenCalledWith('run-1') })
    expect(view.getByRole('button', { name: '核对并补齐交付' }).textContent).toMatchInlineSnapshot('"核对并补齐交付"')
  })

  it.each(['running', 'completed'] as const)('selects the phase-appropriate pane while %s and supports arrow-key navigation', (status) => {
    const view = render(<WorkTaskPanel {...props(task({ status, members: [] }))} />)
    const selected = view.getByRole('tab', { selected: true })
    expect(selected.textContent).toBe(status === 'running' ? '进展' : '成果')
    fireEvent.keyDown(selected, { key: 'ArrowRight' })
    expect(view.getByRole('tab', { selected: true }).textContent).toBe(status === 'running' ? '成果' : '进展')
    expect(document.activeElement).toBe(view.getByRole('tab', { selected: true }))
  })

  it.each(['completed', 'stopped'] as const)('keeps %s member records read-only and restores keyboard focus through a long team list', (status) => {
    const member = task().members[0]!
    const members = Array.from({ length: 24 }, (_, index) => ({ ...member, agentId: `member-${index}`, name: `成员 ${index}`, status }))
    const view = render(<WorkTaskPanel {...props(task({ status, members }))} requestCorrection={vi.fn()} />)
    fireEvent.click(view.getByRole('tab', { name: '进展' }))
    expect(view.getAllByRole('button', { name: /查看 成员/u })).toHaveLength(24)
    fireEvent.click(view.getByRole('tab', { name: '进展' }))
    fireEvent.click(view.getByRole('button', { name: '查看 成员 23 的工作' }))
    expect(document.activeElement).toBe(view.getByRole('button', { name: '返回团队总览' }))
    expect(view.getByText('成员执行记录')).toBeTruthy()
    expect(view.queryByText(/安全点/u)).toBeNull()
    expect(view.queryByRole('button', { name: '向此成员纠偏' })).toBeNull()
    fireEvent.keyDown(document.activeElement!, { key: 'Escape' })
    expect(document.activeElement).toBe(view.getByRole('button', { name: '查看 成员 23 的工作' }))
  })

  it('keeps a targeted correction request, impact, confirmation, and application in the member record', async () => {
    const requestCorrection = vi.fn(() => Promise.resolve(null))
    const confirmCorrection = vi.fn(() => Promise.resolve(null))
    const original = task({ status: 'running', waitKind: '', waitNodeId: '' })
    const view = render(<WorkTaskPanel {...props(original)}
      requestCorrection={requestCorrection} confirmCorrection={confirmCorrection} />)
    fireEvent.click(view.getByRole('button', { name: '纠偏团队' }))
    fireEvent.change(view.getByLabelText('纠偏对象'), { target: { value: 'reviewer' } })
    fireEvent.change(view.getByLabelText('需要修正什么'), { target: { value: '补充证据来源' } })
    fireEvent.click(view.getByRole('button', { name: '提交纠偏请求' }))
    await waitFor(() => { expect(requestCorrection).toHaveBeenCalledWith('run-1', 'member', 'reviewer', '补充证据来源') })
    fireEvent.click(view.getByRole('button', { name: '查看 复核员 的工作' }))
    const correction = { correctionId: 'correction-1', targetKind: 'member' as const, targetMemberId: 'reviewer', instruction: '补充证据来源',
      status: 'requested' as const, safeNodeId: '', restartNodeId: '', affectedNodeIds: [], preservedNodeIds: [], requestedAt: '' }
    view.rerender(<WorkTaskPanel {...props({ ...original, corrections: [correction] })}
      requestCorrection={requestCorrection} confirmCorrection={confirmCorrection} />)
    expect(view.getByRole('region', { name: '纠偏影响范围' })).toBeTruthy()
    view.rerender(<WorkTaskPanel {...props({ ...original, status: 'waiting', waitKind: 'correction', corrections: [{ ...correction,
      status: 'ready', safeNodeId: 'review', restartNodeId: 'review', affectedNodeIds: ['review'], preservedNodeIds: ['research'],
    }] })}
    requestCorrection={requestCorrection} confirmCorrection={confirmCorrection} />)
    expect(view.getByRole('button', { name: '确认应用并继续' })).toBeTruthy()
    fireEvent.click(view.getByRole('button', { name: '确认应用并继续' }))
    await waitFor(() => { expect(confirmCorrection).toHaveBeenCalledWith('run-1', 'correction-1', 'apply') })
    view.rerender(<WorkTaskPanel {...props({ ...original, corrections: [{ ...correction, status: 'applied' }] })}
      requestCorrection={requestCorrection} confirmCorrection={confirmCorrection} />)
    expect(view.getByText('调整已生效')).toBeTruthy()
    expect(view.getByText('成员执行记录')).toBeTruthy()
  })

  it('renders a member output as a document and links the complete bound download', async () => {
    const member = task().members[0]!
    const output = { id: 'output-1', title: '复核报告.md', kind: 'final' as const, contentType: 'text/markdown',
      content: '# 复核结论\n\n证据已齐备。', preview: '# 复核结论', truncated: true, createdAt: '' }
    const projection = task({ status: 'completed', members: [{ ...member, stages: [{ ...member.stages[0]!, outputRefs: ['output-1'] }] }], deliverables: [output] })
    const view = render(<WorkTaskPanel {...props(projection)} />)
    fireEvent.click(view.getByRole('tab', { name: '进展' }))
    fireEvent.click(view.getByRole('button', { name: '查看 复核员 的工作' }))
    fireEvent.click(view.getByText('复核报告.md'))
    expect(await view.findByRole('heading', { name: '复核结论' })).toBeTruthy()
    expect(view.getByText('证据已齐备。')).toBeTruthy()
    expect(view.getByRole('link', { name: '下载文件' }).getAttribute('href')).toBe('/api/weave.deliverable?sessionId=session&runId=run-1&id=output-1&mode=download')
  })

  it('previews SVG and HTML without inventing a live application link', async () => {
    const image = { id: 'image', title: '说明图.svg', kind: 'final' as const, contentType: 'image/svg+xml', content: '<svg/>', preview: '', truncated: false, createdAt: '' }
    const html = { ...image, id: 'page', title: '报告.html', contentType: 'text/html', content: '<h1>报告</h1><script>alert(1)</script>' }
    const view = render(<WorkTaskPanel {...props(task({ status: 'completed', members: [], deliverables: [image, html] }))} />)
    fireEvent.click(view.getByText('说明图.svg'))
    expect((await view.findByRole('img', { name: '说明图.svg' })).getAttribute('src')).toContain('id=image&mode=preview')
    fireEvent.click(view.getByText('报告.html'))
    await waitFor(() => { expect(view.getByTitle('报告.html').getAttribute('sandbox')).toBe('') })
    expect(view.getByTitle('报告.html').getAttribute('srcdoc')).toContain("default-src 'none'")
    expect(within(view.getByRole('region', { name: '交付物' })).getAllByRole('link').every(link => link.getAttribute('href')?.startsWith('/api/weave.deliverable?'))).toBe(true)
  })

  it('does not label an abandoned run as a confirmed stop', () => {
    const recorded = workTaskModel([recordedTool('mcp__weave__team_run_activity', { run_id: 'run-1', status: 'abandoned', stop_unconfirmed: true, cancel_requested_at: null }, 1)])
    expect(recorded).toMatchObject({ status: 'failed', actionError: 'stop_unconfirmed' })
    const view = render(<WorkTaskPanel {...props(task(recorded))} />)
    expect(view.getByText('停止状态未能确认').textContent).toMatchInlineSnapshot('"停止状态未能确认"')
    expect(view.queryByText('已停止')).toBeNull()
    expect(view.queryByRole('button', { name: '从当前阶段重试' })).toBeNull()
    view.unmount()
    const header = render(<WorkTaskHeader {...props(task(recorded))} />)
    expect(header.getByText('停止状态未能确认')).toBeTruthy()
  })

  it('keeps one conversation card per run across refresh and retains accepted-action receipts', async () => {
    const retryStage = vi.fn(() => Promise.resolve(null))
    const projection = task()
    const cardProps = props(projection) as unknown as Parameters<typeof WorkTaskConversationCard>[0]
    const view = render(<WorkTaskConversationCard {...cardProps} retryStage={retryStage} />)
    fireEvent.click(view.getByRole('button', { name: '从当前阶段重试' }))
    fireEvent.click(view.getByRole('button', { name: '确认重试' }))
    await waitFor(() => { expect(retryStage).toHaveBeenCalledWith('run-1', 'review') })
    const action = { kind: 'stage-retry' as const, targetRunId: 'run-1', nodeId: 'review', requestedAt: 1, idempotencyKey: '', clientRequestId: '', brief: '', targetKind: '' as const, targetMemberId: '', correctionId: '', disposition: '' as const, instruction: '' }
    const resumed = task({ status: 'running', waitKind: '', waitNodeId: '', actionHistory: [{ id: 'retry-run-1-review-1', action, outcome: 'accepted', resolvedAt: 2 }] })
    view.rerender(<WorkTaskConversationCard {...props(resumed) as unknown as Parameters<typeof WorkTaskConversationCard>[0]}
      retryStage={retryStage} />)
    view.rerender(<WorkTaskConversationCard {...props(resumed) as unknown as Parameters<typeof WorkTaskConversationCard>[0]}
      retryStage={retryStage} />)
    expect(view.container.querySelectorAll('[data-weave-task-card="run-1"]')).toHaveLength(1)
    expect(view.getAllByText('只重试这个阶段 · 请求已接收')).toHaveLength(1)
    expect(view.queryByRole('button', { name: '从当前阶段重试' })).toBeNull()
  })

  it.each(['completed', 'stopped', 'failed'] as const)('keeps a %s conversation receipt compact even when a member is selected in the scene', (status) => {
    viewStore.actions.selectMember('run-1', 'reviewer')
    const projection = task({ status, deliverables: [{ id: 'stage-output', title: '阶段资料.md', kind: 'stage', contentType: 'text/markdown', content: '# 阶段正文', preview: '阶段正文', truncated: false, createdAt: '' }] })
    const openDetails = vi.fn()
    const view = render(<WorkTaskConversationCard {...props(projection) as unknown as Parameters<typeof WorkTaskConversationCard>[0]}
      openDetails={openDetails} rerun={vi.fn()} assessOutcome={vi.fn()} />)
    expect(view.container.querySelector('[data-weave-task-receipt]')).not.toBeNull()
    expect(view.container.querySelector('[data-weave-work-task]')).toBeNull()
    expect(view.queryByRole('tab')).toBeNull()
    expect(view.queryByText('成员执行记录')).toBeNull()
    expect(view.queryByText('阶段正文')).toBeNull()
    expect(view.queryByText('其他操作')).toBeNull()
    expect(view.queryByRole('progressbar')).toBeNull()
    fireEvent.click(view.getByRole('button', { name: '阶段产物 · 1' }))
    expect(viewStore.getSnapshot().tabs['run-1']).toBe('outputs')
    expect(openDetails).toHaveBeenCalledOnce()
    if (status === 'completed') {
      expect(view.getByText('执行已结束，最终成果待核实')).toBeTruthy()
      expect(view.container.querySelector('[data-status="attention"]')).not.toBeNull()
    }
  })

  it('opens the exact final result from the compact receipt without mounting a duplicate document', () => {
    const projection = task({ status: 'completed', deliverables: [{ id: 'final-report', title: '最终报告.md', kind: 'final', contentType: 'text/markdown', content: '# 最终正文', preview: '最终正文', truncated: false, createdAt: '' }] })
    const openDetails = vi.fn()
    const view = render(<WorkTaskConversationCard {...props(projection) as unknown as Parameters<typeof WorkTaskConversationCard>[0]}
      openDetails={openDetails} />)
    expect(view.queryByText('最终正文')).toBeNull()
    fireEvent.click(view.getByRole('button', { name: '最终报告.md' }))
    expect(viewStore.getSnapshot().outputSelection['run-1']?.id).toBe('final-report')
    expect(openDetails).toHaveBeenCalledOnce()
  })

  it('opens a human answer only on request and submits it from the compact conversation receipt', async () => {
    const projection = task({ waitKind: 'human', humanTask: { interactionId: 'question-1', nodeId: 'review', title: '确认材料范围', instructions: '请选择当前范围', resumeSchema: { type: 'object', properties: { scope: { type: 'string', title: '材料范围' } }, required: ['scope'] } } })
    const completeHumanTask = vi.fn(async () => null)
    const cardProps = props(projection) as unknown as Parameters<typeof WorkTaskConversationCard>[0]
    const view = render(<WorkTaskConversationCard {...cardProps} completeHumanTask={completeHumanTask} />)
    const answer = view.getByText('回答这个问题').closest('details')!
    expect(answer.open).toBe(false)
    view.rerender(<WorkTaskConversationCard {...cardProps} completeHumanTask={completeHumanTask} />)
    expect(answer.open).toBe(false)
    fireEvent.click(view.getByText('回答这个问题'))
    fireEvent.change(view.getByLabelText('材料范围 · 必填'), { target: { value: '最近四年' } })
    fireEvent.click(view.getByRole('button', { name: '提交答复并继续' }))
    await waitFor(() => { expect(completeHumanTask).toHaveBeenCalledWith('run-1', 'question-1', { scope: '最近四年' }) })
    expect(view.container.querySelector('[data-weave-work-task]')).toBeNull()
  })

  it('requests and confirms a correction inside the compact receipt without opening the scene', async () => {
    const requestCorrection = vi.fn(async () => null)
    const confirmCorrection = vi.fn(async () => null)
    const openDetails = vi.fn()
    const original = task({ status: 'running', waitKind: '' })
    const view = render(<WorkTaskConversationCard {...props(original) as unknown as Parameters<typeof WorkTaskConversationCard>[0]}
      requestCorrection={requestCorrection} confirmCorrection={confirmCorrection} openDetails={openDetails} />)
    fireEvent.click(view.getByRole('button', { name: '纠偏团队' }))
    fireEvent.change(view.getByLabelText('需要修正什么'), { target: { value: '改为最近四年' } })
    fireEvent.click(view.getByRole('button', { name: '提交纠偏请求' }))
    await waitFor(() => { expect(requestCorrection).toHaveBeenCalledWith('run-1', 'team', '', '改为最近四年') })
    const corrected = task({ status: 'waiting', waitKind: 'correction', corrections: [{ correctionId: 'correction-2', targetKind: 'team', targetMemberId: '', instruction: '改为最近四年', status: 'ready', safeNodeId: 'review', restartNodeId: 'review', affectedNodeIds: ['review'], preservedNodeIds: ['research'], requestedAt: '' }] })
    view.rerender(<WorkTaskConversationCard {...props(corrected) as unknown as Parameters<typeof WorkTaskConversationCard>[0]}
      requestCorrection={requestCorrection} confirmCorrection={confirmCorrection} openDetails={openDetails} />)
    expect(view.getByText('纠偏影响范围', { selector: 'summary' }).closest('details')?.open).toBe(false)
    fireEvent.click(view.getByText('纠偏影响范围', { selector: 'summary' }))
    fireEvent.click(view.getByRole('button', { name: '确认应用并继续' }))
    await waitFor(() => { expect(confirmCorrection).toHaveBeenCalledWith('run-1', 'correction-2', 'apply') })
    expect(openDetails).not.toHaveBeenCalled()
  })

  it('confirms a stop in the compact receipt without treating its acknowledgement as a completed stop', async () => {
    const stopRun = vi.fn(async () => null)
    const projection = task({ status: 'running', waitKind: '' })
    const view = render(<WorkTaskConversationCard {...props(projection) as unknown as Parameters<typeof WorkTaskConversationCard>[0]}
      stopRun={stopRun} />)
    fireEvent.click(view.getByRole('button', { name: '停止这次运行' }))
    expect(stopRun).not.toHaveBeenCalled()
    fireEvent.click(view.getByRole('button', { name: '确认停止' }))
    await waitFor(() => { expect(stopRun).toHaveBeenCalledWith('run-1') })
    expect(view.queryByText('已停止')).toBeNull()
    expect(view.container.querySelector('[data-weave-work-task]')).toBeNull()
  })

  it('retains member selection across tabs and opens an exact selected output in place', async () => {
    const final = { id: 'final-1', title: '完整报告', kind: 'final' as const, contentType: 'text/markdown', content: '# 最终报告', preview: '', truncated: false, createdAt: '' }
    const projection = task({ status: 'running', deliverables: [final] })
    const view = render(<WorkTaskPanel {...props(projection)} />)
    fireEvent.click(view.getByRole('button', { name: '查看 复核员 的工作' }))
    fireEvent.click(view.getByRole('tab', { name: '成果' }))
    fireEvent.click(view.getByRole('tab', { name: '进展' }))
    expect(view.getByText('成员执行记录')).toBeTruthy()
    viewStore.actions.showOutput('run-1', 'final-1')
    await waitFor(() => { expect(view.getByRole('tab', { name: '成果' }).getAttribute('aria-selected')).toBe('true') })
    const artifact = view.container.querySelector('[data-deliverable-id="final-1"]') as HTMLDetailsElement
    expect(artifact.open).toBe(true)
    expect(view.getByRole('heading', { name: '最终报告' })).toBeTruthy()
    expect(document.activeElement).toBe(artifact.querySelector('summary'))
  })

  it('proposes a member adjustment through the injected main input without submitting one', async () => {
    const beginMemberAdjustment = vi.fn(() => Promise.resolve())
    const requestCorrection = vi.fn()
    const view = render(<WorkTaskPanel {...props(task({ status: 'running' }))} beginMemberAdjustment={beginMemberAdjustment} requestCorrection={requestCorrection} />)
    fireEvent.click(view.getByRole('button', { name: '查看 复核员 的工作' }))
    fireEvent.click(view.getByRole('button', { name: '提出调整' }))
    await waitFor(() => { expect(beginMemberAdjustment).toHaveBeenCalledWith({ runId: 'run-1', memberId: 'reviewer', memberName: '复核员', stages: [{ nodeId: 'review', name: '复核', outputIds: [], outputTitles: [] }] }) })
    expect(requestCorrection).not.toHaveBeenCalled()
    expect(view.queryByRole('textbox')).toBeNull()
  })

})
