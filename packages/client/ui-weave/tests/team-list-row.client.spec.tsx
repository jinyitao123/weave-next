// @vitest-environment jsdom

import { cleanup, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { RunningToolCall, ToolResultNode } from '@deepseek-ai/dsh-client-ui-chat/client'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { TeamListRow, teamListModel } from '../src/client/TeamListRow.tsx'
import { zh } from '../src/client/locales.ts'

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

function props(block: Props['block']): Props {
  return {
    callId: block.callId, toolName: 'mcp__weave__team_list', block,
    openFile: vi.fn(), t,
  } as unknown as Props
}

describe('TeamListRow', () => {
  it('renders candidate business facts and omits internal health and ids', () => {
    const payload = JSON.stringify([{
      team_id: 'team-secret-id', name: '超级项目论证团队', status: 'active',
      objective: '推演超级项目的阶段可行性', primary_scenario: '复杂 FDE 项目',
      success_criteria: '形成可审查的阶段结论', responsibilities: ['需求拆解', '业务推演'],
      default_workflow_id: 'wf-internal', workflow_available: true, health: 'warning',
    }])
    const view = render(<TeamListRow {...props(settled(payload))} />)
    expect(view.container.textContent).toContain('团队匹配')
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
    expect(failed.container.textContent).toContain('http_403')
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
