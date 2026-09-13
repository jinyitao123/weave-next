import { useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import type { PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import css from './ApplicationCenter.module.css'

type Data = Record<string, unknown>
type Scope = 'invoke' | 'read' | 'cancel'
type Props = PropsLocale<'weave'> & PropsRuntime<'settings.section'>
const object = (value: unknown): value is Data => typeof value === 'object' && value !== null && !Array.isArray(value)
const rows = (value: unknown): Data[] => Array.isArray(value) ? value.filter(object) : []
const versionKey = (version: Data) => JSON.stringify([version.capability_id, version.revision])

async function request(action?: object, signal?: AbortSignal): Promise<Data> {
  const response = await fetch('/api/weave.capability-apps', {
    method: action === undefined ? 'GET' : 'POST', headers: { 'Content-Type': 'application/json' }, cache: 'no-store',
    ...(action === undefined ? {} : { body: JSON.stringify(action) }),
    ...(signal === undefined ? {} : { signal }),
  })
  if (response.status === 204) return {}
  const data: unknown = await response.json()
  if (!response.ok || !object(data)) throw new Error('application operation failed')
  return data
}

/** Applications, one-time credentials and exact-version grants for developers. */
export function ApplicationSettingsSection({ t }: Props) {
  const [snapshot, setSnapshot] = useState<Data>({})
  const [application, setApplication] = useState('')
  const [name, setName] = useState('')
  const [credentialName, setCredentialName] = useState('')
  const [scopes, setScopes] = useState<Scope[]>(['invoke', 'read', 'cancel'])
  const [version, setVersion] = useState('')
  const [secret, setSecret] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const lifetime = useRef<AbortController | null>(null)
  const apps = rows(snapshot.apps)
  const selected = apps.find(app => app.id === application)
  const credentials = rows(snapshot.credentials).filter(item => item.app_id === application)
  const grants = rows(snapshot.grants).filter(item => item.app_id === application)
  const versions = rows(snapshot.versions)
  const selectedVersion = versions.find(item => versionKey(item) === version)

  useEffect(() => {
    const controller = new AbortController()
    lifetime.current = controller
    void request(undefined, controller.signal).then(data => { if (!controller.signal.aborted) setSnapshot(data) })
      .catch(() => { if (!controller.signal.aborted) setError(t('app.error')) })
    return () => { controller.abort() }
  }, [t])

  const mutate = async (action?: object) => {
    if (busy) return
    setBusy(true); setError('')
    const signal = lifetime.current?.signal
    try {
      const result = await request(action, signal)
      if (signal?.aborted) return
      if (typeof result.key === 'string') setSecret(result.key)
      if (typeof result.id === 'string') { setApplication(result.id); setName('') }
      const next = action === undefined ? result : await request(undefined, signal)
      if (!signal?.aborted) setSnapshot(next)
    } catch { if (!signal?.aborted) setError(t('app.error')) }
    finally { if (!signal?.aborted) setBusy(false) }
  }
  const create = (event: FormEvent) => { event.preventDefault(); void mutate({ action: 'create', name }) }
  const issue = (event: FormEvent) => { event.preventDefault(); setSecret(''); void mutate({ action: 'issue', app_id: application, name: credentialName, scopes }) }

  return <section className={css.center}>
    <h3>{t('app.title')}</h3>
    {error !== '' && <p role="alert">{error}</p>}
    <form onSubmit={create} className={css.actions}>
      <label>{t('app.name')}<input aria-label={t('app.name')} value={name} maxLength={160} disabled={busy} onChange={event => { setName(event.target.value) }} /></label>
      <button disabled={busy || name.trim() === ''}>{t('app.create')}</button>
      <button type="button" disabled={busy} onClick={() => { void mutate() }}>{t('app.refresh')}</button>
    </form>
    <label>{t('app.select')}<select aria-label={t('app.select')} disabled={busy} value={application} onChange={event => { setApplication(event.target.value); setSecret('') }}>
      <option value="">{t('app.select')}</option>
      {apps.map(app => <option key={String(app.id)} value={String(app.id)}>{String(app.name)}</option>)}
    </select></label>
    {selected !== undefined && <>
      <details><summary>{t('app.connection')}</summary><label>{t('app.identifier')}<code>{application}</code></label></details>
      <label className={css.check}><input type="checkbox" checked={selected.enabled === true} disabled={busy} onChange={event => { void mutate({ action: 'enable', app_id: application, enabled: event.target.checked }) }} />{t('app.enabled')}</label>
      <h4>{t('app.grants')}</h4>
      <div className={css.actions}>
        <label>{t('app.version')}<select aria-label={t('app.version')} value={version} disabled={busy} onChange={event => { setVersion(event.target.value) }}>
          <option value="">{t('app.version')}</option>
          {versions.map(item => <option key={versionKey(item)} value={versionKey(item)}>{String(item.name)} · {t('app.versionLabel', { version: Number(item.revision) })}</option>)}
        </select></label>
        <button type="button" disabled={busy || selectedVersion === undefined} onClick={() => { void mutate({ action: 'grant', app_id: application, capability_id: selectedVersion?.capability_id, revision: selectedVersion?.revision, enabled: true }) }}>{t('app.grant')}</button>
      </div>
      {grants.length === 0 && <p>{t('app.emptyGrants')}</p>}
      {grants.map(grant => <div className={css.row} key={versionKey(grant)}>
        <span>{String(versions.find(item => versionKey(item) === versionKey(grant))?.name ?? t('app.grants'))} · {t('app.versionLabel', { version: Number(grant.revision) })}</span>
        <button type="button" disabled={busy} onClick={() => { void mutate({ action: 'grant', app_id: application, capability_id: grant.capability_id, revision: grant.revision, enabled: grant.enabled !== true }) }}>{t(grant.enabled === true ? 'app.revokeGrant' : 'app.restoreGrant')}</button>
      </div>)}
      <h4>{t('app.credentials')}</h4>
      <form onSubmit={issue}>
        <fieldset disabled={busy || selected.enabled !== true}>
          <label>{t('app.credentialName')}<input aria-label={t('app.credentialName')} value={credentialName} maxLength={160} onChange={event => { setCredentialName(event.target.value) }} /></label>
          <div className={css.actions}>
            {(['invoke', 'read', 'cancel'] as const).map(scope => <label className={css.check} key={scope}>
              <input type="checkbox" checked={scopes.includes(scope)} onChange={event => { setScopes(previous => event.target.checked ? [...previous, scope] : previous.filter(item => item !== scope)) }} />{t(`app.scope.${scope}`)}
            </label>)}
            <button disabled={credentialName.trim() === '' || scopes.length === 0}>{t('app.issue')}</button>
          </div>
        </fieldset>
      </form>
      {secret !== '' && <div className={css.secret}>
        <p>{t('app.secretNotice')}</p>
        <pre aria-label={t('app.secret')}>{secret}</pre>
        <div className={css.actions}>
          <button type="button" onClick={() => { void navigator.clipboard.writeText(secret).catch(() => { setError(t('app.error')) }) }}>{t('app.copy')}</button>
          <button type="button" onClick={() => { setSecret('') }}>{t('app.hide')}</button>
        </div>
      </div>}
      {credentials.length === 0 && <p>{t('app.emptyCredentials')}</p>}
      {credentials.map(credential => <div role="group" aria-label={String(credential.name)} className={css.row} key={String(credential.id)}>
        <span>{String(credential.name)}<small>{credential.revoked_at == null ? t('app.active') : t('app.revoked')}</small></span>
        <button type="button" disabled={busy || credential.revoked_at != null} onClick={() => { void mutate({ action: 'revoke', app_id: application, credential_id: credential.id }) }}>{t('app.revoke')}</button>
      </div>)}
    </>}
  </section>
}
