// @vitest-environment jsdom
import { cleanup, fireEvent, render } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { AgentExecutionPanel } from '../src/client/AgentExecutionPanel.tsx'
import { zh } from '../src/client/locales.ts'
const t = makeTranslate(zh, commonZh)
afterEach(() => { cleanup(); vi.unstubAllGlobals() })
it('clears the previous engine model before publishing the member configuration', async () => {
  const fetcher = vi.fn<typeof fetch>(async (_input, init) => init?.method === 'PUT' ? Response.json({ saved: true }) : Response.json({ agents: [{ name: 'writer', displayName: 'Writer', version: 4, engine: 'codex', runtimeId: 'node', model: 'gpt-old', fallbackModels: ['gpt-backup'], fallbackRetries: 0 }] }))
  vi.stubGlobal('fetch', fetcher)
  const view = render(<AgentExecutionPanel t={t} runtimes={[{ id: 'node', name: 'Local', engines: ['codex', 'claude'], enabled: true }]} />)
  fireEvent.click(await view.findByRole('button', { name: t('agentExecution.configure') }))
  fireEvent.change(view.getByLabelText(t('agentExecution.engine')), { target: { value: 'claude' } })
  expect((view.getByPlaceholderText(t('agentExecution.nodeDefault')) as HTMLInputElement).value).toBe('')
  fireEvent.click(view.getByRole('button', { name: t('agentExecution.save') }))
  expect((await view.findByRole('status')).textContent).toContain(t('agentExecution.saved'))
  const body = fetcher.mock.calls.find(([, init]) => init?.method === 'PUT')?.[1]?.body
  expect(JSON.parse(typeof body === 'string' ? body : '{}')).toEqual({ name: 'writer', expectedVersion: 4, engine: 'claude', runtimeId: 'node', model: '', fallbackModels: [], fallbackRetries: 0 })
})
