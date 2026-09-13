// @vitest-environment jsdom
import { useEffect, useState, useSyncExternalStore } from 'react'
import { act, cleanup, fireEvent, render, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { AccountAccess, AccountButton } from '../src/client/AccountAccess.tsx'
import { AccountController, type AccountUser, type AccountProps } from '../src/client/account-controller.ts'
import { zh } from '../src/client/locales.ts'

const t = makeTranslate(zh, commonZh)
const alice: AccountUser = { id: 'alice-id', workspace_id: 'workspace-shared', username: 'alice', display_name: '小林', role: 'admin' }
const bob: AccountUser = { ...alice, id: 'bob-id', username: 'bob', display_name: '小周', role: 'user' }
const result = (user: AccountUser | null) => Response.json(user === null ? { authenticated: false } : { authenticated: true, user })
const disposers: (() => void)[] = []
beforeEach(() => { localStorage.clear(); sessionStorage.clear() })
afterEach(() => { cleanup(); disposers.splice(0).forEach(dispose => { dispose() }); vi.unstubAllGlobals(); vi.useRealTimers() })

function inputs(controller: AccountController): AccountProps {
  return {
    useAccount: selector => selector(useSyncExternalStore(controller.subscribe, controller.getSnapshot)),
    login: controller.login, logout: controller.logout, retry: controller.retry,
  }
}

function Fixture({ controller, mounted = () => {} }: { controller: AccountController; mounted?: () => void }) {
  const [identity, setIdentity] = useState<string | null>(null)
  const props = { ...inputs(controller), t }
  function Business() {
    useEffect(mounted, [])
    return <div data-testid="tasks">{controller.getSnapshot().user?.display_name}的任务</div>
  }
  return <>
    <AccountAccess {...props as Parameters<typeof AccountAccess>[0]} onAccessChange={setIdentity} />
    {identity !== null && <>
      <Business key={identity} />
      <AccountButton {...props as Parameters<typeof AccountButton>[0]} wide />
    </>}
  </>
}

function fill(view: ReturnType<typeof render>, username: string, password = 'test-password') {
  fireEvent.change(view.getByLabelText('账号'), { target: { value: username } })
  fireEvent.change(view.getByLabelText('密码'), { target: { value: password } })
}

function host(initial: AccountUser | null = null) {
  let user = initial
  const reload = vi.fn()
  const request = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    if (init?.method !== 'POST') return result(user)
    const body = JSON.parse(String(init.body)) as { action: string; username?: string; password?: string }
    if (body.action === 'logout') { user = null; return result(null) }
    if (body.password !== 'test-password') return Response.json({ code: 'invalid_credentials' }, { status: 401 })
    user = body.username === 'alice' ? alice : bob
    return result(user)
  })
  return { request, reload, controller: () => new AccountController(request, reload) }
}

function defer<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(done => { resolve = done })
  return { promise, resolve }
}

describe('Workbench account access', () => {
  it('withholds business views while loading and offers a simple account form', async () => {
    const response = defer<Response>()
    const request = vi.fn(() => response.promise)
    const controller = new AccountController(request, vi.fn())
    const mounted = vi.fn()
    const view = render(<Fixture controller={controller} mounted={mounted} />)
    expect(view.getByRole('status').textContent).toContain('正在确认你的账号')
    expect(mounted).not.toHaveBeenCalled()
    await act(async () => { const pending = controller.refresh(); response.resolve(result(null)); await pending })
    expect(view.getByRole('heading', { name: '登录 Workbench' })).toBeTruthy()
    expect(view.getByLabelText('账号').getAttribute('type')).not.toBe('email')
    expect(view.getByLabelText('密码').getAttribute('type')).toBe('password')
    expect(view.getByText('工作区选项').closest('details')?.open).toBe(false)
    expect(view.queryByTestId('tasks')).toBeNull()
  })

  it('restores the current account after a browser refresh without entering a password', async () => {
    const server = host(alice)
    const controller = server.controller()
    const view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    expect(view.getByText('小林的任务')).toBeTruthy()
    fireEvent.click(view.getByRole('button', { name: '账号：小林' }))
    expect(view.getByRole('dialog', { name: '当前账号' }).textContent).toContain('alice')
    expect(view.getByText('管理员')).toBeTruthy()
    expect(view.container.textContent).not.toMatch(/alice-id|workspace-shared|JWT|session_id/)
    expect(server.request.mock.calls.every(([, init]) => init?.method === undefined)).toBe(true)
    expect(server.reload).not.toHaveBeenCalled()
  })

  it('clears the submitted password immediately, does not persist it, and explains invalid credentials', async () => {
    const server = host()
    const controller = server.controller()
    const view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    fill(view, 'alice', 'wrong-password')
    fireEvent.click(view.getByRole('button', { name: '登录' }))
    expect((view.getByLabelText('密码') as HTMLInputElement).value).toBe('')
    await waitFor(() => { expect(view.getByRole('alert').textContent).toContain('账号或密码不正确') })
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    expect(JSON.stringify(controller.getSnapshot())).not.toContain('wrong-password')
    expect(server.reload).not.toHaveBeenCalled()
    expect(view.queryByTestId('tasks')).toBeNull()
  })

  it('switches from one user to another through sign out and a fresh browser runtime', async () => {
    const server = host(alice)
    let controller = server.controller()
    let view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    fireEvent.click(view.getByRole('button', { name: '账号：小林' }))
    fireEvent.click(view.getByRole('button', { name: '退出登录' }))
    expect(view.queryByText('小林的任务')).toBeNull()
    expect(view.queryByRole('dialog')).toBeNull()
    await waitFor(() => { expect(server.reload).toHaveBeenCalledTimes(1) })
    view.unmount()
    controller = server.controller()
    view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    fill(view, 'bob')
    fireEvent.click(view.getByRole('button', { name: '登录' }))
    await waitFor(() => { expect(server.reload).toHaveBeenCalledTimes(2) })
    expect(view.queryByTestId('tasks')).toBeNull()
    view.unmount()
    controller = server.controller()
    view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    expect(view.getByText('小周的任务')).toBeTruthy()
    expect(view.queryByText('小林的任务')).toBeNull()
  })

  it('submits an optional workspace only when selected', async () => {
    const server = host()
    const controller = server.controller()
    const view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    fill(view, 'alice')
    fireEvent.change(view.getByLabelText('工作区（可选）'), { target: { value: ' design ' } })
    fireEvent.click(view.getByRole('button', { name: '登录' }))
    await waitFor(() => { expect(server.reload).toHaveBeenCalledOnce() })
    expect(JSON.parse(String(server.request.mock.calls[1]?.[1]?.body))).toEqual({ action: 'login', username: 'alice', password: 'test-password', tenant: 'design' })
  })

  it('hides the old view immediately on a business 401 and ignores late successful reads', async () => {
    const late = defer<Response>()
    const request = vi.fn(async (input: RequestInfo | URL) => {
      if (input === '/api/weave.account') return result(alice)
      if (input === '/api/late') return late.promise
      return Response.json({ code: 'login_required' }, { status: 401 })
    })
    vi.stubGlobal('fetch', request)
    const controller = new AccountController(request, vi.fn())
    disposers.push(controller.install(window, 30_000))
    const view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    const oldRequest = window.fetch('/api/late').catch(error => error)
    await act(async () => { await expect(window.fetch('/api/tasks')).rejects.toMatchObject({ name: 'AbortError' }) })
    expect(view.queryByTestId('tasks')).toBeNull()
    expect(view.getByRole('alert').textContent).toContain('登录已失效')
    late.resolve(Response.json({ tasks: ['Alice private task'] }))
    expect(await oldRequest).toMatchObject({ name: 'AbortError' })
  })

  it('sends no business request before the initial account check finishes', async () => {
    const account = defer<Response>()
    const request = vi.fn(async (input: RequestInfo | URL) => input === '/api/weave.account' ? account.promise : Response.json({ tasks: [] }))
    vi.stubGlobal('fetch', request)
    const controller = new AccountController(request, vi.fn())
    disposers.push(controller.install(window, 30_000))
    const business = window.fetch('/api/tasks')
    expect(request).toHaveBeenCalledTimes(1)
    account.resolve(result(alice))
    await business
    expect(request).toHaveBeenCalledTimes(2)
  })

  it('never restores the old view when logout fails and offers a retry', async () => {
    const request = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => init?.method === 'POST'
      ? Response.json({ code: 'weave_unreachable' }, { status: 502 }) : result(alice))
    const controller = new AccountController(request, vi.fn())
    const view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    fireEvent.click(view.getByRole('button', { name: '账号：小林' }))
    fireEvent.click(view.getByRole('button', { name: '退出登录' }))
    await waitFor(() => { expect(view.getByRole('button', { name: '重试退出' })).toBeTruthy() })
    expect(view.queryByTestId('tasks')).toBeNull()
    expect(view.queryByLabelText('密码')).toBeNull()
    expect(view.getByRole('alert').textContent).not.toContain('weave_unreachable')
  })

  it('checks the account on focus and reloads before showing another browser account', async () => {
    let current = alice
    const request = vi.fn(async () => result(current))
    vi.stubGlobal('fetch', request)
    const reload = vi.fn()
    const controller = new AccountController(request, reload)
    disposers.push(controller.install(window, 30_000))
    const view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    current = bob
    await act(async () => { window.dispatchEvent(new Event('focus')); await controller.refresh() })
    expect(view.queryByTestId('tasks')).toBeNull()
    expect(reload).toHaveBeenCalledOnce()
  })

  it('leaves unrelated origin requests untouched and restores fetch when disposed', async () => {
    const request = vi.fn(async (input: RequestInfo | URL) => input === '/api/weave.account' ? result(alice) : new Response('', { status: 401 }))
    vi.stubGlobal('fetch', request)
    const controller = new AccountController(request, vi.fn())
    const dispose = controller.install(window, 30_000)
    await controller.refresh()
    expect((await window.fetch('https://other.example/api/tasks')).status).toBe(401)
    expect(controller.getSnapshot().status).toBe('authenticated')
    dispose()
    expect(window.fetch).toBe(request)
  })

  it('receives account changes from another tab before old content can remain visible', async () => {
    let opened: { onmessage: (() => void) | null; close: () => void } | undefined
    class TabChannel {
      onmessage: (() => void) | null = null
      postMessage = vi.fn()
      close = vi.fn()
      constructor() { opened = this }
    }
    vi.stubGlobal('BroadcastChannel', TabChannel)
    const request = vi.fn(async () => result(alice))
    vi.stubGlobal('fetch', request)
    const reload = vi.fn()
    const controller = new AccountController(request, reload)
    disposers.push(controller.install(window, 30_000))
    const view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    expect(view.getByText('小林的任务')).toBeTruthy()
    act(() => { opened!.onmessage!() })
    expect(view.queryByTestId('tasks')).toBeNull()
    expect(reload).toHaveBeenCalledOnce()
  })

  it('does not send business calls for an anonymous account', async () => {
    const request = vi.fn(async () => result(null))
    vi.stubGlobal('fetch', request)
    const controller = new AccountController(request, vi.fn())
    disposers.push(controller.install(window, 30_000))
    await controller.refresh()
    await expect(window.fetch('/api/tasks')).rejects.toMatchObject({ name: 'AbortError' })
    expect(request).toHaveBeenCalledTimes(1)
  })

  it('keeps a failed initial connection readable and retries through the account route', async () => {
    const request = vi.fn().mockResolvedValueOnce(Response.json({ code: 'weave_disconnected' }, { status: 503 })).mockResolvedValueOnce(result(null))
    const controller = new AccountController(request, vi.fn())
    const view = render(<Fixture controller={controller} />)
    await act(() => controller.refresh())
    expect(view.getByRole('alert').textContent).toBe('暂时无法连接工作台，请稍后重试。')
    expect(view.queryByTestId('tasks')).toBeNull()
    fireEvent.click(view.getByRole('button', { name: '重新连接' }))
    await waitFor(() => { expect(view.getByLabelText('账号')).toBeTruthy() })
    expect(view.queryByRole('alert')).toBeNull()
  })

})
