import { describe, expect, it } from 'vitest'
import type { ChatConversationViewNode, ToolResultNode } from '@deepseek-ai/dsh-client-ui-chat/client'
import { workTaskFactsStale, workTaskModel } from '../src/client/work-task-model.ts'

let sequence = 1

function tool(name: string, args: unknown, value: unknown, isError = false): ChatConversationViewNode {
  const seq = sequence++
  const root: ToolResultNode = {
    kind: 'tool-result',
    seq,
    time: seq,
    callId: `call-${seq}`,
    call: { name, argsRaw: JSON.stringify(args) },
    callTime: seq - 0.5,
    content: [{ type: 'text', text: JSON.stringify(value) }],
    isError,
    subCalls: [],
  }
  return {
    key: `tool:${seq}`,
    id: `${seq}`,
    target: 'chat',
    anchorSeq: seq,
    location: { kind: 'session' },
    visibility: 'visible',
    kind: 'tool-call',
    data: { root },
  }
}

describe('workTaskModel', () => {
  it('does not age terminal facts into a stale warning', () => {
    const observedAt = Date.parse('2026-08-31T08:00:00Z')
    const now = observedAt + 60_000
    expect(workTaskFactsStale('running', observedAt, now)).toBe(true)
    expect(workTaskFactsStale('completed', observedAt, now)).toBe(false)
    expect(workTaskFactsStale('failed', observedAt, now)).toBe(false)
    expect(workTaskFactsStale('stopped', observedAt, now)).toBe(false)
  })

  it('keeps listed teams available as a first-class chooser before dispatch', () => {
    const model = workTaskModel([tool('mcp__weave__team_list', {}, [
      {
        team_id: 'team-ready', name: '可派发团队', objective: '完成复杂业务推演',
        status: 'active', workflow_available: true,
      },
      {
        team_id: 'team-draft', name: '待配置团队', objective: '尚未配置工作流',
        status: 'active', workflow_available: false,
      },
    ])])

    expect(model.runId).toBe('')
    expect(model.teamCandidates).toEqual([
      {
        teamId: 'team-ready', name: '可派发团队', objective: '完成复杂业务推演',
        status: 'active', workflowAvailable: true,
      },
      {
        teamId: 'team-draft', name: '待配置团队', objective: '尚未配置工作流',
        status: 'active', workflowAvailable: false,
      },
    ])
  })

  it('keeps a queued task bound to its team and rejects another run deliverable', () => {
    const nodes = [
      tool('mcp__weave__team_list', {}, [{
        team_id: 'corona-mission-baseline', name: '日冕任务基线团队',
      }]),
      tool('mcp__weave__team_dispatch', { team_id: 'corona-mission-baseline' }, {
        run_id: 'run-current', workflow_name: 'corona-workflow',
      }),
      tool('mcp__weave__dispatch_status', { client_request_id: 'request-current' }, {
        run_id: 'run-current', status: 'queued', completed_stages: 0, total_stages: 9,
      }),
      tool('agent', { agent: 'corona-mission-baseline', run_id: 'run-current' }, { error: 'http_404' }, true),
      tool('mcp__weave__deliverable_list', {}, [{ run_id: 'run-old', id: 'old-file' }]),
    ]

    expect(workTaskModel(nodes)).toMatchObject({
      detected: true,
      status: 'queued',
      teamName: '日冕任务基线团队',
      workflowName: 'corona-workflow',
      runId: 'run-current',
      completedStages: 0,
      totalStages: 9,
      deliverableCount: 0,
      blocker: 'runtime-missing',
    })
  })

  it('projects stages, runtimes, human decisions, and exact-run deliverables', () => {
    const nodes = [
      tool('mcp__weave__team_dispatch', { team_id: 'team-a' }, { run_id: 'run-a' }),
      tool('mcp__weave__team_run_status', { run_id: 'run-a' }, {
        run_id: 'run-a', status: 'running', current_stage: '联合审查',
        stages: [
          { name: '定义任务', status: 'completed' },
          { name: '联合审查', status: 'running' },
        ],
        runtimes: [{ role: '系统分析员', provider: 'local', model: 'luna', status: 'running' }],
      }),
      tool('mcp__weave__human_task_list', {}, [{ run_id: 'run-a', id: 'human-1' }]),
      tool('mcp__weave__deliverable_list', {}, [
        {
          run_id: 'run-a', id: 'file-1', title: '首轮推演结论', content_type: 'text/markdown',
          content: '# 结论\n风险边界已确认。', metadata: { artifact_kind: 'final' }, created_at: '2026-08-30T10:00:00Z',
        },
        {
          run_id: 'run-a', id: 'file-2', title: '约束清单', content_type: 'text/markdown',
          content: '- 预算约束', metadata: '{"artifact_kind":"stage"}', created_at: '2026-08-30T09:00:00Z',
        },
      ]),
    ]

    const model = workTaskModel(nodes)
    expect(model.status).toBe('waiting')
    expect(model.latestStage).toBe('联合审查')
    expect(model.completedStages).toBe(1)
    expect(model.totalStages).toBe(2)
    expect(model.humanTaskCount).toBe(1)
    expect(model.deliverableCount).toBe(2)
    expect(model.deliverables).toEqual([
      {
        id: 'file-1', title: '首轮推演结论', kind: 'final', contentType: 'text/markdown',
        preview: '# 结论\n风险边界已确认。', createdAt: '2026-08-30T10:00:00Z',
      },
      {
        id: 'file-2', title: '约束清单', kind: 'stage', contentType: 'text/markdown',
        preview: '- 预算约束', createdAt: '2026-08-30T09:00:00Z',
      },
    ])
    expect(model.runtimes).toEqual([{
      name: '系统分析员', detail: 'local · luna', status: 'running',
    }])
  })

  it('keeps dispatch lifecycle authoritative when the terminal projection is missing', () => {
    const nodes = [
      tool('mcp__weave__team_list', {}, {
        teams: [{ team_id: 'corona-mission-baseline', name: '日冕任务基线团队' }],
      }),
      tool('mcp__weave__team_dispatch', { team_id: 'corona-mission-baseline' }, {
        run_id: 'run-current',
      }),
      tool('mcp__weave__dispatch_status', {}, {
        run_id: 'run-current', status: 'queued',
        workflow_progress: { status: 'queued', completed_stages: 0, total_stages: 9 },
      }),
      tool('mcp__weave__team_run_status', {}, {
        runs: [{ run_id: 'run-current', status: 'running', classification: 'terminal_missing' }],
      }),
    ]

    expect(workTaskModel(nodes)).toMatchObject({
      status: 'queued',
      teamName: '日冕任务基线团队',
      completedStages: 0,
      totalStages: 9,
      blocker: 'runtime-missing',
    })
  })

  it('shows a parked fanout run as waiting', () => {
    const nodes = [
      tool('mcp__weave__team_dispatch', { team_id: 'team-a' }, { run_id: 'run-a' }),
      tool('mcp__weave__team_run_activity', { run_id: 'run-a' }, {
        run_id: 'run-a', status: 'parked', wait_kind: 'fanout',
      }),
    ]

    expect(workTaskModel(nodes).status).toBe('waiting')
  })

  it('derives member lanes only from reported execution facts', () => {
    const nodes = [
      tool('mcp__weave__team_dispatch', { team_id: 'team-a' }, { run_id: 'run-a' }),
      tool('mcp__weave__team_run_activity', { run_id: 'run-a' }, {
        run_id: 'run-a', status: 'succeeded',
        members: [{
          agent_id: 'worker-1', name: '物理复核员', duty: '复核数量级', role: 'worker', status: 'completed',
          runtime: { runtime_id: 'runtime-1', provider: 'openai', model: 'gpt-5.6-luna' },
          stages: [{
            node_id: 'physics', name: '物理复核', status: 'completed',
            inputs: [{ name: 'brief', expected_type: 'text', source: 'run_input', path: '$' }],
            output_refs: ['file-1'],
            started_at: '2026-08-30T10:00:00Z', completed_at: '2026-08-30T10:00:03Z', duration_ms: 3000,
            tool_calls: 1, tools: [{ call_id: 'tool-1', name: 'calculator', status: 'ok',
              started_at: '2026-08-30T10:00:01Z', completed_at: '2026-08-30T10:00:02Z' }],
          }],
        }],
      }),
    ]

    expect(workTaskModel(nodes).members).toEqual([{
      agentId: 'worker-1', name: '物理复核员', duty: '复核数量级', role: 'worker', status: 'completed',
      runtime: 'runtime-1 · openai/gpt-5.6-luna',
      stages: [{
        nodeId: 'physics', name: '物理复核', status: 'completed',
        inputs: [{ name: 'brief', expectedType: 'text', source: 'run_input', nodeId: '', path: '$', summary: '' }],
        outputRefs: ['file-1'],
        startedAt: '2026-08-30T10:00:00Z', completedAt: '2026-08-30T10:00:03Z', durationMs: 3000, toolCalls: 1,
        tools: [{ callId: 'tool-1', name: 'calculator', status: 'ok',
          startedAt: '2026-08-30T10:00:01Z', completedAt: '2026-08-30T10:00:02Z' }],
      }],
    }])
  })

  it('does not turn an ordinary conversation into a Weave task', () => {
    expect(workTaskModel([]).detected).toBe(false)
  })
})
