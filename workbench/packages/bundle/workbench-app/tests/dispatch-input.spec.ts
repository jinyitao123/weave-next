import { createHash } from 'node:crypto'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Context } from '@deepseek-ai/cordis'
import type { Agent } from '@deepseek-ai/dsh-agent'
import { createUserMessage, ToolCallId, type MessageSource } from '@deepseek-ai/dsh-llm'
import SessionStore, { Session, SessionId, type SessionEvent, type SessionHeader } from '@deepseek-ai/dsh-session'
import SystemPrompt from '@deepseek-ai/dsh-system-prompt'
import ToolRuntime from '@deepseek-ai/dsh-tools'
import { installDispatchInputTool, latestDispatchInput, prepareDispatchInput, type DispatchInputFacts, type DispatchInputRecord } from '../src/dispatch-input.ts'
import type { WorkTaskPendingAction } from '../src/index.ts'

const facts: DispatchInputFacts = { team_id: 'orders', workflow_id: 'reconcile', workflow_version: 1 }
const hash = (value: string): string => createHash('sha256').update(value, 'utf8').digest('hex')
const connection = { apiUrl: 'http://weave.test', apiKey: 'dispatch-test-credential' }

function user(session: Session, text: string, source: MessageSource = { kind: 'user' }): void {
  session.append('user/message', createUserMessage({ content: [{ type: 'text', text }], source }), { surfaceOp: 'append' })
}

function persisted(session: Session): SessionEvent[] {
  return JSON.parse(JSON.stringify(session.events)) as SessionEvent[]
}

interface HTTPRequest {
  readonly path: string
  readonly body: Record<string, unknown>
  readonly signal: AbortSignal | null | undefined
}

function requestOf(input: Parameters<typeof fetch>[0], init: RequestInit | undefined): HTTPRequest {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
  if (typeof init?.body !== 'string') throw new Error('Expected a serialized request body')
  return { path: new URL(url).pathname, body: JSON.parse(init.body) as Record<string, unknown>, signal: init.signal }
}

/** Stateful fake of external HTTP, retaining remote registration across lost responses. */
function api() {
  const requests: HTTPRequest[] = []
  const registrations = new Map<string, { input_revision_id: string; client_request_id: string; task_sha256: string }>()
  const heads = new Map<string, string>()
  const admitted = new Map<string, Record<string, unknown>>()
  const admissions: Record<string, unknown>[] = []
  let beforeRequest: ((request: HTTPRequest) => Promise<Response | undefined>) | undefined
  let beforeReply: ((request: HTTPRequest) => Promise<Response | undefined>) | undefined
  const fetcher = vi.fn<typeof fetch>(async (input, init) => {
    const request = requestOf(input, init)
    requests.push(request)
    const intercepted = await beforeRequest?.(request)
    if (intercepted !== undefined) return intercepted
    let response: Response
    if (request.path === '/v1/workbench/dispatch-inputs') {
      const id = String(request.body.registration_id)
      let revision = registrations.get(id)
      if (revision === undefined) {
        const sessionId = String(request.body.workbench_session_id)
        if (request.body.expected_revision_id !== (heads.get(sessionId) ?? '')) {
          return Response.json({ code: 'dispatch_input_revision_conflict' }, { status: 409 })
        }
        const serial = String(registrations.size + 1).padStart(12, '0')
        revision = { input_revision_id: `10000000-0000-4000-8000-${serial}`,
          client_request_id: `20000000-0000-4000-8000-${serial}`, task_sha256: hash(String(request.body.task)) }
        registrations.set(id, revision)
        heads.set(sessionId, revision.input_revision_id)
      }
      response = Response.json(revision)
    } else if (request.path.endsWith('/reconcile')) {
      const revisionId = request.path.split('/').at(-2)
      const receipt = [...registrations.values()].find(item => item.input_revision_id === revisionId)
      if (receipt === undefined) throw new Error('Expected an existing input revision')
      const result = admitted.get(receipt.input_revision_id)
      response = Response.json(result === undefined ? { state: 'closed', receipt } : { state: 'accepted', receipt, result })
    } else {
      const revisionId = String(request.body.input_revision_id)
      let result = admitted.get(revisionId)
      if (result === undefined) {
        result = { run_id: `run-${String(request.body.client_request_id)}`,
          client_request_id: request.body.client_request_id, input_revision_id: request.body.input_revision_id, status: 'queued' }
        admitted.set(revisionId, result)
        admissions.push(result)
      }
      response = Response.json(result)
    }
    return await beforeReply?.(request) ?? response
  })
  vi.stubGlobal('fetch', fetcher)
  return { requests, registrations, admitted, admissions, fetcher,
    beforeRequest: (callback: typeof beforeRequest) => { beforeRequest = callback },
    beforeReply: (callback: typeof beforeReply) => { beforeReply = callback } }
}

const contexts: Context[] = []

/** Real SessionStore, ToolRuntime, and plugin effects; only HTTP is replaced. */
async function host(seed?: readonly SessionEvent[], header?: SessionHeader) {
  const ctx = new Context()
  contexts.push(ctx)
  await ctx.plugin(SessionStore)
  await ctx.plugin(SystemPrompt)
  await ctx.plugin(ToolRuntime)
  const session = ctx.sessions.create(header?.id ?? SessionId('dispatch-supervisor'), seed === undefined ? {} : {
    seed, ...(header === undefined ? {} : { meta: header }),
  })
  const durable: SessionEvent[][] = []
  ctx.on('session/flush', (current) => { durable.push(persisted(current)) })
  let service: ReturnType<typeof installDispatchInputTool> | undefined
  const fiber = await ctx.plugin(Object.assign((inner: Context) => { service = installDispatchInputTool(inner, connection) }, { inject: ['sessions', 'tools'] }))
  if (service === undefined) throw new Error('Dispatch input service was not installed')
  // The registered executor reads the session; this test does not start a model driver.
  let calls = 0
  const executeIn = (
    current: Session, arguments_: Record<string, unknown> = { ...facts }, signal = new AbortController().signal,
  ) => ctx.tools.execute({
    signal, callId: ToolCallId(`dispatch-call-${++calls}`), name: 'weave_dispatch', arguments: arguments_,
    agent: { id: current.id, session: current } as unknown as Agent,
  })
  const execute = (arguments_: Record<string, unknown> = { ...facts }, signal = new AbortController().signal) => (
    executeIn(session, arguments_, signal)
  )
  return { ctx, session, durable, fiber, service, execute, executeIn }
}

function rerunAction(session: Session, actionId: string, brief: string, targetRunId = 'original-run'): void {
  const action: WorkTaskPendingAction = { kind: 'rerun', targetRunId, idempotencyKey: '', clientRequestId: actionId,
    brief, requestedAt: Date.now(), targetKind: '', targetMemberId: '', correctionId: '', disposition: '', instruction: '', nodeId: '' }
  session.append('weave/work-task-action', { pendingAction: action })
}

afterEach(async () => {
  for (const ctx of contexts.splice(0).reverse()) await ctx.fiber.dispose()
  vi.unstubAllGlobals()
})

describe('dispatch source inputs', () => {
  it('preserves a single original message byte for byte', () => {
    const session = Session.create(SessionId('bytes'))
    const original = '  INV-440\r\n客户说“保留  这段”\n\tSKU=中文-α  \n'
    user(session, original)
    const record = prepareDispatchInput(session, facts)
    expect(Buffer.from(record.task)).toEqual(Buffer.from(original))
    expect(record.sourceMessages).toHaveLength(1)
    const message = session.events.find(event => event.type === 'user/message')!
    expect(record.sourceMessages[0]).toMatchObject({ message_id: message.data.id, event_seq: message.seq,
      sha256: hash(JSON.stringify(message.data.content)), content: message.data.content })
    expect(record.state).toBe('pending')
    expect(latestDispatchInput(session)).toBeUndefined()
  })

  it('retains preparation inputs and confirmation while excluding producer context', () => {
    const session = Session.create(SessionId('multi-turn'))
    user(session, '处理 INV-440，原始数量 12。')
    user(session, '我选择团队 orders，先确认不要派发。', { kind: 'plugin', plugin: 'weave-team-select' })
    user(session, '还需要核对日历和邮件，保留原发票。')
    user(session, '系统摘要：改成 VBR-52。', { kind: 'plugin', plugin: 'context-summary' })
    user(session, '确认并派发。')
    const record = prepareDispatchInput(session, facts)
    expect(record.sourceMessages).toHaveLength(3)
    expect(record.task).toBe('[User input 1]\n处理 INV-440，原始数量 12。\n\n[User input 2]\n还需要核对日历和邮件，保留原发票。\n\n[User input 3]\n确认并派发。')
    expect(record.task).not.toContain('VBR-52')
    expect(record.task).not.toContain('orders')
  })

  it('requires real user input for a new dispatch', () => {
    const session = Session.create(SessionId('no-user'))
    user(session, '请核对已有成果，不要重跑。', { kind: 'plugin', plugin: 'weave-check-result' })
    expect(() => prepareDispatchInput(session, facts)).toThrow('dispatch_input_missing')
  })

  it.each([
    { label: 'a task differing from the frozen source text',
      change: (record: DispatchInputRecord): DispatchInputRecord => ({ ...record, task: '处理旧 VBR-52。' }) },
    { label: 'an unknown source message identity', change: (record: DispatchInputRecord): DispatchInputRecord => ({ ...record,
      sourceMessages: [{ ...record.sourceMessages[0]!, message_id: 'message-from-another-task' }, record.sourceMessages[1]!] }) },
    { label: 'a source identity pointing at another event sequence', change: (record: DispatchInputRecord): DispatchInputRecord => ({ ...record,
      sourceMessages: [{ ...record.sourceMessages[0]!, event_seq: record.sourceMessages[0]!.event_seq + 100 },
        record.sourceMessages[1]!] }) },
    { label: 'reversed source order', change: (record: DispatchInputRecord): DispatchInputRecord => ({ ...record,
      sourceMessages: [record.sourceMessages[1]!, record.sourceMessages[0]!], sourceThroughSeq: record.sourceMessages[0]!.event_seq }) },
    { label: 'a repeated source event', change: (record: DispatchInputRecord): DispatchInputRecord => ({ ...record,
      sourceMessages: [record.sourceMessages[0]!, record.sourceMessages[0]!], sourceThroughSeq: record.sourceMessages[0]!.event_seq }) },
    { label: 'a repeated message identity at another sequence', change: (record: DispatchInputRecord): DispatchInputRecord => ({ ...record,
      sourceMessages: [record.sourceMessages[0]!, { ...record.sourceMessages[1]!, message_id: record.sourceMessages[0]!.message_id }] }) },
    { label: 'self-consistent hashes for text different from the authoritative user message', change: (record: DispatchInputRecord): DispatchInputRecord => {
      const content = [{ type: 'text', text: '处理旧 VBR-52。' }]
      return { ...record, task: '[User input 1]\n处理旧 VBR-52。\n\n[User input 2]\n确认。',
        sourceMessages: [{ ...record.sourceMessages[0]!, content, sha256: hash(JSON.stringify(content)) }, record.sourceMessages[1]!] }
    } },
  ])('rejects a restored pending record containing $label', ({ change }) => {
    const session = Session.create(SessionId('original-input'))
    user(session, '处理 INV-440。')
    user(session, '确认。')
    session.append('weave/dispatch-input', prepareDispatchInput(session, facts))
    const seed = persisted(session).map(event => event.type === 'weave/dispatch-input'
      ? { ...event, data: change(event.data) } : event)
    const restored = Session.create(SessionId('restored-input'), seed)
    expect(() => latestDispatchInput(restored)).toThrow()
  })
})

describe('Workbench-owned dispatch tool', () => {
  it('rejects model task text through the registered executor before transport', async () => {
    const remote = api()
    const app = await host()
    user(app.session, '处理 INV-440。')
    expect(app.ctx.tools.schemas().find(tool => tool.name === 'weave_dispatch')?.parameters).not.toHaveProperty('properties.task')
    expect((await app.execute({ ...facts, task: '处理旧 VBR-52。' })).isError).toBe(true)
    expect(remote.requests).toEqual([])
    expect(latestDispatchInput(app.session)).toBeUndefined()
  })

  it('flushes before transport and dispatches only the API-issued reference', async () => {
    const remote = api()
    const app = await host()
    const original = '  处理 INV-440。\n'
    user(app.session, original)
    remote.beforeReply(async (request) => {
      expect(app.durable.length).toBeGreaterThan(0)
      const last = app.durable.at(-1)!.findLast(event => event.type === 'weave/dispatch-input')!
      expect(last.data.state).toBe(request.path.endsWith('dispatch-inputs') ? 'pending' : 'registered')
      return undefined
    })
    const result = await app.execute()
    expect(result.isError).toBe(false)
    expect(remote.requests).toHaveLength(2)
    expect(remote.requests[0]!.body.task).toBe(original)
    const accepted = latestDispatchInput(app.session)!
    expect(remote.requests[1]!.body).toEqual({ input_revision_id: accepted.revision!.input_revision_id,
      client_request_id: accepted.revision!.client_request_id })
    expect(accepted.state).toBe('accepted')
    expect(JSON.stringify(app.session.events)).not.toContain(connection.apiKey)
    expect(await app.execute()).toEqual(result)
    expect(remote.requests).toHaveLength(2)
  })

  it('does not reuse an in-memory pending request until a failed persistence flush succeeds', async () => {
    const remote = api()
    const app = await host()
    user(app.session, '处理 INV-440，不得丢失派发身份。')
    let unavailable = true
    app.ctx.on('session/flush', () => {
      if (unavailable) throw new Error('durable store unavailable')
    })
    expect((await app.execute()).isError).toBe(true)
    const pending = latestDispatchInput(app.session)!
    expect(remote.requests).toHaveLength(0)
    expect((await app.execute()).isError).toBe(true)
    expect(remote.requests).toHaveLength(0)
    unavailable = false
    expect((await app.execute()).isError).toBe(false)
    expect(latestDispatchInput(app.session)?.registrationId).toBe(pending.registrationId)
    expect(remote.registrations.size).toBe(1)
  })

  it('uses only new INV-440 inputs after accepting VBR-52 in the same session', async () => {
    const remote = api()
    const app = await host()
    user(app.session, '处理 VBR-52，数量 5。')
    expect((await app.execute()).isError).toBe(false)
    const previous = latestDispatchInput(app.session)!
    user(app.session, '处理 INV-440，数量 12。')
    user(app.session, '确认。')
    expect((await app.execute()).isError).toBe(false)
    const current = latestDispatchInput(app.session)!
    expect(current.registrationId).not.toBe(previous.registrationId)
    expect(current.expectedRevisionId).toBe(previous.revision!.input_revision_id)
    expect(current.sourceMessages.every(message => message.event_seq > previous.sourceThroughSeq)).toBe(true)
    expect(current.task).toContain('INV-440')
    expect(current.task).toContain('确认。')
    expect(current.task).not.toContain('VBR-52')
    expect(remote.registrations.size).toBe(2)
  })

  it('keeps forked task inputs and revision heads independent across cold reads with the inherited header', async () => {
    const remote = api()
    const app = await host()
    user(app.session, '父会话 VBR-52 已完成，不得作为子任务重派。')
    expect((await app.execute()).isError).toBe(false)
    const parent = latestDispatchInput(app.session)!
    const child = app.ctx.sessions.fork(app.session, undefined, SessionId('dispatch-child'))
    expect(child.header).toMatchObject({ parentSession: app.session.id, seedLength: app.session.events.length })
    expect(latestDispatchInput(child)).toBeUndefined()
    const beforeChild = remote.requests.length
    expect(() => prepareDispatchInput(child, facts)).toThrow('dispatch_input_missing')
    expect((await app.executeIn(child)).isError).toBe(true)
    expect(remote.requests).toHaveLength(beforeChild)

    const reloaded = await host(persisted(child), structuredClone(child.header))
    expect(reloaded.session.header).toEqual(child.header)
    expect(latestDispatchInput(reloaded.session)).toBeUndefined()
    user(reloaded.session, '子会话只处理 INV-440。')
    const prepared = prepareDispatchInput(reloaded.session, facts)
    expect(prepared).toMatchObject({ expectedRevisionId: '', task: '子会话只处理 INV-440。' })
    expect(prepared.sourceMessages).toHaveLength(1)
    expect(prepared.sourceMessages[0]!.event_seq).toBeGreaterThanOrEqual(child.header.seedLength!)
    expect((await reloaded.execute()).isError).toBe(false)
    const firstChild = latestDispatchInput(reloaded.session)!
    expect(firstChild.revision?.input_revision_id).not.toBe(parent.revision?.input_revision_id)
    expect(firstChild.registrationId).not.toBe(parent.registrationId)
    expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs')).at(-1)?.body).toMatchObject({
      workbench_session_id: child.id, expected_revision_id: '', task: firstChild.task,
    })
    const saved = persisted(reloaded.session)
    const header = structuredClone(reloaded.session.header)
    await reloaded.fiber.dispose()
    const resumed = await host(saved, header)
    expect(resumed.session.header).toEqual(child.header)
    expect(latestDispatchInput(resumed.session)).toEqual(firstChild)
    const beforeReplay = remote.requests.length
    expect((await resumed.execute()).isError).toBe(false)
    expect(remote.requests).toHaveLength(beforeReplay)
    user(resumed.session, '子会话下一项独立任务 INV-441。')
    expect((await resumed.execute()).isError).toBe(false)
    const secondChild = latestDispatchInput(resumed.session)!
    expect(secondChild).toMatchObject({ expectedRevisionId: firstChild.revision!.input_revision_id,
      task: '子会话下一项独立任务 INV-441。' })
    expect(secondChild.sourceMessages).toHaveLength(1)
    expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs')).at(-1)?.body).toMatchObject({
      workbench_session_id: child.id, expected_revision_id: firstChild.revision!.input_revision_id, task: secondChild.task,
    })
    expect(latestDispatchInput(app.session)).toEqual(parent)
    expect(remote.registrations.size).toBe(3)
    expect(remote.admissions).toHaveLength(3)
  })

  it.each(['pending', 'registered'] as const)('requires the parent to resolve an inherited %s input before a fresh fork can dispatch', async (state) => {
    const remote = api()
    remote.beforeReply(async (request) => {
      if (request.path.endsWith(state === 'pending' ? 'dispatch-inputs' : '/dispatch')) throw new TypeError('parent response lost')
      return undefined
    })
    const app = await host()
    user(app.session, '父会话 VBR-52 的受理结果尚未明确。')
    expect((await app.execute()).isError).toBe(true)
    const parent = latestDispatchInput(app.session)!
    expect(parent.state).toBe(state)
    const child = app.ctx.sessions.fork(app.session, undefined, SessionId(`dispatch-child-${state}`))
    user(child, '子会话新任务 INV-440，不得改变父请求。')
    expect(latestDispatchInput(child)).toBeUndefined()
    const beforeChild = remote.requests.length
    expect(() => prepareDispatchInput(child, facts)).toThrow('dispatch_inherited_input_pending')
    const result = await app.executeIn(child)
    expect(result.isError).toBe(true)
    expect(JSON.stringify(result)).toContain('dispatch_inherited_input_pending')
    const reloaded = await host(persisted(child), structuredClone(child.header))
    expect(reloaded.session.header).toEqual(child.header)
    expect((await reloaded.execute()).isError).toBe(true)
    expect(() => prepareDispatchInput(reloaded.session, facts)).toThrow('dispatch_inherited_input_pending')
    expect(remote.requests).toHaveLength(beforeChild)
    expect(latestDispatchInput(app.session)).toEqual(parent)
    expect(child.events.slice(child.header.seedLength).some(event => event.type === 'weave/dispatch-input')).toBe(false)

    remote.beforeReply(undefined)
    expect((await app.execute()).isError).toBe(false)
    const resolvedParent = latestDispatchInput(app.session)!
    expect(resolvedParent.registrationId).toBe(parent.registrationId)
    const afterResolution = remote.requests.length
    expect((await app.executeIn(child)).isError).toBe(true)
    expect(remote.requests).toHaveLength(afterResolution)
    const fresh = app.ctx.sessions.fork(app.session, undefined, SessionId(`dispatch-fresh-${state}`))
    user(fresh, '从父请求已解决的位置分叉，仅处理 INV-440。')
    expect((await app.executeIn(fresh)).isError).toBe(false)
    const accepted = latestDispatchInput(fresh)!
    expect(accepted).toMatchObject({ expectedRevisionId: '', task: '从父请求已解决的位置分叉，仅处理 INV-440。' })
    expect(accepted.sourceMessages).toHaveLength(1)
    expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs')).at(-1)?.body).toMatchObject({
      workbench_session_id: fresh.id, expected_revision_id: '',
    })
    expect(remote.requests.some(request => request.path.endsWith('/reconcile'))).toBe(false)
    expect(remote.registrations.size).toBe(2)
    expect(remote.admissions).toHaveLength(2)
  })

  it('rejects an inherited pending rerun action and accepts a new child-owned action with an empty child head', async () => {
    const remote = api()
    const app = await host()
    user(app.session, '父会话已受理 VBR-52。')
    expect((await app.execute()).isError).toBe(false)
    const parent = latestDispatchInput(app.session)!
    const parentRunId = parent.result?.run_id
    if (typeof parentRunId !== 'string') throw new Error('Expected the original admitted run identity')
    const inheritedActionId = '80000000-0000-4000-8000-000000000010'
    rerunAction(app.session, inheritedActionId, '父会话尚未派发的重跑要求。', parentRunId)
    const child = app.ctx.sessions.fork(app.session, undefined, SessionId('dispatch-rerun-child'))
    expect(latestDispatchInput(child)).toBeUndefined()
    const beforeRerun = remote.requests.length
    await expect(app.service.rerun(child, facts, inheritedActionId, new AbortController().signal)).rejects.toThrow('dispatch_rerun_source_missing')
    const reloaded = await host(persisted(child), structuredClone(child.header))
    expect(reloaded.session.header).toEqual(child.header)
    await expect(reloaded.service.rerun(reloaded.session, facts, inheritedActionId, new AbortController().signal)).rejects.toThrow('dispatch_rerun_source_missing')
    expect(remote.requests).toHaveLength(beforeRerun)
    expect(latestDispatchInput(reloaded.session)).toBeUndefined()
    const childActionId = '80000000-0000-4000-8000-000000000011'
    const brief = '  子会话明确请求重跑 VBR-52，仅采用这里的修改。\n'
    rerunAction(reloaded.session, childActionId, brief, parentRunId)
    await reloaded.service.rerun(reloaded.session, facts, childActionId, new AbortController().signal)
    const accepted = latestDispatchInput(reloaded.session)!
    expect(accepted).toMatchObject({ state: 'accepted', expectedRevisionId: '', task: brief,
      rerun: { actionId: childActionId, targetRunId: parentRunId } })
    expect(accepted.sourceMessages).toHaveLength(1)
    expect(accepted.sourceMessages[0]).toMatchObject({ message_id: `workbench-rerun:${childActionId}` })
    expect(accepted.sourceMessages[0]!.event_seq).toBeGreaterThanOrEqual(child.header.seedLength!)
    expect(accepted.registrationId).not.toBe(parent.registrationId)
    expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs')).at(-1)?.body).toMatchObject({
      workbench_session_id: child.id, expected_revision_id: '', task: brief,
    })
    expect(latestDispatchInput(app.session)).toEqual(parent)
    expect(remote.registrations.size).toBe(2)
    expect(remote.admissions).toHaveLength(2)
  })

  it.each(['registration', 'dispatch'])('reuses identity after a lost %s response and reload', async (phase) => {
    const remote = api()
    let dropped = false
    remote.beforeReply(async (request) => {
      if (!dropped && request.path.endsWith(phase === 'registration' ? 'dispatch-inputs' : '/dispatch')) {
        dropped = true
        throw new TypeError('connection closed after remote admission')
      }
      return undefined
    })
    const app = await host()
    user(app.session, '处理 INV-440，不得重复创建。')
    expect((await app.execute()).isError).toBe(true)
    const pending = latestDispatchInput(app.session)!
    expect(pending.state).toBe(phase === 'registration' ? 'pending' : 'registered')
    const firstRegistration = remote.requests[0]!.body
    const saved = persisted(app.session)
    await app.fiber.dispose()
    const reloaded = await host(saved)
    expect((await reloaded.execute()).isError).toBe(false)
    const accepted = latestDispatchInput(reloaded.session)!
    expect(accepted.registrationId).toBe(pending.registrationId)
    expect(accepted.sourceMessages).toEqual(pending.sourceMessages)
    expect(accepted.task).toBe(pending.task)
    expect(remote.registrations.size).toBe(1)
    expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs'))
      .every(request => JSON.stringify(request.body) === JSON.stringify(firstRegistration))).toBe(true)
    expect(remote.requests.filter(request => request.path.endsWith('/dispatch')).every(request =>
      request.body.input_revision_id === accepted.revision!.input_revision_id
      && request.body.client_request_id === accepted.revision!.client_request_id)).toBe(true)
    expect(accepted.state).toBe('accepted')
  })

  it.each([
    ['pending', 'accepted'], ['pending', 'closed'], ['registered', 'accepted'], ['registered', 'closed'],
  ] as const)('reconciles a %s request as %s when new user input arrives, without resending the old task', async (state, outcome) => {
    const remote = api()
    let dropped = false
    remote.beforeReply(async (request) => {
      if (!dropped && request.path.endsWith(state === 'pending' ? 'dispatch-inputs' : '/dispatch')) {
        dropped = true
        throw new TypeError('response lost')
      }
      return undefined
    })
    const app = await host()
    user(app.session, '处理 VBR-52，保留发票原始材料。')
    expect((await app.execute()).isError).toBe(true)
    const old = latestDispatchInput(app.session)!
    expect(old.state).toBe(state)
    const receipt = [...remote.registrations.values()][0]!
    if (outcome === 'accepted') remote.admitted.set(receipt.input_revision_id, { run_id: 'already-admitted-old-run',
      client_request_id: receipt.client_request_id, input_revision_id: receipt.input_revision_id })
    else remote.admitted.clear()
    const dispatchesBefore = remote.requests.filter(request => request.path.endsWith('/dispatch')).length
    user(app.session, '现在按 INV-440 的要求处理，保留这条修改原话。')
    remote.beforeReply(undefined)
    const reconciled = await app.execute()
    expect(reconciled.isError).toBe(outcome === 'closed')
    expect(latestDispatchInput(app.session)?.state).toBe(outcome)
    expect(latestDispatchInput(app.session)?.registrationId).toBe(old.registrationId)
    expect(remote.requests.filter(request => request.path.endsWith('/dispatch'))).toHaveLength(dispatchesBefore)
    expect(remote.requests.at(-1)).toMatchObject({ path: `/v1/workbench/dispatch-inputs/${receipt.input_revision_id}/reconcile`, body: {} })
    expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs'))).toHaveLength(state === 'pending' ? 2 : 1)
    if (outcome === 'accepted') expect(latestDispatchInput(app.session)?.result?.run_id).toBe('already-admitted-old-run')
    expect((await app.execute()).isError).toBe(false)
    const next = latestDispatchInput(app.session)!
    expect(next.registrationId).not.toBe(old.registrationId)
    expect(next.task).toContain('INV-440')
    if (outcome === 'accepted') expect(next.task).not.toContain('VBR-52')
    else {
      // Closing proves non-admission, not that earlier user instructions may be discarded.
      expect(next.task).toContain('VBR-52')
      expect(next.sourceMessages).toHaveLength(2)
    }
  })

  it('keeps the input unresolved when reconciliation returns another revision receipt', async () => {
    const remote = api()
    let dropped = false
    remote.beforeReply(async (request) => {
      if (!dropped && request.path.endsWith('/dispatch')) { dropped = true; throw new TypeError('response lost') }
      return undefined
    })
    const app = await host()
    user(app.session, '处理 VBR-52。')
    expect((await app.execute()).isError).toBe(true)
    const original = latestDispatchInput(app.session)!
    user(app.session, '补充 INV-440 的原始材料。')
    remote.beforeReply(async request => request.path.endsWith('/reconcile')
      ? Response.json({ state: 'closed', receipt: { ...original.revision, input_revision_id: '90000000-0000-4000-8000-000000000001' } }) : undefined)
    expect((await app.execute()).isError).toBe(true)
    expect(latestDispatchInput(app.session)?.state).toBe('registered')
    expect(latestDispatchInput(app.session)?.revision).toEqual(original.revision)
    expect(remote.requests.filter(request => request.path.endsWith('/dispatch'))).toHaveLength(1)
  })

  it('reruns only the brief saved by a user action and preserves unrelated new source messages', async () => {
    const remote = api()
    const app = await host()
    user(app.session, '原任务 VBR-52。')
    expect((await app.execute()).isError).toBe(false)
    const original = latestDispatchInput(app.session)!
    const originalRunId = original.result?.run_id
    if (typeof originalRunId !== 'string') throw new Error('Expected the original admitted run identity')
    user(app.session, '另一个任务 INV-440，尚未派发。')
    const actionId = '80000000-0000-4000-8000-000000000001'
    const brief = '  重跑 VBR-52，只调整用户指定数量。\n'
    rerunAction(app.session, actionId, brief, originalRunId)
    const result = await app.service.rerun(app.session, facts, actionId, new AbortController().signal)
    const rerun = latestDispatchInput(app.session)!
    expect(rerun.task).toBe(brief)
    expect(rerun.rerun).toEqual({ targetRunId: original.result!.run_id, actionId })
    expect(rerun.sourceMessages).toHaveLength(1)
    expect(rerun.sourceMessages[0]?.message_id).toBe(`workbench-rerun:${actionId}`)
    expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs')).at(-1)?.body.task).toBe(brief)
    const requestCount = remote.requests.length
    expect(await app.service.rerun(app.session, facts, actionId, new AbortController().signal)).toEqual(result)
    expect(remote.requests).toHaveLength(requestCount)
    expect(prepareDispatchInput(app.session, facts).task).toBe('另一个任务 INV-440，尚未派发。')
  })

  it('rejects a rerun with no recorded user action before HTTP', async () => {
    const remote = api()
    const app = await host()
    user(app.session, '原任务 VBR-52。')
    await expect(app.service.rerun(app.session, facts, 'missing-action', new AbortController().signal)).rejects.toThrow('dispatch_rerun_source_missing')
    expect(remote.requests).toHaveLength(0)
  })

  it('rejects a restored rerun whose internally matching text no longer matches its user action', async () => {
    const remote = api()
    remote.beforeReply(async () => { throw new TypeError('response lost') })
    const app = await host()
    const actionId = '80000000-0000-4000-8000-000000000003'
    rerunAction(app.session, actionId, '用户实际编辑的 INV-440 重跑要求。')
    await expect(app.service.rerun(app.session, facts, actionId, new AbortController().signal)).rejects.toThrow('response lost')
    const content = [{ type: 'text', text: '被替换成旧 VBR-52。' }]
    const seed = persisted(app.session).map(event => event.type === 'weave/dispatch-input' ? { ...event, data: {
      ...event.data, task: content[0]!.text,
      sourceMessages: [{ ...event.data.sourceMessages[0]!, content, sha256: hash(JSON.stringify(content)) }],
    } } : event)
    const restored = Session.create(SessionId('restored-rerun'), seed)
    expect(() => latestDispatchInput(restored)).toThrow()
  })

  it('does not change the team or workflow when retrying the same durable rerun action', async () => {
    const remote = api()
    remote.beforeReply(async () => { throw new TypeError('response lost') })
    const app = await host()
    const actionId = '80000000-0000-4000-8000-000000000002'
    rerunAction(app.session, actionId, '用户已编辑的重跑要求。')
    await expect(app.service.rerun(app.session, facts, actionId, new AbortController().signal)).rejects.toThrow('response lost')
    const requestCount = remote.requests.length
    await expect(app.service.rerun(app.session, { ...facts, team_id: 'unconfirmed-team' }, actionId, new AbortController().signal))
      .rejects.toThrow('dispatch_input_pending')
    expect(remote.requests).toHaveLength(requestCount)
  })

  it.each([
    ['normal', 'accepted'], ['normal', 'closed'], ['rerun', 'accepted'], ['rerun', 'closed'],
  ] as const)('rechecks new user input after an in-flight %s registration and reconciles %s before dispatch', async (kind, outcome) => {
    const remote = api()
    const entered = Promise.withResolvers<undefined>()
    const release = Promise.withResolvers<undefined>()
    remote.beforeReply(async (request) => {
      if (request.path.endsWith('dispatch-inputs') && remote.registrations.size === 1) { entered.resolve(undefined); await release.promise }
      return undefined
    })
    const app = await host()
    const actionId = '80000000-0000-4000-8000-000000000004'
    if (kind === 'normal') user(app.session, '处理 VBR-52。')
    else rerunAction(app.session, actionId, '重跑 VBR-52。')
    const first = kind === 'normal' ? app.execute()
      : app.service.rerun(app.session, facts, actionId, new AbortController().signal)
        .then(result => ({ isError: false, result }), (error: unknown) => ({ isError: true, error }))
    await entered.promise
    const frozen = latestDispatchInput(app.session)!
    expect(frozen.state).toBe('pending')
    user(app.session, '改为处理 INV-440，保留这条修改原话。')
    const receipt = [...remote.registrations.values()][0]!
    if (outcome === 'accepted') remote.admitted.set(receipt.input_revision_id, { run_id: 'already-admitted-old-run',
      client_request_id: receipt.client_request_id, input_revision_id: receipt.input_revision_id })
    release.resolve(undefined)
    expect((await first).isError).toBe(outcome === 'closed')
    const reconciled = latestDispatchInput(app.session)!
    expect(reconciled.registrationId).toBe(frozen.registrationId)
    expect(reconciled.state).toBe(outcome)
    expect(reconciled.task).toBe(frozen.task)
    expect(reconciled.sourceMessages).toEqual(frozen.sourceMessages)
    expect(remote.requests.map(request => request.path)).toEqual([
      '/v1/workbench/dispatch-inputs', `/v1/workbench/dispatch-inputs/${receipt.input_revision_id}/reconcile`,
    ])
    expect(remote.requests.at(-1)?.body).toEqual({})
    expect(remote.requests.some(request => request.path.endsWith('/dispatch'))).toBe(false)
    if (outcome === 'accepted') expect(reconciled.result?.run_id).toBe('already-admitted-old-run')
    const next = prepareDispatchInput(app.session, facts)
    expect(next.task).toContain('INV-440')
    if (kind === 'normal' && outcome === 'closed') expect(next.task).toContain('VBR-52')
    else expect(next.task).not.toContain('VBR-52')
  })

  it('deduplicates concurrent matching facts and rejects different facts', async () => {
    const remote = api()
    const entered = Promise.withResolvers<undefined>()
    const release = Promise.withResolvers<undefined>()
    remote.beforeReply(async (request) => {
      if (request.path.endsWith('dispatch-inputs')) { entered.resolve(undefined); await release.promise }
      return undefined
    })
    const app = await host()
    user(app.session, '处理 INV-440。')
    const first = app.execute()
    await entered.promise
    const second = app.execute()
    expect((await app.execute({ ...facts, team_id: 'other-team' })).isError).toBe(true)
    expect(remote.requests).toHaveLength(1)
    release.resolve(undefined)
    const [left, right] = await Promise.all([first, second])
    expect(left.isError).toBe(false)
    expect(right).toEqual(left)
    expect(remote.requests).toHaveLength(2)
    expect(remote.registrations.size).toBe(1)
  })

  it.each([
    { code: 'team_not_found', status: 404 },
    { code: 'team_not_active', status: 409 },
    { code: 'no_default_workflow', status: 409 },
    { code: 'default_workflow_unavailable', status: 409 },
    { code: 'workflow_team_mismatch', status: 409 },
    { code: 'workflow_not_published', status: 409 },
  ])('allows corrected team and workflow facts after registration rejects $code', async ({ code, status }) => {
    const remote = api()
    remote.beforeRequest(async request => request.path.endsWith('dispatch-inputs')
      ? Response.json({ code }, { status }) : undefined)
    const app = await host()
    user(app.session, '处理 INV-440。')
    expect((await app.execute()).isError).toBe(true)
    const rejected = latestDispatchInput(app.session)!
    expect(rejected).toMatchObject({ state: 'rejected', errorCode: code, revision: null })
    expect(remote.registrations.size).toBe(0)
    expect(remote.admissions).toHaveLength(0)
    const correctedFacts = { team_id: 'replacement-team', workflow_id: 'replacement-workflow', workflow_version: 2 }
    const replacement = prepareDispatchInput(app.session, correctedFacts)
    expect(replacement.sourceMessages).toEqual(rejected.sourceMessages)
    expect(replacement.task).toBe(rejected.task)
    expect(replacement.registrationId).not.toBe(rejected.registrationId)
    remote.beforeRequest(undefined)
    expect((await app.execute(correctedFacts)).isError).toBe(false)
    const accepted = latestDispatchInput(app.session)!
    expect(accepted).toMatchObject({ state: 'accepted', facts: correctedFacts, task: rejected.task, sourceMessages: rejected.sourceMessages })
    expect(accepted.registrationId).not.toBe(rejected.registrationId)
    expect(remote.registrations.size).toBe(1)
    expect(remote.admissions).toHaveLength(1)
  })

  it.each([400, 401, 403, 404, 408, 409, 422, 429, 500, 503])('keeps HTTP %s unresolved and forbids changing selected facts', async (status) => {
    const remote = api()
    remote.beforeReply(async () => Response.json({ code: 'dispatch_temporarily_unavailable' }, { status }))
    const app = await host()
    user(app.session, '处理 INV-440。')
    expect((await app.execute()).isError).toBe(true)
    const pending = latestDispatchInput(app.session)!
    expect(pending.state).toBe('pending')
    expect(prepareDispatchInput(app.session, facts)).toEqual(pending)
    expect(() => prepareDispatchInput(app.session, { ...facts, team_id: 'replacement-team' })).toThrow('dispatch_input_pending')
  })

  describe.each(['dispatch', 'reconcile'] as const)('HTTP errors while retrying %s', (phase) => {
    it.each([
      { status: 401, code: 'unauthorized' },
      { status: 403, code: 'forbidden' },
      { status: 404, code: 'team_not_found' },
      { status: 409, code: 'team_not_active' },
    ])('recovers the same admitted run after a lost response and HTTP $status across reloads', async ({ status, code }) => {
      const remote = api()
      let dropped = false
      remote.beforeReply(async (request) => {
        if (!dropped && request.path.endsWith('/dispatch')) {
          dropped = true
          throw new TypeError('admission response lost')
        }
        return undefined
      })
      const app = await host()
      user(app.session, '处理 INV-440，同一请求只能受理一次。')
      expect((await app.execute()).isError).toBe(true)
      const original = latestDispatchInput(app.session)!
      expect(original.state).toBe('registered')
      const admittedRun = remote.admissions[0]!
      expect(remote.admissions).toHaveLength(1)
      const saved = persisted(app.session)
      await app.fiber.dispose()
      const reloaded = await host(saved)
      if (phase === 'reconcile') user(reloaded.session, '补充材料，先恢复上一请求的受理结果。')
      remote.beforeRequest(async request => request.path.endsWith(`/${phase}`)
        ? Response.json({ code }, { status }) : undefined)
      expect((await reloaded.execute()).isError).toBe(true)
      expect(latestDispatchInput(reloaded.session)).toEqual(original)
      expect(() => prepareDispatchInput(reloaded.session, { ...facts, team_id: 'replacement-team' })).toThrow('dispatch_input_pending')
      const afterHTTPError = persisted(reloaded.session)
      await reloaded.fiber.dispose()
      const recovered = await host(afterHTTPError)
      expect(latestDispatchInput(recovered.session)).toEqual(original)
      remote.beforeRequest(undefined)
      expect((await recovered.execute()).isError).toBe(false)
      const accepted = latestDispatchInput(recovered.session)!
      expect(accepted).toMatchObject({ state: 'accepted', registrationId: original.registrationId,
        revision: original.revision, task: original.task, sourceMessages: original.sourceMessages, result: admittedRun })
      expect(remote.registrations.size).toBe(1)
      expect(remote.admissions).toEqual([admittedRun])
      expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs'))).toHaveLength(1)
      const dispatches = remote.requests.filter(request => request.path.endsWith('/dispatch'))
      expect(dispatches).toHaveLength(phase === 'dispatch' ? 3 : 1)
      expect(dispatches.every(request => request.body.input_revision_id === original.revision!.input_revision_id
        && request.body.client_request_id === original.revision!.client_request_id)).toBe(true)
      if (phase === 'reconcile') {
        expect(remote.requests.filter(request => request.path.endsWith('/reconcile')).map(request => request.path)).toEqual([
          `/v1/workbench/dispatch-inputs/${original.revision!.input_revision_id}/reconcile`,
          `/v1/workbench/dispatch-inputs/${original.revision!.input_revision_id}/reconcile`,
        ])
      }
    })
  })

  it('keeps the same registration after its response is lost and authentication fails before recovery', async () => {
    const remote = api()
    let dropped = false
    remote.beforeReply(async (request) => {
      if (!dropped && request.path.endsWith('dispatch-inputs')) {
        dropped = true
        throw new TypeError('registration response lost')
      }
      return undefined
    })
    const app = await host()
    user(app.session, '处理 INV-440，恢复后仍使用这份原始输入。')
    expect((await app.execute()).isError).toBe(true)
    const original = latestDispatchInput(app.session)!
    expect(original).toMatchObject({ state: 'pending', revision: null })
    const revision = [...remote.registrations.values()][0]!
    const firstRegistration = remote.requests[0]!.body
    remote.beforeRequest(async () => Response.json({ code: 'unauthorized' }, { status: 401 }))
    expect((await app.execute()).isError).toBe(true)
    expect(latestDispatchInput(app.session)).toEqual(original)
    expect(() => prepareDispatchInput(app.session, { ...facts, team_id: 'replacement-team' })).toThrow('dispatch_input_pending')
    const saved = persisted(app.session)
    await app.fiber.dispose()
    const reloaded = await host(saved)
    remote.beforeRequest(undefined)
    expect((await reloaded.execute()).isError).toBe(false)
    expect(latestDispatchInput(reloaded.session)).toMatchObject({ state: 'accepted', registrationId: original.registrationId,
      revision, sourceMessages: original.sourceMessages, task: original.task,
      result: { input_revision_id: revision.input_revision_id, client_request_id: revision.client_request_id } })
    expect(remote.registrations.size).toBe(1)
    expect(remote.admissions).toHaveLength(1)
    expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs')).map(request => request.body)).toEqual([
      firstRegistration, firstRegistration, firstRegistration,
    ])
  })

  it('preserves the registered revision after dispatch fails and reuses its request key', async () => {
    const remote = api()
    let failDispatch = true
    remote.beforeReply(async request => request.path.endsWith('/dispatch') && failDispatch
      ? Response.json({ code: 'dispatch_temporarily_unavailable' }, { status: 503 }) : undefined)
    const app = await host()
    user(app.session, '处理 INV-440。')
    expect((await app.execute()).isError).toBe(true)
    const pending = latestDispatchInput(app.session)!
    expect(pending.state).toBe('registered')
    failDispatch = false
    expect((await app.execute()).isError).toBe(false)
    expect(remote.requests.filter(request => request.path.endsWith('dispatch-inputs'))).toHaveLength(1)
    expect(remote.requests.filter(request => request.path.endsWith('/dispatch')).map(request => request.body)).toEqual([
      { input_revision_id: pending.revision!.input_revision_id, client_request_id: pending.revision!.client_request_id },
      { input_revision_id: pending.revision!.input_revision_id, client_request_id: pending.revision!.client_request_id },
    ])
  })

  describe.each(['dispatch', 'reconcile'] as const)('%s admission receipts', (phase) => {
    it.each([
      { label: 'missing run identity', change: { run_id: undefined } },
      { label: 'empty run identity', change: { run_id: '' } },
      { label: 'non-string run identity', change: { run_id: 42 } },
      { label: 'missing request identity', change: { client_request_id: undefined } },
      { label: 'empty request identity', change: { client_request_id: '' } },
      { label: 'another request identity', change: { client_request_id: '30000000-0000-4000-8000-000000000001' } },
      { label: 'missing input revision identity', change: { input_revision_id: undefined } },
      { label: 'empty input revision identity', change: { input_revision_id: '' } },
      { label: 'another input revision identity', change: { input_revision_id: '40000000-0000-4000-8000-000000000001' } },
    ])('keeps admission unresolved for $label without filling from local state', async ({ change }) => {
      const remote = api()
      const app = await host()
      user(app.session, '处理 INV-440。')
      if (phase === 'reconcile') {
        remote.beforeReply(async (request) => {
          if (request.path.endsWith('/dispatch')) throw new TypeError('response lost')
          return undefined
        })
        expect((await app.execute()).isError).toBe(true)
        user(app.session, '补充原始材料，先确定上一请求的受理结果。')
      }
      remote.beforeReply(async (request) => {
        if (!request.path.endsWith(`/${phase}`)) return undefined
        const receipt = [...remote.registrations.values()][0]!
        const result = { run_id: 'admitted-run', client_request_id: receipt.client_request_id,
          input_revision_id: receipt.input_revision_id, ...change }
        return Response.json(phase === 'dispatch' ? result : { state: 'accepted', receipt, result })
      })
      expect((await app.execute()).isError).toBe(true)
      const unresolved = latestDispatchInput(app.session)!
      expect(unresolved.state).toBe('registered')
      expect(unresolved.result).toBeNull()
      expect(app.session.events.some(event => event.type === 'weave/dispatch-input' && event.data.state === 'accepted')).toBe(false)
      const restored = Session.create(SessionId(`unresolved-${phase}`), persisted(app.session))
      expect(latestDispatchInput(restored)).toEqual(unresolved)
      expect(prepareDispatchInput(restored, facts).registrationId).toBe(unresolved.registrationId)
    })
  })

  it('keeps unreadable responses unresolved even when the HTTP status is a rejection', async () => {
    const remote = api()
    remote.beforeReply(async () => new Response('gateway response truncated', { status: 403 }))
    const app = await host()
    user(app.session, '处理 INV-440。')
    expect((await app.execute()).isError).toBe(true)
    expect(latestDispatchInput(app.session)?.state).toBe('pending')
  })

  it('unregisters on HMR disposal, aborts owned HTTP, and retries unchanged after remount', async () => {
    const remote = api()
    const entered = Promise.withResolvers<AbortSignal>()
    let stopped = false
    remote.beforeReply(async (request) => {
      const signal = request.signal
      if (signal === undefined || signal === null) throw new Error('Missing owned request signal')
      entered.resolve(signal)
      return new Promise<Response>((_resolve, reject) => {
        signal.addEventListener('abort', () => {
          stopped = true
          reject(signal.reason instanceof Error ? signal.reason : new Error('Owned HTTP request aborted'))
        }, { once: true })
      })
    })
    const app = await host()
    user(app.session, '处理 INV-440。')
    const executing = app.execute()
    const ownedSignal = await entered.promise
    const pending = latestDispatchInput(app.session)!
    await app.fiber.dispose()
    expect(ownedSignal.aborted).toBe(true)
    expect(stopped).toBe(true)
    expect((await executing).isError).toBe(true)
    expect(app.ctx.tools.schemas().some(tool => tool.name === 'weave_dispatch')).toBe(false)
    expect(latestDispatchInput(app.session)?.registrationId).toBe(pending.registrationId)
    expect(latestDispatchInput(app.session)?.state).toBe('pending')
    remote.beforeReply(undefined)
    await app.ctx.plugin(Object.assign((inner: Context) => { installDispatchInputTool(inner, connection) }, { inject: ['sessions', 'tools'] }))
    expect((await app.execute()).isError).toBe(false)
    expect(latestDispatchInput(app.session)?.registrationId).toBe(pending.registrationId)
    expect(remote.registrations.size).toBe(1)
  })
})
