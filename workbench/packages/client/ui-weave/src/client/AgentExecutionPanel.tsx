import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import css from './RuntimeCenter.module.css'

interface Member {
  readonly name: string
  readonly displayName: string
  readonly version: number
  readonly engine: string
  readonly runtimeId: string
  readonly model: string
  readonly fallbackModels: readonly string[]
  readonly fallbackRetries: number
}
interface RuntimeChoice { readonly id: string; readonly name: string; readonly engines: readonly string[]; readonly enabled: boolean }
type Props = PropsLocale<'weave'> & { readonly runtimes: readonly RuntimeChoice[] }
function object(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? value as Record<string, unknown> : undefined
}
function member(value: unknown): Member | null {
  const item = object(value)
  if (item === undefined || typeof item.name !== 'string' || typeof item.version !== 'number' || typeof item.displayName !== 'string' || typeof item.engine !== 'string' || typeof item.runtimeId !== 'string' || typeof item.model !== 'string') return null
  return { name: item.name, displayName: item.displayName, version: item.version, engine: item.engine,
    runtimeId: item.runtimeId, model: item.model,
    fallbackModels: Array.isArray(item.fallbackModels) ? item.fallbackModels.filter((value): value is string => typeof value === 'string') : [],
    fallbackRetries: typeof item.fallbackRetries === 'number' ? item.fallbackRetries : 0 }
}

/** Settings editor for future member executions; Weave atomically publishes affected workflows. */
export function AgentExecutionPanel({ t, runtimes }: Props) {
  const [members, setMembers] = useState<readonly Member[]>([])
  const [editing, setEditing] = useState<Member | null>(null)
  const [engine, setEngine] = useState('claude')
  const [runtimeId, setRuntimeId] = useState('')
  const [model, setModel] = useState('')
  const [fallbacks, setFallbacks] = useState('')
  const [retries, setRetries] = useState(0)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const load = useCallback(async () => {
    try {
      const response = await fetch('/api/weave.agent-execution', { cache: 'no-store' })
      if (!response.ok) throw new Error(t('agentExecution.loadFailed'))
      const items = object(await response.json() as unknown)?.agents
      if (!Array.isArray(items)) throw new Error(t('agentExecution.loadFailed'))
      setMembers(items.map(member).filter((item): item is Member => item !== null))
    } catch (cause) { setError(cause instanceof Error ? cause.message : t('agentExecution.loadFailed')) }
  }, [t])
  useEffect(() => { void load() }, [load])
  const select = (item: Member) => {
    const nextEngine = ['claude', 'codex', 'opencode'].includes(item.engine) ? item.engine : 'claude'
    setEditing(item); setEngine(nextEngine); setModel(item.model); setFallbacks(item.fallbackModels.join(', ')); setRetries(item.fallbackRetries)
    setRuntimeId(runtimes.some(runtime => runtime.id === item.runtimeId && runtime.engines.includes(nextEngine)) ? item.runtimeId : runtimes.find(runtime => runtime.enabled && runtime.engines.includes(nextEngine))?.id ?? '')
    setError(''); setNotice('')
  }
  const save = async (event: FormEvent) => {
    event.preventDefault()
    if (editing === null || runtimeId === '') return
    const fallbackModels = fallbacks.split(/[,，\n]/u).map(value => value.trim()).filter(Boolean)
    if (fallbackModels.length > 2) { setError(t('agentExecution.tooManyFallbacks')); return }
    setBusy(true); setError(''); setNotice('')
    try {
      const response = await fetch('/api/weave.agent-execution', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({
        name: editing.name, expectedVersion: editing.version, engine, runtimeId, model: model.trim(),
        fallbackModels, fallbackRetries: retries,
      }) })
      if (!response.ok) {
        const code = object(await response.json() as unknown)?.code
        throw new Error(t(code === 'execution_conflict' ? 'agentExecution.conflict' : code === 'runtime_forbidden' ? 'runtimeCenter.error.forbidden' : 'agentExecution.saveFailed'))
      }
      setEditing(null); setNotice(t('agentExecution.saved')); await load()
    } catch (cause) { setError(cause instanceof Error ? cause.message : t('agentExecution.saveFailed')) }
    finally { setBusy(false) }
  }
  return (
    <section className={css.agentSection} aria-label={t('agentExecution.title')}>
      <div className={css.heading}><div><strong>{t('agentExecution.title')}</strong><small>{t('agentExecution.description')}</small></div></div>
      {error === '' ? null : <p className={css.error} role="alert">{error}</p>}
      {notice === '' ? null : <p className={css.notice} role="status">{notice}</p>}
      <div className={css.agentList}>
        {members.map(item => <div className={css.agentRow} key={item.name}>
          <span><strong>{item.displayName}</strong><small>{item.name}</small><small>{item.engine} · {item.model || t('agentExecution.nodeDefault')}</small></span>
          <button type="button" onClick={() => { select(item) }} disabled={busy}>{t('agentExecution.configure')}</button>
        </div>)}
      </div>
      {editing === null ? null : <div className={css.dialogBackdrop} role="presentation">
        <form className={css.dialog} role="dialog" aria-modal="true" aria-labelledby="agent-execution-title" onSubmit={(event) => { void save(event) }}>
          <strong id="agent-execution-title">{editing.displayName}</strong><small>{editing.name}</small><p>{t('agentExecution.description')}</p>
          <label>{t('agentExecution.engine')}<select value={engine} onChange={(event) => { const value = event.currentTarget.value; setEngine(value); setModel(''); setFallbacks(''); setRuntimeId(runtimes.find(runtime => runtime.enabled && runtime.engines.includes(value))?.id ?? '') }}>
            <option value="claude">Claude CLI</option><option value="codex">Codex CLI</option><option value="opencode">OpenCode</option>
          </select></label>
          <label>{t('agentExecution.runtime')}<select value={runtimeId} onChange={(event) => { setRuntimeId(event.currentTarget.value) }}>
            <option value="" disabled>{t('agentExecution.selectRuntime')}</option>
            {runtimes.filter(item => item.enabled && item.engines.includes(engine)).map(item => (
              <option key={item.id} value={item.id}>{item.name}</option>
            ))}
          </select></label>
          <label>{t('agentExecution.model')}<input value={model} maxLength={200} placeholder={t('agentExecution.nodeDefault')} onChange={(event) => { setModel(event.currentTarget.value) }} /><small>{t('agentExecution.modelHelp')}</small></label>
          <label>{t('agentExecution.fallbacks')}<input value={fallbacks} maxLength={404} placeholder={t('agentExecution.fallbackHelp')} onChange={(event) => { setFallbacks(event.currentTarget.value) }} /></label>
          <label>{t('agentExecution.retries')}<select value={retries} onChange={(event) => { setRetries(Number(event.currentTarget.value)) }}><option value={0}>0</option><option value={1}>1</option><option value={2}>2</option></select><small>{t('agentExecution.retryHelp')}</small></label>
          {error === '' ? null : <p className={css.error} role="alert">{error}</p>}
          <div className={css.actions}><button type="button" onClick={() => { setEditing(null) }} disabled={busy}>{t('task.cancel')}</button><button type="submit" data-primary disabled={busy || runtimeId === ''}>{t(busy ? 'agentExecution.saving' : 'agentExecution.save')}</button></div>
        </form>
      </div>}
    </section>
  )
}
