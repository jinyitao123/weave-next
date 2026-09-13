/** Browser account projection. Passwords and bearer credentials never enter its snapshot. */
import type { HostObservable, InjectFace } from '@deepseek-ai/dsh-client-ui-slots'

/** Public account information returned by the Host. */
export interface AccountUser {
  id: string
  workspace_id: string
  username: string
  display_name: string
  role: string
}

/** Product error categories; raw transport messages are never rendered. */
export type AccountError = 'invalid' | 'unavailable' | 'network' | 'expired' | 'logout'

/** One stable account view shared by the gate and sidebar control. */
export interface AccountView {
  status: 'loading' | 'anonymous' | 'authenticated' | 'signing-in' | 'signing-out' | 'changing' | 'error' | 'logout-failed'
  user: AccountUser | null
  error: AccountError | null
}

/** Login input exists only during the submitted request. */
export interface AccountLogin { username: string; password: string; tenant?: string }

/** The renderer turns this projection into useAccount; components receive only commands. */
export interface AccountInjected {
  hooks: { account: HostObservable<AccountView> }
  login: (input: AccountLogin) => Promise<void>
  logout: () => Promise<void>
  retry: () => Promise<void>
}

/** Derived private props for account components. */
export type AccountProps = InjectFace<AccountInjected>

function userFrom(value: unknown): AccountUser | null {
  if (typeof value !== 'object' || value === null) return null
  const user = value as Record<string, unknown>
  if (typeof user.id !== 'string' || user.id === '' || typeof user.workspace_id !== 'string' || user.workspace_id === ''
    || typeof user.username !== 'string' || user.username === '' || typeof user.display_name !== 'string' || typeof user.role !== 'string') return null
  return { id: user.id, workspace_id: user.workspace_id, username: user.username, display_name: user.display_name, role: user.role }
}

function identity(user: AccountUser): string {
  return JSON.stringify([user.workspace_id, user.id])
}

/** Account lifecycle and same-origin API response invalidation, scoped to one plugin lifetime. */
export class AccountController implements HostObservable<AccountView> {
  private view: AccountView = { status: 'loading', user: null, error: null }
  private readonly listeners = new Set<() => void>()
  private revision = 0
  private knownIdentity: string | null = null
  private closed = false
  private checking: Promise<void> | null = null
  private announceChange: () => void = () => {}

  constructor(private readonly request: typeof fetch, private readonly reload: () => void) {}

  /** Read the latest immutable public account projection. */
  getSnapshot = (): AccountView => this.view

  /** Subscribe for the framework's generated hook. */
  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    return () => { this.listeners.delete(listener) }
  }

  private publish(view: AccountView): void {
    if (this.closed) return
    this.view = view
    for (const listener of this.listeners) {
      try { listener() } catch { console.error('Account view subscriber failed') }
    }
  }

  /** Withhold old views and ignore reads started before an identity failure. */
  expire = (): void => {
    if (this.closed || this.view.status !== 'authenticated') return
    this.revision += 1
    this.publish({ status: 'anonymous', user: null, error: 'expired' })
  }

  /** Restore the public account from the Host cookie; stale reads cannot restore old identities. */
  refresh = (): Promise<void> => {
    if (this.closed || ['signing-in', 'signing-out', 'changing', 'logout-failed'].includes(this.view.status)) return Promise.resolve()
    if (this.checking !== null) return this.checking
    const current = this.check()
    this.checking = current
    void current.finally(() => { if (this.checking === current) this.checking = null })
    return current

  }

  private async check(): Promise<void> {
    const revision = this.revision
    try {
      const response = await this.request('/api/weave.account', { credentials: 'same-origin', cache: 'no-store' })
      if (this.closed || revision !== this.revision) return
      if (!response.ok) {
        this.publish({ status: response.status === 401 ? 'anonymous' : 'error', user: null,
          error: response.status === 401 ? 'expired' : 'unavailable' })
        return
      }
      const payload = await response.json() as { authenticated?: unknown; user?: unknown }
      if (this.closed || revision !== this.revision) return
      const user = payload.authenticated === true ? userFrom(payload.user) : null
      if (payload.authenticated === true && user === null) {
        this.publish({ status: 'error', user: null, error: 'unavailable' })
      } else if (user === null) {
        this.publish({ status: 'anonymous', user: null, error: this.knownIdentity === null ? null : 'expired' })
      } else if (this.knownIdentity !== null && this.knownIdentity !== identity(user)) {
        this.publish({ status: 'changing', user: null, error: null })
        this.reload()
      } else {
        this.knownIdentity = identity(user)
        this.publish({ status: 'authenticated', user, error: null })
      }
    } catch {
      if (revision === this.revision) this.publish({ status: 'error', user: null, error: 'network' })
    }
  }

  /** Submit credentials once, then rebuild the browser runtime before business views can mount. */
  login = async (input: AccountLogin): Promise<void> => {
    if (this.closed || !['anonymous', 'error'].includes(this.view.status)) return
    const revision = ++this.revision
    this.publish({ status: 'signing-in', user: null, error: null })
    try {
      const response = await this.request('/api/weave.account', {
        method: 'POST', credentials: 'same-origin', cache: 'no-store',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ action: 'login', username: input.username.trim(), password: input.password,
          ...(input.tenant?.trim() ? { tenant: input.tenant.trim() } : {}) }),
      })
      if (this.closed || revision !== this.revision) return
      if (!response.ok) {
        this.publish({ status: 'anonymous', user: null, error: response.status === 401 ? 'invalid' : 'unavailable' })
        return
      }
      const payload = await response.json() as { authenticated?: unknown; user?: unknown }
      if (this.closed || revision !== this.revision) return
      if (payload.authenticated !== true || userFrom(payload.user) === null) {
        this.publish({ status: 'anonymous', user: null, error: 'unavailable' })
        return
      }
      this.publish({ status: 'changing', user: null, error: null })
      this.announceChange()
      this.reload()
    } catch {
      if (revision === this.revision) this.publish({ status: 'anonymous', user: null, error: 'network' })
    }
  }

  /** Clear visible account data immediately; failed logout cannot reveal the old workspace. */
  logout = async (): Promise<void> => {
    if (this.closed || !['authenticated', 'logout-failed'].includes(this.view.status)) return
    const revision = ++this.revision
    this.publish({ status: 'signing-out', user: null, error: null })
    try {
      const response = await this.request('/api/weave.account', {
        method: 'POST', credentials: 'same-origin', cache: 'no-store',
        headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action: 'logout' }),
      })
      if (this.closed || revision !== this.revision) return
      if (!response.ok && response.status !== 401) {
        this.publish({ status: 'logout-failed', user: null, error: 'logout' })
        return
      }
      this.publish({ status: 'changing', user: null, error: null })
      this.announceChange()
      this.reload()
    } catch {
      if (revision === this.revision) this.publish({ status: 'logout-failed', user: null, error: 'logout' })
    }
  }

  /** Recheck access after a temporary connection failure. */
  retry = async (): Promise<void> => {
    if (this.view.status === 'logout-failed') return this.logout()
    this.publish({ status: 'loading', user: null, error: null })
    return this.refresh()
  }

  /** Watch only same-origin business API calls; account form failures retain their local error. */
  install(browser: Window, intervalMs: number): () => void {
    let channel: BroadcastChannel | null = null
    try { if (typeof BroadcastChannel !== 'undefined') channel = new BroadcastChannel('weave-account') }
    catch { /* Restricted browsers still recheck account access on focus and periodically. */ }
    this.announceChange = () => {
      try { channel?.postMessage('changed') }
      catch { /* Optional tab notification cannot prevent the current tab from signing out. */ }
    }
    if (channel !== null) channel.onmessage = () => {
      this.revision += 1
      this.publish({ status: 'changing', user: null, error: null })
      this.reload()
    }
    const original = browser.fetch
    const isBusinessAPI = (input: RequestInfo | URL): boolean => {
      const url = new URL(typeof input === 'string' || input instanceof URL ? input : input.url, browser.location.href)
      return url.origin === browser.location.origin && (url.pathname === '/api' || url.pathname.startsWith('/api/'))
        && url.pathname !== '/api/weave.account'
    }
    const guarded: typeof fetch = async (input, init) => {
      if (!isBusinessAPI(input)) return original(input, init)
      if (this.view.status === 'loading') await this.refresh()
      if (this.view.status !== 'authenticated') throw new DOMException('Account required', 'AbortError')
      const revision = this.revision
      const response = await original(input, init)
      if (response.status === 401) this.expire()
      if (revision !== this.revision || this.view.status !== 'authenticated') {
        void response.body?.cancel().catch(() => { /* A body already closed by its transport has no data to retain. */ })
        throw new DOMException('Account changed', 'AbortError')
      }
      return response
    }
    browser.fetch = guarded
    const onFocus = (): void => { void this.refresh() }
    const timer = browser.setInterval(onFocus, intervalMs)
    browser.addEventListener('focus', onFocus)
    browser.addEventListener('weave:account-required', this.expire)
    void this.refresh()
    return () => {
      this.closed = true
      this.revision += 1
      channel?.close()
      this.announceChange = () => {}
      browser.clearInterval(timer)
      browser.removeEventListener('focus', onFocus)
      browser.removeEventListener('weave:account-required', this.expire)
      if (browser.fetch === guarded) browser.fetch = original
      this.listeners.clear()
    }
  }
}
