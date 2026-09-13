import { describe, expect, it, vi } from 'vitest'
import { WorkbenchAccounts, type SessionOwner } from '../src/account.ts'

function fixture() {
  const owners = new Map<string, SessionOwner>()
  const fetcher = vi.fn<typeof fetch>(async (input, init) => {
    if (String(input).endsWith('/auth/login')) {
      const { username, password } = JSON.parse(String(init?.body)) as { username: string; password: string }
      if (password !== 'password') return Response.json({}, { status: 401 })
      const claims = Buffer.from(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 })).toString('base64url')
      return Response.json({ token: `${username}.${claims}.signature`, user: { id: username, tenant_id: 'shared', username, display_name: username, role: 'user' } })
    }
    return Response.json({ ok: true })
  })
  const sessions = { owner: async (id: string) => owners.get(id), exists: async (id: string) => owners.has(id) }
  const accounts = new WorkbenchAccounts('http://weave', 'HOST_SECRET', sessions, fetcher)
  const request = (path: string, cookie = '', body?: unknown): Request => new Request('http://host' + path, { method: body === undefined ? 'GET' : 'POST', headers: { cookie, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) }, ...(body === undefined ? {} : { body: JSON.stringify(body) }) })
  const login = async (username: string): Promise<string> => {
    const result = await accounts.handle(request('/api/weave.account', '', { action: 'login', username, password: 'password' }))
    expect(result.status).toBe(200)
    expect(result.headers.get('set-cookie')).toContain('HttpOnly; SameSite=Strict')
    const text = await result.text(); expect(text).not.toContain('signature'); expect(text).not.toContain('HOST_SECRET')
    return result.headers.get('set-cookie')!.split(';')[0]!
  }
  return { accounts, owners, fetcher, sessions, request, login }
}

describe('Workbench platform accounts', () => {
  it('isolates two users sessions, delegated requests, and background work', async () => {
    const f = fixture(); const alice = await f.login('alice'); const bob = await f.login('bob')
    await Promise.all([['alice', alice], ['bob', bob]].map(async ([name, cookie]) => {
      const result = await f.accounts.guard(f.request('/api/session/create', cookie), async () => {
        f.owners.set(name!, f.accounts.currentOwner()!); f.accounts.bindNewSession(name!); return Response.json({ ok: true })
      })
      expect(result.status).toBe(200)
    }))
    for (const [name, cookie, other] of [['alice', alice, 'bob'], ['bob', bob, 'alice']]) {
      const response = await f.accounts.guard(f.request('/api/session/list', cookie), async () => {
        const visible = f.accounts.visibility()
        expect(await visible(name!)).toBe(true); expect(await visible(other!)).toBe(false)
        expect(f.accounts.currentHeaders().get('X-Weave-User-Authorization')).toMatch(new RegExp(`^Bearer ${name}\\.`))
        return Response.json({ ok: true })
      })
      expect(response.status).toBe(200)
      const denied = await f.accounts.guard(f.request('/api/session/prompt', cookie, { params: [{ sessionId: other, content: 'read their task' }] }), async () => { throw new Error('cross-user handler reached') })
      expect(denied.status).toBe(404)
      expect(f.accounts.sessionHeaders(name!).get('X-Weave-User-Authorization')).toMatch(new RegExp(`^Bearer ${name}\\.`))
    }
    await f.accounts.handle(f.request('/api/weave.account', alice, { action: 'logout' }))
    expect(() => f.accounts.sessionHeaders('alice')).toThrow('login_required')
    expect(f.accounts.sessionHeaders('bob').get('X-Weave-User-Authorization')).toMatch(/^Bearer bob\./u)
    expect((await f.accounts.guard(f.request('/api/session/list', alice), async () => Response.json({}))).status).toBe(401)
  })

  it('does not restore credentials after restart or accept caller supplied delegation', async () => {
    const f = fixture(); const cookie = await f.login('alice')
    await f.accounts.requestFetch(f.request('/api/weave.capability-operations', cookie))('http://weave/v1/capability-invocations', { headers: { 'X-Weave-User-Authorization': 'Bearer forged', 'X-Weave-Actor-ID': 'forged' } })
    const headers = new Headers(f.fetcher.mock.calls.at(-1)?.[1]?.headers)
    expect(headers.get('X-Weave-User-Authorization')).toMatch(/^Bearer alice\./u); expect(headers.has('X-Weave-Actor-ID')).toBe(false)
    const restarted = new WorkbenchAccounts('http://weave', 'HOST_SECRET', f.sessions, f.fetcher)
    expect(await (await restarted.handle(f.request('/api/weave.account', cookie))).json()).toEqual({ authenticated: false })
    expect((await restarted.guard(f.request('/api/session/list', cookie), async () => Response.json({}))).status).toBe(401)
  })

  it('keeps shared Host administration outside ordinary accounts and validates all Session address forms', async () => {
    const f = fixture(); const cookie = await f.login('alice')
    const fail = async (): Promise<Response> => { throw new Error('unauthorized handler reached') }
    for (const path of ['settings/describe', 'credentials/set', 'directoryPicker/list', 'workspace/create', 'session/openWorkspacePath']) {
      expect((await f.accounts.guard(f.request('/api/' + path, cookie, {}), fail)).status).toBe(403)
    }
    expect((await f.accounts.guard(f.request('/api/session/create', cookie, { payload: { args: { request: { cwd: '/private/another-user' } } } }), fail)).status).toBe(403)
    for (const selector of [
      { address: { kind: 'session', sessionId: 'bob' } },
      { address: { kind: 'subagent', parentSessionId: 'bob', childSessionId: 'child' } },
      { agentId: 'bob' }, { beforeSessionId: 'bob' },
    ]) {
      expect((await f.accounts.guard(f.request('/api/session/page', cookie, { payload: { args: { request: selector } } }), fail)).status).toBe(404)
    }
  })

  it('accepts a human response only from the account and stream that received the question', async () => {
    const f = fixture(); const alice = await f.login('alice'), bob = await f.login('bob')
    f.owners.set('alice-session', { userId: 'alice', workspaceId: 'shared' })
    const stream = f.request('/api/$events', alice, {})
    await f.accounts.guard(stream, async () => {
      expect(await f.accounts.streamVisible(stream, { type: 'ready', clientId: 'alice-client' })).toBe(true)
      expect(await f.accounts.streamVisible(stream, { type: 'waterfall', eventId: 'question', agentId: 'alice-session' })).toBe(true)
      return Response.json({})
    })
    const body = { payload: { args: { clientId: 'alice-client', eventId: 'question', outcome: { kind: 'result', value: 'approved' } } } }
    expect((await f.accounts.guard(f.request('/api/$events/result', alice, body), async () => Response.json({}))).status).toBe(200)
    expect((await f.accounts.guard(f.request('/api/$events/result', bob, body), async () => { throw new Error('cross-user approval reached') })).status).toBe(404)
    const bobStream = f.request('/api/$events', bob, {})
    await f.accounts.guard(bobStream, async () => {
      expect(await f.accounts.streamVisible(bobStream, { type: 'ready', clientId: 'bob-client' })).toBe(true)
      expect(await f.accounts.streamVisible(bobStream, { type: 'waterfall', eventId: 'question', agentId: 'alice-session' })).toBe(false)
      expect(await f.accounts.streamVisible(bobStream, { type: 'cancel', eventId: 'question' })).toBe(false)
      return Response.json({})
    })
    const forged = { payload: { args: { clientId: 'bob-client', eventId: 'question', outcome: { kind: 'result', value: 'approved' } } } }
    expect((await f.accounts.guard(f.request('/api/$events/result', bob, forged), async () => { throw new Error('undelivered approval reached') })).status).toBe(404)
  })

  it('rejects failed login without installing a session or leaking credentials', async () => {
    const f = fixture()
    const result = await f.accounts.handle(f.request('/api/weave.account', '', { action: 'login', username: 'alice', password: 'wrong' }))
    expect(result.status).toBe(401); expect(result.headers.has('set-cookie')).toBe(false)
    expect(await result.json()).toEqual({ code: 'invalid_credentials' })
  })
})
