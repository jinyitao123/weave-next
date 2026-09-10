/** Test-only Host interaction and external-HTTP fixture mounted by the shipped headless profile. */
import { createHash } from 'node:crypto'
import { writeFile } from 'node:fs/promises'
import type { Context } from '@deepseek-ai/cordis'
import type { SessionRequestId } from '@deepseek-ai/dsh-api-session-controller/types'
import { installDispatchInputTool } from '../../../src/dispatch-input.ts'

export const inject = ['sessionController', 'sessions', 'tools']

export function apply(ctx: Context): void {
  const admitted = new Set<string>()
  let task = ''
  let sourceMessageCount = 0
  const inputRevisionId = '10000000-0000-4000-8000-000000000001'
  const clientRequestId = '20000000-0000-4000-8000-000000000001'
  const previousFetch = globalThis.fetch
  ctx.effect(() => {
    globalThis.fetch = async (input, init) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url)
      if (url.origin !== 'http://weave.fixture') return previousFetch(input, init)
      if (typeof init?.body !== 'string') throw new Error('expected a serialized dispatch request')
      const body = JSON.parse(init.body) as Record<string, unknown>
      if (url.pathname === '/v1/workbench/dispatch-inputs') {
        if (typeof body.task !== 'string' || !Array.isArray(body.source_messages)) throw new Error('expected source inputs')
        task = body.task
        sourceMessageCount = body.source_messages.length
        return Response.json({ input_revision_id: inputRevisionId, client_request_id: clientRequestId,
          task_sha256: createHash('sha256').update(task).digest('hex') })
      }
      if (url.pathname !== '/v1/teams/orders/dispatch') throw new Error(`unexpected fixture route: ${url.pathname}`)
      await writeFile('dispatch-source.json', JSON.stringify({ task, sourceMessageCount, dispatchFields: Object.keys(body).sort() }, null, 2) + '\n')
      return Response.json({ run_id: 'source-fixture-run', client_request_id: clientRequestId,
        input_revision_id: inputRevisionId, status: 'queued' })
    }
    return () => { globalThis.fetch = previousFetch }
  }, 'dispatch snapshot external HTTP')
  installDispatchInputTool(ctx, { apiUrl: 'http://weave.fixture', apiKey: 'fixture-only' })
  ctx.on('agent/request', async ({ agent, signal }, next) => {
    if (!admitted.has(agent.id)) {
      admitted.add(agent.id)
      await ctx.sessionController.prompt({
        requestId: 'ui-control-snapshot' as SessionRequestId,
        sessionId: agent.id, mode: 'queue', origin: 'ui-control',
        clientTimeZone: 'Asia/Shanghai',
        content: [{ type: 'text', text: '我选择团队“订单核对团队”。这是按钮生成的控制指令，不是业务材料。' }],
      }, signal)
    }
    return next()
  })
}
