import { describe, expect, it } from 'vitest'
import type { SessionEvent } from '@deepseek-ai/dsh-session'
import { snapshotJsonValue } from '@deepseek-ai/dsh-session'
import { applyWorkTaskProjection, workbenchCapabilityAuthoringSection, workbenchTeamRoutingSection, workTaskProjectionDefinition } from '../src/index.ts'

const event = (type: string, data: unknown, seq = 0, time = 100): SessionEvent => ({
  type, data, seq, time,
} as SessionEvent)

const call = (id: string, name: string, args: unknown, seq: number): SessionEvent => event('tool/call', {
  turn: 1, step: 1, callId: id, name, arguments: JSON.stringify(args),
}, seq, 100 + seq)

const result = (id: string, value: unknown, seq: number, isError = false): SessionEvent => event('tool/result', {
  turn: 1,
  step: 1,
  message: {
    id: `message-${id}`,
    role: 'user',
    source: { kind: 'tool', callId: id },
    content: [{ type: 'tool-result', toolCallId: id, isError, content: [{ type: 'text', text: JSON.stringify(value) }] }],
  },
}, seq, 100 + seq)

describe('Workbench work-task projection', () => {

  it('persists mixed legacy and durable member progress through the lossless session boundary', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('dispatch', { run_id: 'run-1', status: 'running' }, 1),
      call('activity', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 2),
      result('activity', { run_id: 'run-1', status: 'running', members: [
        { agent_id: 'lead', name: 'lead', status: 'completed', stages: [{ node_id: 'brief', status: 'completed' }] },
        { agent_id: 'worker', name: 'worker', status: 'running', stages: [{ node_id: 'compute', status: 'running',
          member_run_id: 'member-1', checkpoint_saved_at: '2026-09-07T09:17:16Z' }] },
      ] }, 3),
    ]) state = applyWorkTaskProjection(state, item)
    expect(state.task?.members[0]?.stages[0]).not.toHaveProperty('memberRunId')
    expect(state.task?.members[1]?.stages[0]).toMatchObject({ memberRunId: 'member-1', checkpointSavedAt: '2026-09-07T09:17:16Z' })
    expect(snapshotJsonValue(state.task)).toEqual(state.task)
    expect(applyWorkTaskProjection(state, event('weave/work-task', snapshotJsonValue(state.task), 4)).task).toEqual(state.task)
  })

  it('correlates runless dispatch-status replies with the exact current request', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('old', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('old', { run_id: 'run-old', client_request_id: 'request-old', status: 'running' }, 1),
      call('old-status', 'mcp__weave__dispatch_status', { client_request_id: 'request-old' }, 2),
      call('current', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 3),
      result('current', { run_id: 'run-current', client_request_id: 'request-current', status: 'completed' }, 4),
    ]) state = applyWorkTaskProjection(state, item)
    const current = state.task
    state = applyWorkTaskProjection(state, result('old-status', { status: 'failed' }, 5))
    expect(state.task).toEqual(current)
    state = applyWorkTaskProjection(state, call('current-status', 'mcp__weave__dispatch_status', { client_request_id: 'request-current' }, 6))
    state = applyWorkTaskProjection(state, result('current-status', { status: 'completed', workflow_progress: { completed_stages: 3, total_stages: 3 } }, 7))
    expect(state.task).toMatchObject({ runId: 'run-current', clientRequestId: 'request-current', status: 'completed', completedStages: 3, totalStages: 3 })
  })

  it.each(['passed', 'failed'])('keeps a %s build stable against older, equal, or undated progress and accepts a newer source update', (runStatus) => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('create', 'mcp__weave__team_create', { definition: { display_name: '核验团队' } }, 0),
      result('create', { build_id: 'build-current', status: 'building' }, 1),
      call('terminal', 'mcp__weave__build_status', { build_id: 'build-current' }, 2),
      result('terminal', { build_run_id: 'build-current', run_status: runStatus, updated_at: new Date(20).toISOString(),
        steps: [{ operation_id: 'review', display_label: '检查职责', status: 'succeeded', attempt: 1 }] }, 3),
    ]) state = applyWorkTaskProjection(state, item)
    const terminal = state.task
    expect(terminal?.preparation?.state).toBe(runStatus === 'passed' ? 'ready' : 'failed')
    expect(terminal?.observedAt).toBe(103)
    let seq = 4
    for (const updatedAt of [new Date(10).toISOString(), new Date(20).toISOString(), undefined]) {
      state = applyWorkTaskProjection(state, call(`late-${seq}`, 'mcp__weave__build_status', { build_id: 'build-current' }, seq))
      state = applyWorkTaskProjection(state, result(`late-${seq}`, { build_run_id: 'build-current', run_status: 'executing',
        ...(updatedAt === undefined ? {} : { updated_at: updatedAt }), steps: [] }, seq + 1))
      expect(state.task).toEqual(terminal)
      seq += 2
    }
    state = applyWorkTaskProjection(state, call('resumed-build', 'mcp__weave__build_status', { build_id: 'build-current' }, seq))
    state = applyWorkTaskProjection(state, result('resumed-build', { build_run_id: 'build-current', run_status: 'executing',
      updated_at: new Date(30).toISOString(), steps: [{ operation_id: 'review', display_label: '检查职责', status: 'running', attempt: 2 }] }, seq + 1))
    expect(state.task).toMatchObject({ status: 'preparing', preparation: { state: 'building',
      steps: [{ id: 'review', label: '检查职责', status: 'running', attempt: 2 }] } })
    expect(state.task?.observedAt).toBe(100 + seq + 1)
  })

  it.each(['completed', 'failed', 'stopped'])('keeps new creation failure visible after a %s run while retaining the old attempt', (status) => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('old', 'mcp__weave__team_dispatch', { team_id: 'team-old', task: '旧任务' }, 0),
      result('old', { run_id: 'run-old', client_request_id: 'request-old', status }, 1),
    ]) state = applyWorkTaskProjection(state, item)
    const attempts = state.task?.attempts
    state = applyWorkTaskProjection(state, call('create-new', 'mcp__weave__team_create', {
      definition: { display_name: '新的核验团队', purpose: '新的核验目标' },
    }, 2))
    expect(state.task).toMatchObject({ runId: '', clientRequestId: '', teamName: '新的核验团队', brief: '新的核验目标',
      preparation: { callId: 'create-new', state: 'submitting' } })
    expect(state.task?.attempts).toEqual(attempts)
    state = applyWorkTaskProjection(state, result('create-new', { error: 'template_build_failed' }, 3, true))
    expect(state.task).toMatchObject({ runId: '', status: 'failed', preparation: { callId: 'create-new', state: 'failed' } })
    expect(state.task?.attempts).toEqual(attempts)
    expect(workTaskProjectionDefinition.wire.view(state)?.displayState).toBe('buildFailed')
  })

  it.each([
    ['template_build_failed', 'failed', 'buildFailed'],
    ['http_502', 'unknown', 'buildUnknown'],
  ])('retains %s creation evidence when available teams are refreshed', (error, preparationState, displayState) => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('create', 'mcp__weave__team_create', { definition: { display_name: '材料核验团队', purpose: '核验用户材料' } }, 0),
      result('create', { code: error }, 1, true),
      call('list', 'mcp__weave__team_list', {}, 2),
      result('list', { teams: [{ id: 'unrelated-team', name: '其他团队' }] }, 3),
    ]) state = applyWorkTaskProjection(state, item)
    expect(state.task).toMatchObject({ runId: '', teamName: '材料核验团队', brief: '核验用户材料',
      status: preparationState === 'failed' ? 'failed' : 'preparing',
      preparation: { callId: 'create', state: preparationState, error } })
    expect(workTaskProjectionDefinition.wire.view(state)?.displayState).toBe(displayState)
  })

  it('records real creation steps only for the current build in both request and response', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('create', 'mcp__weave__team_create', { definition: { display_name: '材料核验团队' } }, 0),
      result('create', { build_id: 'build-current', status: 'building' }, 1),
      call('other', 'mcp__weave__build_status', { build_id: 'build-old' }, 2),
      result('other', { build_id: 'build-current', run_status: 'failed' }, 3),
      call('mismatched', 'mcp__weave__build_status', { build_id: 'build-current' }, 4),
      result('mismatched', { build_id: 'build-old', run_status: 'failed' }, 5),
    ]) state = applyWorkTaskProjection(state, item)
    expect(state.task?.preparation).toMatchObject({ buildId: 'build-current', state: 'building' })
    state = applyWorkTaskProjection(state, call('current', 'mcp__weave__build_status', { build_id: 'build-current' }, 6))
    state = applyWorkTaskProjection(state, result('current', { build_run_id: 'build-current', run_status: 'passed',
      final_ref: { team_id: 'team-ready' }, steps: [
        { operation_id: 'evaluation-2', display_label: '检查成员职责', status: 'succeeded', attempt: 2 },
      ] }, 7))
    expect(state.task).toMatchObject({ teamId: 'team-ready', runId: '', preparation: { state: 'ready',
      steps: [{ id: 'evaluation-2', label: '检查成员职责', status: 'succeeded', attempt: 2 }] } })
    expect(workTaskProjectionDefinition.wire.view(state)?.displayState).toBe('buildReady')
  })

  it('rejects old run responses and older observations after final execution evidence', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('first', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('first', { run_id: 'run-old', status: 'running' }, 1),
      call('old-poll', 'mcp__weave__team_run_activity', { run_id: 'run-old' }, 2),
      call('second', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 3),
      result('second', { run_id: 'run-current', status: 'running' }, 4),
      call('completed', 'mcp__weave__team_run_activity', { run_id: 'run-current' }, 5),
      result('completed', { run_id: 'run-current', status: 'completed' }, 6),
    ]) state = applyWorkTaskProjection(state, item)
    const completed = state.task
    state = applyWorkTaskProjection(state, call('late-running', 'mcp__weave__team_run_activity', { run_id: 'run-current' }, 7))
    state = applyWorkTaskProjection(state, result('late-running', { run_id: 'run-current', status: 'running' }, 8))
    expect(state.task).toEqual(completed)
    state = applyWorkTaskProjection(state, result('old-poll', { run_id: 'run-old', status: 'failed' }, 7))
    expect(state.task).toEqual(completed)
    state = applyWorkTaskProjection(state, event('weave/work-task', { ...completed, status: 'running', observedAt: 1 }, 8))
    expect(state.task).toEqual(completed)
    state = applyWorkTaskProjection(state, event('weave/work-task', { ...completed, runId: 'run-old', status: 'failed', observedAt: 999 }, 9))
    expect(state.task).toEqual(completed)
    expect(workTaskProjectionDefinition.wire.view(state)?.displayState).toBe('outputsMissing')
  })

  it('recovers the dispatched team from the recorded team-status response after YAML creation', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('create', 'mcp__weave__team_create', { yaml: 'name: acceptance\ndisplay_name: 验收四人组' }, 0),
      result('create', { team_id: 'team-qa', build_run_id: 'build-qa', status: 'ready' }, 1),
      call('team', 'mcp__weave__team_status', { team_id: 'team-qa' }, 2),
      result('team', { team: { id: 'team-qa', name: 'acceptance', display_name: '验收四人组' }, lead: {}, workers: [], summary: {} }, 3),
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-qa', task: '核对三条材料', wait: false }, 4),
      result('dispatch', { client_request_id: 'request-qa', run_id: 'run-qa', status: 'queued', task_id: 'task-qa', workflow_id: 'qa-flow', workflow_version: 1 }, 5),
    ]) state = applyWorkTaskProjection(state, item)
    expect(state.task).toMatchObject({ teamId: 'team-qa', teamName: '验收四人组', runId: 'run-qa', workflowName: 'qa-flow' })
    state = applyWorkTaskProjection(state, event('weave/work-task', { ...state.task, teamName: '' }, 6))
    expect(state.task).toMatchObject({ teamName: '验收四人组', status: 'queued' })
    expect(workTaskProjectionDefinition.wire.view(state)).toMatchSnapshot()
  })

  it('hands a dispatched Weave run to background ownership', () => {
    expect(workbenchTeamRoutingSection.name).toBe('workbench:team-routing')
    expect(workbenchTeamRoutingSection.text).toContain('do not create or update a DSH goal')
    expect(workbenchTeamRoutingSection.text).toContain('make at most one activity or status call')
    expect(workbenchTeamRoutingSection.text).toContain('do not save a duplicate foreground deliverable')
    expect(workbenchTeamRoutingSection.text).toContain('short user-facing completion summary')
    expect(workbenchTeamRoutingSection.text).toContain('Keep internal run IDs')
    expect(workbenchTeamRoutingSection.text).toContain('Do not include internal identifiers')
    expect(workbenchTeamRoutingSection.text).not.toContain('return the team, run ID')
    expect(workbenchTeamRoutingSection.text).toContain('confirmed the selected team, task scope, and expected deliverables')
    expect(workbenchTeamRoutingSection.text).toContain('make method discovery part of the team definition')
    expect(workbenchTeamRoutingSection.text).toContain('choose a fallback or escalation path')
    expect(workbenchTeamRoutingSection.text).toContain('without asking again')
    expect(workbenchTeamRoutingSection.text).toContain('Internal construction and evaluation steps are not additional user approval gates')
    expect(workbenchTeamRoutingSection.text).toContain('do not claim delivery is complete')
    expect(workbenchTeamRoutingSection.text).toContain('do not repeat team matching or dispatch a new task')
    expect(workbenchTeamRoutingSection.text).toContain('never describe an accepted request as an applied change')
    expect(workbenchTeamRoutingSection.text).toMatchSnapshot()
  })

  it('keeps capability authoring in the conversation and freezes confirmed revisions', () => {
    expect(workbenchCapabilityAuthoringSection.name).toBe('workbench:capability-authoring')
    expect(workbenchCapabilityAuthoringSection.text).toContain('Call capability_list first')
    expect(workbenchCapabilityAuthoringSection.text).toContain('capability_plan to create a reviewable draft')
    expect(workbenchCapabilityAuthoringSection.text).toContain('capability_publish only after the user explicitly confirms')
    expect(workbenchCapabilityAuthoringSection.text).toContain('Published revisions are immutable')
    expect(workbenchCapabilityAuthoringSection.text).toContain('Do not send the user to a manual capability editor')
    expect(workbenchCapabilityAuthoringSection.text).toContain('exact-version grants remain separate administration actions')
  })

  it.each(['timer', 'fanout', 'human', 'correction', 'runtime'])('retains the exact %s wait and clears it when work resumes', (waitKind) => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('dispatch', { run_id: 'run-1', status: 'running' }, 1),
      call('activity', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 2),
      result('activity', { run_id: 'run-1', status: 'parked', wait_kind: waitKind, wait_node_id: 'review',
        human_tasks: waitKind === 'human' ? [{ run_id: 'run-1', node_id: 'review' }] : [] }, 3),
    ]) state = applyWorkTaskProjection(state, item)
    expect(state.task).toMatchObject({ status: 'waiting', waitKind, waitNodeId: 'review', humanTaskCount: waitKind === 'human' ? 1 : 0 })
    state = applyWorkTaskProjection(state, call('resumed', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 4))
    state = applyWorkTaskProjection(state, result('resumed', { run_id: 'run-1', status: 'running' }, 5))
    expect(state.task).toMatchObject({ status: 'running', waitKind: '', waitNodeId: '', humanTaskCount: 0 })
  })

  it('distinguishes an abandoned stop from cancellation confirmed by Weave', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('dispatch', { run_id: 'run-1', status: 'running' }, 1),
      call('abandoned', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 2),
      result('abandoned', { run_id: 'run-1', status: 'abandoned', stop_unconfirmed: true, cancel_requested_at: null }, 3),
    ]) state = applyWorkTaskProjection(state, item)
    expect(state.task).toMatchObject({ status: 'failed', actionError: 'stop_unconfirmed' })
    state = applyWorkTaskProjection(state, call('cancelled', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 4))
    state = applyWorkTaskProjection(state, result('cancelled', { run_id: 'run-1', status: 'cancelled' }, 5))
    expect(state.task).toMatchObject({ status: 'stopped', actionError: '' })
  })

  it('does not describe an abandoned execution without a cancellation request as a stop', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0), result('dispatch', { run_id: 'run-1', status: 'running' }, 1), call('activity', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 2), result('activity', { run_id: 'run-1', status: 'abandoned', cancel_requested_at: null }, 3)]) state = applyWorkTaskProjection(state, item)
    expect(state.task).toMatchObject({ status: 'failed', actionError: '' })
  })

  it('replaces public snapshots and retains only explicitly public event kinds', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0), result('dispatch', { run_id: 'run-1', status: 'running' }, 1)]) state = applyWorkTaskProjection(state, item)
    const activity = (text: string, seq: number) => ({ run_id: 'run-1', status: 'running', members: [{ agent_id: 'member-1', name: '研究员', runtime: { name: 'worker', update_mode: 'live' }, stages: [{ node_id: 'research', status: 'running', current_task_id: 'attempt-2', public_updates_state: 'live', public_updates: [
      { event_id: `public-${seq}`, task_id: 'attempt-2', seq, kind: 'text', text, occurred_at: '2026-09-05T04:00:00Z', truncated: false },
      { event_id: 'private-1', task_id: 'attempt-2', seq: 9, kind: 'reasoning', text: 'private content' },
    ] }] }] })
    state = applyWorkTaskProjection(state, call('first-update', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 2))
    state = applyWorkTaskProjection(state, result('first-update', activity('a', 1), 3))
    state = applyWorkTaskProjection(state, call('next-update', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 4))
    state = applyWorkTaskProjection(state, result('next-update', activity('abc', 3), 5))
    expect(state.task?.members[0]).toMatchObject({ updateMode: 'live' })
    expect(state.task?.members[0]?.stages[0]).toMatchObject({ currentTaskId: 'attempt-2', publicUpdatesState: 'live', publicUpdates: [{ taskId: 'attempt-2', seq: 3, text: 'abc' }] })
    expect(state.task?.members[0]?.stages[0]?.publicUpdates).toHaveLength(1)
    expect(JSON.stringify(state.task)).not.toContain('private content')
  })

  it('keeps one actual action receipt across repeated projection refreshes', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0), result('dispatch', { run_id: 'run-1', status: 'running' }, 1)]) state = applyWorkTaskProjection(state, item)
    const pendingAction = { kind: 'stop', targetRunId: 'run-1', idempotencyKey: 'exact-stop', clientRequestId: '', brief: '', requestedAt: 102, targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: '', nodeId: '' }
    state = applyWorkTaskProjection(state, event('weave/work-task-action', { pendingAction }, 2))
    const accepted = event('weave/work-task', { ...state.task, pendingAction: null, status: 'stopping', actionError: '', actionHistory: [{ id: 'exact-stop', action: pendingAction, outcome: 'accepted', resolvedAt: 103 }] }, 3, 103)
    state = applyWorkTaskProjection(state, accepted)
    state = applyWorkTaskProjection(state, accepted)
    expect(state.task?.actionHistory).toEqual([{ id: 'exact-stop', action: pendingAction, outcome: 'accepted', resolvedAt: 103 }])
    expect(state.task?.status).toBe('stopping')
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

  it('accepts a newer authoritative parked observation after a prior terminal state', () => {
    let state = workTaskProjectionDefinition.init()
    state = applyWorkTaskProjection(state, event('weave/work-task', {
      clientRequestId: 'request-1', runId: 'run-1', teamId: 'team-1', teamName: 'Team', workflowName: 'baseline',
      status: 'stopped', completedStages: 0, totalStages: 0, latestStage: '', runtimes: [],
      humanTaskCount: 0, deliverableCount: 0, deliverables: [], blocker: 'none', updatedAt: 100,
      brief: 'first', attempts: [], pendingAction: null, completeness: {}, observedAt: 100,
    }))
    state = applyWorkTaskProjection(state, call('status', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 1))
    state = applyWorkTaskProjection(state, result('status', {
      run_id: 'run-1', status: 'parked', observed_at: new Date(200).toISOString(), wait_kind: 'fanout', completed_stages: 3,
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
      runtime: 'mac-codex-live · codex · openai/gpt-5.6-luna', updateMode: 'on_completion',
      stages: [{
        nodeId: 'verify', name: '约束复核', status: 'completed',
        inputs: [{ name: 'facts', expectedType: 'text', source: 'node_output', nodeId: 'draft', path: '$.facts', summary: '' }],
        outputRefs: ['deliverable-1'],
        startedAt: '2026-08-30T10:00:00Z', completedAt: '2026-08-30T10:00:04Z', durationMs: 4000, toolCalls: 1,
        failureClass: '', failureReason: '', retryable: false,
        currentTaskId: '', publicUpdatesState: 'unavailable', publicUpdates: [], publicUpdatesTruncated: false,
        tools: [{ callId: 'tool-1', taskId: '', name: 'evidence_lookup', status: 'ok',
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

  it('corrects a cached final label from run activity when the file count stays unchanged', () => {
    let state = workTaskProjectionDefinition.init()
    for (const item of [
      call('dispatch', 'mcp__weave__team_dispatch', { team_id: 'team-1' }, 0),
      result('dispatch', { run_id: 'run-1', status: 'running' }, 1),
    ]) state = applyWorkTaskProjection(state, item)
    state = applyWorkTaskProjection(state, event('weave/work-task', {
      ...state.task, deliverableCount: 1,
      deliverables: [{ id: 'lead-file', title: 'baseline.yaml', kind: 'final', contentType: 'application/yaml', preview: 'version: 1', content: 'version: 1', truncated: false, createdAt: '' }],
    }, 2))
    state = applyWorkTaskProjection(state, call('status', 'mcp__weave__team_run_activity', { run_id: 'run-1' }, 3))
    state = applyWorkTaskProjection(state, result('status', { run_id: 'run-1', status: 'running', deliverables: [{ id: 'lead-file', kind: 'stage' }] }, 4))
    expect(state.task?.deliverableCount).toBe(1)
    expect(state.task?.deliverables[0]).toMatchObject({ id: 'lead-file', kind: 'stage', content: 'version: 1' })
    expect(workTaskProjectionDefinition.wire.view(state)?.deliverables).toMatchInlineSnapshot(`
      [
        {
          "content": "version: 1",
          "contentType": "application/yaml",
          "createdAt": "",
          "id": "lead-file",
          "kind": "stage",
          "preview": "version: 1",
          "title": "baseline.yaml",
          "truncated": false,
        },
      ]
    `)
  })

  it.each(['final', 'summary'] as const)('retains a host %s output after the conversation turn ends', (kind) => {
    const state = workTaskProjectionDefinition.init()
    const next = applyWorkTaskProjection(state, event('weave/work-task', {
      clientRequestId: 'request-1', runId: 'run-1', teamId: 'team-1', teamName: 'Team', workflowName: '',
      status: 'completed', completedStages: 9, totalStages: 9, latestStage: '交付', runtimes: [],
      humanTaskCount: 0, deliverableCount: 1,
      deliverables: [{
        id: 'file-1', title: '最终报告', kind, contentType: 'text/markdown',
        preview: '# 完成', content: '# 完成', truncated: false, createdAt: '2026-08-30T10:00:00Z',
      }],
      blocker: 'none', updatedAt: 999,
      brief: '任务简报', attempts: [], pendingAction: null, completeness: { run: 'complete' }, observedAt: 999,
    }))
    expect(next.task?.status).toBe('completed')
    expect(workTaskProjectionDefinition.wire.view(next)?.deliverableCount).toBe(1)
    expect(workTaskProjectionDefinition.wire.view(next)?.deliverables[0]?.title).toBe('最终报告')
    expect(workTaskProjectionDefinition.wire.view(next)?.deliverables[0]?.kind).toBe(kind)
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
