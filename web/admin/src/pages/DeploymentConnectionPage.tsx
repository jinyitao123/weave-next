import { useEffect, useState } from 'react'
import { CopyField, InlineError } from '../components/ui'
import { errorMessage, type AdminSession } from '../lib/api'
import { readOrganizationConnection, saveOrganizationConnection, type OrganizationConnection } from '../lib/organization-connection'

export function DeploymentConnectionPage({ session }: { session: AdminSession }) {
  const administrator = ['admin', 'owner'].includes(session.role)
  const [config, setConfig] = useState<OrganizationConnection>()
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (!administrator) return
    let active = true
    void readOrganizationConnection().then(({ config }) => { if (active) setConfig(config) }).catch(reason => { if (active) setError(errorMessage(reason)) })
    return () => { active = false }
  }, [administrator])
  const download = async () => {
    if (!config || busy) return
    setBusy(true); setError(''); setNotice('')
    try {
      const latest = await readOrganizationConnection()
      if (latest.config.forgeOrigin !== config.forgeOrigin || latest.config.weaveOrigin !== config.weaveOrigin) {
        setConfig(latest.config); setNotice('部署地址已更新，请核对后重新下载。')
      } else saveOrganizationConnection(latest.file)
    } catch (reason) { setConfig(undefined); setError(errorMessage(reason)) }
    finally { setBusy(false) }
  }
  if (!administrator) return <section className="page"><InlineError message="只有管理员可以查看部署连接。" /></section>
  return <section className="page">
    <header className="page__header"><h1>部署连接</h1></header>
    {error ? <InlineError message={error} onRetry={() => { void readOrganizationConnection().then(({ config }) => { setConfig(config); setError('') }).catch(reason => setError(errorMessage(reason))) }} /> : null}
    {notice ? <p className="notice" role="status">{notice}</p> : null}
    {config ? <div className="stack">
      <CopyField label="业务系统地址（Forge）" value={config.forgeOrigin} />
      <CopyField label="团队服务地址（Weave）" value={config.weaveOrigin} />
      <div className="toolbar"><button type="button" className="button button--primary" disabled={busy} onClick={() => void download()}>{busy ? '正在下载…' : '下载组织连接文件'}</button></div>
    </div> : !error ? <p role="status">正在读取部署连接…</p> : null}
  </section>
}
