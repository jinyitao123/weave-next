import { useCallback, useEffect, useState } from 'react'
import { Badge, CopyField, InlineError } from '../components/ui'
import { ApiError, errorMessage, type AdminSession } from '../lib/api'
import { checkFeishuApp, checkReasons, deleteFeishuApp, readFeishuApp, saveFeishuApp, type FeishuApp, type FeishuAppInput } from '../lib/feishu'
import { relativeTime } from '../lib/format'
import '../components/feishu/feishu.css'

const canAdminister = (session: AdminSession) => ['admin', 'owner'].includes(session.role)
const sources = { workspace: '本工作区应用', deployment: '部署环境应用', none: '未配置' } as const

export function IntegrationsPage({ session }: { session: AdminSession }) {
  const [app, setApp] = useState<FeishuApp>()
  const [error, setError] = useState('')
  const load = useCallback(async () => {
    try {
      setApp(await readFeishuApp())
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [])
  useEffect(() => { void load() }, [load])
  return <section className="page">
    <header className="page__header"><h1>集成</h1></header>
    {error ? <InlineError message={error} onRetry={() => void load()} /> : null}
    {app ? <FeishuAppPanel key={`${app.source}-${app.revision}`} app={app} editable={canAdminister(session)} onChanged={setApp} /> : null}
  </section>
}

function FeishuAppPanel({ app, editable, onChanged }: { app: FeishuApp; editable: boolean; onChanged(app: FeishuApp): void }) {
  const stored = app.source === 'workspace'
  const [appId, setAppId] = useState(stored ? app.appId ?? '' : '')
  const [tenantKey, setTenantKey] = useState(stored ? app.tenantKey ?? '' : '')
  const [secrets, setSecrets] = useState({ appSecret: '', verificationToken: '', encryptKey: '' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [checked, setChecked] = useState<{ ok: boolean; text: string }>()
  const [confirmDelete, setConfirmDelete] = useState(false)
  const needsSecrets = !stored || appId.trim() !== app.appId
  const run = async (action: () => Promise<void>) => {
    setBusy(true)
    setError('')
    try {
      await action()
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const save = () => run(async () => {
    if (!appId.trim() || !tenantKey.trim()) throw new ApiError(400, '请填写应用 ID 和租户标识')
    if (needsSecrets && Object.values(secrets).some((value) => !value.trim())) throw new ApiError(422, '首次保存或更换应用时需要填写全部三项密钥')
    const input: FeishuAppInput = { expectedRevision: stored ? app.revision : 0, appId: appId.trim(), tenantKey: tenantKey.trim() }
    for (const key of ['appSecret', 'verificationToken', 'encryptKey'] as const) if (secrets[key].trim()) input[key] = secrets[key].trim()
    onChanged(await saveFeishuApp(input))
  })
  const check = () => run(async () => {
    try {
      const result = await checkFeishuApp()
      setChecked({ ok: result.ok, text: result.ok ? '连接正常' : checkReasons[result.reason ?? ''] ?? '检查未通过' })
    } catch (failure) {
      if (failure instanceof ApiError && failure.status === 503 && failure.code === 'http_503') setChecked({ ok: false, text: checkReasons.unreachable })
      else throw failure
    }
    onChanged(await readFeishuApp())
  })
  const remove = () => run(async () => { onChanged(await deleteFeishuApp(app.revision)) })
  const secretField = (key: keyof typeof secrets, label: string) => <label className="field"><span>{label}</span>
    <input className="input mono" type="password" autoComplete="new-password" spellCheck={false} disabled={!editable} value={secrets[key]}
      placeholder={needsSecrets ? '必填' : '已保存，留空保持不变'} onChange={(event) => { setSecrets((current) => ({ ...current, [key]: event.target.value })); setError('') }} /></label>

  return <section className="integration" aria-label="飞书">
    <header className="integration__header"><h2>飞书</h2><Badge tone={stored ? 'success' : app.source === 'deployment' ? 'accent' : 'neutral'}>{sources[app.source]}</Badge></header>
    {app.source === 'deployment' ? <p className="muted small">正在使用部署环境的应用 {app.appId}。保存本工作区的应用后，员工需要重新绑定。</p> : null}
    <dl className="integration__facts">
      <div><dt>已绑定员工</dt><dd>{app.boundEmployees} 人</dd></div>
      {app.lastCheck ? <div><dt>最近检查</dt><dd>{app.lastCheck.ok ? '连接正常' : checkReasons[app.lastCheck.reason ?? ''] ?? '检查未通过'} · {relativeTime(app.lastCheck.at)}</dd></div> : null}
      {stored && app.updatedAt ? <div><dt>最近修改</dt><dd>{app.updatedBy || '管理员'} · {relativeTime(app.updatedAt)}</dd></div> : null}
    </dl>
    {app.callbackPath ? <CopyField label="事件回调地址" value={`${window.location.origin}${app.callbackPath}`} /> : null}
    <form className="stack" onSubmit={(event) => { event.preventDefault(); void save() }}>
      <div className="integration__grid">
        <label className="field"><span>应用 ID</span><input className="input mono" spellCheck={false} disabled={!editable} value={appId} placeholder="cli_" onChange={(event) => { setAppId(event.target.value); setError('') }} /></label>
        <label className="field"><span>租户标识</span><input className="input mono" spellCheck={false} disabled={!editable} value={tenantKey} onChange={(event) => { setTenantKey(event.target.value); setError('') }} /></label>
      </div>
      <div className="integration__grid">
        {secretField('appSecret', 'App Secret')}
        {secretField('verificationToken', 'Verification Token')}
        {secretField('encryptKey', 'Encrypt Key')}
      </div>
      {error ? <InlineError message={error} /> : null}
      {checked ? <p className={checked.ok ? 'integration__ok' : 'integration__failed'} role="status">{checked.text}</p> : null}
      {editable ? <div className="toolbar">
        <button type="submit" className="button button--primary" disabled={busy}>{busy ? '正在处理…' : '保存'}</button>
        {stored ? <button type="button" className="button" disabled={busy} onClick={() => void check()}>检查连接</button> : null}
        {stored ? <button type="button" className="button button--danger" disabled={busy} onClick={() => setConfirmDelete(true)}>删除</button> : null}
      </div> : null}
      {confirmDelete ? <div className="confirm" role="alertdialog" aria-label="确认删除飞书应用">
        <p>删除后本工作区的员工绑定会失效，已受理的工作继续运行。</p>
        <div className="toolbar"><button type="button" className="button" onClick={() => setConfirmDelete(false)}>取消</button><button type="button" className="button button--danger" disabled={busy} onClick={() => void remove()}>删除</button></div>
      </div> : null}
    </form>
  </section>
}
