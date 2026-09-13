import { useCallback, useEffect, useState } from 'react'
import type { PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import css from './ApplicationCenter.module.css'

type Props = PropsLocale<'weave'> & PropsRuntime<'settings.section'>
type Data = Record<string, unknown>

const objects = (value: unknown): Data[] => Array.isArray(value)
  ? value.filter((item): item is Data => typeof item === 'object' && item !== null && !Array.isArray(item))
  : []
const object = (value: unknown): Data => typeof value === 'object' && value !== null && !Array.isArray(value) ? value as Data : {}

/** Show durable capability runs, decisions, documents, and workspace limits. */
export function CapabilityOperationsSettingsSection({ t }: Props) {
  const [data, setData] = useState<Data>({})
  const [detail, setDetail] = useState<Data>({})
  const [selected, setSelected] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const quota = object(data.quota)
  const [steps, setSteps] = useState(100)
  const [active, setActive] = useState(20)

  const load = useCallback(async () => {
    setBusy(true)
    setError('')
    try {
      const response = await fetch('/api/weave.capability-operations', { cache: 'no-store' })
      const value = await response.json() as Data
      if (!response.ok) throw new Error()
      setData(value)
      const nextQuota = object(value.quota)
      if (typeof nextQuota.max_steps_per_invocation === 'number') setSteps(nextQuota.max_steps_per_invocation)
      if (typeof nextQuota.max_active_invocations === 'number') setActive(nextQuota.max_active_invocations)
    } catch {
      setError(t('capabilityOps.loadError'))
    } finally {
      setBusy(false)
    }
  }, [t])

  useEffect(() => { void load() }, [load])

  const mutate = async (body: Data) => {
    setBusy(true)
    setError('')
    try {
      const response = await fetch('/api/weave.capability-operations', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      })
      if (!response.ok) throw new Error()
      setSelected('')
      setDetail({})
      await load()
    } catch {
      setError(t('capabilityOps.actionError'))
      setBusy(false)
    }
  }

  const inspect = async (invocationId: string) => {
    if (selected === invocationId) {
      setSelected('')
      setDetail({})
      return
    }
    setBusy(true)
    setError('')
    try {
      const response = await fetch(`/api/weave.capability-operations?invocation_id=${encodeURIComponent(invocationId)}`, { cache: 'no-store' })
      const value = await response.json() as Data
      if (!response.ok) throw new Error()
      setSelected(invocationId)
      setDetail(value)
    } catch {
      setError(t('capabilityOps.loadError'))
    } finally {
      setBusy(false)
    }
  }

  const rows = objects(data.invocations)
  const statusLabel = (status: unknown) => t(`capabilityOps.status.${String(status)}` as 'capabilityOps.status.running')
  const events = objects(detail.events)
  const human = object(detail.human_task)

  return <section className={css.center}>
    <h3>{t('capabilityOps.title')}</h3>
    <p>{t('capabilityOps.description')}</p>
    {error !== '' && <p role="alert">{error}</p>}
    <h4>{t('capabilityOps.quota')}</h4>
    <div className={css.actions}>
      <label>{t('capabilityOps.steps')}<input type="number" min={1} max={1000} value={steps} disabled={busy} onChange={event => setSteps(Number(event.target.value))}/></label>
      <label>{t('capabilityOps.active')}<input type="number" min={1} max={1000} value={active} disabled={busy} onChange={event => setActive(Number(event.target.value))}/></label>
      <button disabled={busy || steps < 1 || active < 1} onClick={() => void mutate({ action: 'quota', max_steps_per_invocation: steps, max_active_invocations: active })}>{t('capabilityOps.save')}</button>
      <button disabled={busy} onClick={() => void load()}>{t('capabilityOps.refresh')}</button>
    </div>
    <h4>{t('capabilityOps.history')}</h4>
    {rows.length === 0 ? <p>{t('capabilityOps.empty')}</p> : rows.map((run) => {
      const invocationId = String(run.invocation_id)
      const canStop = ['running', 'queued', 'waiting'].includes(String(run.status))
      const isSelected = selected === invocationId
      return <article className={css.row} key={invocationId}>
        <div>
          <strong>{String(run.capability_name || run.capability_id)}</strong>
          <small>{statusLabel(run.status)} · {t('capabilityOps.usage', { used: Number(run.used_steps || 0), total: Number(run.max_steps || quota.max_steps_per_invocation || 0) })}</small>
          {isSelected && <div>
            {human.status === 'waiting' && <div className={css.actions}>
              <strong>{String(human.title || t('capabilityOps.decision'))}</strong>
              <button disabled={busy} onClick={() => void mutate({ action: 'resume', invocation_id: invocationId, step_id: human.step_id, approved: true })}>{t('capabilityOps.approve')}</button>
              <button disabled={busy} onClick={() => void mutate({ action: 'resume', invocation_id: invocationId, step_id: human.step_id, approved: false })}>{t('capabilityOps.reject')}</button>
            </div>}
            <small>{t('capabilityOps.progress', { count: events.length })}</small>
            {run.status === 'completed' && <a href={`/api/weave.capability-operations?invocation_id=${encodeURIComponent(invocationId)}&download=1`}>{t('capabilityOps.download')}</a>}
          </div>}
        </div>
        <div className={css.actions}>
          <button disabled={busy} onClick={() => void inspect(invocationId)}>{t(isSelected ? 'capabilityOps.hide' : 'capabilityOps.view')}</button>
          {canStop && <button disabled={busy} onClick={() => void mutate({ action: 'cancel', invocation_id: invocationId })}>{t('capabilityOps.stop')}</button>}
        </div>
      </article>
    })}
  </section>
}
