/** Platform login and per-request identity for a shared Workbench Host. */
import { AsyncLocalStorage } from 'node:async_hooks'
import { createHash, randomBytes } from 'node:crypto'

export interface WorkbenchUser { readonly id: string; readonly workspace_id: string; readonly username: string; readonly display_name: string; readonly role: string }
interface Account { readonly id: string; readonly token: string; readonly user: WorkbenchUser; readonly expiresAt: number; readonly clients: Map<string, Set<string>> }
export interface SessionOwner { readonly userId: string; readonly workspaceId: string }
export interface AccountSessions {
  owner(id: string): Promise<SessionOwner | undefined>
  exists(id: string): Promise<boolean>
}
function object(value: unknown): Record<string, unknown> | undefined { return typeof value === 'object' && value !== null && !Array.isArray(value) ? value as Record<string, unknown> : undefined }
function response(status: number, code: string): Response { return Response.json({ code }, { status, headers: { 'Cache-Control': 'no-store' } }) }
function same(owner: SessionOwner, user: WorkbenchUser): boolean { return owner.userId === user.id && owner.workspaceId === user.workspace_id }

/** JWTs live only in this Host process. Logout/restart destroys their authority. */
export class WorkbenchAccounts {
  private readonly records = new Map<string, Account>()
  private readonly scope = new AsyncLocalStorage<Account>()
  private readonly sessionGrants = new Map<string, string>()
  private readonly streamClients = new WeakMap<Request, string>()
  constructor(private readonly apiUrl: string, private readonly apiKey: string, private readonly sessions: AccountSessions, private readonly fetcher: typeof fetch = fetch) {}
  private cookieName(request: Request): string { return 'weave-user-' + createHash('sha256').update(new URL(request.url).host).digest('hex').slice(0, 16) }
  private cookie(request: Request, value: string, age: number): string { return `${this.cookieName(request)}=${value}; Path=/; HttpOnly; SameSite=Strict; Max-Age=${age}${new URL(request.url).protocol === 'https:' ? '; Secure' : ''}` }
  private active(id: string | undefined): Account | undefined {
    if (id === undefined) return undefined
    const record = this.records.get(id)
    if (record !== undefined && record.expiresAt > Date.now()) return record
    this.records.delete(id); return undefined
  }
  private fromRequest(request: Request): Account | undefined {
    const name = this.cookieName(request)
    const value = request.headers.get('cookie')?.split(';').map(part => part.trim()).find(part => part.startsWith(name + '='))?.slice(name.length + 1)
    return this.active(value)
  }
  currentUser(): WorkbenchUser | undefined { return this.active(this.scope.getStore()?.id)?.user }
  currentOwner(): SessionOwner | undefined { const user = this.currentUser(); return user === undefined ? undefined : { userId: user.id, workspaceId: user.workspace_id } }
  bindNewSession(id: string): void { const current = this.scope.getStore(); if (this.active(current?.id) === undefined) throw new Error('login_required'); this.sessionGrants.set(id, current!.id) }
  visibility(): (id: string) => Promise<boolean> {
    const current = this.scope.getStore()
    return async id => {
      if (this.active(current?.id) === undefined) return false
      const owner = await this.sessions.owner(id)
      if (owner === undefined || !same(owner, current!.user)) return false
      this.sessionGrants.set(id, current!.id); return true
    }
  }
  inheritSession(id: string, parentId: string): void { const grant = this.sessionGrants.get(parentId); if (this.active(grant) !== undefined) this.sessionGrants.set(id, grant!) }
  async visible(id: string): Promise<boolean> { return this.visibility()(id) }
  async authorizeSession(id: string, allowNew = false): Promise<void> {
    if (await this.visible(id)) return
    if (allowNew && !(await this.sessions.exists(id))) return
    throw new Error('session_not_found')
  }
  sessionHeaders(id: string): Headers {
    const account = this.active(this.sessionGrants.get(id))
    if (account === undefined) throw new Error('login_required')
    return this.headers(account)
  }
  currentHeaders(): Headers {
    const account = this.active(this.scope.getStore()?.id)
    if (account === undefined) throw new Error('login_required')
    return this.headers(account)
  }
  private headers(account: Account): Headers { return new Headers({ Authorization: `Bearer ${this.apiKey}`, 'X-Weave-User-Authorization': `Bearer ${account.token}` }) }
  requestFetch(request: Request): typeof fetch {
    return async (input, init = {}) => {
      const account = this.fromRequest(request)
      if (account === undefined) return response(401, 'login_required')
      const headers = new Headers(init.headers); for (const [key, value] of this.headers(account)) headers.set(key, value)
      headers.delete('X-Weave-Actor-ID')
      return this.fetcher(input, { ...init, headers })
    }
  }
  async streamVisible(request: Request, value: unknown): Promise<boolean> {
    const frame = object(value)
    const account = this.active(this.scope.getStore()?.id)
    if (account === undefined) return false
    if (frame?.type === 'ready' && typeof frame.clientId === 'string') {
      account.clients.set(frame.clientId, new Set()); this.streamClients.set(request, frame.clientId); return true
    }
    const deliveries = account.clients.get(this.streamClients.get(request) ?? '')
    if (frame?.type === 'waterfall' && typeof frame.agentId === 'string') {
      const allowed = await this.visible(frame.agentId)
      if (allowed && typeof frame.eventId === 'string') deliveries?.add(frame.eventId)
      return allowed
    }
    if (frame?.type === 'cancel') {
      if (typeof frame.eventId !== 'string' || !deliveries?.has(frame.eventId)) return false
      deliveries.delete(frame.eventId); return true
    }
    if (frame?.type === 'emit') {
      if (typeof frame.event !== 'string') return false
      if (frame.event.startsWith('api-session/')) {
        const first: unknown = Array.isArray(frame.args) ? frame.args[0] : undefined
        const id = typeof first === 'string' ? first : object(first)?.sessionId
        return typeof id === 'string' && await this.visible(id)
      }
      // Shared catalog notifications contain no Session business content.
      return ['commands/change', 'llm/adapters-updated'].includes(frame.event)
    }
    return true
  }
  async handle(request: Request): Promise<Response> {
    const current = this.fromRequest(request)
    const view = (account: Account | undefined, headers?: HeadersInit): Response => Response.json(account === undefined ? { authenticated: false } : { authenticated: true, user: account.user }, { headers: { 'Cache-Control': 'no-store', ...Object.fromEntries(new Headers(headers)) } })
    if (request.method === 'GET') return view(current)
    if (request.method !== 'POST') return response(405, 'method_not_allowed')
    let input: Record<string, unknown> | undefined
    try { input = object(await request.json()) } catch { return response(400, 'invalid_input') }
    if (input?.action === 'logout') {
      if (current !== undefined) this.records.delete(current.id)
      return view(undefined, { 'Set-Cookie': this.cookie(request, '', 0) })
    }
    if (input?.action !== 'login' || typeof input.username !== 'string' || typeof input.password !== 'string' || input.username.length === 0 || input.password.length === 0 || input.username.length > 200 || input.password.length > 1024 || (input.tenant !== undefined && typeof input.tenant !== 'string')) return response(400, 'invalid_input')
    if (this.apiKey === '') return response(503, 'weave_disconnected')
    try {
      const login = await this.fetcher(`${this.apiUrl}/v1/auth/login`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ username: input.username, password: input.password, tenant: input.tenant ?? 'default' }), signal: AbortSignal.timeout(15_000), redirect: 'error' })
      if (!login.ok) { await login.body?.cancel(); return response(login.status === 401 ? 401 : 502, login.status === 401 ? 'invalid_credentials' : 'weave_unreachable') }
      const data = object(await login.json()); const user = object(data?.user)
      if (typeof data?.token !== 'string' || typeof user?.id !== 'string' || typeof user.tenant_id !== 'string' || typeof user.username !== 'string' || typeof user.role !== 'string') return response(502, 'weave_unreachable')
      // Verify the Host is allowed to act for this account before installing it.
      const checked = await this.fetcher(`${this.apiUrl}/v1/auth/me`, { headers: { Authorization: `Bearer ${this.apiKey}`, 'X-Weave-User-Authorization': `Bearer ${data.token}` }, signal: AbortSignal.timeout(15_000), redirect: 'error' })
      if (!checked.ok) { await checked.body?.cancel(); return response(401, 'invalid_credentials') }
      await checked.body?.cancel()
      let expiresAt = Date.now() + 8 * 60 * 60 * 1000
      try { const claims = object(JSON.parse(Buffer.from(data.token.split('.')[1] ?? '', 'base64url').toString('utf8'))); if (typeof claims?.exp === 'number') expiresAt = Math.min(expiresAt, claims.exp * 1000) } catch { return response(502, 'weave_unreachable') }
      if (expiresAt <= Date.now()) return response(401, 'invalid_credentials')
      const account: Account = { id: randomBytes(32).toString('base64url'), token: data.token, expiresAt, clients: new Map(), user: { id: user.id, workspace_id: user.tenant_id, username: user.username, display_name: typeof user.display_name === 'string' ? user.display_name : user.username, role: user.role } }
      if (current !== undefined) this.records.delete(current.id)
      this.records.set(account.id, account)
      return view(account, { 'Set-Cookie': this.cookie(request, account.id, Math.floor((expiresAt - Date.now()) / 1000)) })
    } catch { return response(502, 'weave_unreachable') }
  }
  async guard(request: Request, next: (request: Request) => Promise<Response>): Promise<Response> {
    if (new URL(request.url).pathname === '/api/weave.account') return next(request)
    const account = this.fromRequest(request)
    if (account === undefined) return response(401, 'login_required')
    const pathname = new URL(request.url).pathname
    if (account.user.role !== 'admin' && pathname !== '/api/workspace/archiveSession' && ['settings/', 'credentials/', 'directoryPicker/', 'dynamicCordisRunner/', 'workspace/'].some(prefix => pathname.startsWith('/api/' + prefix))) return response(403, 'host_admin_required')
    if (account.user.role !== 'admin' && pathname === '/api/session/openWorkspacePath') return response(403, 'host_admin_required')
    return this.scope.run(account, async () => {
      const sessionId = new URL(request.url).searchParams.get('sessionId')
      if (sessionId !== null) { try { await this.authorizeSession(sessionId) } catch { return response(404, 'session_not_found') } }
      if (request.method === 'POST' && request.headers.get('content-type')?.includes('application/json') === true) {
        let payload: unknown
        try { payload = await request.clone().json() } catch { return response(400, 'invalid_input') }
        const inspect = async (value: unknown, depth = 0): Promise<void> => {
          if (depth > 8) throw new Error('invalid_input')
          if (Array.isArray(value)) { for (const entry of value) await inspect(entry, depth + 1); return }
          const fields = object(value); if (fields === undefined) return
          if (pathname === '/api/$events/result' && ('clientId' in fields || 'eventId' in fields) && (typeof fields.clientId !== 'string' || typeof fields.eventId !== 'string' || !account.clients.get(fields.clientId)?.has(fields.eventId))) throw new Error('session_not_found')
          if (account.user.role !== 'admin' && pathname === '/api/session/create' && (fields.cwd !== undefined || fields.workspaceId !== undefined)) throw new Error('host_admin_required')
          for (const [key, child] of Object.entries(fields)) {
            if ((['sessionId', 'parentSessionId', 'childSessionId', 'beforeSessionId'].includes(key) || (key === 'agentId' && !pathname.startsWith('/api/weave.'))) && typeof child === 'string') await this.authorizeSession(child, key === 'sessionId' && pathname.endsWith('/create'))
            else if (['payload', 'params', 'args', 'request', 'address'].includes(key) && typeof child === 'object' && child !== null) await inspect(child, depth + 1)
          }
        }
        try { await inspect(payload) } catch (error) { return error instanceof Error && error.message === 'host_admin_required' ? response(403, 'host_admin_required') : response(404, 'session_not_found') }
      }
      const result = await next(request)
      return this.active(account.id) === undefined ? response(401, 'login_required') : result
    })
  }
}
