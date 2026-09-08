import { afterEach, describe, expect, it, vi } from 'vitest'
import { createHash } from 'node:crypto'
import type { Context } from '@deepseek-ai/cordis'
import { Session, SessionId, type SessionEvent } from '@deepseek-ai/dsh-session'
import { createUserMessage } from '@deepseek-ai/dsh-llm'
import { apply, applyWorkTaskProjection, workTaskProjectionDefinition, type WorkTaskProjection } from '../src/index.ts'
import { prepareDispatchInput } from '../src/dispatch-input.ts'

const urlOf = (input: Parameters<typeof fetch>[0]): string => typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
function jsonBody(init: RequestInit | undefined): unknown {
  if (typeof init?.body !== 'string') throw new Error('Expected a JSON request body')
  return JSON.parse(init.body) as unknown
}

/** Host services remain in memory; Weave HTTP is the only mocked external transport. */
function host(overrides: Partial<WorkTaskProjection> = {}) {
  let state = workTaskProjectionDefinition.init()
  const initial = workTaskProjectionDefinition.wire.viewSchema.parse({
    runId: 'run-1', clientRequestId: 'request-1', teamId: 'team-1', teamName: '研究团队', workflowName: '',
    status: 'waiting', waitKind: 'human', waitNodeId: 'approval', completedStages: 1, totalStages: 2, latestStage: '审核',
    runtimes: [], humanTaskCount: 1, deliverableCount: 0, blocker: 'none', updatedAt: 1, ...overrides,
  })
  const events: SessionEvent[] = []
  const eventListeners: ((event: SessionEvent) => void)[] = []
  const header = Session.create(SessionId('session-1')).header
  const session = { id: header.id, header, events, append: (type: string, data: unknown) => {
    const event = { type, data, seq: events.length, time: Date.now() } as SessionEvent
    events.push(event); state = applyWorkTaskProjection(state, event)
    for (const listener of eventListeners) listener(event)
  } }
  session.append('weave/work-task', initial)
  const routes = new Map<string, (request: Request) => Promise<Response>>()
  const disposers: (() => unknown)[] = []
  const ctx = {
    effect: (callback: () => unknown) => { const dispose = callback(); if (typeof dispose === 'function') disposers.push(dispose as () => unknown) },
    on: (name: string, listener: (targetSession: typeof session, event: SessionEvent) => void) => {
      if (name === 'session/event') eventListeners.push((event) => { listener(session, event) })
      return () => {}
    },
    systemPrompt: { section: () => () => {} },
    commands: { register: () => () => {} },
    tools: { register: () => () => {} },
    connection: { fetch: { register: (route: { path: string; fetch: (request: Request) => Promise<Response> }) => {
      routes.set(route.path, route.fetch); return () => routes.delete(route.path)
    } } },
    sessions: { list: () => [session], get: (id: string) => id === session.id ? session : undefined, flush: async () => {} },
    sessionProjections: { register: () => {}, stateOf: () => state },
    sessionController: { registerHistoryProjection: () => () => {} },
  }
  apply(ctx as unknown as Context, { apiUrl: 'http://weave.test', apiKey: 'test-only', pollIntervalMs: 500 })
  return { state: () => state, events, session, publish: (data: WorkTaskProjection) => { session.append('weave/work-task', data) }, action: (body: unknown) => routes.get('/api/weave.task-action')!(new Request('http://host/api/weave.task-action', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })), dispose: () => { for (const dispose of disposers.reverse()) dispose() } }
}

afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals() })

describe('exact human wait actions', () => {
  it('reruns the exact edited brief with its admitted workflow and project, retaining the server request identity', async () => {
    vi.useFakeTimers()
    const brief = '  修改后的原文\r\nINV-440  \n'
    const revision = { input_revision_id: '10000000-0000-4000-8000-000000000001',
      client_request_id: '20000000-0000-4000-8000-000000000001',
      task_sha256: createHash('sha256').update(brief, 'utf8').digest('hex') }
    const posts: { url: string; body: unknown }[] = []
    vi.stubGlobal('fetch', vi.fn<typeof fetch>((input, init) => {
      const url = urlOf(input)
      if (init?.method === 'POST') {
        posts.push({ url, body: jsonBody(init) })
        return Promise.resolve(Response.json(url.endsWith('/dispatch-inputs') ? revision
          : { run_id: 'run-2', input_revision_id: revision.input_revision_id, client_request_id: revision.client_request_id, status: 'queued' }))
      }
      if (url.includes('/deliverables?')) return Promise.resolve(Response.json([]))
      return Promise.resolve(Response.json({ run_id: url.includes('run-2') ? 'run-2' : 'run-1', status: 'completed', completeness: { members: 'complete' } }))
    }))
    const app = host({ status: 'completed', waitKind: '', humanTaskCount: 0 })
    try {
      app.session.append('user/message', createUserMessage({ content: [{ type: 'text', text: '原始请求 INV-440' }], source: { kind: 'user' } }))
      const initial = prepareDispatchInput(app.session as unknown as Session, { team_id: 'team-1' })
      const initialRevision = { input_revision_id: '10000000-0000-4000-8000-000000000009',
        client_request_id: '20000000-0000-4000-8000-000000000009',
        task_sha256: createHash('sha256').update(initial.task, 'utf8').digest('hex') }
      app.session.append('weave/dispatch-input', { ...initial, state: 'accepted', revision: initialRevision,
        result: { run_id: 'run-1', ...initialRevision, status: 'completed', workflow_id: 'wf-orders', workflow_version: 1, project_id: 'project-1' } })
      expect((await app.action({ action: 'rerun', sessionId: 'session-1', runId: 'run-1', brief })).status).toBe(204)
      await vi.advanceTimersByTimeAsync(0)
      expect(posts).toHaveLength(2)
      expect(posts[0]?.body).toMatchObject({ task: brief, team_id: 'team-1', mode: 'workflow', workbench_session_id: 'session-1',
        workflow_id: 'wf-orders', workflow_version: 1, project_id: 'project-1', expected_revision_id: initialRevision.input_revision_id })
      expect(posts[1]?.body).toEqual({ input_revision_id: revision.input_revision_id, client_request_id: revision.client_request_id })
      expect(app.state().task).toMatchObject({ runId: 'run-2', clientRequestId: revision.client_request_id, brief, pendingAction: null })
      expect(app.state().task?.actionHistory).toHaveLength(1)
      expect(app.events.filter(event => event.type === 'weave/dispatch-input').at(-1)?.data)
        .toMatchObject({ state: 'accepted', rerun: { targetRunId: 'run-1' }, task: brief })
    } finally { app.dispose() }
  })

  it('writes unchanged observations only at the freshness interval and retains the attempt change time', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(100_000)
    let completedStages = 1
    const fetcher = vi.fn<typeof fetch>(() => Promise.resolve(Response.json({ run_id: 'run-1', status: 'running',
      observed_at: new Date(Date.now()).toISOString(), workflow_progress: { completed_stages: completedStages, total_stages: 3 },
    })))
    vi.stubGlobal('fetch', fetcher)
    const app = host({ status: 'running', waitKind: '', humanTaskCount: 0 })
    const snapshots = () => app.events.filter(event => event.type === 'weave/work-task')
    try {
      await vi.advanceTimersByTimeAsync(0)
      const firstAttempt = app.state().task!.attempts[0]!
      expect(snapshots()).toHaveLength(2)
      await vi.advanceTimersByTimeAsync(29_999)
      expect(fetcher).toHaveBeenCalledTimes(60)
      expect(snapshots()).toHaveLength(2)
      await vi.advanceTimersByTimeAsync(1)
      expect(snapshots()).toHaveLength(3)
      expect(app.state().task!.observedAt).toBe(130_000)
      expect(app.state().task!.attempts[0]).toEqual(firstAttempt)
      completedStages = 2
      await vi.advanceTimersByTimeAsync(500)
      expect(snapshots()).toHaveLength(4)
      expect(app.state().task!.attempts[0]).toMatchObject({ completedStages: 2,
        createdAt: firstAttempt.createdAt, updatedAt: 130_500 })
    } finally { app.dispose() }
  })

  it('keeps the configured polling interval when publishing changing member activity', async () => {
    vi.useFakeTimers()
    const fetcher = vi.fn<typeof fetch>(() => Promise.resolve(Response.json({ run_id: 'run-1', status: 'running',
      members: [{ agent_id: 'lead', name: '负责人', status: 'running', stages: [{ node_id: 'lead', status: 'running', duration_ms: Date.now() }] }],
    })))
    vi.stubGlobal('fetch', fetcher)
    const app = host({ status: 'running', waitKind: '', humanTaskCount: 0 })
    try {
      await vi.advanceTimersByTimeAsync(1499)
      expect(fetcher).toHaveBeenCalledTimes(3)
      expect(app.events.filter(event => event.type === 'weave/work-task')).toHaveLength(4)
    } finally { app.dispose() }
  })

  it('keeps terminal activity and retries missing team and deliverable details before stopping', async () => {
    vi.useFakeTimers()
    let detailReads = 0
    let outputReads = 0
    const fetcher = vi.fn<typeof fetch>((input) => {
      const url = urlOf(input)
      if (url.endsWith('/activity')) return Promise.resolve(Response.json({ run_id: 'run-1', team_id: 'team-1', status: 'failed',
        members: [{ agent_id: 'lead', name: '负责人', status: 'failed', stages: [{ node_id: 'lead', status: 'failed', failure_class: 'verification', failure_reason: 'The referenced result file was not saved.' }] }],
        completeness: { members: 'complete' },
      }))
      if (url.includes('/teams/')) return Promise.resolve(++detailReads === 1 ? new Response(null, { status: 503 })
        : Response.json({ team: { id: 'team-1', display_name: '验收四人组' } }))
      if (url.includes('/deliverables?')) return Promise.resolve(++outputReads === 1 ? new Response(null, { status: 503 }) : Response.json([]))
      throw new Error(`Unexpected path: ${url}`)
    })
    vi.stubGlobal('fetch', fetcher)
    const app = host({ status: 'failed', teamId: '', teamName: '', waitKind: '', humanTaskCount: 0 })
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(app.state().task?.members[0]?.stages[0]?.failureReason).toBe('The referenced result file was not saved.')
      expect(app.state().task).toMatchObject({ teamId: 'team-1', teamName: '', status: 'failed' })
      await vi.advanceTimersByTimeAsync(500)
      expect(app.state().task).toMatchObject({ teamName: '验收四人组', status: 'failed' })
      expect(detailReads).toBe(2)
      expect(outputReads).toBe(2)
      const settledCalls = fetcher.mock.calls.length
      await vi.advanceTimersByTimeAsync(2000)
      expect(fetcher).toHaveBeenCalledTimes(settledCalls)
    } finally { app.dispose() }
  })

  it('still schedules an external task update that arrives during an activity request', async () => {
    vi.useFakeTimers()
    let resolveFirst: ((value: Response) => void) | undefined
    const fetcher = vi.fn<typeof fetch>(() => new Promise((resolve) => { resolveFirst ??= resolve }))
    vi.stubGlobal('fetch', fetcher)
    const app = host({ status: 'running', waitKind: '', humanTaskCount: 0 })
    try {
      await vi.advanceTimersByTimeAsync(100)
      app.publish({ ...app.state().task!, observedAt: Date.now() + 1, latestStage: '新活动' })
      resolveFirst?.(Response.json({ run_id: 'run-1', status: 'running' }))
      await vi.advanceTimersByTimeAsync(1)
      expect(fetcher).toHaveBeenCalledTimes(2)
      expect(app.state().task?.latestStage).toBe('新活动')
    } finally { app.dispose() }
  })

  it('loads terminal members after an earlier response only confirmed the run status', async () => {
    vi.useFakeTimers()
    let activityReads = 0
    const fetcher = vi.fn<typeof fetch>((input) => {
      if (urlOf(input).includes('/deliverables?')) return Promise.resolve(Response.json([]))
      return Promise.resolve(Response.json(++activityReads === 1
        ? { run_id: 'run-1', status: 'failed', completeness: { members: 'unavailable' } }
        : { run_id: 'run-1', status: 'failed', completeness: { members: 'complete' },
          members: [{ agent_id: 'lead', name: '负责人', status: 'failed', stages: [{ node_id: 'lead', status: 'failed', failure_reason: 'The result was not saved.' }] }] }))
    })
    vi.stubGlobal('fetch', fetcher)
    const app = host({ status: 'failed', waitKind: '', humanTaskCount: 0 })
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(app.state().task?.members).toEqual([])
      await vi.advanceTimersByTimeAsync(500)
      expect(app.state().task?.members[0]?.stages[0]?.failureReason).toBe('The result was not saved.')
      await vi.advanceTimersByTimeAsync(1500)
      expect(activityReads).toBe(2)
    } finally { app.dispose() }
  })

  it('keeps a new action during a delayed activity read and records no receipt before the action response', async () => {
    vi.useFakeTimers()
    let resolveActivity: ((value: Response) => void) | undefined
    let resolveStop: ((value: Response) => void) | undefined
    const fetcher = vi.fn<typeof fetch>((input) => {
      const url = urlOf(input)
      if (url.endsWith('/activity')) return new Promise((resolve) => { resolveActivity = resolve })
      if (url.endsWith('/stop')) return new Promise((resolve) => { resolveStop = resolve })
      throw new Error(`Unexpected path: ${url}`)
    })
    vi.stubGlobal('fetch', fetcher)
    const app = host({ status: 'running', waitKind: '', waitNodeId: '', humanTaskCount: 0 })
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(fetcher).toHaveBeenCalledTimes(1)
      await app.action({ action: 'stop', sessionId: 'session-1', runId: 'run-1' })
      await vi.advanceTimersByTimeAsync(1000)
      expect(fetcher).toHaveBeenCalledTimes(1)
      expect(app.state().task?.pendingAction?.kind).toBe('stop')
      resolveActivity?.(Response.json({ run_id: 'run-1', status: 'running' }))
      await vi.advanceTimersByTimeAsync(0)
      expect(app.state().task?.pendingAction?.kind).toBe('stop')
      expect(app.state().task?.actionHistory).toHaveLength(0)
      await vi.advanceTimersByTimeAsync(1)
      expect(fetcher.mock.calls.filter(([url]) => urlOf(url).endsWith('/stop'))).toHaveLength(1)
      resolveStop?.(Response.json({ run_id: 'run-1', status: 'cancel_requested' }))
      await vi.advanceTimersByTimeAsync(0)
      expect(app.state().task?.actionHistory).toHaveLength(1)
      expect(app.state().task?.actionHistory[0]?.outcome).toBe('accepted')
    } finally { app.dispose() }
  })

  it('does not let an old activity response overwrite a newer dispatched task', async () => {
    vi.useFakeTimers()
    let settle: ((value: Response) => void) | undefined
    vi.stubGlobal('fetch', vi.fn<typeof fetch>(() => new Promise((resolve) => { settle = resolve })))
    const app = host({ status: 'running', waitKind: '', humanTaskCount: 0 })
    try {
      await vi.advanceTimersByTimeAsync(0)
      app.publish({ ...app.state().task!, runId: 'run-new', clientRequestId: 'request-new', status: 'running', brief: '新的任务要求' })
      settle?.(Response.json({ run_id: 'run-1', status: 'running' }))
      await vi.advanceTimersByTimeAsync(0)
      expect(app.state().task).toMatchObject({ runId: 'run-new', brief: '新的任务要求', status: 'running' })
      expect(app.state().task?.actionHistory).toHaveLength(0)
    } finally { app.dispose() }
  })

  it('reuses one retry identity after a lost response and only creates another for another explicit retry', async () => {
    vi.useFakeTimers()
    const keys: string[] = []
    const activity = { run_id: 'run-1', status: 'parked', wait_kind: 'runtime', wait_node_id: 'review', members: [{ agent_id: 'reviewer', name: '复核员', status: 'failed', stages: [{ node_id: 'review', status: 'failed', failure_class: 'infrastructure', retryable: true }] }] }
    vi.stubGlobal('fetch', vi.fn<typeof fetch>(async (input, init) => {
      const url = urlOf(input)
      if (url.endsWith('/activity')) return Response.json(activity)
      if (url.endsWith('/retry')) {
        const body = jsonBody(init) as { idempotency_key: string }
        keys.push(body.idempotency_key)
        if (keys.length === 1) throw new Error('Response lost after successful enqueue')
        return Response.json({ status: 'queued', idempotent: true })
      }
      throw new Error(`Unexpected path: ${url}`)
    }))
    const app = host({ status: 'running', waitKind: '', humanTaskCount: 0 })
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect((await app.action({ action: 'stage-retry', sessionId: 'session-1', runId: 'run-1', nodeId: 'review' })).status).toBe(204)
      const identity = app.state().task?.pendingAction?.idempotencyKey
      expect(identity).toMatch(/^workbench-stage-retry:/)
      await vi.advanceTimersByTimeAsync(1000)
      expect(keys).toEqual([identity, identity])
      expect(app.state().task?.actionHistory).toHaveLength(1)
      expect((await app.action({ action: 'stage-retry', sessionId: 'session-1', runId: 'run-1', nodeId: 'review' })).status).toBe(204)
      expect(app.state().task?.pendingAction?.idempotencyKey).not.toBe(identity)
    } finally { app.dispose() }
  })

  it('reads the real detail fields and persists a matching answer before sending its original payload', async () => {
    vi.useFakeTimers()
    let queued = false
    const fetcher = vi.fn<typeof fetch>(async (input, init) => {
      const url = urlOf(input)
      if (url.endsWith('/activity')) return Response.json({ run_id: 'run-1', status: queued ? 'queued' : 'parked', wait_kind: queued ? '' : 'human', wait_node_id: queued ? '' : 'approval', human_tasks: queued ? [] : [{ run_id: 'run-1' }] })
      if (url.endsWith('/human-tasks/run-1')) return Response.json({ run_id: 'run-1', title: '确认发布范围', interaction_id: 'question-a', node_id: 'approval', instructions: '请确认已阅读前序材料。', updated_at: '2026-09-05T04:00:00Z', resume_schema: { type: 'object', properties: { decision: { type: 'string', enum: ['approve', 'revise'] } }, required: ['decision'] }, predecessor_outputs: { research: '已完成材料' } })
      if (url.endsWith('/human-tasks/run-1/complete')) {
        const body = jsonBody(init) as { payload: unknown; idempotency_key: string }
        expect(body).toMatchObject({ payload: { decision: 'approve' }, interaction_id: 'question-a' })
        expect(body.idempotency_key).toMatch(/^workbench-human:/)
        queued = true
        return Response.json({ run_id: 'run-1', status: 'queued', task_id: 'resume-1', idempotent: false }, { status: 202 })
      }
      throw new Error(`Unexpected path: ${url}`)
    })
    vi.stubGlobal('fetch', fetcher)
    const app = host()
    try {
      await vi.advanceTimersByTimeAsync(0)
      const task = app.state().task
      expect(task?.humanTask).toMatchObject({ title: '确认发布范围', instructions: '请确认已阅读前序材料。', nodeId: 'approval', resumeSchema: { required: ['decision'] } })
      const wrong = await app.action({ action: 'human-complete', sessionId: 'session-1', runId: 'run-1', interactionId: 'old-wait', payload: { decision: 'approve' } })
      expect(wrong.status).toBe(409)
      const response = await app.action({ action: 'human-complete', sessionId: 'session-1', runId: 'run-1', interactionId: task?.humanTask?.interactionId, payload: { decision: 'approve' } })
      expect(response.status).toBe(204)
      expect(app.state().task?.pendingAction?.kind).toBe('human-complete')
      expect(fetcher.mock.calls.some(([url]) => urlOf(url).endsWith('/complete'))).toBe(false)
      await vi.advanceTimersByTimeAsync(500)
      expect(app.state().task).toMatchObject({ status: 'waiting', pendingAction: null })
      await vi.advanceTimersByTimeAsync(1)
      expect(app.state().task).toMatchObject({ status: 'queued', humanTask: null, humanTaskCount: 0, pendingAction: null })
      expect(app.state().task?.actionHistory).toHaveLength(1)
      expect(app.state().task?.actionHistory[0]).toMatchObject({ outcome: 'accepted', action: { kind: 'human-complete', humanPayload: { decision: 'approve' } } })
    } finally { app.dispose() }
  })

  it('uses the authoritative question node and retains its identity if another question replaces it before submission', async () => {
    vi.useFakeTimers()
    let replacement = false
    const fetcher = vi.fn<typeof fetch>(async (input, init) => {
      const url = urlOf(input)
      if (url.endsWith('/activity')) return Response.json({ run_id: 'run-1', status: 'parked', wait_kind: 'human', wait_node_id: 'older-node' })
      if (url.endsWith('/human-tasks/run-1')) return Response.json({ run_id: 'run-1', interaction_id: replacement ? 'question-b' : 'question-a', node_id: 'actual-node', title: replacement ? '后续问题' : '当前问题', resume_schema: { type: 'boolean' } })
      if (url.endsWith('/complete')) {
        expect(jsonBody(init)).toMatchObject({ payload: true, interaction_id: 'question-a' })
        return Response.json({ error: 'human_task_conflict' }, { status: 409 })
      }
      throw new Error(`Unexpected path: ${url}`)
    })
    vi.stubGlobal('fetch', fetcher)
    const app = host()
    try {
      await vi.advanceTimersByTimeAsync(0)
      expect(app.state().task).toMatchObject({ waitNodeId: 'actual-node', humanTask: { interactionId: 'question-a', nodeId: 'actual-node' } })
      expect((await app.action({ action: 'human-complete', sessionId: 'session-1', runId: 'run-1', interactionId: 'question-a', payload: true })).status).toBe(204)
      replacement = true
      await vi.advanceTimersByTimeAsync(1000)
      expect(app.state().task?.humanTask?.interactionId).toBe('question-b')
      expect(app.state().task?.actionHistory).toHaveLength(1)
      expect(app.state().task?.actionHistory[0]?.outcome).toBe('rejected')
      expect(fetcher.mock.calls.filter(([url]) => urlOf(url).endsWith('/complete'))).toHaveLength(1)
    } finally { app.dispose() }
  })

  it('aborts observation on disposal without scheduling a replacement poll or writing late data', async () => {
    vi.useFakeTimers()
    let settle: ((value: Response) => void) | undefined
    const fetcher = vi.fn<typeof fetch>(() => new Promise((resolve) => { settle = resolve }))
    vi.stubGlobal('fetch', fetcher)
    const app = host({ status: 'running', waitKind: '', humanTaskCount: 0 })
    await vi.advanceTimersByTimeAsync(0)
    app.dispose()
    settle?.(Response.json({ run_id: 'run-1', status: 'running', latest_stage: 'late-data' }))
    await vi.advanceTimersByTimeAsync(1000)
    expect(fetcher).toHaveBeenCalledTimes(1)
    expect(app.events).toHaveLength(1)
  })

  it('records a replayed answer acknowledgement without replacing a later observed question with its queued status', async () => {
    vi.useFakeTimers()
    let acknowledge: ((value: Response) => void) | undefined
    vi.stubGlobal('fetch', vi.fn<typeof fetch>((input) => {
      if (urlOf(input).endsWith('/complete')) return new Promise((resolve) => { acknowledge = resolve })
      return new Promise(() => {})
    }))
    const app = host({ humanTask: {
      interactionId: 'question-a', nodeId: 'approval', title: '当前问题', instructions: '', resumeSchema: { type: 'boolean' },
    } })
    try {
      await app.action({ action: 'human-complete', sessionId: 'session-1', runId: 'run-1', interactionId: 'question-a', payload: true })
      await vi.advanceTimersByTimeAsync(0)
      app.publish({ ...app.state().task!, waitNodeId: 'follow-up', humanTask: {
        interactionId: 'question-b', nodeId: 'follow-up', title: '后续问题', instructions: '', resumeSchema: { type: 'string' },
      } })
      acknowledge?.(Response.json({ run_id: 'run-1', status: 'queued', idempotent: true }, { status: 202 }))
      await vi.advanceTimersByTimeAsync(0)
      expect(app.state().task).toMatchObject({ status: 'waiting', waitKind: 'human', waitNodeId: 'follow-up',
        pendingAction: null, humanTask: { interactionId: 'question-b' } })
      expect(app.state().task?.actionHistory).toHaveLength(1)
      expect(app.state().task?.actionHistory[0]?.outcome).toBe('accepted')
    } finally { app.dispose() }
  })
})
