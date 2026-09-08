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
import { installDispatchInputTool } from '../src/dispatch-input.ts'

const SID = SessionId('ui-control-loader')
const ORIGINAL = '  INV-440\r\n客户说“保留  这段”\n\tSKU=中文-α  \n'
const CONTROL = '我选择团队“订单核对团队”。请先整理任务简报。'
const CONFIRM = '确认由订单核对团队执行，保留原始材料。'
const FACTS = { team_id: 'orders', workflow_id: 'reconcile', workflow_version: 1 }
const RECORDING = new URL('./fixtures/dispatch-input/session.jsonl', import.meta.url)
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
async function loaded() {
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
        installDispatchInputTool(ctx, { apiUrl: 'http://weave.fixture', apiKey: 'fixture-only' })
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
    const { ctx, root, adapter } = await loaded()
    await ctx.sessionController.create({ sessionId: SID, cwd: root })
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
