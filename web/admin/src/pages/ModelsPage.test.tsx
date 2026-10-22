import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { AdminSession } from '../lib/api'
import type { Provider, SystemProvider } from '../lib/providers'
import { ModelsSection as ModelsPage } from './ModelsPage'

const admin: AdminSession = { name: '管理员', role: 'admin', source: 'forge' }
const deepseek: Provider = { id: 'deepseek', name: 'deepseek', base_url: 'https://api.deepseek.com/v1', protocol: 'openai', models: ['deepseek-chat', 'deepseek-reasoner'], api_key: '••••••••', thinking_default_mode: 'disabled' }
const nimble: Provider = { id: 'nimble', name: 'nimble', base_url: 'http://127.0.0.1:11434', protocol: 'systemone', models: ['nimble'] }
const system: SystemProvider[] = [{ id: 'deepseek', name: 'DeepSeek', base_url: 'https://api.deepseek.com', models: ['deepseek-v4'], mirrored: true, mirrored_as: 'system/deepseek' }]

type Call = { url: string; method: string; body?: Record<string, unknown> }
function serve(providers: Provider[] = [deepseek, nimble], catalog: string[] = ['deepseek-chat', 'deepseek-reasoner', 'deepseek-v4']) {
  const calls: Call[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? 'GET'
    calls.push({ url, method, body: init.body ? JSON.parse(String(init.body)) : undefined })
    if (method === 'DELETE') return new Response(null, { status: 204 })
    if (method === 'POST' || method === 'PUT') return new Response(JSON.stringify({ status: 'created' }), { status: 201 })
    if (url === '/v1/providers/system') return new Response(JSON.stringify(system), { status: 200 })
    if (url === '/v1/development/model-providers') return new Response(JSON.stringify({ version: '1', providers }), { status: 200 })
    if (url === '/v1/development/model-catalog') return new Response(JSON.stringify({ version: '1', models: catalog }), { status: 200 })
    return new Response(JSON.stringify({ error: 'not_found' }), { status: 404 })
  }))
  return calls
}

const saved = (calls: Call[]) => calls.filter((call) => call.method === 'PUT' && call.url.startsWith('/v1/development/model-providers/'))
const reads = (calls: Call[], url: string) => calls.filter((call) => call.method === 'GET' && call.url === url).length
const open = async (name: RegExp) => fireEvent.click(await screen.findByRole('button', { name }))
const type = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } })
const choose = (select: string, option: string) => {
  fireEvent.click(screen.getByRole('button', { name: select }))
  fireEvent.mouseDown(screen.getByRole('option', { name: option }))
}

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('models page', () => {
  it('lists workspace providers with their protocol, address, models and key state, and the built-in ones apart', async () => {
    serve()
    render(<ModelsPage session={admin} />)
    const own = await screen.findByRole('region', { name: '本工作区 provider' })
    const first = await within(own).findByRole('button', { name: /^deepseek/ })
    expect(first.textContent).toContain('OpenAI 兼容')
    expect(first.textContent).toContain('https://api.deepseek.com/v1')
    expect(first.textContent).toContain('deepseek-reasoner')
    expect(first.textContent).toContain('密钥已设置')
    const second = within(own).getByRole('button', { name: /^nimble/ })
    expect(second.textContent).toContain('System One 决策模型')
    expect(second.textContent).toContain('密钥未设置')
    expect(second.textContent).toContain('成员暂不可选')
    const builtIn = await screen.findByRole('region', { name: '内置 provider' })
    expect(builtIn.textContent).toContain('DeepSeek')
    expect(builtIn.textContent).toContain('内置')
    expect(builtIn.textContent).toContain('已镜像到本工作区')
    expect(within(screen.getByRole('region', { name: '成员可选模型' })).getByText('deepseek-v4')).toBeTruthy()
  })

  it('adds a provider without a key and reloads the list and the model catalog', async () => {
    const calls = serve()
    render(<ModelsPage session={admin} />)
    fireEvent.click((await screen.findAllByRole('button', { name: /添加 provider/ }))[0])
    type('名称', 'jev')
    type('地址', 'http://127.0.0.1:9000')
    type('模型 1', 'typesafe-jev')
    fireEvent.click(screen.getByRole('button', { name: '添加模型' }))
    type('模型 2', 'typesafe-jev-fast')
    expect((screen.getByLabelText('密钥') as HTMLInputElement).value).toBe('')
    const before = reads(calls, '/v1/development/model-catalog')
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(saved(calls)).toHaveLength(1))
    expect(saved(calls)[0].body).toEqual({
      name: 'jev', base_url: 'http://127.0.0.1:9000', protocol: 'openai', models: ['typesafe-jev', 'typesafe-jev-fast'],
      json_object_mode: false,
    })
    await waitFor(() => expect(reads(calls, '/v1/development/model-catalog')).toBeGreaterThan(before))
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('checks the address and the models before saving', async () => {
    const calls = serve()
    render(<ModelsPage session={admin} />)
    fireEvent.click((await screen.findAllByRole('button', { name: /添加 provider/ }))[0])
    type('名称', 'jev')
    type('地址', 'ftp://example.com')
    type('模型 1', 'jev')
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText('地址须以 http:// 或 https:// 开头')).toBeTruthy()
    type('地址', 'https://example.com')
    type('模型 1', ' ')
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText('请至少填写一个模型')).toBeTruthy()
    type('名称', 'nimble')
    type('模型 1', 'jev')
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText('已有同名的 provider，请换一个名称，或打开它进行编辑')).toBeTruthy()
    expect(saved(calls)).toHaveLength(0)
  })

  it('keeps, replaces or clears the stored key on edit', async () => {
    const calls = serve()
    render(<ModelsPage session={admin} />)
    const edit = async () => { await open(/^deepseek/); return screen.getByRole('group', { name: '密钥' }) }

    // Keep: the key is left out.
    let keys = await edit()
    expect(within(keys).getByRole('button', { name: '保持不变' }).getAttribute('aria-pressed')).toBe('true')
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(saved(calls)).toHaveLength(1))
    expect(saved(calls)[0].body).not.toHaveProperty('api_key')
    expect(saved(calls)[0].body).toMatchObject({ name: 'deepseek', protocol: 'openai' })

    // Replace: the new key is sent; an empty replacement is refused.
    keys = await edit()
    fireEvent.click(within(keys).getByRole('button', { name: '替换' }))
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(await screen.findByText('请填写新密钥，或选择“保持不变”')).toBeTruthy()
    type('新密钥', 'sk-new')
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(saved(calls)).toHaveLength(2))
    expect(saved(calls)[1].body?.api_key).toBe('sk-new')

    // Clear: asks first, then sends an empty key.
    keys = await edit()
    fireEvent.click(within(keys).getByRole('button', { name: '清空' }))
    const confirm = screen.getByRole('alertdialog', { name: '确认清空密钥' })
    fireEvent.click(within(confirm).getByRole('button', { name: '取消' }))
    expect(within(keys).getByRole('button', { name: '保持不变' }).getAttribute('aria-pressed')).toBe('true')
    fireEvent.click(within(keys).getByRole('button', { name: '清空' }))
    fireEvent.click(within(screen.getByRole('alertdialog', { name: '确认清空密钥' })).getByRole('button', { name: '清空密钥' }))
    expect(screen.getByText('保存后清空密钥')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(saved(calls)).toHaveLength(3))
    expect(saved(calls)[2].body?.api_key).toBe('')
  })

  it('offers the tuning options only for OpenAI-compatible providers', async () => {
    const calls = serve()
    render(<ModelsPage session={admin} />)
    fireEvent.click((await screen.findAllByRole('button', { name: /添加 provider/ }))[0])
    expect(screen.getByText('高级选项')).toBeTruthy()
    expect(screen.getByRole('switch', { name: '结构化输出使用 JSON object 模式' })).toBeTruthy()
    choose('协议', 'System One 决策模型')
    expect(screen.getByText('决策模型回答结构化问题，不调用工具；可以和其他模型成员一起参与团队流程。')).toBeTruthy()
    expect(screen.queryByText('高级选项')).toBeNull()
    expect(screen.queryByLabelText('单次超时（秒）')).toBeNull()
    type('名称', 'jev')
    type('地址', 'http://127.0.0.1:9000')
    type('模型 1', 'typesafe-jev')
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(saved(calls)).toHaveLength(1))
    expect(saved(calls)[0].body).toEqual({ name: 'jev', base_url: 'http://127.0.0.1:9000', protocol: 'systemone', models: ['typesafe-jev'] })
    fireEvent.click((await screen.findAllByRole('button', { name: /添加 provider/ }))[0])
    choose('协议', 'System One 决策模型')
    choose('协议', 'OpenAI 兼容')
    expect(screen.getByText('高级选项')).toBeTruthy()
  })

  it('asks before deleting and warns about members using its models', async () => {
    const calls = serve()
    render(<ModelsPage session={admin} />)
    await open(/^nimble/)
    fireEvent.click(screen.getByRole('button', { name: '删除' }))
    const confirm = screen.getByRole('alertdialog', { name: '确认删除 provider' })
    expect(confirm.textContent).toContain('使用该 provider 模型的成员将无模型可用，需改选')
    expect(calls.some((call) => call.method === 'DELETE')).toBe(false)
    fireEvent.click(within(confirm).getByRole('button', { name: '删除' }))
    await waitFor(() => expect(calls.find((call) => call.method === 'DELETE')?.url).toBe('/v1/development/model-providers/nimble'))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it('is read-only for members', async () => {
    serve()
    render(<ModelsPage session={{ ...admin, role: 'member' }} />)
    await open(/^deepseek/)
    expect(screen.queryByRole('button', { name: /添加 provider/ })).toBeNull()
    expect((screen.getByLabelText('名称') as HTMLInputElement).disabled).toBe(true)
    expect((screen.getByLabelText('模型 1') as HTMLInputElement).disabled).toBe(true)
    expect(screen.queryByRole('group', { name: '密钥' })).toBeNull()
    expect(screen.queryByRole('button', { name: '保存' })).toBeNull()
    expect(screen.queryByRole('button', { name: '删除' })).toBeNull()
    expect(screen.queryByRole('button', { name: '添加模型' })).toBeNull()
  })
})
