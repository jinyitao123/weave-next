import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { serialGraph } from '../lib/graph'
import type { DevelopmentDocument, DevelopmentDraft, DevelopmentMember } from '../lib/teams'
import { TeamDetailPage, type TeamTab } from './TeamDetailPage'

const member = (id: string, role = 'worker'): DevelopmentMember => ({ id, configuration: { display_name: id, role, engine: 'loom', runtime_id: '', model: 'model-a', system_prompt: '职责' }, relationship: { duty: '职责', result_requirement: '结果', enabled: true } })
const members = [member('负责人', 'avatar'), member('起草'), member('复核')]
const step = (id: string) => ({ member: { id, displayName: id }, label: id, requirement: '交付要求' })
// Flow B has one member step more than flow A.
const document: DevelopmentDocument = {
  name: '合同团队', objective: '处理合同', audience: [], members,
  workflows: [
    { id: 'flow-a', name: '起草流程', description: 'A', graph_definition: serialGraph([step('起草')]), trigger_config: {} },
    { id: 'flow-b', name: '复核流程', description: 'B', graph_definition: serialGraph([step('起草'), step('复核')]), trigger_config: {} },
  ],
}
const draft: DevelopmentDraft = { revision: 3, published_revision: 3, document, updated_at: '2026-10-21T00:00:00Z', trials: [] }

type Call = { url: string; method: string; body?: Record<string, unknown> }
function serve() {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? 'GET'
    calls.push({ url, method, body: init.body ? JSON.parse(String(init.body)) : undefined })
    if (url.endsWith('/trials')) return new Response(JSON.stringify({ run_id: 'run-1' }), { status: 201 })
    if (url.endsWith('/teams/team-1/development')) return new Response(JSON.stringify(draft), { status: 200 })
    if (url.includes('business-capabilities')) return new Response(JSON.stringify({ available: true, capabilities: [] }), { status: 200 })
    if (url.endsWith('/runtimes')) return new Response(JSON.stringify({ runtimes: [] }), { status: 200 })
    return new Response('[]', { status: 200 })
  }))
  return calls
}

// Tabs are addresses of one page, so the harness switches them in place.
function Harness({ initial = 'workflow' as TeamTab, navigate = vi.fn() }) {
  const [tab, setTab] = useState<TeamTab>(initial)
  return <TeamDetailPage teamId="team-1" tab={tab} navigate={(path, options) => {
    const next = path.split('/')[3] as TeamTab | undefined
    if (path.startsWith('/teams/team-1')) setTab(next ?? 'profile')
    else navigate(path, options)
  }} />
}

const chooseFlow = (name: string) => {
  fireEvent.click(screen.getByRole('button', { name: '当前流程' }))
  fireEvent.mouseDown(within(screen.getByRole('listbox', { name: '当前流程' })).getByRole('option', { name }))
}
const tab = (name: string) => fireEvent.click(screen.getByRole('tab', { name }))

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('team with several flows', () => {
  it('shows the second flow and keeps each flow’s unsaved edits apart', async () => {
    const calls = serve()
    render(<Harness />)
    expect(await screen.findByRole('button', { name: '编辑步骤 起草' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: '编辑步骤 复核' })).toBeNull()
    expect(screen.getByText('团队共 2 条流程，保存和发布对全部流程一起生效')).toBeTruthy()

    chooseFlow('复核流程')
    expect(screen.getByRole('button', { name: '编辑步骤 复核' })).toBeTruthy()
    expect(screen.getByText(`${document.workflows[1].graph_definition.nodes.length} 个节点`)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '编辑步骤 复核' }))
    fireEvent.change(screen.getByRole('textbox', { name: '工作要求' }), { target: { value: 'B 的新要求' } })
    fireEvent.click(screen.getByRole('button', { name: '流程基本信息' }))
    fireEvent.change(screen.getByRole('textbox', { name: '流程名称' }), { target: { value: '复核流程二' } })
    fireEvent.click(screen.getByRole('button', { name: '完成' }))

    chooseFlow('起草流程')
    fireEvent.click(screen.getByRole('button', { name: '编辑步骤 起草' }))
    expect((screen.getByRole('textbox', { name: '工作要求' }) as HTMLTextAreaElement).value).toBe('交付要求')
    fireEvent.change(screen.getByRole('textbox', { name: '工作要求' }), { target: { value: 'A 的新要求' } })

    // Leaving the flow page and coming back keeps the choice and both edits.
    tab('成员')
    tab('流程')
    expect(screen.getByRole('button', { name: '当前流程' }).textContent).toContain('起草流程')
    chooseFlow('复核流程二')
    fireEvent.click(screen.getByRole('button', { name: '编辑步骤 复核' }))
    expect((screen.getByRole('textbox', { name: '工作要求' }) as HTMLTextAreaElement).value).toBe('B 的新要求')

    // Choosing flows never saves or publishes.
    expect(calls.filter((call) => call.method !== 'GET')).toEqual([])

    fireEvent.click(screen.getByRole('button', { name: '保存草稿' }))
    await waitFor(() => expect(calls.some((call) => call.method === 'PUT')).toBe(true))
    const saved = calls.find((call) => call.method === 'PUT')!.body!.document as DevelopmentDocument
    const requirement = (flow: number, label: string) => saved.workflows[flow].graph_definition.nodes.find((node) => node.label === label)?.config?.result_requirement
    expect(saved.workflows.map((flow) => flow.name)).toEqual(['起草流程', '复核流程二'])
    expect(requirement(0, '起草')).toBe('A 的新要求')
    expect(requirement(1, '起草')).toBe('交付要求')
    expect(requirement(1, '复核')).toBe('B 的新要求')
  })

  it('runs the trial of the flow chosen on the flow page', async () => {
    const calls = serve()
    render(<Harness />)
    await screen.findByRole('button', { name: '编辑步骤 起草' })
    chooseFlow('复核流程')
    tab('试跑')
    expect(screen.getByRole('button', { name: '试跑的流程' }).textContent).toContain('复核流程')
    fireEvent.change(screen.getByRole('textbox', { name: '试跑任务' }), { target: { value: '复核这份合同' } })
    fireEvent.click(screen.getByRole('button', { name: '开始试跑' }))
    await waitFor(() => expect(calls.some((call) => call.url.endsWith('/trials'))).toBe(true))
    expect(calls.find((call) => call.url.endsWith('/trials'))!.body!.workflow_id).toBe('flow-b')

    // Choosing on the trial page carries back to the flow page.
    fireEvent.click(screen.getByRole('button', { name: '试跑的流程' }))
    fireEvent.mouseDown(within(screen.getByRole('listbox', { name: '试跑的流程' })).getByRole('option', { name: '起草流程' }))
    tab('流程')
    expect(screen.getByRole('button', { name: '当前流程' }).textContent).toContain('起草流程')
  })

  it('keeps a single-flow team to its name without a chooser', async () => {
    serve()
    draft.document = { ...document, workflows: [document.workflows[0]] }
    try {
      render(<Harness />)
      expect(await screen.findByText('起草流程')).toBeTruthy()
      expect(screen.queryByRole('button', { name: '当前流程' })).toBeNull()
      expect(screen.queryByText(/条流程，保存和发布/)).toBeNull()
    } finally {
      draft.document = document
    }
  })
})

describe('live team changed after the draft was opened', () => {
  function serveChanged(rebase: (keepDraft: boolean) => Response) {
    const calls: Call[] = []
    vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
      const method = init.method ?? 'GET'
      const body = init.body ? JSON.parse(String(init.body)) : undefined
      calls.push({ url, method, body })
      if (url.endsWith('/development/rebase')) return rebase(Boolean(body?.keep_draft))
      if (url.endsWith('/teams/team-1/development')) return new Response(JSON.stringify({ ...draft, baseline_changed: true }), { status: 200 })
      if (url.includes('business-capabilities')) return new Response(JSON.stringify({ available: true, capabilities: [] }), { status: 200 })
      if (url.endsWith('/runtimes')) return new Response(JSON.stringify({ runtimes: [] }), { status: 200 })
      return new Response('[]', { status: 200 })
    }))
    return calls
  }

  it('merges the live changes and says what it took in', async () => {
    const merged = { ...draft, revision: 4, document: { ...document, name: '合同审核组' }, rebase_changes: [{ kind: 'team', field: 'name', name: '合同审核组', action: 'taken' }] }
    const calls = serveChanged(() => new Response(JSON.stringify(merged), { status: 200 }))
    render(<Harness initial="profile" />)
    fireEvent.click(await screen.findByRole('button', { name: '读取线上变化并合并' }))
    expect(await screen.findByText('已合并线上变化：团队名称（已采用线上）。')).toBeTruthy()
    expect(screen.queryByRole('button', { name: '读取线上变化并合并' })).toBeNull()
    expect(calls.find((call) => call.url.endsWith('/rebase'))?.body).toEqual({ expected_revision: 3, keep_draft: false })
  })

  it('names what both sides changed and keeps the draft’s edits on request', async () => {
    const conflict = { error: 'development_rebase_conflict', message: '冲突', conflicts: [{ kind: 'team', field: 'objective', name: '合同团队' }] }
    const calls = serveChanged((keepDraft) => keepDraft
      ? new Response(JSON.stringify({ ...draft, revision: 4, rebase_changes: [{ kind: 'team', field: 'objective', name: '合同团队', action: 'kept' }] }), { status: 200 })
      : new Response(JSON.stringify(conflict), { status: 409 }))
    render(<Harness initial="profile" />)
    fireEvent.click(await screen.findByRole('button', { name: '读取线上变化并合并' }))
    expect(await screen.findByText('草稿和线上都改了这些地方：团队目标。')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '保留草稿的修改' }))
    expect(await screen.findByText('已合并线上变化：团队目标（保留草稿）。')).toBeTruthy()
    expect(calls.filter((call) => call.url.endsWith('/rebase')).map((call) => call.body?.keep_draft)).toEqual([false, true])
  })
})
