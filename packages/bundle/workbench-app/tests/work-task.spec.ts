import { describe, expect, it } from 'vitest'
import type { SessionEvent } from '@deepseek-ai/dsh-session'
import { applyWorkTaskProjection, workbenchTeamRoutingSection, workTaskProjectionDefinition } from '../src/index.ts'

const event = (type: string, data: unknown, seq = 0, time = 100): SessionEvent => ({
  type, data, seq, time,
} as SessionEvent)

const call = (id: string, name: string, args: unknown, seq: number): SessionEvent => event('tool/call', {
  turn: 1, step: 1, callId: id, name, arguments: JSON.stringify(args),
}, seq, 100 + seq)

const result = (id: string, value: unknown, seq: number): SessionEvent => event('tool/result', {
  turn: 1,
  step: 1,
  message: {
    id: `message-${id}`,
    role: 'user',
    source: { kind: 'tool', callId: id },
    content: [{ type: 'tool-result', toolCallId: id, content: [{ type: 'text', text: JSON.stringify(value) }] }],
  },
}, seq, 100 + seq)

describe('Workbench work-task projection', () => {
  it('hands a dispatched Weave run to background ownership', () => {
    expect(workbenchTeamRoutingSection.name).toBe('workbench:team-routing')
    expect(workbenchTeamRoutingSection.text).toContain('do not create or update a DSH goal')
    expect(workbenchTeamRoutingSection.text).toContain('make at most one activity or status call')
    expect(workbenchTeamRoutingSection.text).toContain('do not save a duplicate foreground deliverable')
    expect(workbenchTeamRoutingSection.text).toContain('short user-facing completion summary')
    expect(workbenchTeamRoutingSection.text).toContain('Keep internal run IDs')
    expect(workbenchTeamRoutingSection.text).toContain('Do not include internal identifiers')
    expect(workbenchTeamRoutingSection.text).not.toContain('return the team, run ID')
  })

  it('materializes a dispatch with its matched team and durable request identity', () => {
    let state = workTaskProjectionDefinition.init()
    const events = [
      call('list', 'mcp__weave__team_list', {}, 0),
      result('list', { teams: [{ id: 'team-1', name: '日冕首轮推演团队' }] }, 1),
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1', workflow_id: 'baseline' }, 2),
      result('dispatch', { client_request_id: 'request-1', run_id: 'run-1', status: 'queued' }, 3),
    ]
    for (const item of events) state = applyWorkTaskProjection(state, item)

    expect(state.task).toMatchObject({
      clientRequestId: 'request-1', runId: 'run-1', teamId: 'team-1',
      teamName: '日冕首轮推演团队', workflowName: 'baseline', status: 'queued', blocker: 'queued',
      brief: '', pendingAction: null,
    })
    expect(state.task?.attempts).toHaveLength(1)
  })

  it('keeps the business name when a newly created team is dispatched immediately', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('create', 'mcp__weave__team_create', { definition: { display_name: '日冕计划任务定义与先期论证筹备组' } }, 0),
      result('create', { team_id: 'team-new', status: 'ready' }, 1),
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-new', task: '开展先期论证' }, 2),
      result('dispatch', { run_id: 'run-new', status: 'queued' }, 3),
    ]) state = applyWorkTaskProjection(state, item)

    expect(state.task).toMatchObject({
      teamId: 'team-new', teamName: '日冕计划任务定义与先期论证筹备组', brief: '开展先期论证', status: 'queued',
    })
  })

  it('refreshes a restored task when the team business name becomes available', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1', task: '继续研究' }, 0),
      result('dispatch', { run_id: 'run-1', status: 'running' }, 1),
      call('list', 'mcp__weave__team_list', {}, 2),
      result('list', { teams: [{ team_id: 'team-1', name: 'coronal-program-research', display_name: '日冕计划任务定义与先期论证筹备组' }] }, 3),
    ]) state = applyWorkTaskProjection(state, item)

    expect(state.task?.teamName).toBe('日冕计划任务定义与先期论证筹备组')
  })

  it('folds reconnect-safe progress and runtime assignment from dispatch status', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('dispatch', { client_request_id: 'request-1', run_id: 'run-1', status: 'queued' }, 1),
      call('status', 'mcp__weave__dispatch_status', { client_request_id: 'request-1' }, 2),
      result('status', {
        status: 'running', workflow_progress: { completed_stages: 3, total_stages: 9, latest_stage: '约束推演' },
        runtime_assignment: { lead: { model: 'gpt-5.6-luna', provider: 'openai' } },
      }, 3),
    ]) state = applyWorkTaskProjection(state, item)

    expect(state.task).toMatchObject({ status: 'running', completedStages: 3, totalStages: 9, latestStage: '约束推演' })
    expect(state.task?.runtimes).toEqual([{ name: 'lead', detail: 'openai · gpt-5.6-luna', status: 'running' }])
  })

  it('shows a parked server run as waiting instead of inheriting a prior terminal state', () => {
    let state = workTaskProjectionDefinition.init()
    state = applyWorkTaskProjection(state, event('weave/work-task', {
      clientRequestId: 'request-1', runId: 'run-1', teamId: 'team-1', teamName: 'Team', workflowName: 'baseline',
      status: 'stopped', completedStages: 0, totalStages: 0, latestStage: '', runtimes: [],
      humanTaskCount: 0, deliverableCount: 0, deliverables: [], blocker: 'none', updatedAt: 100,
      brief: 'first', attempts: [], pendingAction: null, completeness: {}, observedAt: 100,
    }))
    state = applyWorkTaskProjection(state, call('status', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 1))
    state = applyWorkTaskProjection(state, result('status', {
      run_id: 'run-1', status: 'parked', wait_kind: 'fanout', completed_stages: 3,
      stages: [
        { name: '任务定义', status: 'completed' },
        { name: '物理复核', status: 'completed' },
        { name: '证据分析', status: 'completed' },
      ],
    }, 2))

    expect(state.task?.status).toBe('waiting')
    expect(state.task).toMatchObject({ completedStages: 3, latestStage: '证据分析' })
  })

  it('moves retained runtime evidence into the exact terminal state', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('dispatch', { run_id: 'run-1', status: 'running' }, 1),
      call('running', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 2),
      result('running', {
        run_id: 'run-1', status: 'running',
        runtimes: [{ name: 'teamrun:worker-1', status: 'running' }],
      }, 3),
      call('completed', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 4),
      result('completed', { run_id: 'run-1', status: 'completed', runtimes: [] }, 5),
    ]) state = applyWorkTaskProjection(state, item)

    expect(state.task?.status).toBe('completed')
    expect(state.task?.runtimes).toEqual([{ name: 'teamrun:worker-1', detail: '', status: 'completed' }])
  })

  it('starts a new run without inheriting terminal facts from the prior attempt', () => {
    let state = workTaskProjectionDefinition.init()
    state = applyWorkTaskProjection(state, event('weave/work-task', {
      clientRequestId: 'request-1', runId: 'run-1', teamId: 'team-1', teamName: 'Team', workflowName: 'baseline',
      status: 'failed', completedStages: 2, totalStages: 3, latestStage: '汇总',
      members: [{ agentId: 'worker-1', name: '旧成员', duty: '', role: 'worker', status: 'failed', runtime: '', stages: [] }],
      corrections: [{ correctionId: 'correction-1', targetKind: 'team', targetMemberId: '', instruction: '旧纠偏',
        status: 'ready', safeNodeId: 'review', restartNodeId: 'review', affectedNodeIds: ['review'], preservedNodeIds: [], requestedAt: '' }],
      runtimes: [{ name: 'old-runtime', detail: '', status: 'failed' }], humanTaskCount: 1,
      deliverableCount: 1, deliverables: [{ id: 'old', title: '旧交付', kind: 'stage', contentType: 'text/plain', preview: '', content: '', truncated: false, createdAt: '' }],
      blocker: 'failed', updatedAt: 100, brief: '旧任务', attempts: [], pendingAction: null, actionError: '',
      completeness: { run: 'complete' }, startedAt: '2026-08-30T10:00:00Z', finishedAt: '2026-08-30T10:01:00Z',
      tokensIn: 100, tokensOut: 20, costUSD: 1, outcome: 'needs-revision', outcomeNote: '旧判断', observedAt: 100,
    }))
    state = applyWorkTaskProjection(state, call('rerun', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 1))
    state = applyWorkTaskProjection(state, result('rerun', {
      run_id: 'run-2', client_request_id: 'request-2', status: 'queued',
    }, 2))

    expect(state.task).toMatchObject({
      runId: 'run-2', status: 'queued', completedStages: 0, totalStages: 0, latestStage: '',
      members: [], corrections: [], runtimes: [], humanTaskCount: 0, deliverableCount: 0, deliverables: [],
      completeness: {}, startedAt: '', finishedAt: '', tokensIn: 0, tokensOut: 0, costUSD: 0,
      outcome: 'unrated', outcomeNote: '',
    })
  })

  it('persists exact member stages, input sources, outputs, and runtime facts', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('dispatch', { run_id: 'run-1', status: 'running' }, 1),
      call('activity', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 2),
      result('activity', {
        run_id: 'run-1', status: 'succeeded',
        members: [{
          agent_id: 'worker-1', name: '约束分析员', duty: '复核关键假设', role: 'worker', status: 'completed',
          runtime: { runtime_id: 'runtime-1', name: 'mac-codex-live', engine: 'codex', provider: 'openai', model: 'gpt-5.6-luna' },
          stages: [{
            node_id: 'verify', name: '约束复核', status: 'completed',
            inputs: [{ name: 'facts', expected_type: 'text', source: 'node_output', node_id: 'draft', path: '$.facts' }],
            output_refs: ['deliverable-1'],
            started_at: '2026-08-30T10:00:00Z', completed_at: '2026-08-30T10:00:04Z', duration_ms: 4000,
            tool_calls: 1, tools: [{ call_id: 'tool-1', name: 'evidence_lookup', status: 'ok',
              started_at: '2026-08-30T10:00:01Z', completed_at: '2026-08-30T10:00:02Z',
              input: '{"query":"关键假设"}', output: '2 条证据' }],
          }],
        }],
      }, 3),
    ]) state = applyWorkTaskProjection(state, item)

    expect(state.task?.members).toEqual([{
      agentId: 'worker-1', name: '约束分析员', duty: '复核关键假设', role: 'worker', status: 'completed',
      runtime: 'mac-codex-live · codex · openai/gpt-5.6-luna',
      stages: [{
        nodeId: 'verify', name: '约束复核', status: 'completed',
        inputs: [{ name: 'facts', expectedType: 'text', source: 'node_output', nodeId: 'draft', path: '$.facts', summary: '' }],
        outputRefs: ['deliverable-1'],
        startedAt: '2026-08-30T10:00:00Z', completedAt: '2026-08-30T10:00:04Z', durationMs: 4000, toolCalls: 1,
        failureClass: '', failureReason: '', retryable: false,
        tools: [{ callId: 'tool-1', name: 'evidence_lookup', status: 'ok',
          startedAt: '2026-08-30T10:00:01Z', completedAt: '2026-08-30T10:00:02Z',
          input: '{"query":"关键假设"}', output: '2 条证据' }],
      }],
    }])
  })

  it('retains a ready correction impact plan for reconnect-safe confirmation', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('dispatch', { run_id: 'run-1', status: 'running' }, 1),
      call('activity', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 2),
      result('activity', { run_id: 'run-1', status: 'parked', wait_kind: 'correction', corrections: [{
        correction_id: 'correction-1', target_kind: 'member', target_member_id: 'worker-1',
        instruction: '重查证据边界', status: 'ready', safe_node_id: 'deliver', restart_node_id: 'review',
        affected_node_ids: ['review', 'deliver'], preserved_node_ids: ['research'], requested_at: '2026-08-30T10:00:00Z',
      }] }, 3),
    ]) state = applyWorkTaskProjection(state, item)

    expect(state.task?.status).toBe('waiting')
    expect(state.task?.corrections[0]).toEqual({
      correctionId: 'correction-1', targetKind: 'member', targetMemberId: 'worker-1',
      instruction: '重查证据边界', status: 'ready', safeNodeId: 'deliver', restartNodeId: 'review',
      affectedNodeIds: ['review', 'deliver'], preservedNodeIds: ['research'], requestedAt: '2026-08-30T10:00:00Z',
    })
  })

  it('lets a host snapshot remain authoritative after the conversation turn ends', () => {
    const state = workTaskProjectionDefinition.init()
    const next = applyWorkTaskProjection(state, event('weave/work-task', {
      clientRequestId: 'request-1', runId: 'run-1', teamId: 'team-1', teamName: 'Team', workflowName: '',
      status: 'completed', completedStages: 9, totalStages: 9, latestStage: '交付', runtimes: [],
      humanTaskCount: 0, deliverableCount: 1,
      deliverables: [{
        id: 'file-1', title: '最终报告', kind: 'final', contentType: 'text/markdown',
        preview: '# 完成', content: '# 完成', truncated: false, createdAt: '2026-08-30T10:00:00Z',
      }],
      blocker: 'none', updatedAt: 999,
      brief: '任务简报', attempts: [], pendingAction: null, completeness: { run: 'complete' }, observedAt: 999,
    }))
    expect(next.task?.status).toBe('completed')
    expect(workTaskProjectionDefinition.wire.view(next)?.deliverableCount).toBe(1)
    expect(workTaskProjectionDefinition.wire.view(next)?.deliverables[0]?.title).toBe('最终报告')
  })

  it('retains ordered run attempts and exactly one durable pending action', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('first', 'mcp__weave__team_dispatch', { team_id: 'team-1', task: '原始简报' }, 0),
      result('first', { client_request_id: 'request-1', run_id: 'run-1', status: 'cancelled' }, 1),
      event('weave/work-task-action', {
        pendingAction: {
          kind: 'rerun', targetRunId: 'run-1', idempotencyKey: '',
          clientRequestId: 'request-2', brief: '完整修订简报', requestedAt: 102,
        },
      }, 2, 102),
    ]) state = applyWorkTaskProjection(state, item)

    expect(state.task?.pendingAction).toMatchObject({ kind: 'rerun', targetRunId: 'run-1' })

    state = applyWorkTaskProjection(state, event('weave/work-task', {
      ...state.task,
      brief: '完整修订简报', clientRequestId: 'request-2', runId: 'run-2', status: 'queued',
      pendingAction: null, updatedAt: 103,
      attempts: [
        ...state.task!.attempts,
        {
          clientRequestId: 'request-2', runId: 'run-2', brief: '完整修订简报', status: 'queued',
          completedStages: 0, totalStages: 0, latestStage: '', deliverableCount: 0, deliverables: [],
          createdAt: 103, updatedAt: 103,
        },
      ],
    }, 3, 103))

    expect(state.task?.pendingAction).toBeNull()
    expect(state.task?.attempts.map(attempt => [attempt.runId, attempt.brief])).toEqual([
      ['run-1', '原始简报'],
      ['run-2', '完整修订简报'],
    ])
  })

  it('resets progress for a new run and counts completed member stages instead of published artifacts', () => {
    let state = workTaskProjectionDefinition.init()
    state = applyWorkTaskProjection(state, event('weave/work-task', {
      clientRequestId: 'request-1', runId: 'run-1', teamId: 'team-1', teamName: 'Team', workflowName: 'baseline',
      status: 'completed', completedStages: 5, totalStages: 5, latestStage: '交付', runtimes: [],
      humanTaskCount: 0, deliverableCount: 1, deliverables: [], blocker: 'none', updatedAt: 100,
      brief: 'first', attempts: [], pendingAction: null, corrections: [], members: [], actionError: '',
      completeness: {}, observedAt: 100,
    }))

    for (const item of [
      call('dispatch-2', 'mcp__weave__team_dispatch', { team_id: 'team-1', task: 'retry' }, 1),
      result('dispatch-2', { client_request_id: 'request-2', run_id: 'run-2', status: 'queued' }, 2),
    ]) state = applyWorkTaskProjection(state, item)
    expect(state.task).toMatchObject({ runId: 'run-2', completedStages: 0, totalStages: 0 })

    state = applyWorkTaskProjection(state, call('activity-2', 'mcp__weave__team_run_activity', { run_id: 'run-2' }, 3))
    state = applyWorkTaskProjection(state, result('activity-2', {
      run_id: 'run-2', status: 'parked', completed_stages: 3, total_stages: 3,
      members: [
        { agent_id: 'orders', status: 'completed', stages: [{ node_id: 'orders', name: '订单分析', status: 'completed' }] },
        { agent_id: 'audit', status: 'completed', stages: [{ node_id: 'audit', name: '审计', status: 'completed' }] },
        { agent_id: 'final', status: 'running', stages: [{ node_id: 'final', name: '汇总', status: 'running' }] },
      ],
    }, 4))

    expect(state.task).toMatchObject({ runId: 'run-2', completedStages: 2, totalStages: 3, latestStage: '汇总' })
  })
})
