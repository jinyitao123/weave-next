import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ModelProvider } from '../lib/providers'
import { ModelProvidersPage } from './ModelProvidersPage'

const saved: ModelProvider = {
  id: 'kimi', name: 'Kimi', protocol: 'openai-completions', base_url: 'https://api.kimi.test', chat_path: '/v1/chat/completions',
  chat_url: 'https://api.kimi.test/v1/chat/completions', models: ['kimi-k3', 'kimi-k3-mini'], json_object_mode: false,
  thinking_default_mode: '', thinking_disable_with_tools: false, attempt_timeout_seconds: 0, deployment: false,
}
const stale: ModelProvider = { ...saved, id: 'old', name: '旧供应方', chat_path: '', chat_url: '', models: ['old-model'] }

type Call = { url: string; method: string; body?: Record<string, unknown> }
function serve() {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? 'GET'
    const body = init.body ? JSON.parse(String(init.body)) : undefined
    calls.push({ url, method, body })
    if (url.endsWith('/endpoint')) {
      // The server's rule, reduced to the case this page shows: a base that
      // already ends with the default path's first segment.
      if (body.base_url.endsWith('/v1') && !body.chat_path) {
        return new Response(JSON.stringify({ code: 'model_provider_endpoint_invalid', reason: 'duplicate_path_segment', error: '供应方地址和接口路径拼接后会出现重复的路径段，请求会发到错误的地址',
          suggestion: { base_url: body.base_url.slice(0, -3), chat_path: '/v1/chat/completions', chat_url: `${body.base_url}/chat/completions` } }), { status: 422 })
      }
      const path = body.chat_path || '/v1/chat/completions'
      return new Response(JSON.stringify({ base_url: body.base_url, chat_path: path, chat_url: body.base_url + path }), { status: 200 })
    }
    if (url.endsWith('/probe')) {
      return new Response(JSON.stringify({ ok: false, chat_url: saved.chat_url, model: body.model, category: 'endpoint_not_found', status: 404, latency_ms: 30, message: '服务存在，但这个接口路径不存在：请检查供应方地址和接口路径' }), { status: 200 })
    }
    if (method === 'PUT') return new Response(JSON.stringify({ id: 'glm', chat_url: '' }), { status: 200 })
    return new Response(JSON.stringify([saved, stale]), { status: 200 })
  }))
  return calls
}

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('model providers page', () => {
  it('shows where each provider sends requests and asks to resave one without a path', async () => {
    serve()
    render(<ModelProvidersPage />)
    expect(await screen.findByText('https://api.kimi.test/v1/chat/completions')).toBeTruthy()
    const old = screen.getByRole('region', { name: '旧供应方' })
    expect(within(old).getByText('还没有记录接口路径，请编辑后重新保存。')).toBeTruthy()
    expect((within(old).getByRole('button', { name: '检查连接' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('probes the chosen model and says what kind of answer came back', async () => {
    const calls = serve()
    render(<ModelProvidersPage />)
    const kimi = await screen.findByRole('region', { name: 'Kimi' })
    fireEvent.click(within(kimi).getByRole('button', { name: 'Kimi检查用的模型' }))
    fireEvent.mouseDown(within(kimi).getByRole('option', { name: 'kimi-k3-mini' }))
    fireEvent.click(within(kimi).getByRole('button', { name: '检查连接' }))
    expect(await within(kimi).findByText(/接口路径不存在/)).toBeTruthy()
    expect(within(kimi).getByText(/HTTP 404/)).toBeTruthy()
    expect(calls.find((call) => call.url.endsWith('/kimi/probe'))?.body).toEqual({ model: 'kimi-k3-mini' })
  })

  it('shows the resolved address before saving and offers the split to confirm', async () => {
    const calls = serve()
    render(<ModelProvidersPage />)
    fireEvent.click(await screen.findByRole('button', { name: '添加供应方' }))
    const dialog = screen.getByRole('dialog')
    fireEvent.change(within(dialog).getByRole('textbox', { name: '标识' }), { target: { value: 'glm' } })
    fireEvent.change(within(dialog).getByRole('textbox', { name: '名称' }), { target: { value: 'GLM' } })
    fireEvent.change(within(dialog).getByRole('textbox', { name: '服务地址' }), { target: { value: 'https://open.glm.test/v1' } })
    expect(await within(dialog).findByText(/重复的路径段/)).toBeTruthy()
    expect((within(dialog).getByRole('button', { name: '保存' }) as HTMLButtonElement).disabled).toBe(true)

    fireEvent.click(within(dialog).getByRole('button', { name: '按建议填写' }))
    expect((within(dialog).getByRole('textbox', { name: '服务地址' }) as HTMLInputElement).value).toBe('https://open.glm.test')
    expect((within(dialog).getByRole('textbox', { name: '接口路径' }) as HTMLInputElement).value).toBe('/v1/chat/completions')
    await waitFor(() => expect(within(dialog).queryByText(/重复的路径段/)).toBeNull())
    expect(within(dialog).getByText('https://open.glm.test/v1/chat/completions')).toBeTruthy()

    fireEvent.change(within(dialog).getByRole('textbox', { name: '模型（每行一个）' }), { target: { value: 'glm-5\nglm-5-air' } })
    fireEvent.change(within(dialog).getByLabelText('密钥'), { target: { value: 'test-key' } })
    fireEvent.click(within(dialog).getByRole('button', { name: '保存' }))
    await waitFor(() => expect(calls.some((call) => call.method === 'PUT')).toBe(true))
    const put = calls.find((call) => call.method === 'PUT')!
    expect(put.url).toBe('/v1/development/model-providers/glm')
    expect(put.body).toMatchObject({ name: 'GLM', protocol: 'openai-completions', base_url: 'https://open.glm.test', chat_path: '/v1/chat/completions', models: ['glm-5', 'glm-5-air'], api_key: 'test-key' })
  })
})
