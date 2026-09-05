// @vitest-environment jsdom

import { act, cleanup, fireEvent, render, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ChatConversationViewNode, RunningToolCall, ToolResultNode } from '@deepseek-ai/dsh-client-ui-chat/client'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { TeamListRow, teamListModel } from '../src/client/TeamListRow.tsx'
import { zh } from '../src/client/locales.ts'
import { workTaskModel } from '../src/client/work-task-model.ts'

type Props = Parameters<typeof TeamListRow>[0]
const t: Props['t'] = makeTranslate(zh, commonZh)

afterEach(cleanup)

function settled(text: string, over: Partial<ToolResultNode> = {}): ToolResultNode {
  return {
    kind: 'tool-result', seq: 3, time: 3_000, callId: 'call-team-list',
    call: { name: 'mcp__weave__team_list', argsRaw: '{}' }, callTime: 2_000,
    content: [{ type: 'text', text }], isError: false, subCalls: [], ...over,
  }
}

function running(): RunningToolCall {
  return {
    callId: 'call-team-list', name: 'mcp__weave__team_list', argsRaw: '{}',
    turn: 1, step: 1, time: 2_000, subCalls: [],
  }
}

function props(block: Props['block'], nodes: ChatConversationViewNode[] = [], projection: unknown = null): Props {
  return {
    callId: block.callId, toolName: 'mcp__weave__team_list', block,
    openFile: vi.fn(), t,
    useChat: (select: (value: unknown) => unknown) => select({ nodes: { values: () => nodes } }),
    useProjection: () => projection,
  } as unknown as Props
}

describe('TeamListRow', () => {

  it.each(['user', 'steering', 'tool-call'] as const)('retires an old directory after a later %s decision without removing its team facts', (kind) => {
    const block = settled(JSON.stringify([{
      team_id: 'team-1', name: '材料核验团队', status: 'active', workflow_available: true,
    }]))
    const subsequent: ChatConversationViewNode = {
      key: 'later-decision', id: 'later-decision', target: 'chat', kind, anchorSeq: 4,
      location: { kind: 'session' }, visibility: 'visible',
      data: kind === 'tool-call' ? { root: {
        ...running(), name: 'mcp__weave__team_create', callId: 'create-team',
      } } : { kind, seq: 4, time: 4_000, source: null, content: [{ type: 'text', text: '改为创建专门的核验团队' }] },
    }
    const selectTeam = vi.fn()
    const view = render(<TeamListRow {...props(block)} selectTeam={selectTeam} />)
    expect(view.getByRole('button', { name: '选用并查看方案' })).toBeTruthy()
    view.rerender(<TeamListRow {...props(block, [subsequent])} selectTeam={selectTeam} />)
    expect(view.queryByRole('button', { name: '选用并查看方案' })).toBeNull()
    expect(view.getByText('该目录为历史记录，请以当前方案为准')).toBeTruthy()
    expect(view.getByText('材料核验团队')).toBeTruthy()
    expect(selectTeam).not.toHaveBeenCalled()
  })

  it('keeps the existing preparation as the current proposal instead of offering an older directory again', () => {
    const selectTeam = vi.fn()
    const projection = { ...workTaskModel([]), runId: '', status: 'preparing',
      preparation: { callId: 'create-current', buildId: 'build-current', state: 'building', error: '' } }
    const view = render(<TeamListRow {...props(settled(JSON.stringify([{
      team_id: 'team-1', name: '材料核验团队', status: 'active', workflow_available: true,
    }])), [], projection)} selectTeam={selectTeam} />)
    expect(view.queryByRole('button', { name: '选用并查看方案' })).toBeNull()
    expect(view.getByText('该目录为历史记录，请以当前方案为准')).toBeTruthy()
    expect(view.getByText('材料核验团队')).toBeTruthy()
    expect(selectTeam).not.toHaveBeenCalled()
  })

  it('submits a selection once, allows retry after failure, and keeps success selected', async () => {
    let rejectSelection: (error: Error) => void = () => {}
    const selectTeam = vi.fn().mockImplementationOnce(() => new Promise<void>((_resolve, reject) => { rejectSelection = reject }))
      .mockResolvedValueOnce(undefined)
    const view = render(<TeamListRow {...props(settled(JSON.stringify([{
      team_id: 'team-1', name: '材料核验团队', status: 'active', workflow_available: true,
    }])))} selectTeam={selectTeam} />)
    const select = view.getByRole('button', { name: '选用并查看方案' })
    fireEvent.click(select)
    fireEvent.click(select)
    expect(selectTeam).toHaveBeenCalledTimes(1)
    expect(selectTeam).toHaveBeenCalledWith('team-1', '材料核验团队')
    expect(view.getByRole('button', { name: '正在提交选择' }).hasAttribute('disabled')).toBe(true)
    await act(async () => { rejectSelection(new Error('offline')) })
    expect(view.getByRole('alert').textContent).toBe('选择未提交，请重试')
    expect(view.container.textContent).not.toContain('offline')
    fireEvent.click(view.getByRole('button', { name: '选用并查看方案' }))
    await waitFor(() => { expect(view.getByRole('button', { name: '已选择，请在对话中查看方案' }).hasAttribute('disabled')).toBe(true) })
    fireEvent.click(view.getByRole('button', { name: '已选择，请在对话中查看方案' }))
    expect(selectTeam).toHaveBeenCalledTimes(2)
    expect(view.queryByRole('alert')).toBeNull()
  })

  it('renders candidate business facts and omits internal health and ids', () => {
    const payload = JSON.stringify([{
      team_id: 'team-secret-id', name: '超级项目论证团队', status: 'active',
      objective: '推演超级项目的阶段可行性', primary_scenario: '复杂 FDE 项目',
      success_criteria: '形成可审查的阶段结论', responsibilities: ['需求拆解', '业务推演'],
      default_workflow_id: 'wf-internal', workflow_available: true, health: 'warning',
    }])
    const view = render(<TeamListRow {...props(settled(payload))} />)
    expect(view.container.textContent).toContain('可用团队')
    expect(view.container.textContent).toContain('发现 1 个候选团队')
    expect(view.container.textContent).toContain('超级项目论证团队')
    expect(view.container.textContent).toContain('可通过默认工作流派发')
    expect(view.container.textContent).toContain('需求拆解 · 业务推演')
    expect(view.container.textContent).not.toContain('warning')
    expect(view.container.textContent).not.toContain('team-secret-id')
    expect(view.container.textContent).not.toContain('wf-internal')
  })

  it('keeps running, empty, failed, stopped, and malformed results honest', () => {
    expect(teamListModel(running()).state).toBe('running')
    expect(teamListModel(settled('[]')).state).toBe('empty')
    expect(teamListModel(settled('bad json')).state).toBe('invalid')
    expect(teamListModel(settled('failure', { isError: true })).state).toBe('error')
    expect(teamListModel(settled('', {
      content: [], error: { name: 'InterruptedError', code: 'interrupted' },
    })).state).toBe('stopped')

    const failed = render(<TeamListRow {...props(settled('http_403', { isError: true }))} />)
    expect(failed.container.textContent).toContain('读取团队失败')
    expect(failed.container.textContent).not.toContain('http_403')
  })

  it('marks unavailable teams without treating them as selected', () => {
    const payload = JSON.stringify([{
      team_id: 't1', name: '待修复团队', status: 'needs_repair', objective: '研究',
      responsibilities: [], workflow_available: false,
    }, {
      team_id: 't2', name: '未发布团队', status: 'active', objective: '分析',
      responsibilities: [], workflow_available: false,
    }, {
      team_id: 't3', name: '组建中团队', status: 'building', objective: '分析',
      responsibilities: [], workflow_available: true,
    }])
    const view = render(<TeamListRow {...props(settled(payload))} />)
    expect(view.container.textContent).toContain('需要修复')
    expect(view.container.textContent).toContain('缺少默认工作流')
    expect(view.container.textContent).toContain('正在组建')
    expect(view.container.querySelectorAll('[data-dispatchable]')).toHaveLength(0)
    expect(view.container.textContent).not.toContain('已选择')
  })
})
