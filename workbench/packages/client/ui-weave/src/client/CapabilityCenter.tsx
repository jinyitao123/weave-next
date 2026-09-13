import { useEffect, useRef, useState } from 'react'
import type { PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import css from './CapabilityCenter.module.css'

type Props = PropsLocale<'weave'> & PropsRuntime<'settings.section'>
type Data = Record<string, unknown>
const object = (value: unknown): value is Data => typeof value === 'object' && value !== null && !Array.isArray(value)

function sample(t: Props['t']): Data {
  return {
    schema_version: 1, capability_id: 'my-capability', name: t('cap.sampleName'),
    input_schema: { type: 'object' }, output_schema: { type: 'object' },
    runtime: { engine: 'loom', model: '' }, resources: {},
    roles: ['first', 'second'].map(id => ({ id, name: t('cap.sampleRole') + ' ' + id })),
    steps: ['first', 'second'].map(id => ({ id, name: id, role_id: id, kind: 'worker', instruction: t('cap.sampleInstruction') })),
    relations: [{ from: 'first', to: 'second', kind: 'sequence' }],
  }
}

class CapabilityRequestError extends Error { constructor(readonly status: number) { super('capability request failed') } }

async function request(body?: object, signal?: AbortSignal): Promise<Data> {
  const response = await fetch('/api/weave.capabilities', {
    method: body === undefined ? 'GET' : 'POST', cache: 'no-store',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    ...(signal === undefined ? {} : { signal }),
  })
  const data: unknown = await response.json()
  if (!response.ok || !object(data)) throw new CapabilityRequestError(response.status)
  return data
}

/** Workbench authoring and invocation controls backed by the Host credential. */
export function CapabilitySettingsSection({ t }: Props) {
  const [definition, setDefinition] = useState(() => JSON.stringify(sample(t), null, 2))
  const [drafts, setDrafts] = useState<Data[]>([])
  const [input, setInput] = useState('{}')
  const [revision, setRevision] = useState(1)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [invocation, setInvocation] = useState<Data | null>(null)
  const [retry, setRetry] = useState<object | null>(null)
  const lifetime = useRef<AbortController | null>(null)
  const id = typeof invocation?.invocation_id === 'string' ? invocation.invocation_id : ''

  useEffect(() => {
    const controller = new AbortController()
    lifetime.current = controller
    void request(undefined, controller.signal).then(data => {
      if (!controller.signal.aborted) setDrafts(Array.isArray(data.drafts) ? data.drafts.filter(object) : [])
    }).catch(() => { if (!controller.signal.aborted) setError(t('cap.error')) })
    return () => { controller.abort() }
  }, [t])

  useEffect(() => {
    if (id === '') return
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    const poll = async () => {
      try {
        const next = await request({ action: 'status', id }, controller.signal)
        if (controller.signal.aborted) return
        setInvocation(previous => previous !== null && (['completed', 'failed', 'cancelled'].includes(String(previous.status)) || (previous.status === 'cancel_requested' && next.status === 'running')) ? previous : next)
        if (['completed', 'failed', 'cancelled'].includes(String(next.status))) return
      } catch { if (!controller.signal.aborted) setError(t('cap.error')) }
      if (!controller.signal.aborted) timer = setTimeout(() => { void poll() }, 1000)
    }
    void poll()
    return () => { controller.abort(); clearTimeout(timer) }
  }, [id, t])

  const run = async (action: 'save' | 'publish' | 'invoke' | 'retry' | 'cancel' | 'refresh') => {
    if (busy) return
    setBusy(true); setError(''); setNotice('')
    const signal = lifetime.current?.signal
    try {
      if (action === 'cancel') {
        const next = await request({ action, id }, signal)
        if (!signal?.aborted) setInvocation(previous => ({ ...previous, ...next }))
        return
      }
      if (action === 'refresh') {
        const next = await request(undefined, signal)
        if (!signal?.aborted) setDrafts(Array.isArray(next.drafts) ? next.drafts.filter(object) : [])
        return
      }
      if (action === 'retry') {
        if (retry !== null) {
          const next = await request(retry, signal)
          if (!signal?.aborted) { setInvocation(next); setRetry(null) }
        }
        return
      }
      let doc: unknown, value: unknown
      try { doc = JSON.parse(definition) as unknown; value = JSON.parse(input) as unknown }
      catch { setError(t('cap.jsonError')); return }
      if (!object(doc) || typeof doc.capability_id !== 'string' || !object(value)) { setError(t('cap.jsonError')); return }
      if (action === 'save' || action === 'publish') {
        await request({ action: 'save', definition: doc }, signal)
        if (action === 'publish') await request({ action, id: doc.capability_id, revision }, signal)
        if (!signal?.aborted) setNotice(t(action === 'save' ? 'cap.saved' : 'cap.published'))
        const next = await request(undefined, signal)
        if (!signal?.aborted) setDrafts(Array.isArray(next.drafts) ? next.drafts.filter(object) : [])
      } else {
        const body = { action, id: doc.capability_id, revision, requestId: crypto.randomUUID(), input: value }
        setRetry(body)
        const next = await request(body, signal)
        if (!signal?.aborted) { setInvocation(next); setRetry(null) }
      }
    } catch (cause) {
      if (cause instanceof CapabilityRequestError && cause.status >= 400 && cause.status < 500) setRetry(null)
      if (!signal?.aborted) setError(t('cap.error'))
    }
    finally { if (!signal?.aborted) setBusy(false) }
  }

  return <section className={css.center}>
    <h3>{t('cap.title')}</h3>
    <p>{t('cap.scope')}</p>
    {error !== '' && <p role="alert">{error}</p>}
    {notice !== '' && <p role="status">{notice}</p>}
    <fieldset disabled={busy}>
      <label>{t('cap.drafts')}<select aria-label={t('cap.drafts')} value="" onChange={event => {
        const draft = drafts.find(item => item.capability_id === event.target.value)
        if (draft !== undefined) setDefinition(JSON.stringify(draft, null, 2))
      }}>
        <option value="">{t('cap.drafts')}</option>
        {drafts.map(draft => <option key={String(draft.capability_id)} value={String(draft.capability_id)}>{String(draft.name)}</option>)}
      </select></label>
      <div className={css.actions}>
        <button type="button" onClick={() => { setDefinition(JSON.stringify(sample(t), null, 2)); setRevision(1) }}>{t('cap.new')}</button>
        <button type="button" onClick={() => { void run('refresh') }}>{t('cap.refresh')}</button>
      </div>
      <label>{t('cap.definition')}<textarea aria-label={t('cap.definition')} spellCheck={false} rows={18} value={definition} onChange={event => { setDefinition(event.target.value) }} /></label>
      <div className={css.actions}>
        <button type="button" onClick={() => { void run('save') }}>{t('cap.save')}</button>
        <label>{t('cap.revision')}<input aria-label={t('cap.revision')} type="number" min={1} step={1} value={revision} onChange={event => { setRevision(Number(event.target.value)) }} /></label>
        <button type="button" onClick={() => { void run('publish') }}>{t('cap.publish')}</button>
      </div>
      <label>{t('cap.input')}<textarea aria-label={t('cap.input')} spellCheck={false} rows={4} value={input} onChange={event => { setInput(event.target.value) }} /></label>
      <div className={css.actions}>
        <button type="button" disabled={retry !== null || (invocation !== null && !['completed', 'failed', 'cancelled'].includes(String(invocation.status)))} onClick={() => { void run('invoke') }}>{t('cap.invoke')}</button>
        {retry !== null && <button type="button" onClick={() => { void run('retry') }}>{t('cap.retry')}</button>}
        {id !== '' && <button type="button" disabled={['completed', 'failed', 'cancelled'].includes(String(invocation?.status))} onClick={() => { void run('cancel') }}>{t('cap.cancel')}</button>}
      </div>
    </fieldset>
    <h4>{t('cap.result')}</h4>
    {invocation === null ? <p>{t('cap.empty')}</p> : <pre>{JSON.stringify(invocation, null, 2)}</pre>}
  </section>
}
