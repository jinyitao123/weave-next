import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'
import { ApiError, readConfig, readSession, signOut, type AdminConfig, type AdminSession } from './lib/api'
import { embedded, withForgePageApproval } from './lib/forge-embed'

vi.mock('./lib/api', async (original) => ({
  ...await original<typeof import('./lib/api')>(),
  readConfig: vi.fn(), readSession: vi.fn(), signOut: vi.fn(),
}))
vi.mock('./lib/forge-embed', () => ({ embedded: vi.fn(() => false), withForgePageApproval: vi.fn() }))
const developer: AdminSession = { name: '小周', role: 'developer', source: 'forge' }
vi.mock('./pages/SignInPage', () => ({
  SignInPage: ({ onSignedIn, initialError }: { onSignedIn(session: AdminSession): void; initialError?: string }) => <>{initialError ? <p role="alert">{initialError}</p> : null}<button type="button" onClick={() => onSignedIn({ name: '小周', role: 'developer', source: 'forge' })}>登录页</button></>,
}))
vi.mock('./pages/Shell', () => ({ Shell: ({ session }: { session: AdminSession }) => <p>已进入：{session.name}</p> }))

const config: AdminConfig = { sign_in_methods: ['forge', 'api_key'], forge_origin: 'https://forge.example', secure: true }

afterEach(() => {
  cleanup()
  vi.resetAllMocks()
})

describe('console start', () => {
  it('opens the console for a session that may use it', async () => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(readSession).mockResolvedValue(developer)
    render(<App />)
    expect(await screen.findByText('已进入：小周')).toBeTruthy()
  })

  it('shows the sign-in page when nobody is signed in', async () => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(readSession).mockRejectedValue(new ApiError(401, 'missing authorization header'))
    render(<App />)
    expect(await screen.findByRole('button', { name: '登录页' })).toBeTruthy()
    expect(screen.queryByText('Weave 暂时无法访问')).toBeNull()
  })

  it('tells an account the console is not open to so, and lets it leave to sign in as another', async () => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(readSession).mockRejectedValue(new ApiError(403, 'forbidden'))
    vi.mocked(signOut).mockResolvedValue(undefined)
    render(<App />)
    expect(await screen.findByRole('heading', { name: '当前账号不能进入管理端' })).toBeTruthy()
    // It is an answer about this account, not a fault to retry.
    expect(screen.queryByText('Weave 暂时无法访问')).toBeNull()
    expect(screen.queryByRole('button', { name: '重试' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '退出并重新登录' }))
    await waitFor(() => expect(signOut).toHaveBeenCalledOnce())
    fireEvent.click(await screen.findByRole('button', { name: '登录页' }))
    expect(await screen.findByText('已进入：小周')).toBeTruthy()
    // Leaving went through the server; the page never read the session again to get past the refusal.
    expect(readSession).toHaveBeenCalledOnce()
  })

  it('keeps the refused account where it is when leaving does not go through', async () => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(readSession).mockRejectedValue(new ApiError(403, 'forbidden'))
    vi.mocked(signOut).mockRejectedValueOnce(new ApiError(403, 'admin_request_rejected')).mockResolvedValueOnce(undefined)
    render(<App />)
    fireEvent.click(await screen.findByRole('button', { name: '退出并重新登录' }))
    expect((await screen.findByRole('alert')).textContent).toBe('退出没有完成，请重试。')
    expect(screen.queryByRole('button', { name: '登录页' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '退出并重新登录' }))
    expect(await screen.findByRole('button', { name: '登录页' })).toBeTruthy()
  })

  it.each([
    ['a server fault', new ApiError(500, 'http_500')],
    ['a gateway fault', new ApiError(502, 'http_502')],
    ['no connection', new TypeError('Failed to fetch')],
  ])('says Weave cannot be reached, with a retry, on %s', async (_name, failure) => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(readSession).mockRejectedValueOnce(failure).mockResolvedValueOnce(developer)
    render(<App />)
    expect(await screen.findByRole('heading', { name: 'Weave 暂时无法访问' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: '退出并重新登录' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    expect(await screen.findByText('已进入：小周')).toBeTruthy()
  })

  it('says Weave cannot be reached when its settings cannot be read, whatever the session would say', async () => {
    vi.mocked(readConfig).mockRejectedValue(new ApiError(403, 'forbidden'))
    render(<App />)
    expect(await screen.findByRole('heading', { name: 'Weave 暂时无法访问' })).toBeTruthy()
    expect(readSession).not.toHaveBeenCalled()
  })

  it('enters as the person signed in to the Forge page around it, whatever session it held', async () => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(embedded).mockReturnValue(true)
    vi.mocked(withForgePageApproval).mockResolvedValue({ name: '小林', role: 'admin', source: 'forge' })
    render(<App />)
    expect(await screen.findByText('已进入：小林')).toBeTruthy()
    expect(readSession).not.toHaveBeenCalled()
    expect(signOut).toHaveBeenCalledOnce()
    expect(vi.mocked(signOut).mock.invocationCallOrder[0]).toBeLessThan(vi.mocked(withForgePageApproval).mock.invocationCallOrder[0])
    expect(vi.mocked(withForgePageApproval).mock.calls[0]?.[0]).toBe('https://forge.example')
  })

  it('does not open for an employee who came in through Forge', async () => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(embedded).mockReturnValue(true)
    vi.mocked(withForgePageApproval).mockResolvedValue({ name: '小王', role: 'member', source: 'forge' })
    render(<App />)
    expect(await screen.findByRole('heading', { name: '当前账号不能进入管理端' })).toBeTruthy()
  })

  it.each(['forge_page_unavailable', 'forge_approval_denied', 'forge_approval_unavailable', 'forge_client_refused'])('does not reuse a prior administrator session when embedded approval fails with %s', async (code) => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(embedded).mockReturnValue(true)
    vi.mocked(withForgePageApproval).mockRejectedValue(new ApiError(0, code))
    vi.mocked(readSession).mockResolvedValue({ name: '上一位管理员', role: 'admin', source: 'forge' })
    render(<App />)
    expect(await screen.findByRole('button', { name: '登录页' })).toBeTruthy()
    expect(readSession).not.toHaveBeenCalled()
    expect(signOut).toHaveBeenCalledOnce()
    expect(screen.queryByText('已进入：上一位管理员')).toBeNull()
    expect(screen.getByRole('alert').textContent).toBe(code === 'forge_page_unavailable' ? 'Forge 页面没有回应，请重试' : code === 'forge_approval_denied' ? '已在 Forge 页面拒绝' : code === 'forge_approval_unavailable' ? '这个 Forge 还没有启用浏览器确认登录，请联系管理员' : '这个 Forge 不接受 Weave 管理端登录，请联系管理员')
  })

  it('stops if the previous embedded identity cannot be cleared', async () => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(embedded).mockReturnValue(true)
    vi.mocked(signOut).mockRejectedValue(new ApiError(500, 'http_500'))
    render(<App />)
    expect(await screen.findByRole('heading', { name: 'Weave 暂时无法访问' })).toBeTruthy()
    expect(withForgePageApproval).not.toHaveBeenCalled()
    expect(readSession).not.toHaveBeenCalled()
  })

  it('cancels the pending embedded approval when the console closes', async () => {
    vi.mocked(readConfig).mockResolvedValue(config)
    vi.mocked(embedded).mockReturnValue(true)
    vi.mocked(withForgePageApproval).mockReturnValue(new Promise(() => undefined))
    const view = render(<App />)
    await waitFor(() => expect(withForgePageApproval).toHaveBeenCalledOnce())
    const signal = vi.mocked(withForgePageApproval).mock.calls[0]![1]
    expect(signal.aborted).toBe(false)
    view.unmount()
    expect(signal.aborted).toBe(true)
  })

  it('does not ask a surrounding page when no Forge sign-in is configured', async () => {
    vi.mocked(readConfig).mockResolvedValue({ sign_in_methods: ['api_key'], secure: true })
    vi.mocked(embedded).mockReturnValue(true)
    vi.mocked(readSession).mockResolvedValue(developer)
    render(<App />)
    expect(await screen.findByText('已进入：小周')).toBeTruthy()
    expect(withForgePageApproval).not.toHaveBeenCalled()
  })
})
