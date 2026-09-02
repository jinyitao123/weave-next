import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import type { PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import css from './RuntimeCenter.module.css'

interface RuntimeView {
  readonly id: string
  readonly name: string
  readonly engines: readonly string[]
  readonly healthStatus: 'healthy' | 'busy' | 'degraded' | 'quarantined' | 'offline'
  readonly totalSlots: number
  readonly activeSlots: number
  readonly poolId: string
  readonly enabled: boolean
  readonly online: boolean
  readonly lastHeartbeatAt: string
  readonly createdAt: string
}

interface CreatedRuntime { readonly id: string; readonly name: string; readonly token: string }
type Props = PropsLocale<'weave'>
type HomeProps = PropsRuntime<'conversation.input.dock'> & Props

function object(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined
}

function runtime(value: unknown): RuntimeView | null {
  const item = object(value)
  if (item === undefined || typeof item.id !== 'string' || typeof item.name !== 'string') return null
  const healthStatus = ['healthy', 'busy', 'degraded', 'quarantined', 'offline'].includes(String(item.healthStatus))
    ? item.healthStatus as RuntimeView['healthStatus']
    : 'offline'
  return {
    id: item.id, name: item.name,
    engines: Array.isArray(item.engines) ? item.engines.filter((engine): engine is string => typeof engine === 'string') : [],
    healthStatus,
    totalSlots: typeof item.totalSlots === 'number' ? item.totalSlots : 0,
    activeSlots: typeof item.activeSlots === 'number' ? item.activeSlots : 0,
    poolId: typeof item.poolId === 'string' ? item.poolId : '',
    enabled: item.enabled !== false, online: item.online === true,
    lastHeartbeatAt: typeof item.lastHeartbeatAt === 'string' ? item.lastHeartbeatAt : '',
    createdAt: typeof item.createdAt === 'string' ? item.createdAt : '',
  }
}

function runtimeList(value: unknown): readonly RuntimeView[] | null {
  const items = object(value)?.runtimes
  if (!Array.isArray(items)) return null
  return items.map(runtime).filter((item): item is RuntimeView => item !== null)
}

function engineLabel(engine: string, t: Props['t']): string {
  const normalized = engine.trim().toLowerCase()
  if (normalized === 'codex') return t('runtimeCenter.engine.codex')
  if (normalized === 'claude' || normalized === 'claude-code') return t('runtimeCenter.engine.claude')
  if (normalized === 'opencode') return t('runtimeCenter.engine.opencode')
  if (normalized === 'loom' || normalized === 'toolloop') return t('runtimeCenter.engine.builtin')
  return t('runtimeCenter.engine.other')
}

function runtimeName(value: string, t: Props['t']): string {
  const name = value.trim()
  if (name === '' || /^[0-9a-f]{8}-[0-9a-f-]{27,}$/iu.test(name)) return t('task.runtime.assigned')
  if (!/^[a-z0-9_-]+$/u.test(name) || (!name.includes('-') && !name.includes('_'))) return name
  return name.split(/[-_]+/u).filter(Boolean).map(word => word.charAt(0).toUpperCase() + word.slice(1)).join(' ')
}

function poolName(value: string, t: Props['t']): string {
  const name = value.trim()
  if (name === '') return t('runtimeCenter.pool.none')
  if (/^[0-9a-f]{8}-[0-9a-f-]{27,}$/iu.test(name)) return t('runtimeCenter.pool.joined')
  if (!/^[a-z0-9_-]+$/u.test(name)) return name
  return name.split(/[-_]+/u).filter(Boolean).map(word => word.charAt(0).toUpperCase() + word.slice(1)).join(' ')
}

function timeLabel(value: string, t: Props['t']): string {
  const date = new Date(value)
  if (value === '' || Number.isNaN(date.getTime())) return t('runtimeCenter.neverConnected')
  const elapsed = Math.max(0, Date.now() - date.getTime())
  const minutes = Math.floor(elapsed / 60_000)
  if (minutes < 1) return t('runtimeCenter.justNow')
  if (minutes < 60) return t('runtimeCenter.minutesAgo', { count: minutes })
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return t('runtimeCenter.hoursAgo', { count: hours })
  return t('runtimeCenter.daysAgo', { count: Math.floor(hours / 24) })
}

async function responseError(response: Response, t: Props['t'], fallback: string): Promise<string> {
  try {
    const value = object(await response.json() as unknown)
    const code = typeof value?.code === 'string' ? value.code : ''
    if (code === 'runtime_forbidden') return t('runtimeCenter.error.forbidden')
    if (code === 'runtime_missing') return t('runtimeCenter.error.missing')
    if (code === 'weave_disconnected') return t('runtimeCenter.error.disconnected')
    if (code === 'invalid_input') return t('runtimeCenter.error.input')
    if (code === 'runtime_token_missing') return t('runtimeCenter.error.token')
    if (code === 'weave_unreachable') return t('runtimeCenter.error.unreachable')
    return fallback
  } catch { return fallback }
}

/** Product runtime registry backed by host-authenticated Weave requests. */
export function RuntimeCenter({ t }: Props) {
  const [runtimes, setRuntimes] = useState<readonly RuntimeView[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<CreatedRuntime | null>(null)
  const [name, setName] = useState('')
  const [editing, setEditing] = useState<RuntimeView | null>(null)
  const [editedName, setEditedName] = useState('')
  const [poolId, setPoolId] = useState('')
  const [removing, setRemoving] = useState<RuntimeView | null>(null)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')

  const refresh = useCallback(async () => {
    setLoading(true); setError('')
    try {
      const response = await fetch('/api/weave.runtimes', { headers: { Accept: 'application/json' }, cache: 'no-store' })
      if (!response.ok) throw new Error(await responseError(response, t, t('runtimeCenter.error.load')))
      const list = runtimeList(await response.json() as unknown)
      if (list === null) throw new Error(t('runtimeCenter.error.load'))
      setRuntimes(list)
    } catch (cause) { setError(cause instanceof Error ? cause.message : t('runtimeCenter.error.load')) }
    finally { setLoading(false) }
  }, [t])

  useEffect(() => { void refresh() }, [refresh])

  const mutate = async (method: 'POST' | 'PUT' | 'DELETE', body: object): Promise<Response | null> => {
    setBusy(true); setError(''); setNotice('')
    try {
      const response = await fetch('/api/weave.runtimes', {
        method, headers: { Accept: 'application/json', 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      })
      if (!response.ok) throw new Error(await responseError(response, t, t('runtimeCenter.error.action')))
      return response
    } catch (cause) { setError(cause instanceof Error ? cause.message : t('runtimeCenter.error.action')); return null }
    finally { setBusy(false) }
  }

  const create = async (event: FormEvent) => {
    event.preventDefault()
    if (name.trim() === '') return
    const response = await mutate('POST', { action: 'create', name: name.trim() })
    if (response === null) return
    const value = object(await response.json() as unknown)
    if (typeof value?.id !== 'string' || typeof value.name !== 'string' || typeof value.token !== 'string') {
      setError(t('runtimeCenter.error.token')); return
    }
    setCreated({ id: value.id, name: value.name, token: value.token })
    setCreating(false); setName('')
    await refresh()
  }

  const configure = async (event: FormEvent) => {
    event.preventDefault()
    if (editing === null || editedName.trim() === '') return
    if (await mutate('PUT', { action: 'configure', id: editing.id, name: editedName.trim(), poolId: poolId.trim() }) === null) return
    setEditing(null); setNotice(t('runtimeCenter.saved')); await refresh()
  }

  const remove = async () => {
    if (removing === null) return
    if (await mutate('DELETE', { action: 'delete', id: removing.id }) === null) return
    setRemoving(null); setNotice(t('runtimeCenter.removed')); await refresh()
  }

  const copyToken = async () => {
    if (created === null) return
    try { await navigator.clipboard.writeText(created.token); setNotice(t('runtimeCenter.token.copied')) }
    catch { setNotice(t('runtimeCenter.token.copyFailed')) }
  }

  const available = runtimes.filter(item => item.online && item.enabled && !['offline', 'quarantined'].includes(item.healthStatus)).length
  return (
    <section className={css.center} aria-label={t('runtimeCenter.title')}>
      <div className={css.heading}>
        <div><span>{t('runtimeCenter.eyebrow')}</span><strong>{t('runtimeCenter.title')}</strong><small>{t('runtimeCenter.summary', { available, total: runtimes.length })}</small></div>
        <div className={css.headingActions}>
          <button type="button" onClick={() => { void refresh() }} disabled={loading || busy}>{t('runtimeCenter.refresh')}</button>
          <button type="button" data-primary onClick={() => { setCreating(true); setCreated(null) }}>{t('runtimeCenter.create')}</button>
        </div>
      </div>
      {error === '' ? null : <div className={css.error} role="alert"><span>{error}</span><button type="button" onClick={() => { void refresh() }}>{t('runtimeCenter.retry')}</button></div>}
      {notice === '' ? null : <p className={css.notice} role="status">{notice}</p>}
      {loading && runtimes.length === 0 ? <p className={css.empty}>{t('runtimeCenter.loading')}</p> : runtimes.length === 0 ? <p className={css.empty}>{t('runtimeCenter.empty')}</p> : (
        <div className={css.list}>
          {runtimes.map(item => (
            <details className={css.runtime} key={item.id}>
              <summary>
                <span className={css.status} data-status={item.healthStatus} aria-hidden />
                <span className={css.identity}><strong>{runtimeName(item.name, t)}</strong><small>{item.engines.length === 0 ? t('runtimeCenter.engine.pending') : item.engines.map(engine => engineLabel(engine, t)).join(' · ')}</small></span>
                <span className={css.capacity}>{t('runtimeCenter.capacity', { active: item.activeSlots, total: item.totalSlots })}</span>
                <span className={css.health}>{t(`runtimeCenter.status.${item.healthStatus}` as const)}</span>
              </summary>
              <div className={css.body}>
                <dl><div><dt>{t('runtimeCenter.lastSeen')}</dt><dd>{timeLabel(item.lastHeartbeatAt, t)}</dd></div><div><dt>{t('runtimeCenter.pool')}</dt><dd>{poolName(item.poolId, t)}</dd></div></dl>
                <div className={css.actions}>
                  <button type="button" onClick={() => { setEditing(item); setEditedName(item.name); setPoolId(item.poolId) }}>{t('runtimeCenter.configure')}</button>
                  <button type="button" data-danger onClick={() => { setRemoving(item) }}>{t('runtimeCenter.remove')}</button>
                </div>
              </div>
            </details>
          ))}
        </div>
      )}
      {!creating ? null : (
        <div className={css.dialogBackdrop} role="presentation">
          <form className={css.dialog} role="dialog" aria-modal="true" aria-labelledby="runtime-create-title" onSubmit={(event) => { void create(event) }}>
            <strong id="runtime-create-title">{t('runtimeCenter.create.title')}</strong><p>{t('runtimeCenter.create.notice')}</p>
            <label>{t('runtimeCenter.name')}<input autoFocus value={name} maxLength={120} onChange={(event) => { setName(event.currentTarget.value) }} placeholder={t('runtimeCenter.name.placeholder')} /></label>
            <div className={css.actions}><button type="button" onClick={() => { setCreating(false) }}>{t('task.cancel')}</button><button type="submit" data-primary disabled={busy || name.trim() === ''}>{t('runtimeCenter.create.confirm')}</button></div>
          </form>
        </div>
      )}
      {editing === null ? null : (
        <div className={css.dialogBackdrop} role="presentation">
          <form className={css.dialog} role="dialog" aria-modal="true" aria-labelledby="runtime-edit-title" onSubmit={(event) => { void configure(event) }}>
            <strong id="runtime-edit-title">{t('runtimeCenter.configure.title')}</strong>
            <label>{t('runtimeCenter.name')}<input autoFocus value={editedName} maxLength={120} onChange={(event) => { setEditedName(event.currentTarget.value) }} /></label>
            <label>{t('runtimeCenter.pool')}<input value={poolId} maxLength={80} onChange={(event) => { setPoolId(event.currentTarget.value) }} placeholder={t('runtimeCenter.pool.placeholder')} /><small>{t('runtimeCenter.pool.notice')}</small></label>
            <div className={css.actions}><button type="button" onClick={() => { setEditing(null) }}>{t('task.cancel')}</button><button type="submit" data-primary disabled={busy || editedName.trim() === ''}>{t('runtimeCenter.save')}</button></div>
          </form>
        </div>
      )}
      {removing === null ? null : (
        <div className={css.dialogBackdrop} role="presentation">
          <section className={css.dialog} role="dialog" aria-modal="true" aria-labelledby="runtime-remove-title">
            <strong id="runtime-remove-title">{t('runtimeCenter.remove.title')}</strong><p>{t('runtimeCenter.remove.notice', { name: runtimeName(removing.name, t) })}</p>
            <div className={css.actions}><button type="button" onClick={() => { setRemoving(null) }}>{t('task.cancel')}</button><button type="button" data-danger disabled={busy} onClick={() => { void remove() }}>{t('runtimeCenter.remove.confirm')}</button></div>
          </section>
        </div>
      )}
      {created === null ? null : (
        <div className={css.dialogBackdrop} role="presentation">
          <section className={css.dialog} role="dialog" aria-modal="true" aria-labelledby="runtime-token-title">
            <strong id="runtime-token-title">{t('runtimeCenter.token.title')}</strong><p>{t('runtimeCenter.token.notice')}</p>
            <code className={css.token}>{created.token}</code>
            <details className={css.install}><summary>{t('runtimeCenter.install')}</summary><code>{t('runtimeCenter.install.command', { token: created.token })}</code></details>
            <div className={css.actions}><button type="button" onClick={() => { void copyToken() }}>{t('runtimeCenter.token.copy')}</button><button type="button" data-primary onClick={() => { setCreated(null) }}>{t('runtimeCenter.token.saved')}</button></div>
          </section>
        </div>
      )}
    </section>
  )
}

/** Main-page entry for runtime management while the current Session is blank. */
export function RuntimeHomeEntry({ session, t }: HomeProps) {
  const [open, setOpen] = useState(false)
  useEffect(() => {
    if (!open) return
    const close = (event: KeyboardEvent) => { if (event.key === 'Escape') setOpen(false) }
    window.addEventListener('keydown', close)
    return () => { window.removeEventListener('keydown', close) }
  }, [open])
  if (!session.blank) return null
  return (
    <>
      <button type="button" className={css.homeLauncher} aria-haspopup="dialog" onClick={() => { setOpen(true) }}>
        <span className={css.homeMark} aria-hidden />
        <span><strong>{t('runtimeCenter.title')}</strong><small>{t('runtimeCenter.home.notice')}</small></span>
        <span>{t('runtimeCenter.home.open')}</span>
      </button>
      {!open ? null : (
        <div className={css.homeBackdrop} role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) setOpen(false) }}>
          <section className={css.homePanel} role="dialog" aria-modal="true" aria-label={t('runtimeCenter.title')}>
            <button type="button" className={css.homeClose} autoFocus aria-label={t('runtimeCenter.close')} onClick={() => { setOpen(false) }}>×</button>
            <RuntimeCenter t={t} />
          </section>
        </div>
      )}
    </>
  )
}
