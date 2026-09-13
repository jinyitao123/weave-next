import { createHash } from 'node:crypto'
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Context } from '@deepseek-ai/cordis'
import Loader from '@deepseek-ai/cordis-plugin-loader'
import Include from '@deepseek-ai/cordis-plugin-include'
import AgentRegistry from '@deepseek-ai/dsh-agent'
import AgentLoop from '@deepseek-ai/dsh-agent-loop'
import AgentDefaultModel from '@deepseek-ai/dsh-agent-default-model'
import SessionController from '@deepseek-ai/dsh-api-session-controller'
import WorkspaceController from '@deepseek-ai/dsh-api-workspace-controller'
import type { SessionRequestId } from '@deepseek-ai/dsh-api-session-controller/types'
import AttachmentLocal from '@deepseek-ai/dsh-attachment-local'
import LlmRuntime, { LlmAdapter, ToolCallId, type GenerateOptions, type StreamChunk } from '@deepseek-ai/dsh-llm'
import SessionStore, { SessionId, type SessionEvent, type SessionHeader } from '@deepseek-ai/dsh-session'
import SessionPersistenceJsonl from '@deepseek-ai/dsh-session-persistence-jsonl'
import SessionProjectionRegistry from '@deepseek-ai/dsh-session-projection'
import SessionQuerySqlite from '@deepseek-ai/dsh-session-query-sqlite'
import Storage from '@deepseek-ai/dsh-storage'
import * as StorageDomain from '@deepseek-ai/dsh-storage-domain'
import * as StorageJson from '@deepseek-ai/dsh-storage-json'
import SystemPrompt from '@deepseek-ai/dsh-system-prompt'
import ToolRuntime from '@deepseek-ai/dsh-tools'
import TypertRegistry from '@deepseek-ai/dsh-typert-registry'
import WorkspaceRegistry from '@deepseek-ai/dsh-workspace'
import CommandRuntime from '@deepseek-ai/dsh-commands'
import LocalCredentialProvider from '@deepseek-ai/dsh-credentials-local'
import { HostConnectionService } from '../../../client/connection/src/rpc-host.ts'
import { BrowserAuth } from '../../../client/connection/src/browser-auth.ts'
import * as WorkbenchApp from '../src/index.ts'
import { installDispatchInputTool } from '../src/dispatch-input.ts'

const SID = SessionId('ui-control-loader')
const ORIGINAL = '  INV-440\r\n客户说“保留  这段”\n\tSKU=中文-α  \n'
const CONTROL = '我选择团队“订单核对团队”。请先整理任务简报。'
const CONFIRM = '确认由订单核对团队执行，保留原始材料。'
const FACTS = { team_id: 'orders', workflow_id: 'reconcile', workflow_version: 1 }
const RECORDING = new URL('./fixtures/dispatch-input/session.jsonl', import.meta.url)
const DELIVERY_RECORDING = new URL('./fixtures/delivery-verification/session.jsonl', import.meta.url)
const contexts: Context[] = []
const roots: string[] = []

/** Fixed model responses; Host admission, the Agent loop, tool execution, and persistence are real. */
class DispatchAdapter extends LlmAdapter {
  readonly requests: GenerateOptions[] = []

  async * stream(options: GenerateOptions): AsyncIterable<StreamChunk> {
    this.requests.push(options)
    const last = options.messages.at(-1)
    if (last?.content.some(part => part.type === 'text' && part.text === CONFIRM)) {
      const args = JSON.stringify(FACTS)
      yield { type: 'block-start', index: 0, blockType: 'tool-call' }
      yield { type: 'tool-call-delta', index: 0, id: ToolCallId('dispatch-input-loader-call'), name: 'weave_dispatch', argumentsDelta: args }
      yield { type: 'block-end', index: 0, block: { type: 'tool-call', id: ToolCallId('dispatch-input-loader-call'), name: 'weave_dispatch', arguments: args } }
      yield { type: 'finish', reason: { kind: 'tool-calls' } }
      return
    }
    const text = last?.content.some(part => part.type === 'tool-result') ? '任务已派发。' : '已保留当前输入。'
    yield { type: 'block-start', index: 0, blockType: 'text' }
    yield { type: 'text-delta', index: 0, text }
    yield { type: 'block-end', index: 0, block: { type: 'text', text } }
    yield { type: 'finish', reason: { kind: 'stop' } }
  }
}

/** A YAML-mounted owner composition; it creates no Agent until the public Host command does. */
async function loaded(workbench = false) {
  const root = await mkdtemp(join(tmpdir(), 'weave-dispatch-loader-'))
  roots.push(root)
  const adapter = new DispatchAdapter()
  const modules = new Map<string, unknown>([
    ['@deepseek-ai/dsh-agent', AgentRegistry],
    ['@deepseek-ai/dsh-agent-loop', AgentLoop],
    ['@deepseek-ai/dsh-agent-default-model', AgentDefaultModel],
    ['@deepseek-ai/dsh-api-session-controller', SessionController],
    ['@deepseek-ai/dsh-attachment-local', AttachmentLocal],
    ['@deepseek-ai/dsh-llm', LlmRuntime],
    ['@deepseek-ai/dsh-session', SessionStore],
    ['@deepseek-ai/dsh-session-persistence-jsonl', SessionPersistenceJsonl],
    ['@deepseek-ai/dsh-session-projection', SessionProjectionRegistry],
    ['@deepseek-ai/dsh-session-query-sqlite', SessionQuerySqlite],
    ['@deepseek-ai/dsh-storage', Storage],
    ['@deepseek-ai/dsh-storage-domain', StorageDomain],
    ['@deepseek-ai/dsh-storage-json', StorageJson],
    ['@deepseek-ai/dsh-system-prompt', SystemPrompt],
    ['@deepseek-ai/dsh-tools', ToolRuntime],
    ['@deepseek-ai/dsh-typert-registry', TypertRegistry],
    ['@deepseek-ai/dsh-workspace', WorkspaceRegistry],
    ['fixture:adapter', {
      inject: ['llm'],
      apply(ctx: Context) { ctx.llm.registerAdapter(['source-fixture'], adapter) },
    }],
    ['fixture:dispatch', {
      inject: ['sessions', 'tools'],
      apply(ctx: Context) {
        installDispatchInputTool(ctx, { apiUrl: 'http://weave.fixture', headers: () => new Headers({ Authorization: 'Bearer fixture-only' }) })
      },
    }],
  ])
  const configs: Record<string, unknown> = {
    '@deepseek-ai/dsh-agent-default-model': { provider: 'source-fixture', model: 'source-fixture' },
    '@deepseek-ai/dsh-agent-loop': { agents: [] },
    '@deepseek-ai/dsh-attachment-local': { dshHome: join(root, 'home') },
    '@deepseek-ai/dsh-session-persistence-jsonl': { root: join(root, 'sessions'), compression: 'none', packChunks: false },
    '@deepseek-ai/dsh-session-query-sqlite': { path: ':memory:', openAt: 'never' },
    '@deepseek-ai/dsh-storage-domain': { backend: 'json' },
    '@deepseek-ai/dsh-storage-json': { root: join(root, 'storage') },
    '@deepseek-ai/dsh-system-prompt': { persona: 'Preserve inputs and dispatch only after confirmation.' },
  }
  if (workbench) {
    modules.delete('fixture:dispatch')
    modules.set('@deepseek-ai/dsh-api-workspace-controller', WorkspaceController)
    modules.set('@deepseek-ai/dsh-commands', CommandRuntime)
    modules.set('@deepseek-ai/dsh-credentials-local', LocalCredentialProvider)
    modules.set('fixture:connection', { inject: ['credentials'], async apply(ctx: Context) {
      new HostConnectionService(ctx, [], await BrowserAuth.create(ctx.root, ctx.credentials, 1))
    } })
    modules.set('@deepseek-ai/dsh-workbench-app', WorkbenchApp)
    configs['@deepseek-ai/dsh-credentials-local'] = { path: join(root, 'credentials.yaml'), watch: false }
    configs['@deepseek-ai/dsh-workbench-app'] = { apiUrl: 'http://weave.fixture', apiKey: 'fixture-only', pollIntervalMs: 500, userDataRoot: root }
  }
  const configPath = join(root, 'cordis.yml')
  await writeFile(configPath, [...modules.keys()].map(name =>
    `- name: ${JSON.stringify(name)}\n  config: ${JSON.stringify(configs[name] ?? {})}\n`).join(''))
  const ctx = new Context()
  contexts.push(ctx)
  ctx.baseUrl = pathToFileURL(root).href + '/'
  await ctx.plugin(Loader)
  ctx.loader.builtins.include = Include
  ctx.loader.internal = {
    version: 'v2',
    async import(specifier: string) {
      if (!modules.has(specifier)) throw new Error(`unexpected Loader import: ${specifier}`)
      return modules.get(specifier)
    },
  } as unknown as NonNullable<typeof ctx.loader.internal>
  await ctx.loader.create({ name: 'cordis:include', config: { path: pathToFileURL(configPath).href } })
  await ctx.loader.await()
  expect([...ctx.loader.entries()].filter(entry => entry.fiber === undefined && !entry.disabled)).toEqual([])
  return { ctx, root, adapter }
}

function assertSources(events: readonly SessionEvent[]): void {
  const prompts = events.filter(event => event.type === 'user/message')
  expect(prompts.map(event => event.data.source.kind)).toEqual(['user', 'plugin', 'user'])
  expect(prompts[0]?.data.content).toEqual([{ type: 'text', text: ORIGINAL }])
  expect(prompts[1]?.data).toMatchObject({
    content: [{ type: 'text', text: CONTROL }],
    source: {
      kind: 'plugin', plugin: 'ui-control', form: 'relay',
      rpcId: 'source-1', clientTimeZone: 'Asia/Shanghai',
    },
  })
  const accepted = events.findLast(event => event.type === 'weave/dispatch-input' && event.data.state === 'accepted')
  if (accepted?.type !== 'weave/dispatch-input') throw new Error('expected a durable accepted dispatch')
  expect(accepted.data.sourceMessages.map(message => message.content)).toEqual([
    [{ type: 'text', text: ORIGINAL }], [{ type: 'text', text: CONFIRM }],
  ])
  expect(accepted.data.task).not.toContain(CONTROL)
  expect(accepted.data.task).toContain(ORIGINAL)
  expect(events.filter(event => event.type === 'turn/end')).toHaveLength(3)
}

afterEach(async () => {
  for (const ctx of contexts.splice(0).reverse()) await ctx.fiber.dispose()
  vi.unstubAllGlobals()
  for (const root of roots.splice(0)) await rm(root, { recursive: true, force: true })
})

describe('dispatch input through real Loader, Host prompts, and JSONL replay', () => {
  it('keeps two authenticated users isolated across real Host sessions, streams, replay and background polling', async () => {
    const delegated: { path: string; actor: string | null }[] = []
    vi.stubGlobal('fetch', vi.fn<typeof fetch>(async (input, init) => {
      const path = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url).pathname
      if (path === '/v1/auth/login') {
        const { username } = JSON.parse(String(init?.body)) as { username: string }
        return Response.json({ token: `${username}.${Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url')}.signed`,
          user: { id: username, tenant_id: 'shared', username, role: 'user' } })
      }
      delegated.push({ path, actor: new Headers(init?.headers).get('X-Weave-User-Authorization') })
      if (path === '/v1/auth/me') return Response.json({ ok: true })
      return Response.json({ run_id: path.split('/').at(-2), status: 'running', members: [], completeness: { members: 'complete' } })
    }))
    const { ctx, root } = await loaded(true)
    const connection = ctx.get('connection') as HostConnectionService
    const route = connection.createSharedFetchHandler('/api')
    const request = (path: string, cookie = '', body?: unknown): Request => new Request('http://host/api/' + path, {
      method: body === undefined ? 'GET' : 'POST', headers: { cookie, 'Content-Type': 'application/json' },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    })
    const login = async (username: string): Promise<string> => {
      const response = await route.fetch(request('weave.account', '', { action: 'login', username, password: 'fixture-password' }))
      expect(response.status).toBe(200)
      return response.headers.get('set-cookie')!.split(';')[0]!
    }
    connection.fetch.register({ path: '/api/fixture/create', methods: ['POST'], fetch: async input => {
      const { sessionId } = await input.json() as { sessionId: string }
      return Response.json(await ctx.sessionController.create({ sessionId: SessionId(sessionId) }))
    } })
    connection.fetch.register({ path: '/api/fixture/list', methods: ['GET'], fetch: async () => Response.json(await ctx.sessionController.list({}, new AbortController().signal)) })
    connection.fetch.register({ path: '/api/fixture/page', methods: ['POST'], fetch: async input => {
      const { sessionId } = await input.json() as { sessionId: string }
      return Response.json(await ctx.sessionController.inspect(SessionId(sessionId)))
    } })
    connection.fetch.register({ path: '/api/workspace/archiveSession', methods: ['POST'], fetch: async input => Response.json(await ctx.workspaceController.archiveSession(await input.json() as { sessionId: SessionId })) })
    const alice = await login('alice'), bob = await login('bob')
    for (const [name, cookie] of [['alice', alice], ['bob', bob]] as const) {
      expect((await route.fetch(request('fixture/create', cookie, { sessionId: name }))).status).toBe(200)
      const session = ctx.sessions.get(SessionId(name))!
      expect(session.events.find(event => event.type === 'session/actor')?.data).toEqual({ userId: name, workspaceId: 'shared' })
      session.append('weave/work-task', WorkbenchApp.workTaskProjectionDefinition.wire.viewSchema.parse({
        runId: name + '-run', clientRequestId: name + '-request', teamId: 'orders', teamName: '订单团队', workflowName: 'reconcile',
        status: 'running', completedStages: 0, totalStages: 1, latestStage: '', runtimes: [], humanTaskCount: 0,
        deliverableCount: 0, blocker: 'none', updatedAt: Date.now(),
      })!)
      await ctx.sessions.flush(session)
    }
    const sideEffects: string[] = []
    const tool = (name: string) => ({ name, description: name, parameters: { type: 'object' as const, properties: {} },
      output: { schema: { type: 'string' as const }, render: (_args: unknown, value: unknown) => [{ type: 'text' as const, text: String(value) }] },
      execute: async () => { sideEffects.push(name); return 'executed' },
    })
    for (const name of ['host_shell', 'host_read_file', 'mcp__other__host_environment', 'mcp__weave__account_probe']) ctx.tools.register(tool(name))
    const aliceAgent = ctx.agents.get(SessionId('alice'))!
    aliceAgent.ctx.tools.register(tool('scope_local_host_shell'))
    expect(ctx.tools.schemas(aliceAgent).map(item => item.name)).not.toContain('host_shell')
    expect(ctx.tools.schemas(aliceAgent).map(item => item.name)).toContain('mcp__weave__account_probe')
    for (const name of ['host_shell', 'host_read_file', 'mcp__other__host_environment', 'scope_local_host_shell', 'run_code']) {
      await ctx.tools.execute({ name, arguments: {}, agent: aliceAgent, callId: ToolCallId(name), signal: new AbortController().signal })
    }
    await ctx.tools.execute({ name: 'mcp__weave__account_probe', arguments: {}, callId: ToolCallId('unscoped'), signal: new AbortController().signal })
    expect(sideEffects).toEqual([])
    await ctx.tools.execute({ name: 'mcp__weave__account_probe', arguments: {}, agent: aliceAgent, callId: ToolCallId('authorized-product-tool'), signal: new AbortController().signal })
    expect(sideEffects).toEqual(['mcp__weave__account_probe'])
    expect(ctx.sessions.get(SessionId('alice'))!.header.cwd).not.toBe(ctx.sessions.get(SessionId('bob'))!.header.cwd)
    for (const name of ['alice', 'bob']) expect(ctx.sessions.get(SessionId(name))!.header.cwd).toContain(join(root, 'users'))
    await vi.waitFor(() => {
      for (const name of ['alice', 'bob']) expect(delegated.some(call => call.path.includes(name + '-run') && call.actor?.startsWith('Bearer ' + name + '.'))).toBe(true)
    })
    for (const [name, cookie, other] of [['alice', alice, 'bob'], ['bob', bob, 'alice']] as const) {
      const listed = await (await route.fetch(request('fixture/list', cookie))).json() as { items: { sessionId: string }[] }
      expect(listed.items.map(item => item.sessionId)).toEqual([name])
      expect((await route.fetch(request('fixture/page', cookie, { sessionId: other }))).status).toBe(404)
      expect((await route.fetch(request('session/page', cookie, { payload: { args: { request: { address: { kind: 'session', sessionId: other } } } } }))).status).toBe(404)
      expect((await route.fetch(request('commands/execute', cookie, { payload: { args: { agentId: other, line: '/clear' } } }))).status).toBe(404)
      expect((await route.fetch(request('fixture/create', cookie, { sessionId: other }))).status).toBe(404)
      const control = connection.stream(request('session/control', cookie, {}), async () => ctx.sessionController.control(new AbortController().signal))[Symbol.asyncIterator]()
      const initial = (await control.next()).value as { value: { queues: Record<string, unknown>; jobs: Record<string, unknown>; projections: Record<string, unknown> } }
      expect(Object.keys(initial.value.queues)).toEqual([name]); expect(Object.keys(initial.value.jobs)).toEqual([name]); expect(Object.keys(initial.value.projections)).toEqual([name])
      await control.return?.()
      const frames = [name, other].flatMap(sessionId => [
        { type: 'emit', event: 'api-session/added', args: [{ sessionId, title: sessionId + ' private' }] },
        { type: 'emit', event: 'api-session/error', args: [sessionId, sessionId + ' private error'] },
        { type: 'waterfall', agentId: sessionId, prompt: sessionId + ' private prompt' },
      ])
      const events: unknown[] = []
      for await (const frame of connection.stream(request('$events', cookie, {}), async () => (async function* () { yield* frames })())) events.push(frame)
      expect(events).toEqual(frames.slice(0, 3))
    }
    expect((await route.fetch(request('workspace/archiveSession', bob, { sessionId: 'alice' }))).status).toBe(404)
    const archived = await route.fetch(request('workspace/archiveSession', alice, { sessionId: 'alice' }))
    expect(archived.status).toBe(200); expect(await archived.json()).toEqual({ archivedSessionIds: ['alice'] })
    let release!: () => void
    const barrier = new Promise<void>(resolve => { release = resolve })
    const late = connection.stream(request('$events', alice, {}), async () => (async function* () { await barrier; yield { type: 'emit', event: 'api-session/error', args: ['alice', 'late private result'] } })())[Symbol.asyncIterator]()
    const pending = late.next()
    await route.fetch(request('weave.account', alice, { action: 'logout' }))
    release()
    await expect(pending).rejects.toThrow('login_required')
    expect((await route.fetch(request('fixture/list', alice))).status).toBe(401)
    await ctx.tools.execute({ name: 'mcp__weave__account_probe', arguments: {}, agent: aliceAgent, callId: ToolCallId('expired-product-tool'), signal: new AbortController().signal })
    expect(sideEffects).toEqual(['mcp__weave__account_probe'])
    expect((await route.fetch(request('fixture/list', bob))).status).toBe(200)
    expect(delegated.filter(call => call.path.includes('alice-run')).every(call => call.actor?.startsWith('Bearer alice.'))).toBe(true)
    expect(delegated.filter(call => call.path.includes('bob-run')).every(call => call.actor?.startsWith('Bearer bob.'))).toBe(true)
    const restored = await ctx.sessionPersistence.inspect(SessionId('alice'))
    expect(restored.events.find(event => event.type === 'session/actor')?.data).toEqual({ userId: 'alice', workspaceId: 'shared' })
  })

  it('persists independent delivery checks and version-bound browser assessments through the real Host composition and JSONL reader', async () => {
    const delivery = { revision_id: 'revision-1', contract_digest: 'contract-1', verification_id: 'verification-1',
      verification_status: 'unknown', checks: [{ check_id: 'invoice-check', status: 'unknown', reason: 'verifier_unavailable' }],
      check_counts: { unknown: 1 }, available: true, evidence_completeness: 'complete' }
    const fetcher = vi.fn<typeof fetch>(async (input) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
      if (url.pathname === '/v1/auth/login') return Response.json({ token: `fixture.${Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url')}.signed`, user: { id: 'user-1', tenant_id: 'default', username: 'tester', display_name: 'Tester', role: 'user' } })
      if (url.pathname === '/v1/auth/me') return Response.json({ id: 'user-1' })
      if (url.pathname === '/v1/deliverables') return Response.json({ deliverables: [{ id: 'file-1', run_id: 'delivery-run',
        title: 'report.md', content: 'PASS', metadata: { artifact_kind: 'final' } }] })
      return Response.json({ run_id: 'delivery-run', status: 'succeeded', members: [], completeness: { members: 'complete' }, delivery })
    })
    vi.stubGlobal('fetch', fetcher)
    const { ctx, adapter } = await loaded(true)
    const connection = ctx.get('connection') as HostConnectionService
    const route = connection.createSharedFetchHandler('/api')
    const login = await route.fetch(new Request('http://host/api/weave.account', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'login', username: 'tester', password: 'fixture-only' }) }))
    expect(login.status).toBe(200)
    const cookie = login.headers.get('set-cookie')!.split(';')[0]!
    connection.fetch.register({ path: '/api/fixture/create', methods: ['POST'], fetch: async () => Response.json(await ctx.sessionController.create({ sessionId: SID })) })
    expect((await route.fetch(new Request('http://host/api/fixture/create', { method: 'POST', headers: { cookie, 'Content-Type': 'application/json' }, body: '{}' }))).status).toBe(200)
    const session = ctx.sessions.get(SID)!
    session.append('weave/work-task', WorkbenchApp.workTaskProjectionDefinition.wire.viewSchema.parse({
      runId: 'delivery-run', clientRequestId: 'delivery-request', teamId: 'orders', teamName: '订单团队', workflowName: 'reconcile',
      status: 'running', completedStages: 0, totalStages: 1, latestStage: '', runtimes: [], humanTaskCount: 0,
      deliverableCount: 0, blocker: 'none', updatedAt: Date.now(),
    })!)
    await vi.waitFor(() => {
      expect(ctx.sessionProjections.stateOf(session, 'workTask')?.task).toMatchObject({ status: 'completed',
        delivery: { verificationStatus: 'unknown', revisionId: 'revision-1' }, outcome: 'unrated', deliverableCount: 1 })
    })
    const assess = (deliveryRevisionId: string) => route.fetch(new Request('http://host/api/weave.task-action', {
      method: 'POST', headers: { cookie, 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'assess', sessionId: SID,
        runId: 'delivery-run', deliveryRevisionId, outcome: 'adopted', note: '用户审阅了当前成果' }),
    }))
    expect((await assess('old-revision')).status).toBe(409)
    expect((await assess('revision-1')).status).toBe(204)
    expect(ctx.sessionProjections.stateOf(session, 'workTask')?.task).toMatchObject({ outcome: 'adopted', outcomeRevisionId: 'revision-1',
      delivery: { verificationStatus: 'unknown' } })
    expect(adapter.requests).toEqual([])
    await ctx.sessions.flush(session)
    const location = ctx.sessionPersistence.locate(session.header)!
    const recorded = await readFile(location.path, 'utf8')
    const header = session.header
    if (process.env.DSH_SNAPSHOT === 'record') {
      await mkdir(new URL('.', DELIVERY_RECORDING), { recursive: true })
      await writeFile(DELIVERY_RECORDING, recorded)
    }
    await ctx.fiber.dispose()
    contexts.splice(contexts.indexOf(ctx), 1)
    expect((await assess('revision-1')).status).toBe(404)
    const replay = await loaded(true)
    const target = replay.ctx.sessionPersistence.locate(header)!
    await mkdir(dirname(target.path), { recursive: true })
    await writeFile(target.path, recorded)
    const restored = await replay.ctx.sessionPersistence.inspect(SID)
    let state = WorkbenchApp.workTaskProjectionDefinition.init()
    for (const event of restored.events) state = WorkbenchApp.applyWorkTaskProjection(state, event)
    expect(WorkbenchApp.workTaskProjectionDefinition.wire.view(state)).toMatchObject({ outcome: 'adopted', outcomeRevisionId: 'revision-1',
      delivery: { verificationStatus: 'unknown' } })
    expect(replay.adapter.requests).toEqual([])
  })

  it('replays the committed delivery assessment recording without model execution', async () => {
    const recording = await readFile(DELIVERY_RECORDING, 'utf8')
    const header = JSON.parse(recording.split('\n')[0] ?? '') as SessionHeader
    const { ctx, adapter } = await loaded(true)
    const target = ctx.sessionPersistence.locate(header)!
    await mkdir(dirname(target.path), { recursive: true })
    await writeFile(target.path, recording)
    const restored = await ctx.sessionPersistence.inspect(header.id)
    let state = WorkbenchApp.workTaskProjectionDefinition.init()
    for (const event of restored.events) state = WorkbenchApp.applyWorkTaskProjection(state, event)
    expect(WorkbenchApp.workTaskProjectionDefinition.wire.view(state)).toMatchObject({ status: 'completed', outcome: 'adopted',
      outcomeRevisionId: 'revision-1', latestAssessment: { runId: 'delivery-run', revisionId: 'revision-1', outcome: 'adopted' },
      delivery: { verificationStatus: 'unknown' } })
    expect(adapter.requests).toEqual([])
  })

  it('keeps generated UI controls out of the task body sent by the model-visible tool', async () => {
    const requests: { path: string; body: Record<string, unknown> }[] = []
    vi.stubGlobal('fetch', vi.fn<typeof fetch>(async (input, init) => {
      if (typeof init?.body !== 'string') throw new Error('expected the serialized Weave request')
      const path = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url).pathname
      const body = JSON.parse(init.body) as Record<string, unknown>
      requests.push({ path, body })
      return Response.json(path === '/v1/workbench/dispatch-inputs'
        ? { input_revision_id: '10000000-0000-4000-8000-000000000001', client_request_id: '20000000-0000-4000-8000-000000000001',
          task_sha256: createHash('sha256').update(String(body.task)).digest('hex') }
        : { run_id: 'source-fixture-run', client_request_id: body.client_request_id,
          input_revision_id: body.input_revision_id, status: 'queued' })
    }))
    const { ctx, adapter } = await loaded()
    await ctx.sessionController.create({ sessionId: SID })
    const session = ctx.sessions.get(SID)
    if (session === undefined) throw new Error('Host did not publish the created session')
    for (const [index, text] of [ORIGINAL, CONTROL, CONFIRM].entries()) {
      await ctx.sessionController.prompt({
        requestId: `source-${String(index)}` as SessionRequestId,
        sessionId: SID, mode: 'queue', content: [{ type: 'text', text }],
        clientTimeZone: 'Asia/Shanghai',
        ...(index === 1 ? { origin: 'ui-control' as const } : {}),
      }, new AbortController().signal)
      await vi.waitFor(() => {
        expect(session.events.filter(event => event.type === 'turn/end')).toHaveLength(index + 1)
        expect(ctx.agents.get(SID)?.status).toBe('idle')
      })
    }
    const schema = adapter.requests[0]?.tools?.find(tool => tool.name === 'weave_dispatch')
    expect(schema).toBeDefined()
    expect(adapter.requests[1]?.messages.some(message => message.content.some(part =>
      part.type === 'text' && part.text.includes(CONTROL)))).toBe(true)
    expect(schema?.parameters).toHaveProperty('properties.team_id')
    expect(schema?.parameters).not.toHaveProperty('properties.task')
    expect(schema?.parameters).not.toHaveProperty('properties.input_revision_id')
    expect(requests.map(request => request.path)).toEqual(['/v1/workbench/dispatch-inputs', '/v1/teams/orders/dispatch'])
    expect(requests[0]?.body.task).toContain(ORIGINAL)
    expect(requests[0]?.body.task).not.toContain(CONTROL)
    expect(requests[1]?.body).not.toHaveProperty('task')
    assertSources(session.events)
    await ctx.sessions.flush(session)
    const location = ctx.sessionPersistence.locate(session.header)
    if (location === undefined) throw new Error('JSONL persistence did not provide its artifact')
    const recorded = await readFile(location.path, 'utf8')
    if (process.env.DSH_SNAPSHOT === 'record') {
      await mkdir(new URL('.', RECORDING), { recursive: true })
      await writeFile(RECORDING, recorded)
    }
    await ctx.fiber.dispose()
    contexts.splice(contexts.indexOf(ctx), 1)
    const replay = await loaded()
    const target = replay.ctx.sessionPersistence.locate(session.header)
    if (target === undefined) throw new Error('replay JSONL locator is unavailable')
    await mkdir(dirname(target.path), { recursive: true })
    await writeFile(target.path, recorded)
    const restored = await replay.ctx.sessionPersistence.inspect(SID)
    assertSources(restored.events)
    expect(replay.adapter.requests).toEqual([])
  })

  it('replays the committed keyless session recording through the production JSONL reader', async () => {
    const recording = await readFile(RECORDING, 'utf8')
    const header = JSON.parse(recording.split('\n')[0] ?? '') as SessionHeader
    const { ctx, adapter } = await loaded()
    const target = ctx.sessionPersistence.locate(header)
    if (target === undefined) throw new Error('replay JSONL locator is unavailable')
    await mkdir(dirname(target.path), { recursive: true })
    await writeFile(target.path, recording)
    const restored = await ctx.sessionPersistence.inspect(header.id)
    assertSources(restored.events)
    expect(adapter.requests).toEqual([])
  })
})
