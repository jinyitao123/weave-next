import { Lock, LockOpen } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { errorMessage, signInWithAPIKey, signInWithForge, type AdminConfig, type AdminSession } from '../lib/api'

export function SignInPage({ config, onSignedIn }: { config: AdminConfig; onSignedIn(session: AdminSession): void }) {
  const forgeAvailable = config.sign_in_methods.includes('forge') && Boolean(config.forge_origin)
  const [method, setMethod] = useState<'forge' | 'api_key'>(forgeAvailable ? 'forge' : 'api_key')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [apiKey, setAPIKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const choose = (next: 'forge' | 'api_key') => {
    setMethod(next)
    setError('')
  }
  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (method === 'forge' ? !email.trim() || !password : !apiKey.trim()) {
      setError(method === 'forge' ? '请输入账号和密码' : '请输入 API Key')
      return
    }
    setBusy(true)
    setError('')
    try {
      onSignedIn(method === 'forge' ? await signInWithForge(config.forge_origin!, email, password) : await signInWithAPIKey(apiKey.trim()))
    } catch (failure) {
      setError(errorMessage(failure))
      setPassword('')
    } finally {
      setBusy(false)
    }
  }

  return <main className="sign-in">
    <section className="sign-in__panel" aria-labelledby="sign-in-title">
      <h1 id="sign-in-title">登录 Weave</h1>
      {forgeAvailable ? <div className="segmented" role="group" aria-label="登录方式">
        <button type="button" aria-pressed={method === 'forge'} onClick={() => choose('forge')}>Forge 账号</button>
        <button type="button" aria-pressed={method === 'api_key'} onClick={() => choose('api_key')}>API Key</button>
      </div> : null}
      <form onSubmit={(event) => void submit(event)} noValidate>
        {method === 'forge' ? <>
          <label className="field"><span>账号</span>
            <input className="input" type="email" autoComplete="username" value={email} onChange={(event) => setEmail(event.target.value)} autoFocus />
          </label>
          <label className="field"><span>密码</span>
            <input className="input" type="password" autoComplete="current-password" value={password} onChange={(event) => setPassword(event.target.value)} />
          </label>
        </> : <label className="field"><span>API Key</span>
          <input className="input" type="password" autoComplete="off" spellCheck={false} value={apiKey} onChange={(event) => setAPIKey(event.target.value)} autoFocus />
        </label>}
        {error ? <p className="alert" role="alert">{error}</p> : null}
        <button type="submit" className="button button--primary" disabled={busy}>{busy ? '正在登录…' : '登录'}</button>
      </form>
      <p className="sign-in__footer">
        {config.secure ? <Lock size={12} aria-hidden="true" /> : <LockOpen size={12} aria-hidden="true" />}
        {config.secure ? 'HTTPS 连接' : 'HTTP 连接'}
      </p>
    </section>
  </main>
}
