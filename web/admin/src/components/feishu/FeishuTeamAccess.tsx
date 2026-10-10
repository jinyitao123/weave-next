import { useCallback, useEffect, useState } from 'react'
import { InlineError, Select, Switch } from '../ui'
import { errorMessage } from '../../lib/api'
import { notifyKinds, readTeamFeishuAccess, saveTeamFeishuAccess, type FeishuTeamAccess } from '../../lib/feishu'
import { relativeTime } from '../../lib/format'
import './feishu.css'

// Team access is saved on its own revision, apart from the team draft.
export function FeishuTeamAccessPanel({ teamId, workflows }: { teamId: string; workflows: Array<{ id: string; name: string }> }) {
  const [saved, setSaved] = useState<FeishuTeamAccess>()
  const [access, setAccess] = useState<FeishuTeamAccess>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const load = useCallback(async () => {
    try {
      const current = await readTeamFeishuAccess(teamId)
      setSaved(current)
      setAccess(current)
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [teamId])
  useEffect(() => { void load() }, [load])
  if (!access || !saved) return error ? <InlineError message={error} onRetry={() => void load()} /> : null
  const dirty = JSON.stringify(access) !== JSON.stringify(saved)
  const save = async () => {
    setBusy(true)
    setError('')
    try {
      const next = await saveTeamFeishuAccess(teamId, access)
      setSaved(next)
      setAccess(next)
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const workflowOptions = [{ value: '', label: '团队默认流程' }, ...workflows.map((flow) => ({ value: flow.id, label: flow.name || '未命名流程' }))]
  return <section className="integration" aria-label="飞书接入">
    <header className="integration__header"><h2>飞书接入</h2></header>
    <Switch checked={access.enabled} label="可在飞书中使用" onChange={(enabled) => setAccess({ ...access, enabled })} />
    <div className="field"><span>飞书发起时使用的流程</span><Select label="飞书发起时使用的流程" value={access.workflowId ?? ''} options={workflowOptions}
      onChange={(value) => setAccess({ ...access, workflowId: value || null })} /></div>
    <div className="field"><span>推送的通知</span><div className="integration__notify" role="group" aria-label="推送的通知">
      {notifyKinds.map((kind) => <Switch key={kind.key} checked={access.notify[kind.key]} label={kind.label} onChange={(on) => setAccess({ ...access, notify: { ...access.notify, [kind.key]: on } })} />)}
    </div></div>
    {saved.updatedAt ? <p className="muted small">{saved.updatedBy || '开发者'} · {relativeTime(saved.updatedAt)}</p> : null}
    {error ? <InlineError message={error} /> : null}
    <div className="toolbar">
      <button type="button" className="button button--primary" disabled={busy || !dirty} onClick={() => void save()}>{busy ? '正在保存…' : '保存'}</button>
      {dirty ? <button type="button" className="button" disabled={busy} onClick={() => { setAccess(saved); setError('') }}>放弃修改</button> : null}
    </div>
  </section>
}
