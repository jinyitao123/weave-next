import { useCallback, useEffect, useState } from 'react'
import { ApiError, errorMessage, signOut, type AdminConfig } from '../lib/api'
import { answerConnect, describeConnect, readConnectRequest, type ConnectDescription } from '../lib/mcp'
import { SignInPage } from './SignInPage'

type State =
  | { kind: 'loading' }
  | { kind: 'sign-in' }
  | { kind: 'ask'; description: ConnectDescription; busy?: boolean; error?: string }
  | { kind: 'invalid' }
  | { kind: 'failed'; message: string }
  | { kind: 'done'; allowed: boolean }

// An MCP client sent the person's browser here. Whoever is signed in — any
// account, not only those the console is open to — says whether that client
// may use Weave as them.
export function ConnectPage({ config }: { config: AdminConfig }) {
  // The request is fixed by the address this page was opened with.
  const [request] = useState(() => readConnectRequest(window.location.search))
  const [state, setState] = useState<State>({ kind: 'loading' })
  const load = useCallback(async () => {
    if (!request) { setState({ kind: 'invalid' }); return }
    try {
      setState({ kind: 'ask', description: await describeConnect(request) })
    } catch (failure) {
      const status = failure instanceof ApiError ? failure.status : 0
      setState(status === 401 ? { kind: 'sign-in' } : status === 400 ? { kind: 'invalid' } : { kind: 'failed', message: errorMessage(failure) })
    }
  }, [request])
  useEffect(() => { void load() }, [load])

  if (state.kind === 'loading') return null
  if (state.kind === 'sign-in') return <SignInPage config={config} anyRole onSignedIn={() => void load()} />

  const answer = async (allow: boolean) => {
    if (!request || state.kind !== 'ask') return
    setState({ ...state, busy: true, error: undefined })
    try {
      const next = await answerConnect(request, allow)
      setState({ kind: 'done', allowed: allow })
      window.location.assign(next)
    } catch (failure) {
      setState({ ...state, busy: false, error: errorMessage(failure) })
    }
  }
  const switchAccount = async () => {
    await signOut().catch(() => undefined)
    setState({ kind: 'sign-in' })
  }

  return <main className="sign-in"><section className="sign-in__panel" aria-labelledby="connect-title">
    {state.kind === 'invalid' ? <>
      <h1 id="connect-title">接入请求无效</h1>
      <p className="muted">请回到客户端重新发起。</p>
    </> : state.kind === 'failed' ? <>
      <h1 id="connect-title">暂时无法处理接入请求</h1>
      <p className="alert" role="alert">{state.message}</p>
      <button type="button" className="button" onClick={() => void load()}>重试</button>
    </> : state.kind === 'done' ? <>
      <h1 id="connect-title">{state.allowed ? '已允许接入' : '已拒绝接入'}</h1>
      <p className="muted">可以回到客户端了。</p>
    </> : <>
      <h1 id="connect-title">允许接入 Weave</h1>
      <p>「{state.description.client_name}」请求以你的账号使用 Weave。</p>
      <dl className="connect__facts">
        <div><dt>账号</dt><dd>{state.description.account_name}</dd></div>
        <div><dt>确认后回到</dt><dd>{['127.0.0.1', 'localhost', '::1'].includes(state.description.returns_to) ? '这台电脑上的客户端' : state.description.returns_to}</dd></div>
        <div><dt>有效期</dt><dd>两周，可随时在管理端收回</dd></div>
      </dl>
      {state.error ? <p className="alert" role="alert">{state.error}</p> : null}
      <div className="connect__actions">
        <button type="button" className="button button--primary" disabled={state.busy} onClick={() => void answer(true)}>允许</button>
        <button type="button" className="button" disabled={state.busy} onClick={() => void answer(false)}>拒绝</button>
        <button type="button" className="button" disabled={state.busy} onClick={() => void switchAccount()}>换一个账号</button>
      </div>
    </>}
  </section></main>
}
