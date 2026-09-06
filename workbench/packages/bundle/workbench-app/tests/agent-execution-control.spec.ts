import { describe, expect, it, vi } from 'vitest'
import { handleAgentExecutionRequest } from '../src/agent-execution-control.ts'

describe('member execution settings', () => {
  it('returns only editable fields and never forwards provider secrets', async () => {
    const fetcher = vi.fn(async () => Response.json([{ name: 'writer', display_name: 'Writer', version: 4, engine: 'codex', runtime_id: 'node', model: 'old', api_key: 'secret', system_prompt: 'private', fallback_models: [], fallback_retries: 0 }]))
    const response = await handleAgentExecutionRequest('https://weave.example', 'token', new Request('https://host/api', { method: 'GET' }), fetcher)
    expect(await response.json()).toEqual({ agents: [{ name: 'writer', displayName: 'Writer', version: 4, engine: 'codex', runtimeId: 'node', model: 'old', fallbackModels: [], fallbackRetries: 0 }] })
  })
  it('publishes through the atomic API and preserves a conflict as unsaved', async () => {
    const fetcher = vi.fn(async () => Response.json({ error: 'private detail' }, { status: 409 }))
    const input = { name: 'writer', expectedVersion: 4, engine: 'claude', runtimeId: 'node', model: '', fallbackModels: [], fallbackRetries: 0 }
    const response = await handleAgentExecutionRequest('https://weave.example', 'token', new Request('https://host/api', { method: 'PUT', body: JSON.stringify(input) }), fetcher)
    expect(response.status).toBe(409)
    expect(await response.json()).toEqual({ code: 'execution_conflict' })
    expect(fetcher).toHaveBeenCalledWith('https://weave.example/v1/agents/writer/execution', expect.objectContaining({ method: 'PUT', body: JSON.stringify({ expected_version: 4, engine: 'claude', runtime_id: 'node', model: '', fallback_models: [], fallback_retries: 0 }) }))
  })
})
