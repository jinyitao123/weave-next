import { RefreshCw } from 'lucide-react'
import { useEffect, useState } from 'react'
import { ForgeApprovalPending, approvalCancelled, useForgeApproval } from '../ForgeApproval'
import { Dialog, InlineError } from '../ui'
import { errorMessage, readConfig } from '../../lib/api'
import { relativeTime } from '../../lib/format'
import { refreshBusinessCatalog, type BusinessCatalog } from '../../lib/teams'

// Reads the business action catalog again. The console keeps no Forge
// credential, so the signed-in person approves once more on Forge.
export function CatalogRefresh({ catalog, onRefreshed }: { catalog?: BusinessCatalog; onRefreshed(catalog: BusinessCatalog): void }) {
  const [open, setOpen] = useState(false)
  const [forgeOrigin, setForgeOrigin] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const { pending, approve, cancel } = useForgeApproval(forgeOrigin || undefined)
  useEffect(() => {
    if (open && forgeOrigin === undefined) void readConfig().then((config) => setForgeOrigin(config.forge_origin ?? '')).catch(() => setForgeOrigin(''))
  }, [open, forgeOrigin])
  const close = () => { cancel(); setOpen(false); setError('') }
  const refresh = async () => {
    if (!forgeOrigin || busy) return
    setBusy(true)
    setError('')
    try {
      onRefreshed(await approve(refreshBusinessCatalog))
      setOpen(false)
    } catch (failure) {
      if (!approvalCancelled(failure)) setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  return <>
    <button type="button" className="button" onClick={() => setOpen(true)}><RefreshCw size={14} />刷新目录</button>
    {catalog?.available && catalog.fetchedAt ? <span className="muted small">目录读取于 {relativeTime(catalog.fetchedAt)}{catalog.fetchedBy ? ` · ${catalog.fetchedBy}` : ''}</span> : null}
    {open ? <Dialog title="刷新业务动作目录" onClose={close} footer={forgeOrigin && !pending ? <>
      <button type="button" className="button" onClick={close}>取消</button>
      <button type="button" className="button button--primary" disabled={busy} onClick={() => void refresh()}>{busy ? '正在读取…' : '确认并刷新'}</button>
    </> : undefined}>
      {forgeOrigin === '' ? <p>当前登录方式没有连接 Forge，不能刷新目录。</p> : <div className="stack">
        {pending ? <ForgeApprovalPending pending={pending} onCancel={cancel} /> : <p className="muted small">用当前登录的 Forge 账号在 Forge 页面再确认一次，读取最新的业务动作。</p>}
        {error ? <InlineError message={error} /> : null}
      </div>}
    </Dialog> : null}
  </>
}
