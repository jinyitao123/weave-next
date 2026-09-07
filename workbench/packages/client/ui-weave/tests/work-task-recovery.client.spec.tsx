// @vitest-environment jsdom

import { useSyncExternalStore } from 'react'
import { act, cleanup, fireEvent, render, waitFor, within } from '@testing-library/react'
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
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

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

  it('indexes completed outputs, recorded input references, and members without mixing stage files into the overview', () => {
    const member = task().members[0]!
    const input = { name: '任务材料', path: '/inputs/brief.md', source: 'file', nodeId: '', expectedType: 'text', summary: '' }
    const projection = task({ status: 'completed', waitKind: '', brief: '核实三条路线', members: [{ ...member, status: 'completed', stages: [{ ...member.stages[0]!, inputs: [input, input] }] }],
      deliverables: [
        { id: 'final', title: '报告.md', kind: 'final', contentType: 'text/markdown', content: '正文', preview: '', truncated: false, createdAt: '' },
        { id: 'stage', title: '草稿.md', kind: 'stage', contentType: 'text/markdown', content: '草稿', preview: '', truncated: false, createdAt: '' },
        { id: 'code', title: '辅助.py', kind: 'final', contentType: 'text/plain', content: '', preview: '', truncated: false, createdAt: '' },
        { id: 'web', title: '应用.html', kind: 'final', contentType: 'text/html', content: '', preview: '', truncated: false, createdAt: '' },
        { id: 'drawing', title: '示意.svg', kind: 'final', contentType: 'image/svg+xml', content: '', preview: '', truncated: false, createdAt: '' },
      ] })
    const view = render(<WorkTaskPanel {...props(projection)} />)
    const overview = within(view.getByRole('tabpanel', { name: '总览' }))
    expect(overview.queryByText('草稿.md')).toBeNull()
    expect(overview.getAllByRole('button').slice(0, 3).map(button => button.textContent)).toEqual(['应用.html', '示意.svg', '报告.md'])
    expect(overview.getByText('输入引用 · 1')).toBeTruthy()
    expect(overview.getAllByText('/inputs/brief.md')).toHaveLength(1)
    expect(overview.getAllByRole('heading').map(heading => heading.textContent)).toMatchInlineSnapshot(`
      [
        "输出内容",
        "团队成员",
        "来源",
      ]
    `)
    act(() => { viewStore.actions.rememberReading('scene:run-1:progress:', { top: 900, follow: false, lastEvent: '' }) })
    fireEvent.click(overview.getByRole('button', { name: '1 / 1 位已完成' }))
    expect(viewStore.getSnapshot().reading['scene:run-1:progress:']?.top).toBe(0)
    expect(view.getByRole('tab', { name: '进展' }).getAttribute('aria-selected')).toBe('true')
    expect(view.getByRole('button', { name: '查看 复核员 的工作' })).toBeTruthy()
    fireEvent.click(view.getByRole('tab', { name: '总览' }))
    fireEvent.click(overview.getByRole('button', { name: '报告.md' }))
    expect(viewStore.getSnapshot().outputSelection['run-1']?.id).toBe('final')
    expect(view.getByRole('tab', { name: '成果' }).getAttribute('aria-selected')).toBe('true')
  })

  it('finds stage files by name and clears a conflicting filter when an exact output link arrives', async () => {
    const deliverables = Array.from({ length: 6 }, (_, index) => ({ id: `file-${index}`, title: `File-${index}.md`, kind: index < 2 ? 'final' as const : 'stage' as const,
      contentType: 'text/markdown', content: `Body ${index}`, preview: '', truncated: false, createdAt: '' }))
    viewStore.actions.selectTab('run-1', 'outputs')
    const view = render(<WorkTaskPanel {...props(task({ status: 'completed', waitKind: '', deliverables }))} />)
    const outputs = within(view.getByRole('tabpanel', { name: '成果' }))
    const search = outputs.getByRole('searchbox', { name: '查找文件名称' })
    fireEvent.change(search, { target: { value: 'file-5' } })
    expect(outputs.getByText('File-5.md')).toBeTruthy()
    expect(outputs.queryByText('File-0.md')).toBeNull()
    fireEvent.change(search, { target: { value: 'absent' } })
    expect(outputs.getByRole('status').textContent).toBe('没有匹配的文件，试试其他名称。')
    act(() => { viewStore.actions.showOutput('run-1', 'file-4') })
    await waitFor(() => { expect((search as HTMLInputElement).value).toBe('') })
    await waitFor(() => { expect(document.activeElement?.textContent).toContain('File-4.md') })
    expect(outputs.getByText('File-4.md').closest('details')?.open).toBe(true)
  })

  it('keeps tab focus, selected member, preview zoom, and outer reading position across tab changes', async () => {
    const image = { id: 'drawing', title: '流程.svg', kind: 'final' as const, contentType: 'image/svg+xml', content: '<svg/>', preview: '', truncated: false, createdAt: '' }
    const view = render(<div style={{ overflowY: 'auto', height: 500 }}><WorkTaskPanel {...props(task({ deliverables: [image] }))} /></div>)
    const scroller = view.container.firstElementChild as HTMLElement
    fireEvent.click(view.getByRole('button', { name: '查看 复核员 的工作' }))
    const progress = view.getByRole('tab', { name: '进展' })
    const outputs = view.getByRole('tab', { name: '成果' })
    scroller.scrollTop = 345
    progress.focus()
    fireEvent.keyDown(progress, { key: 'ArrowRight' })
    expect(document.activeElement).toBe(outputs)
    expect(scroller.scrollTop).toBe(0)
    fireEvent.click(within(view.getByRole('tabpanel', { name: '成果' })).getByText('流程.svg'))
    const imageElement = await view.findByRole('img', { name: '流程.svg' })
    fireEvent.load(imageElement)
    fireEvent.click(view.getByRole('button', { name: '放大图纸' }))
    expect(view.getByText('125%')).toBeTruthy()
    const disclosure = imageElement.closest('details')!
    fireEvent.click(disclosure.querySelector('summary')!)
    await waitFor(() => { expect(disclosure.open).toBe(false) })
    fireEvent.click(disclosure.querySelector('summary')!)
    await waitFor(() => { expect(disclosure.open).toBe(true) })
    expect(view.getByRole('img', { name: '流程.svg' })).toBe(imageElement)
    expect(view.getByText('125%')).toBeTruthy()
    outputs.focus()
    fireEvent.keyDown(outputs, { key: 'ArrowLeft' })
    expect(document.activeElement).toBe(progress)
    expect(scroller.scrollTop).toBe(345)
    expect(view.getByRole('heading', { name: '复核员' })).toBeTruthy()
    fireEvent.keyDown(progress, { key: 'ArrowRight' })
    expect(view.getByText('125%')).toBeTruthy()
  })

  it('consumes an output deep link once so returning to Outputs keeps tab focus', async () => {
    const output = { id: 'final', title: '最终稿', kind: 'final' as const, contentType: 'text/plain', content: '正文', preview: '', truncated: false, createdAt: '' }
    viewStore.actions.showOutput('run-1', 'final')
    const view = render(<WorkTaskPanel {...props(task({ deliverables: [output] }))} />)
    await waitFor(() => { expect(document.activeElement?.tagName).toBe('SUMMARY') })
    const progress = view.getByRole('tab', { name: '进展' })
    const outputs = view.getByRole('tab', { name: '成果' })
    fireEvent.click(progress)
    progress.focus()
    fireEvent.keyDown(progress, { key: 'ArrowRight' })
    expect(document.activeElement).toBe(outputs)
  })

  it('retains the scene position when navigation arrives from the conversation receipt', () => {
    const view = render(<div style={{ overflowY: 'auto', height: 500 }}><WorkTaskPanel {...props(task())} /></div>)
    fireEvent.click(view.getByRole('button', { name: '查看 复核员 的工作' }))
    const scroller = view.container.firstElementChild as HTMLElement
    scroller.scrollTop = 480
    fireEvent.scroll(scroller)
    // Shared actions also serve the separately mounted conversation receipt.
    act(() => { viewStore.actions.selectTab('run-1', 'outputs') })
    act(() => { viewStore.actions.selectTab('run-1', 'progress') })
    view.rerender(<div style={{ overflowY: 'auto', height: 500 }}><WorkTaskPanel {...props(task())} /></div>)
    expect(scroller.scrollTop).toBe(480)
  })

  it('reads the known final-review response without hiding its exact original contents', async () => {
    const body = '{"decision":"approve","comments":"保留两项待确认信息。"}'
    const review = { id: 'review', title: '终审记录', kind: 'summary' as const, contentType: 'application/json', content: body, preview: '', truncated: false, createdAt: '' }
    const view = render(<WorkTaskPanel {...props(task({ status: 'completed', deliverables: [review] }))} />)
    fireEvent.click(view.getByRole('button', { name: '终审记录' }))
    expect(await view.findByText('终审通过')).toBeTruthy()
    expect(view.getByText('保留两项待确认信息。')).toBeTruthy()
    const original = view.getByText('查看原始记录').closest('details')!
    expect(original.open).toBe(false)
    fireEvent.click(view.getByText('查看原始记录'))
    expect(view.getByText(body)).toBeTruthy()
    view.rerender(<WorkTaskPanel {...props(task({ status: 'completed', deliverables: [{ ...review, content: '{"decision":"approve","comments":"保留","additional":"不可丢失"}' }] }))} />)
    expect(view.queryByText('终审通过')).toBeNull()
    expect(view.getByText(/additional/u)).toBeTruthy()
  })

  it('waits for restored scene content to fit without saving a clamped browser offset', () => {
    let notifyResize: () => void = () => {}
    const disconnect = vi.fn()
    vi.stubGlobal('ResizeObserver', class {
      constructor(callback: () => void) { notifyResize = callback }
      observe = vi.fn()
      disconnect = disconnect
    })
    viewStore.actions.rememberReading('scene:run-1:progress:', { top: 600, follow: false, lastEvent: '' })
    const view = render(<div style={{ overflowY: 'auto' }} />)
    const scroller = view.container.firstElementChild as HTMLElement
    let available = 100
    let top = 0
    Object.defineProperty(scroller, 'scrollTop', { configurable: true, get: () => top, set: (value: number) => { top = Math.min(value, available) } })
    view.rerender(<div style={{ overflowY: 'auto' }}><WorkTaskPanel {...props(task())} /></div>)
    expect(scroller.scrollTop).toBe(100)
    fireEvent.scroll(scroller)
    expect(viewStore.getSnapshot().reading['scene:run-1:progress:']?.top).toBe(600)
    available = 900
    act(() => { notifyResize() })
    expect(scroller.scrollTop).toBe(600)
    expect(disconnect).toHaveBeenCalled()
    view.unmount()
    expect(disconnect).toHaveBeenCalledTimes(2)
  })

  it.each([
    ['outputsMissing', '执行已结束，最终成果待核实'],
    ['runtimeStop', '运行中断，等待原节点确认停止'],
    ['retryable', '阶段中断，可重试'],
  ] as const)('uses the shared %s display state in the header, conversation receipt, and progress panel', (displayState, label) => {
    const projection = task({ displayState, status: 'running', waitKind: '', members: [] })
    const header = render(<WorkTaskHeader {...props(projection)} />)
    expect(header.getByText(label)).toBeTruthy()
    header.unmount()
    const card = render(<WorkTaskConversationCard {...props(projection) as unknown as Parameters<typeof WorkTaskConversationCard>[0]} />)
    expect(card.getByText(label)).toBeTruthy()
    card.unmount()
    const panel = render(<WorkTaskPanel {...props(projection)} />)
    expect(panel.getByText(label)).toBeTruthy()
  })

  it('keeps the reader on progress when completion and a final output arrive', () => {
    const projection = task({ status: 'running', waitKind: '', members: [] })
    const panel = render(<WorkTaskPanel {...props(projection)} />)
    expect(panel.getByRole('tab', { name: '进展' }).getAttribute('aria-selected')).toBe('true')
    panel.rerender(<WorkTaskPanel {...props(task({ ...projection, status: 'completed', deliverableCount: 1,
      deliverables: [{ id: 'final-1', title: '核验报告', kind: 'final', contentType: 'text/plain',
        content: '已核验', preview: '', truncated: false, createdAt: '' }] }))} />)
    expect(panel.getByRole('tab', { name: '进展' }).getAttribute('aria-selected')).toBe('true')
    fireEvent.click(panel.getByRole('tab', { name: /成果/u }))
    expect(within(panel.getByRole('tabpanel', { name: '成果' })).getByText('核验报告')).toBeTruthy()
  })

  it.each([
    ['building', 'buildBuilding', '正在创建团队'],
    ['unknown', 'buildUnknown', '创建结果待核实'],
    ['failed', 'buildFailed', '团队创建失败'],
  ] as const)('shows a %s creation without invented execution progress', (state, displayState, label) => {
    const projection = task({ runId: '', clientRequestId: '', status: 'preparing', displayState, members: [],
      completedStages: 0, totalStages: 0, preparation: { callId: 'create-1', buildId: 'build-1', state, error: '',
        steps: [{ id: 'check', label: '检查成员职责', status: 'running', attempt: 2 }] } })
    const panel = render(<WorkTaskPanel {...props(projection)} />)
    expect(panel.getByRole('heading', { name: label })).toBeTruthy()
    expect(panel.getByText('检查成员职责')).toBeTruthy()
    expect(panel.getByText(/第 2 次尝试/u)).toBeTruthy()
    expect(panel.queryByRole('tab')).toBeNull()
    expect(panel.queryByRole('progressbar')).toBeNull()
    expect(panel.container.textContent).not.toMatch(/0\s*[/／]\s*0/u)
    expect(panel.container.textContent).not.toContain('build-1')
    if (state === 'unknown') expect(panel.getByText('尚未确认是否创建成功。请先核实原请求的结果，避免重复创建团队。')).toBeTruthy()
  })

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
    expect(card.queryByText('复核员 · 复核')).toBeNull()
    expect(card.getByText(help)).toBeTruthy()
    expect(card.container.querySelector('[data-executing]')).toBeNull()
    expect(card.container.querySelector('[data-weave-task-receipt]')?.textContent).toMatchInlineSnapshot('"等待运行节点确认停止请先恢复运行节点的连接，等待原执行确认停止；确认后会显示重试入口，已完成的工作会保留。查看进展与成果"')
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
    fireEvent.click(view.getByRole('button', { name: '汇总交付 · 最终成果' }))
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
    expect(view.getByRole('tab', { selected: true }).textContent).toBe(status === 'running' ? '成果' : '总览')
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
    expect(view.getByRole('region', { name: '执行记录' })).toBeTruthy()
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
    expect(view.getByRole('region', { name: '执行记录' })).toBeTruthy()
  })

  it('renders a member output as a document and links the complete bound download', async () => {
    const member = task().members[0]!
    const output = { id: 'output-1', title: '复核报告.md', kind: 'final' as const, contentType: 'text/markdown',
      content: '# 复核结论\n\n证据已齐备。', preview: '# 复核结论', truncated: true, createdAt: '' }
    const projection = task({ status: 'completed', members: [{ ...member, stages: [{ ...member.stages[0]!, outputRefs: ['output-1'] }] }], deliverables: [output] })
    const view = render(<WorkTaskPanel {...props(projection)} />)
    fireEvent.click(view.getByRole('tab', { name: '进展' }))
    fireEvent.click(view.getByRole('button', { name: '查看 复核员 的工作' }))
    fireEvent.click(within(view.getByRole('tabpanel', { name: '进展' })).getByText('复核报告.md'))
    expect(await view.findByRole('heading', { name: '复核结论' })).toBeTruthy()
    expect(within(view.getByRole('tabpanel', { name: '进展' })).getByText('证据已齐备。')).toBeTruthy()
    expect(view.getByRole('link', { name: '下载文件' }).getAttribute('href')).toBe('/api/weave.deliverable?sessionId=session&runId=run-1&id=output-1&mode=download')
  })

  it('previews SVG and HTML without inventing a live application link', async () => {
    const image = { id: 'image', title: '说明图.svg', kind: 'final' as const, contentType: 'image/svg+xml', content: '<svg/>', preview: '', truncated: false, createdAt: '' }
    const html = { ...image, id: 'page', title: '报告.html', contentType: 'text/html', content: '<h1>报告</h1><script>alert(1)</script>' }
    const view = render(<WorkTaskPanel {...props(task({ status: 'completed', members: [], deliverables: [image, html] }))} />)
    fireEvent.click(view.getByRole('button', { name: '说明图.svg' }))
    expect((await view.findByRole('img', { name: '说明图.svg' })).getAttribute('src')).toContain('id=image&mode=preview')
    fireEvent.click(within(view.getByRole('tabpanel', { name: '成果' })).getByText('报告.html'))
    await waitFor(() => { expect(view.container.querySelector('iframe[title="报告.html"]')!.getAttribute('sandbox')).toBe('') })
    expect(view.container.querySelector('iframe[title="报告.html"]')!.getAttribute('srcdoc')).toContain("default-src 'none'")
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

  it('keeps one conversation card per run and leaves durable receipts in the work scene', async () => {
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
    expect(view.queryByText('操作回执')).toBeNull()
    expect(view.queryByRole('button', { name: '从当前阶段重试' })).toBeNull()
    view.unmount()
    const scene = render(<WorkTaskPanel {...props(resumed)} />)
    expect(scene.getAllByText('只重试这个阶段 · 请求已接收')).toHaveLength(1)
    expect(scene.getByText('操作回执').closest('details')?.open).toBe(false)
  })

  it('makes the header a named work-scene entry without repeating the long team name', () => {
    const openDetails = vi.fn()
    const view = render(<WorkTaskHeader {...props(task({ status: 'stopped', teamName: '本地知识库体验验收与恢复协作团队' }))} openDetails={openDetails} />)
    expect(view.getByText('工作现场')).toBeTruthy()
    expect(view.getByText('已停止')).toBeTruthy()
    expect(view.queryByText('本地知识库体验验收与恢复协作团队')).toBeNull()
    fireEvent.click(view.getByRole('button', { name: '打开工作现场' }))
    expect(openDetails).toHaveBeenCalledOnce()
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
    expect(view.queryByRole('region', { name: '执行记录' })).toBeNull()
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
    expect(view.getByRole('region', { name: '执行记录' })).toBeTruthy()
    viewStore.actions.showOutput('run-1', 'final-1')
    await waitFor(() => { expect(view.getByRole('tab', { name: '成果' }).getAttribute('aria-selected')).toBe('true') })
    const artifact = view.container.querySelector('[data-deliverable-id="final-1"]') as HTMLDetailsElement
    expect(artifact.open).toBe(true)
    expect(view.getByRole('heading', { name: '最终报告' })).toBeTruthy()
    expect(document.activeElement).toBe(artifact.querySelector('summary'))
  })

  it('uses readable member labels consistently without changing identity or named members', async () => {
    const base = task().members[0]!
    const leadId = '550e8400-e29b-41d4-a716-446655440000'
    const workerId = '8d253618-c675-4d71-b9e5-2e441d1c9b29'
    const members = [
      { ...base, agentId: leadId, name: leadId, role: 'lead' as const },
      { ...base, agentId: workerId, name: '9A3DE5C7-6012-4F64-B17A-3F7E5C881245' },
      { ...base, agentId: 'empty-name', name: '' },
      { ...base, agentId: 'internal-review-agent', name: 'internal-review-agent' },
      { ...base, agentId: 'named-agent', name: '陈颖' },
    ]
    const beginMemberAdjustment = vi.fn(() => Promise.resolve())
    const requestCorrection = vi.fn(async () => null)
    const view = render(<WorkTaskPanel {...props(task({ status: 'running', waitKind: '', members }))}
      beginMemberAdjustment={beginMemberAdjustment} requestCorrection={requestCorrection} />)
    for (const name of ['团队负责人', '成员 2', '成员 3', '成员 4', '陈颖']) {
      expect(view.getByRole('button', { name: `查看 ${name} 的工作` })).toBeTruthy()
    }
    expect(view.queryByRole('button', { name: /550e8400|9A3DE5C7|internal-review-agent/u })).toBeNull()
    expect(view.getByText(workerId).closest('details')?.open).toBe(false)

    fireEvent.click(view.getByRole('button', { name: '纠偏团队' }))
    expect(view.getByRole('option', { name: '成员 2' }).getAttribute('value')).toBe(workerId)
    fireEvent.click(view.getByRole('button', { name: '查看 成员 2 的工作' }))
    expect(view.getByRole('heading', { name: '成员 2' })).toBeTruthy()
    fireEvent.click(view.getByRole('button', { name: '关注此成员' }))
    fireEvent.click(view.getByRole('button', { name: '提出调整' }))
    await waitFor(() => {
      expect(beginMemberAdjustment).toHaveBeenCalledWith(expect.objectContaining({ memberId: workerId, memberName: '成员 2' }))
    })
    expect(requestCorrection).not.toHaveBeenCalled()
    expect(members[1]?.name).toBe('9A3DE5C7-6012-4F64-B17A-3F7E5C881245')
    fireEvent.click(view.getByRole('button', { name: '返回团队总览' }))
    expect(within(view.getByRole('navigation', { name: '关注中的成员' })).getByRole('button', { name: /成员 2/u })).toBeTruthy()
  })

  it('keeps real duplicate names distinguishable and names an anonymous interrupted owner', () => {
    const base = task().members[0]!
    const members = [
      { ...base, agentId: 'first-reviewer', name: '复核员' },
      { ...base, agentId: 'second-reviewer', name: '复核员', stages: [] },
      { ...base, agentId: 'anonymous-lead-1', name: '', role: 'lead' as const, stages: [] },
      { ...base, agentId: 'anonymous-lead-2', name: '', role: 'lead' as const, stages: [] },
    ]
    const view = render(<WorkTaskPanel {...props(task({ members }))} />)
    for (const name of ['复核员（1）', '复核员（2）', '团队负责人（3）', '团队负责人（4）']) {
      expect(view.getByRole('button', { name: `查看 ${name} 的工作` })).toBeTruthy()
    }
    view.rerender(<WorkTaskPanel {...props(task({ members: [{ ...base,
      agentId: '9a3de5c7-6012-4f64-b17a-3f7e5c881245', name: '9a3de5c7-6012-4f64-b17a-3f7e5c881245',
    }] }))} />)
    expect(view.getByText('由 成员 1 执行')).toBeTruthy()
    expect(view.getByRole('button', { name: /成员 1 · 复核\s*由 成员 1 执行/u })).toBeTruthy()
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
