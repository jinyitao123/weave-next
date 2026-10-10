import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AccountCard } from './AccountCard'

afterEach(cleanup)

describe('account card', () => {
  it('shows identity and sign-out directly without a popup', () => {
    const onSignOut = vi.fn()
    render(<AccountCard session={{ name: '本地开发者', role: 'developer', source: 'session' }} onSignOut={onSignOut} />)
    expect(screen.getByRole('region', { name: '登录账户' })).toBeTruthy()
    expect(screen.getByText('本地开发者')).toBeTruthy()
    expect(screen.getByText('开发者')).toBeTruthy()
    expect(screen.getByRole('button', { name: '退出登录' })).toBeTruthy()
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(onSignOut).not.toHaveBeenCalled()
  })

  it('keeps the session facts and signs out only when the direct action is chosen', () => {
    const onSignOut = vi.fn()
    render(<AccountCard session={{ name: 'Yitao Kim', role: 'admin', source: 'forge' }} onSignOut={onSignOut} />)
    expect(screen.getByText('管理员')).toBeTruthy()
    expect(onSignOut).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '退出登录' }))
    expect(onSignOut).toHaveBeenCalledOnce()
  })
})
