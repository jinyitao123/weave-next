import { Lock, LockOpen } from 'lucide-react'
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { ForgeApprovalPending, approvalCancelled, useForgeApproval } from '../components/ForgeApproval'
import { errorMessage, signInWithAPIKey, signInWithForge, signOut, type AdminConfig, type AdminSession } from '../lib/api'

type Method = 'forge' | 'api_key'

export function SignInPage({ config, onSignedIn }: { config: AdminConfig; onSignedIn(session: AdminSession): void }) {
  const forgeAvailable = config.sign_in_methods.includes('forge') && Boolean(config.forge_origin)
  const keyAvailable = config.sign_in_methods.includes('api_key')
  // An account is the usual way in; the key is offered beside it, and alone
  // where no account system is connected.
  const [method, setMethod] = useState<Method>(() => keyAvailable && (!forgeAvailable || new URLSearchParams(window.location.search).get('login') === 'api_key') ? 'api_key' : 'forge')
  const available = method === 'forge' ? forgeAvailable : keyAvailable
  const [apiKey, setAPIKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const { pending, approve, cancel } = useForgeApproval(config.forge_origin)
  // Choosing the key puts the cursor in its field, so typing can start at once.
  const first = useRef<HTMLInputElement>(null)
  useEffect(() => { first.current?.focus() }, [method])

  const choose = (next: Method) => {
    cancel()
    setMethod(next)
    setError('')
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (!available || busy) return
    if (method === 'api_key' && !apiKey.trim()) {
      setError('请输入认证密钥')
      return
    }
    setBusy(true)
    setError('')
    try {
      // An account is confirmed on Forge's own page; no password is typed here.
      const session = method === 'forge' ? await approve(signInWithForge) : await signInWithAPIKey(apiKey.trim())
      if (session.role === 'member') {
        await signOut().catch(() => undefined)
        setError('当前账号没有管理端权限，请使用 Forge 管理员或开发者账号。')
        return
      }
      onSignedIn(session)
    } catch (failure) {
      if (!approvalCancelled(failure)) setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }

  return <main className="sign-in">
    <section className="sign-in__panel" aria-labelledby="sign-in-title">
      <h1 id="sign-in-title">登录 Weave</h1>
      {forgeAvailable && keyAvailable ? <div className="segmented" role="group" aria-label="登录方式">
        <button type="button" aria-pressed={method === 'forge'} onClick={() => choose('forge')}>账号登录</button>
        <button type="button" aria-pressed={method === 'api_key'} onClick={() => choose('api_key')}>密钥登录</button>
      </div> : null}
      {!available ? <p className="alert" role="alert">登录尚未配置，请联系管理员。</p> : <form onSubmit={(event) => void submit(event)} noValidate>
        {method === 'api_key' ? <>
          <label className="field"><span>认证密钥</span>
            <input ref={first} className="input" type="password" autoComplete="off" spellCheck={false} value={apiKey} onChange={(event) => setAPIKey(event.target.value)} />
          </label>
          {config.default_key ? <p className="muted small">部署时没有设置密钥，默认密钥是 admin。</p> : null}
        </> : null}
        {error ? <p className="alert" role="alert">{error}</p> : null}
        {pending ? <ForgeApprovalPending pending={pending} onCancel={cancel} />
          : <button type="submit" className="button button--primary" disabled={busy}>{busy ? '正在登录…' : method === 'forge' ? '用 Forge 账号登录' : '登录'}</button>}
      </form>}
      <p className="sign-in__footer">
        {config.secure ? <Lock size={12} aria-hidden="true" /> : <LockOpen size={12} aria-hidden="true" />}
        {config.secure ? 'HTTPS 连接' : 'HTTP 连接'}
      </p>
    </section>
  </main>
}
