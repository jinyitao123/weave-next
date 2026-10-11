import { useCallback, useEffect, useState } from 'react'
import { ApiError, readConfig, readSession, signOut, type AdminConfig, type AdminSession } from './lib/api'
import { Shell } from './pages/Shell'
import { SignInPage } from './pages/SignInPage'

type State =
  | { kind: 'loading' }
  | { kind: 'unavailable' }
  | { kind: 'signed-out'; config: AdminConfig }
  // Signed in, but as an account the console is not open to.
  | { kind: 'refused'; config: AdminConfig; leaving?: 'busy' | 'failed' }
  | { kind: 'signed-in'; config: AdminConfig; session: AdminSession }

export function App() {
  const [state, setState] = useState<State>({ kind: 'loading' })
  const load = useCallback(async () => {
    let config: AdminConfig
    try {
      config = await readConfig()
    } catch {
      setState({ kind: 'unavailable' })
      return
    }
    try {
      setState({ kind: 'signed-in', config, session: await readSession() })
    } catch (error) {
      const status = error instanceof ApiError ? error.status : 0
      // Not signed in and not allowed in are answers; only the rest means Weave could not be reached.
      setState(status === 401 ? { kind: 'signed-out', config } : status === 403 ? { kind: 'refused', config } : { kind: 'unavailable' })
    }
  }, [])
  useEffect(() => { void load() }, [load])

  if (state.kind === 'loading') return null
  if (state.kind === 'unavailable') {
    return <main className="sign-in"><section className="sign-in__panel" role="alert">
      <h1>Weave 暂时无法访问</h1>
      <button type="button" className="button" onClick={() => void load()}>重试</button>
    </section></main>
  }
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
    return <SignInPage config={state.config} onSignedIn={(session) => setState({ kind: 'signed-in', config: state.config, session })} />
  }
  return <Shell session={state.session} onSignOut={async () => {
    await signOut().catch(() => undefined)
    setState({ kind: 'signed-out', config: state.config })
  }} />
}
