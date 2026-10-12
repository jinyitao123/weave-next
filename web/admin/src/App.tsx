import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, errorMessage, readConfig, readSession, signInWithForge, signOut, type AdminConfig, type AdminSession } from './lib/api'
import { embedded, withForgePageApproval } from './lib/forge-embed'
import { currentPath } from './lib/router'
import { ConnectPage } from './pages/ConnectPage'
import { Shell } from './pages/Shell'
import { SignInPage } from './pages/SignInPage'

type State =
  | { kind: 'loading' }
  | { kind: 'unavailable' }
  | { kind: 'signed-out'; config: AdminConfig; error?: string }
  | { kind: 'connecting'; config: AdminConfig }
  // Signed in, but as an account the console is not open to.
  | { kind: 'refused'; config: AdminConfig; leaving?: 'busy' | 'failed' }
  | { kind: 'signed-in'; config: AdminConfig; session: AdminSession }

export function App() {
  // An MCP client's sign-in lands here and is answered by whoever signs in, whatever their role.
  const [connecting] = useState(() => currentPath() === '/connect')
  const [state, setState] = useState<State>({ kind: 'loading' })
  const loading = useRef<AbortController | undefined>(undefined)
  const load = useCallback(async () => {
    loading.current?.abort()
    const controller = new AbortController()
    loading.current = controller
    const { signal } = controller
    const update = (next: State) => { if (!signal.aborted) setState(next) }
    let config: AdminConfig
    try {
      config = await readConfig(signal)
    } catch {
      update({ kind: 'unavailable' })
      return
    }
    if (signal.aborted) return
    if (connecting) {
      update({ kind: 'connecting', config })
      return
    }
    // Shown inside the Forge it signs in with, the console enters as whoever is signed in there,
    // whatever session it held before. A failed approval must never revive that previous identity.
    if (embedded() && config.forge_origin && config.sign_in_methods.includes('forge')) {
      try {
        await signOut(signal)
      } catch {
        update({ kind: 'unavailable' })
        return
      }
      if (signal.aborted) return
      try {
        const session = await withForgePageApproval(config.forge_origin, signal, (token) => signInWithForge(token, signal))
        update(session.role === 'member' ? { kind: 'refused', config } : { kind: 'signed-in', config, session })
      } catch (failure) {
        update({ kind: 'signed-out', config, error: errorMessage(failure) })
      }
      return
    }
    try {
      update({ kind: 'signed-in', config, session: await readSession(signal) })
    } catch (error) {
      const status = error instanceof ApiError ? error.status : 0
      // Not signed in and not allowed in are answers; only the rest means Weave could not be reached.
      update(status === 401 ? { kind: 'signed-out', config } : status === 403 ? { kind: 'refused', config } : { kind: 'unavailable' })
    }
  }, [connecting])
  useEffect(() => { void load(); return () => loading.current?.abort() }, [load])

  if (state.kind === 'loading') return null
  if (state.kind === 'unavailable') {
    return <main className="sign-in"><section className="sign-in__panel" role="alert">
      <h1>Weave 暂时无法访问</h1>
      <button type="button" className="button" onClick={() => void load()}>重试</button>
    </section></main>
  }
  if (state.kind === 'connecting') return <ConnectPage config={state.config} />
  if (state.kind === 'refused') {
    const { config } = state
    // Leaving ends this session on the server; the sign-in page then takes another account.
    const leave = async () => {
      setState({ kind: 'refused', config, leaving: 'busy' })
      try {
        await signOut()
        setState({ kind: 'signed-out', config })
      } catch {
        setState({ kind: 'refused', config, leaving: 'failed' })
      }
    }
    return <main className="sign-in"><section className="sign-in__panel">
      <h1>当前账号不能进入管理端</h1>
      <p className="muted">管理端只对管理员和开发者开放。</p>
      {state.leaving === 'failed' ? <p className="alert" role="alert">退出没有完成，请重试。</p> : null}
      <button type="button" className="button button--primary" disabled={state.leaving === 'busy'} onClick={() => void leave()}>{state.leaving === 'busy' ? '正在退出…' : '退出并重新登录'}</button>
    </section></main>
  }
  if (state.kind === 'signed-out') {
    return <SignInPage config={state.config} initialError={state.error} onSignedIn={(session) => setState({ kind: 'signed-in', config: state.config, session })} />
  }
  return <Shell session={state.session} onSignOut={async () => {
    await signOut().catch(() => undefined)
    setState({ kind: 'signed-out', config: state.config })
  }} />
}
