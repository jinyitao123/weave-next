import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, signInWithAPIKey, signInWithForge, signOut, type AdminConfig } from '../lib/api'
import { withForgeApproval } from '../lib/forge-approval'
import { SignInPage } from './SignInPage'

vi.mock('../lib/api', async (original) => ({
  ...await original<typeof import('../lib/api')>(),
  signInWithAPIKey: vi.fn(), signInWithForge: vi.fn(), signOut: vi.fn(),
}))
vi.mock('../lib/forge-approval', async (original) => ({
  ...await original<typeof import('../lib/forge-approval')>(),
  withForgeApproval: vi.fn(),
}))

const configured: AdminConfig = { sign_in_methods: ['forge', 'api_key'], forge_origin: 'https://forge.example', secure: true }
const PAGE = 'https://forge.example/_console/auth/device?user_code=WXYZ2345'
const forgeTab = { opener: {} as unknown, location: { replace: vi.fn() }, close: vi.fn() }

// Forge issues the code, the person approves, and the released session reaches the caller.
const approved = () => vi.mocked(withForgeApproval).mockImplementation(async (_origin, options, use) => {
  options.onPending({ userCode: 'WXYZ2345', page: PAGE })
  return use('forge-session')
})

beforeEach(() => {
  forgeTab.opener = {}
  vi.spyOn(window, 'open').mockReturnValue(forgeTab as unknown as Window)
})

afterEach(() => {
  cleanup()
  vi.resetAllMocks()
  window.history.replaceState({}, '', '/admin/')
})

describe('management sign-in', () => {
  it('offers an account first, confirmed on Forge without a password typed here', async () => {
    const onSignedIn = vi.fn()
    const session = { name: '开发者', role: 'developer', source: 'forge' as const }
    vi.mocked(signInWithForge).mockResolvedValue(session)
    approved()
    render(<SignInPage config={configured} onSignedIn={onSignedIn} />)
    expect(screen.getByRole('group', { name: '登录方式' })).toBeTruthy()
    expect(screen.getByRole('button', { name: '账号登录' }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.queryByLabelText('认证密钥')).toBeNull()
    expect(document.querySelector('input')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '用 Forge 账号登录' }))
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledWith(session))
    expect(vi.mocked(withForgeApproval).mock.calls[0][0]).toBe('https://forge.example')
    expect(signInWithForge).toHaveBeenCalledWith('forge-session')
    expect(signInWithAPIKey).not.toHaveBeenCalled()
    // The Forge page opens in its own tab, cut off from this one.
    expect(forgeTab.location.replace).toHaveBeenCalledWith(PAGE)
    expect(forgeTab.opener).toBeNull()
  })

  it('shows the code to compare while Forge waits, and stops quietly when cancelled', async () => {
    vi.mocked(withForgeApproval).mockImplementation((_origin, options) => new Promise((_resolve, reject) => {
      options.onPending({ userCode: 'WXYZ2345', page: PAGE })
      options.signal.addEventListener('abort', () => reject(new ApiError(0, 'forge_approval_cancelled')))
    }))
    render(<SignInPage config={configured} onSignedIn={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: '用 Forge 账号登录' }))
    await waitFor(() => expect(screen.getByLabelText('确认码').textContent).toBe('WXYZ2345'))
    expect(screen.getByRole('link', { name: '打开 Forge 确认页' }).getAttribute('href')).toBe(PAGE)
    expect(screen.queryByRole('button', { name: '用 Forge 账号登录' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '取消' }))
    await waitFor(() => expect(screen.getByRole('button', { name: '用 Forge 账号登录' })).toBeTruthy())
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByLabelText('确认码')).toBeNull()
  })

  it('says why Forge could not confirm and closes the tab it opened for it', async () => {
    vi.mocked(withForgeApproval).mockRejectedValue(new ApiError(404, 'forge_approval_unavailable'))
    render(<SignInPage config={configured} onSignedIn={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: '用 Forge 账号登录' }))
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('还没有启用浏览器确认登录'))
    expect(forgeTab.close).toHaveBeenCalledOnce()
    expect(signInWithForge).not.toHaveBeenCalled()
  })

  it('signs in with a key alone where no account system is connected, and names the default', async () => {
    const onSignedIn = vi.fn()
    const session = { name: '管理员', role: 'admin', source: 'api_key' as const, default_key: true }
    vi.mocked(signInWithAPIKey).mockResolvedValue(session)
    render(<SignInPage config={{ sign_in_methods: ['api_key'], secure: false, default_key: true }} onSignedIn={onSignedIn} />)
    expect(screen.queryByRole('group', { name: '登录方式' })).toBeNull()
    expect(screen.queryByRole('button', { name: '用 Forge 账号登录' })).toBeNull()
    expect(screen.getByText('部署时没有设置密钥，默认密钥是 admin。')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '登录' }))
    expect(screen.getByRole('alert').textContent).toContain('请输入认证密钥')
    fireEvent.change(screen.getByLabelText('认证密钥'), { target: { value: 'admin' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledWith(session))
    expect(signInWithAPIKey).toHaveBeenCalledWith('admin')
    expect(window.open).not.toHaveBeenCalled()
  })

  it('switches between the two ways in and says nothing about a default once a key is set', async () => {
    const onSignedIn = vi.fn()
    const session = { name: '运维', role: 'admin', source: 'api_key' as const }
    vi.mocked(signInWithAPIKey).mockResolvedValue(session)
    render(<SignInPage config={configured} onSignedIn={onSignedIn} />)
    fireEvent.click(screen.getByRole('button', { name: '密钥登录' }))
    expect(screen.queryByRole('button', { name: '用 Forge 账号登录' })).toBeNull()
    expect(screen.queryByText(/默认密钥/)).toBeNull()
    fireEvent.change(screen.getByLabelText('认证密钥'), { target: { value: 'set-at-deployment' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledWith(session))
    expect(withForgeApproval).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '账号登录' }))
    expect(screen.getByRole('button', { name: '用 Forge 账号登录' })).toBeTruthy()
    expect(screen.queryByLabelText('认证密钥')).toBeNull()
  })

  it('keeps the key form on the explicitly requested operator URL', async () => {
    window.history.replaceState({}, '', '/admin/?login=api_key')
    const onSignedIn = vi.fn()
    const session = { name: '运维', role: 'admin', source: 'api_key' as const }
    vi.mocked(signInWithAPIKey).mockResolvedValue(session)
    render(<SignInPage config={configured} onSignedIn={onSignedIn} />)
    expect(screen.getByRole('button', { name: '密钥登录' }).getAttribute('aria-pressed')).toBe('true')
    fireEvent.change(screen.getByLabelText('认证密钥'), { target: { value: ' test-key ' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledWith(session))
    expect(signInWithAPIKey).toHaveBeenCalledWith('test-key')
    expect(withForgeApproval).not.toHaveBeenCalled()
  })

  it('explains the existing member refusal and releases that console session', async () => {
    vi.mocked(signInWithForge).mockResolvedValue({ name: '员工', role: 'member', source: 'forge' })
    vi.mocked(signOut).mockResolvedValue(undefined)
    approved()
    const onSignedIn = vi.fn()
    render(<SignInPage config={configured} onSignedIn={onSignedIn} />)
    fireEvent.click(screen.getByRole('button', { name: '用 Forge 账号登录' }))
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('管理员或开发者'))
    expect(signOut).toHaveBeenCalledOnce()
    expect(onSignedIn).not.toHaveBeenCalled()
  })
})
