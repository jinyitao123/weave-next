import { useCallback, useEffect, useState } from 'react'
import { ApiError, readConfig, readSession, signOut, type AdminConfig, type AdminSession } from './lib/api'
import { Shell } from './pages/Shell'
import { SignInPage } from './pages/SignInPage'

type State =
  | { kind: 'loading' }
  | { kind: 'unavailable' }
  | { kind: 'signed-out'; config: AdminConfig }
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
      setState(error instanceof ApiError && error.status === 401 ? { kind: 'signed-out', config } : { kind: 'unavailable' })
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
  if (state.kind === 'signed-out') {
    return <SignInPage config={state.config} onSignedIn={(session) => setState({ kind: 'signed-in', config: state.config, session })} />
  }
  return <Shell config={state.config} session={state.session} onSignOut={async () => {
    await signOut().catch(() => undefined)
    setState({ kind: 'signed-out', config: state.config })
  }} />
}
