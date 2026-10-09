import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { signInWithAPIKey, signInWithForge, signOut, type AdminConfig } from '../lib/api'
import { SignInPage } from './SignInPage'

vi.mock('../lib/api', async (original) => ({
  ...await original<typeof import('../lib/api')>(),
  signInWithAPIKey: vi.fn(), signInWithForge: vi.fn(), signOut: vi.fn(),
}))

const configured: AdminConfig = { sign_in_methods: ['forge', 'api_key'], forge_origin: 'https://forge.example', secure: true }

afterEach(() => {
  cleanup()
  vi.resetAllMocks()
  window.history.replaceState({}, '', '/admin/')
})

describe('management sign-in', () => {
  it('defaults to Forge credentials without an API Key switch', async () => {
    const onSignedIn = vi.fn()
    const session = { name: '开发者', role: 'developer', source: 'forge' as const }
    vi.mocked(signInWithForge).mockResolvedValue(session)
    render(<SignInPage config={configured} onSignedIn={onSignedIn} />)
    expect(screen.queryByText('API Key')).toBeNull()
    expect(screen.queryByRole('group', { name: '登录方式' })).toBeNull()
    fireEvent.change(screen.getByLabelText('账号'), { target: { value: 'dev@example.test' } })
    fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'test-password' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledWith(session))
    expect(signInWithForge).toHaveBeenCalledWith('https://forge.example', 'dev@example.test', 'test-password')
    expect(signInWithAPIKey).not.toHaveBeenCalled()
  })

  it('reports missing Forge configuration instead of falling back to a key', () => {
    render(<SignInPage config={{ sign_in_methods: ['api_key'], secure: false }} onSignedIn={vi.fn()} />)
    expect(screen.getByRole('alert').textContent).toContain('Forge 登录尚未配置')
    expect(screen.queryByLabelText('API Key')).toBeNull()
    expect(screen.queryByRole('button', { name: '登录' })).toBeNull()
  })

  it('keeps the key form on the explicitly requested operator URL', async () => {
    window.history.replaceState({}, '', '/admin/?login=api_key')
    const onSignedIn = vi.fn()
    const session = { name: '运维', role: 'admin', source: 'api_key' as const }
    vi.mocked(signInWithAPIKey).mockResolvedValue(session)
    render(<SignInPage config={configured} onSignedIn={onSignedIn} />)
    expect(screen.queryByLabelText('账号')).toBeNull()
    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: ' test-key ' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))
    await waitFor(() => expect(onSignedIn).toHaveBeenCalledWith(session))
    expect(signInWithAPIKey).toHaveBeenCalledWith('test-key')
    expect(signInWithForge).not.toHaveBeenCalled()
  })

  it('explains the existing member refusal and releases that console session', async () => {
    vi.mocked(signInWithForge).mockResolvedValue({ name: '员工', role: 'member', source: 'forge' })
    vi.mocked(signOut).mockResolvedValue(undefined)
    const onSignedIn = vi.fn()
    render(<SignInPage config={configured} onSignedIn={onSignedIn} />)
    fireEvent.change(screen.getByLabelText('账号'), { target: { value: 'member@example.test' } })
    fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'test-password' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))
    await waitFor(() => expect(screen.getByRole('alert').textContent).toContain('管理员或开发者'))
    expect(signOut).toHaveBeenCalledOnce()
    expect(onSignedIn).not.toHaveBeenCalled()
  })
})
