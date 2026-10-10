import { RefreshCw } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Dialog, InlineError } from '../ui'
import { errorMessage, readConfig } from '../../lib/api'
import { relativeTime } from '../../lib/format'
import { refreshBusinessCatalog, type BusinessCatalog } from '../../lib/teams'

// Reads the business action catalog again. The console keeps no Forge
// credential, so the signed-in person enters the Forge password once more; it
// is sent to Forge only.
export function CatalogRefresh({ catalog, onRefreshed }: { catalog?: BusinessCatalog; onRefreshed(catalog: BusinessCatalog): void }) {
  const [open, setOpen] = useState(false)
  const [forgeOrigin, setForgeOrigin] = useState<string>()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => {
    if (open && forgeOrigin === undefined) void readConfig().then((config) => setForgeOrigin(config.forge_origin ?? '')).catch(() => setForgeOrigin(''))
  }, [open, forgeOrigin])
  const close = () => { setOpen(false); setPassword(''); setError('') }
  const refresh = async () => {
    if (!forgeOrigin || !email.trim() || !password) { setError('请填写 Forge 账号和密码'); return }
    setBusy(true)
    setError('')
    try {
      onRefreshed(await refreshBusinessCatalog(forgeOrigin, email, password))
      close()
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  return <>
    <button type="button" className="button" onClick={() => setOpen(true)}><RefreshCw size={14} />刷新目录</button>
    {catalog?.available && catalog.fetchedAt ? <span className="muted small">目录读取于 {relativeTime(catalog.fetchedAt)}{catalog.fetchedBy ? ` · ${catalog.fetchedBy}` : ''}</span> : null}
    {open ? <Dialog title="刷新业务动作目录" onClose={close} footer={forgeOrigin ? <>
      <button type="button" className="button" onClick={close}>取消</button>
      <button type="button" className="button button--primary" disabled={busy} onClick={() => void refresh()}>{busy ? '正在读取…' : '验证并刷新'}</button>
    </> : undefined}>
      {forgeOrigin === '' ? <p>当前登录方式没有连接 Forge，不能刷新目录。</p> : <form className="stack" onSubmit={(event) => { event.preventDefault(); void refresh() }}>
        <p className="muted small">用当前登录的 Forge 账号再验证一次，读取最新的业务动作。密码只发给 Forge。</p>
        <label className="field"><span>Forge 账号</span><input className="input" type="email" autoComplete="username" value={email} onChange={(event) => { setEmail(event.target.value); setError('') }} autoFocus /></label>
        <label className="field"><span>密码</span><input className="input" type="password" autoComplete="current-password" value={password} onChange={(event) => { setPassword(event.target.value); setError('') }} /></label>
        {error ? <InlineError message={error} /> : null}
      </form>}
    </Dialog> : null}
  </>
}
