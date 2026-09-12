import { useCallback, useEffect, useMemo, useState } from 'react'
import type { FormEvent } from 'react'
import type { PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import css from './CapabilityCenter.module.css'

type Props = PropsLocale<'weave'>

interface ServiceAppView {
  readonly id: string
  readonly name: string
  readonly description: string
  readonly enabled: boolean
  readonly max_concurrent_invocations: number
}

interface CredentialView {
  readonly id: string
  readonly app_id: string
  readonly name: string
  readonly scopes: readonly string[]
  readonly revoked_at?: string
  readonly last_used_at?: string
  readonly created_at: string
}

interface CapabilityView {
  readonly id: string
  readonly key: string
  readonly name: string
  readonly description: string
  readonly enabled: boolean
}

interface ReleaseView {
  readonly id: string
  readonly capability_id: string
  readonly version: number
  readonly workflow_id: string
  readonly workflow_version: number
  readonly artifact_content_hash: string
  readonly enabled: boolean
  readonly execution_limits: Record<string, number>
  readonly result_policy: { readonly exposed_fields: readonly string[] }
}

interface GrantView {
  readonly id: string
  readonly app_id: string
  readonly capability_id: string
  readonly release_version: number
  readonly max_concurrent: number
  readonly revoked_at?: string
}

interface InvocationView {
  readonly invocation: {
    readonly app_id: string
    readonly invocation_id: string
    readonly request_id: string
    readonly capability_id: string
    readonly release_version: number
    readonly accepted_at: string
    readonly deadline_at: string
  }
  readonly execution_status: string
  readonly result_availability: string
  readonly result?: unknown
  readonly result_problems?: readonly { readonly path?: string; readonly code: string }[]
  readonly cancellation_status: string
  readonly failure_code?: string
}

interface WorkflowView {
  readonly workflow_id: string
  readonly workflow_name: string
  readonly workflow_version: number
  readonly team_id: string
  readonly team_name: string
}

interface CapabilitySnapshot {
  readonly apps: readonly ServiceAppView[]
  readonly credentials: readonly CredentialView[]
  readonly capabilities: readonly CapabilityView[]
  readonly releases: readonly ReleaseView[]
  readonly grants: readonly GrantView[]
  readonly invocations: readonly InvocationView[]
  readonly publishable_workflows: readonly WorkflowView[]
}

function object(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined
}

function list(value: unknown, field: string): readonly unknown[] | null {
  const items = object(value)?.[field]
  return Array.isArray(items) ? items : null
}

function text(value: unknown): string { return typeof value === 'string' ? value : '' }
function count(value: unknown): number { return typeof value === 'number' && Number.isInteger(value) ? value : 0 }

function parseSnapshot(value: unknown): CapabilitySnapshot | null {
  const apps = list(value, 'apps')
  const credentials = list(value, 'credentials')
  const capabilities = list(value, 'capabilities')
  const releases = list(value, 'releases')
  const grants = list(value, 'grants')
  const invocations = list(value, 'invocations')
  const workflows = list(value, 'publishable_workflows')
  if ([apps, credentials, capabilities, releases, grants, invocations, workflows].some(items => items === null)) return null
  return {
    apps: apps!.flatMap((value): ServiceAppView[] => {
      const item = object(value)
      return item === undefined || text(item.id) === '' || text(item.name) === '' ? [] : [{
        id: text(item.id), name: text(item.name), description: text(item.description),
        enabled: item.enabled === true, max_concurrent_invocations: count(item.max_concurrent_invocations),
      }]
    }),
    credentials: credentials!.flatMap((value): CredentialView[] => {
      const item = object(value)
      return item === undefined || text(item.id) === '' || text(item.app_id) === '' ? [] : [{
        id: text(item.id), app_id: text(item.app_id), name: text(item.name),
        scopes: Array.isArray(item.scopes) ? item.scopes.filter((scope): scope is string => typeof scope === 'string') : [],
        ...(text(item.revoked_at) === '' ? {} : { revoked_at: text(item.revoked_at) }),
        ...(text(item.last_used_at) === '' ? {} : { last_used_at: text(item.last_used_at) }),
        created_at: text(item.created_at),
      }]
    }),
    capabilities: capabilities!.flatMap((value): CapabilityView[] => {
      const item = object(value)
      return item === undefined || text(item.id) === '' || text(item.name) === '' ? [] : [{
        id: text(item.id), key: text(item.key), name: text(item.name),
        description: text(item.description), enabled: item.enabled === true,
      }]
    }),
    releases: releases!.flatMap((value): ReleaseView[] => {
      const item = object(value)
      const policy = object(item?.result_policy)
      return item === undefined || text(item.id) === '' || text(item.capability_id) === '' ? [] : [{
        id: text(item.id), capability_id: text(item.capability_id), version: count(item.version),
        workflow_id: text(item.workflow_id), workflow_version: count(item.workflow_version),
        artifact_content_hash: text(item.artifact_content_hash), enabled: item.enabled === true,
        execution_limits: Object.fromEntries(Object.entries(object(item.execution_limits) ?? {}).flatMap(([key, raw]) => typeof raw === 'number' ? [[key, raw]] : [])),
        result_policy: { exposed_fields: Array.isArray(policy?.exposed_fields) ? policy.exposed_fields.filter((field): field is string => typeof field === 'string') : [] },
      }]
    }),
    grants: grants!.flatMap((value): GrantView[] => {
      const item = object(value)
      return item === undefined || text(item.id) === '' ? [] : [{
        id: text(item.id), app_id: text(item.app_id), capability_id: text(item.capability_id),
        release_version: count(item.release_version), max_concurrent: count(item.max_concurrent),
        ...(text(item.revoked_at) === '' ? {} : { revoked_at: text(item.revoked_at) }),
      }]
    }),
    invocations: invocations!.flatMap((value): InvocationView[] => {
      const item = object(value)
      const invocation = object(item?.invocation)
      if (item === undefined || invocation === undefined || text(invocation.invocation_id) === '') return []
      return [{
        invocation: {
          app_id: text(invocation.app_id), invocation_id: text(invocation.invocation_id),
          request_id: text(invocation.request_id), capability_id: text(invocation.capability_id),
          release_version: count(invocation.release_version), accepted_at: text(invocation.accepted_at),
          deadline_at: text(invocation.deadline_at),
        },
        execution_status: text(item.execution_status), result_availability: text(item.result_availability),
        ...(item.result === undefined ? {} : { result: item.result }),
        ...(Array.isArray(item.result_problems) ? { result_problems: item.result_problems.flatMap((problem) => {
          const parsed = object(problem)
          return parsed === undefined || text(parsed.code) === '' ? [] : [{ path: text(parsed.path), code: text(parsed.code) }]
        }) } : {}),
        cancellation_status: text(item.cancellation_status), failure_code: text(item.failure_code),
      }]
    }),
    publishable_workflows: workflows!.flatMap((value): WorkflowView[] => {
      const item = object(value)
      return item === undefined || text(item.workflow_id) === '' ? [] : [{
        workflow_id: text(item.workflow_id), workflow_name: text(item.workflow_name),
        workflow_version: count(item.workflow_version), team_id: text(item.team_id), team_name: text(item.team_name),
      }]
    }),
  }
}

function named<T extends { readonly id: string; readonly name: string }>(items: readonly T[], id: string): string {
  return items.find(item => item.id === id)?.name ?? id
}

function timeLabel(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString()
}

async function responseError(response: Response, t: Props['t']): Promise<string> {
  try {
    const code = text(object(await response.json() as unknown)?.code)
    if (code === 'capability_forbidden') return t('capabilityCenter.error.forbidden')
    if (code === 'invalid_input') return t('capabilityCenter.error.input')
    if (code === 'capability_missing') return t('capabilityCenter.error.missing')
    if (code === 'capability_conflict') return t('capabilityCenter.error.conflict')
    if (code === 'capability_release_unavailable') return t('capabilityCenter.error.release')
    if (code === 'weave_disconnected') return t('capabilityCenter.error.disconnected')
  } catch { /* A non-JSON failure has no browser-safe detail. */ }
  return t('capabilityCenter.error.action')
}

const defaultLimits = {
  max_input_bytes: 65_536, max_output_bytes: 32_768, max_nesting_depth: 20,
  max_object_fields: 256, max_array_items: 1_000, max_string_bytes: 8_192,
  queue_timeout_seconds: 60, execution_timeout_seconds: 300, max_output_tokens: 1_200,
} as const

const executionStatusKeys = {
  accepted: 'capabilityCenter.status.accepted', starting: 'capabilityCenter.status.starting',
  queued: 'capabilityCenter.status.queued', running: 'capabilityCenter.status.running',
  parked: 'capabilityCenter.status.parked', cancel_requested: 'capabilityCenter.status.cancel_requested',
  succeeded: 'capabilityCenter.status.succeeded', failed: 'capabilityCenter.status.failed',
  cancelled: 'capabilityCenter.status.cancelled', abandoned: 'capabilityCenter.status.abandoned',
  unknown: 'capabilityCenter.status.unknown',
} as const
const resultStatusKeys = {
  pending: 'capabilityCenter.result.pending', available: 'capabilityCenter.result.available',
  contract_invalid: 'capabilityCenter.result.contract_invalid', unavailable: 'capabilityCenter.result.unavailable',
} as const
const cancellationStatusKeys = {
  not_requested: 'capabilityCenter.cancellation.not_requested', requested: 'capabilityCenter.cancellation.requested',
  confirmed: 'capabilityCenter.cancellation.confirmed', not_applied: 'capabilityCenter.cancellation.not_applied',
} as const

function statusKey<T extends Record<string, string>>(values: T, value: string, fallback: T[keyof T]): T[keyof T] {
  return Object.prototype.hasOwnProperty.call(values, value) ? values[value as keyof T] : fallback
}

/** Workbench administrator page for applications, immutable releases, grants, and invocations. */
export function CapabilityCenter({ t }: Props) {
  const [snapshot, setSnapshot] = useState<CapabilitySnapshot | null>(null)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [appName, setAppName] = useState('')
  const [appDescription, setAppDescription] = useState('')
  const [appLimit, setAppLimit] = useState(2)
  const [capabilityName, setCapabilityName] = useState('')
  const [capabilityKey, setCapabilityKey] = useState('')
  const [capabilityDescription, setCapabilityDescription] = useState('')
  const [credentialFor, setCredentialFor] = useState('')
  const [credentialName, setCredentialName] = useState('')
  const [credentialSecret, setCredentialSecret] = useState('')
  const [publishFor, setPublishFor] = useState('')
  const [workflowChoice, setWorkflowChoice] = useState('')
  const [exposedFields, setExposedFields] = useState('')
  const [grantFor, setGrantFor] = useState<{ capabilityId: string; version: number } | null>(null)
  const [grantApp, setGrantApp] = useState('')
  const [grantLimit, setGrantLimit] = useState(1)
  const [cancelFor, setCancelFor] = useState('')
  const [cancelReason, setCancelReason] = useState('')

  const refresh = useCallback(async () => {
    setLoading(true); setError('')
    try {
      const response = await fetch('/api/weave.capabilities', { headers: { Accept: 'application/json' }, cache: 'no-store' })
      if (!response.ok) throw new Error(await responseError(response, t))
      const parsed = parseSnapshot(await response.json() as unknown)
      if (parsed === null) throw new Error(t('capabilityCenter.error.load'))
      setSnapshot(parsed)
    } catch (cause) { setError(cause instanceof Error ? cause.message : t('capabilityCenter.error.load')) }
    finally { setLoading(false) }
  }, [t])

  useEffect(() => { void refresh() }, [refresh])

  const mutate = async (body: object): Promise<Response | null> => {
    setError(''); setNotice('')
    try {
      const response = await fetch('/api/weave.capabilities', {
        method: 'POST', headers: { Accept: 'application/json', 'Content-Type': 'application/json' }, body: JSON.stringify(body),
      })
      if (!response.ok) throw new Error(await responseError(response, t))
      return response
    } catch (cause) { setError(cause instanceof Error ? cause.message : t('capabilityCenter.error.action')); return null }
  }

  const perform = async (body: object, success: string) => {
    setBusy(true)
    try {
      if (await mutate(body) === null) return false
      setNotice(success); await refresh(); return true
    } finally { setBusy(false) }
  }

  const createApp = async (event: FormEvent) => {
    event.preventDefault()
    if (await perform({ action: 'create_app', name: appName.trim(), description: appDescription.trim(), max_concurrent: appLimit }, t('capabilityCenter.app.created'))) {
      setAppName(''); setAppDescription(''); setAppLimit(2)
    }
  }

  const createCapability = async (event: FormEvent) => {
    event.preventDefault()
    if (await perform({ action: 'create_capability', key: capabilityKey.trim(), name: capabilityName.trim(), description: capabilityDescription.trim() }, t('capabilityCenter.capability.created'))) {
      setCapabilityKey(''); setCapabilityName(''); setCapabilityDescription('')
    }
  }

  const issueCredential = async (event: FormEvent) => {
    event.preventDefault()
    setBusy(true)
    try {
      const response = await mutate({ action: 'create_credential', app_id: credentialFor, name: credentialName.trim(), scopes: ['invoke', 'read', 'cancel'] })
      if (response === null) return
      const secret = text(object(await response.json() as unknown)?.secret)
      if (secret === '') { setError(t('capabilityCenter.error.secret')); return }
      setCredentialSecret(secret); setCredentialName(''); setCredentialFor(''); await refresh()
    } finally { setBusy(false) }
  }

  const publish = async (event: FormEvent) => {
    event.preventDefault()
    if (snapshot === null) return
    const workflow = snapshot.publishable_workflows.find(item => `${item.workflow_id}:${item.workflow_version}` === workflowChoice)
    const fields = exposedFields.split(',').map(item => item.trim()).filter(Boolean)
    const versions = snapshot.releases.filter(item => item.capability_id === publishFor).map(item => item.version)
    if (workflow === undefined || fields.length === 0) return
    if (await perform({
      action: 'publish_release', capability_id: publishFor,
      release_version: Math.max(0, ...versions) + 1, workflow_id: workflow.workflow_id,
      workflow_version: workflow.workflow_version, execution_limits: defaultLimits,
      result_policy: { exposed_fields: fields, include_usage: false, include_contract_evidence: true },
    }, t('capabilityCenter.release.published'))) {
      setPublishFor(''); setWorkflowChoice(''); setExposedFields('')
    }
  }

  const grant = async (event: FormEvent) => {
    event.preventDefault()
    if (grantFor === null) return
    if (await perform({
      action: 'grant_release', app_id: grantApp, capability_id: grantFor.capabilityId,
      release_version: grantFor.version, max_concurrent: grantLimit,
    }, t('capabilityCenter.grant.created'))) {
      setGrantFor(null); setGrantApp(''); setGrantLimit(1)
    }
  }

  const cancel = async (event: FormEvent) => {
    event.preventDefault()
    if (await perform({ action: 'cancel_invocation', invocation_id: cancelFor, reason: cancelReason.trim() }, t('capabilityCenter.invocation.cancelled'))) {
      setCancelFor(''); setCancelReason('')
    }
  }

  const apps = snapshot?.apps ?? []
  const capabilities = snapshot?.capabilities ?? []
  const activeApps = apps.filter(item => item.enabled)
  const terminalStatuses = useMemo(() => new Set(['succeeded', 'failed', 'cancelled', 'abandoned']), [])

  return <section className={css.center} aria-label={t('capabilityCenter.title')}>
    <div className={css.heading}>
      <div><span>{t('capabilityCenter.eyebrow')}</span><strong>{t('capabilityCenter.title')}</strong><small>{t('capabilityCenter.summary', { apps: apps.length, capabilities: capabilities.length })}</small></div>
      <button type="button" onClick={() => { void refresh() }} disabled={loading || busy}>{t('capabilityCenter.refresh')}</button>
    </div>
    {error === '' ? null : <div className={css.error} role="alert"><span>{error}</span><button type="button" onClick={() => { void refresh() }}>{t('capabilityCenter.retry')}</button></div>}
    {notice === '' ? null : <p className={css.notice} role="status">{notice}</p>}
    {loading && snapshot === null ? <p className={css.empty}>{t('capabilityCenter.loading')}</p> : snapshot === null ? null : <>
      <div className={css.grid}>
        <section className={css.panel} aria-label={t('capabilityCenter.apps.title')}>
          <header><div><strong>{t('capabilityCenter.apps.title')}</strong><small>{t('capabilityCenter.apps.help')}</small></div></header>
          <form className={css.compactForm} onSubmit={(event) => { void createApp(event) }}>
            <input aria-label={t('capabilityCenter.app.name')} value={appName} maxLength={120} onChange={event => { setAppName(event.currentTarget.value) }} placeholder={t('capabilityCenter.app.name.placeholder')} />
            <input aria-label={t('capabilityCenter.app.description')} value={appDescription} maxLength={2_000} onChange={event => { setAppDescription(event.currentTarget.value) }} placeholder={t('capabilityCenter.app.description.placeholder')} />
            <label>{t('capabilityCenter.concurrency')}<input type="number" min={1} max={1_024} value={appLimit} onChange={event => { setAppLimit(Number(event.currentTarget.value)) }} /></label>
            <button type="submit" data-primary disabled={busy || appName.trim() === ''}>{t('capabilityCenter.app.create')}</button>
          </form>
          <div className={css.list}>{apps.map(app => <details className={css.item} key={app.id}>
            <summary><span><strong>{app.name}</strong><small>{app.description || t('capabilityCenter.description.empty')}</small></span><em data-enabled={app.enabled}>{app.enabled ? t('capabilityCenter.enabled') : t('capabilityCenter.disabled')}</em></summary>
            <div className={css.itemBody}>
              <p>{t('capabilityCenter.app.capacity', { count: app.max_concurrent_invocations })}</p>
              <div className={css.actions}>
                <button type="button" disabled={busy} onClick={() => { void perform({ action: 'set_app_enabled', app_id: app.id, enabled: !app.enabled }, app.enabled ? t('capabilityCenter.app.disabled') : t('capabilityCenter.app.enabled')) }}>{app.enabled ? t('capabilityCenter.disable') : t('capabilityCenter.enable')}</button>
                <button type="button" disabled={busy || !app.enabled} onClick={() => { setCredentialFor(app.id); setCredentialName('') }}>{t('capabilityCenter.credential.issue')}</button>
              </div>
              {credentialFor !== app.id ? null : <form className={css.inlineForm} onSubmit={(event) => { void issueCredential(event) }}><input autoFocus aria-label={t('capabilityCenter.credential.name')} value={credentialName} maxLength={120} onChange={event => { setCredentialName(event.currentTarget.value) }} placeholder={t('capabilityCenter.credential.name.placeholder')} /><button type="button" onClick={() => { setCredentialFor('') }}>{t('task.cancel')}</button><button type="submit" data-primary disabled={busy || credentialName.trim() === ''}>{t('capabilityCenter.credential.confirm')}</button></form>}
              <div className={css.sublist}>{snapshot.credentials.filter(item => item.app_id === app.id).map(credential => <div key={credential.id}><span><strong>{credential.name}</strong><small>{credential.scopes.join(' · ')} · {credential.last_used_at === undefined ? t('capabilityCenter.credential.unused') : t('capabilityCenter.credential.used', { time: timeLabel(credential.last_used_at) })}</small></span>{credential.revoked_at === undefined ? <button type="button" data-danger disabled={busy} onClick={() => { void perform({ action: 'revoke_credential', credential_id: credential.id }, t('capabilityCenter.credential.revoked')) }}>{t('capabilityCenter.credential.revoke')}</button> : <em>{t('capabilityCenter.credential.revokedLabel')}</em>}</div>)}</div>
            </div>
          </details>)}</div>
        </section>

        <section className={css.panel} aria-label={t('capabilityCenter.capabilities.title')}>
          <header><div><strong>{t('capabilityCenter.capabilities.title')}</strong><small>{t('capabilityCenter.capabilities.help')}</small></div></header>
          <form className={css.compactForm} onSubmit={(event) => { void createCapability(event) }}>
            <input aria-label={t('capabilityCenter.capability.name')} value={capabilityName} maxLength={120} onChange={event => { setCapabilityName(event.currentTarget.value) }} placeholder={t('capabilityCenter.capability.name.placeholder')} />
            <input aria-label={t('capabilityCenter.capability.key')} value={capabilityKey} maxLength={63} onChange={event => { setCapabilityKey(event.currentTarget.value) }} placeholder={t('capabilityCenter.capability.key.placeholder')} />
            <input aria-label={t('capabilityCenter.capability.description')} value={capabilityDescription} maxLength={2_000} onChange={event => { setCapabilityDescription(event.currentTarget.value) }} placeholder={t('capabilityCenter.capability.description.placeholder')} />
            <button type="submit" data-primary disabled={busy || capabilityName.trim() === '' || !/^[a-z][a-z0-9_]{0,62}$/u.test(capabilityKey.trim())}>{t('capabilityCenter.capability.create')}</button>
          </form>
          <div className={css.list}>{capabilities.map(capability => <details className={css.item} key={capability.id}>
            <summary><span><strong>{capability.name}</strong><small>{capability.description || t('capabilityCenter.description.empty')}</small></span><em data-enabled={capability.enabled}>{capability.enabled ? t('capabilityCenter.enabled') : t('capabilityCenter.disabled')}</em></summary>
            <div className={css.itemBody}>
              <div className={css.actions}><button type="button" disabled={busy} onClick={() => { void perform({ action: 'set_capability_enabled', capability_id: capability.id, enabled: !capability.enabled }, capability.enabled ? t('capabilityCenter.capability.disabled') : t('capabilityCenter.capability.enabled')) }}>{capability.enabled ? t('capabilityCenter.disable') : t('capabilityCenter.enable')}</button><button type="button" disabled={busy || !capability.enabled || snapshot.publishable_workflows.length === 0} onClick={() => { setPublishFor(capability.id); setWorkflowChoice(''); setExposedFields('') }}>{t('capabilityCenter.release.publish')}</button></div>
              {publishFor !== capability.id ? null : <form className={css.stackForm} onSubmit={(event) => { void publish(event) }}>
                <label>{t('capabilityCenter.release.workflow')}<select value={workflowChoice} onChange={event => { setWorkflowChoice(event.currentTarget.value) }}><option value="">{t('capabilityCenter.release.workflow.placeholder')}</option>{snapshot.publishable_workflows.map(workflow => <option key={`${workflow.workflow_id}:${workflow.workflow_version}`} value={`${workflow.workflow_id}:${workflow.workflow_version}`}>{workflow.team_name} · {workflow.workflow_name} · v{workflow.workflow_version}</option>)}</select></label>
                <label>{t('capabilityCenter.release.fields')}<input value={exposedFields} onChange={event => { setExposedFields(event.currentTarget.value) }} placeholder={t('capabilityCenter.release.fields.placeholder')} /><small>{t('capabilityCenter.release.limits')}</small></label>
                <div className={css.actions}><button type="button" onClick={() => { setPublishFor('') }}>{t('task.cancel')}</button><button type="submit" data-primary disabled={busy || workflowChoice === '' || exposedFields.trim() === ''}>{t('capabilityCenter.release.confirm')}</button></div>
              </form>}
              <div className={css.sublist}>{snapshot.releases.filter(item => item.capability_id === capability.id).map(release => {
                const activeGrants = snapshot.grants.filter(item => item.capability_id === capability.id && item.release_version === release.version && item.revoked_at === undefined)
                return <div className={css.release} key={release.id}><span><strong>{t('capabilityCenter.release.version', { version: release.version })}</strong><small>{t('capabilityCenter.release.fieldsValue', { fields: release.result_policy.exposed_fields.join(', ') })}</small><small>{t('capabilityCenter.release.timeouts', { queue: release.execution_limits.queue_timeout_seconds ?? 0, execution: release.execution_limits.execution_timeout_seconds ?? 0 })}</small></span><div className={css.actions}><button type="button" disabled={busy} onClick={() => { void perform({ action: 'set_release_enabled', capability_id: capability.id, release_version: release.version, enabled: !release.enabled }, release.enabled ? t('capabilityCenter.release.disabled') : t('capabilityCenter.release.enabled')) }}>{release.enabled ? t('capabilityCenter.disable') : t('capabilityCenter.enable')}</button><button type="button" disabled={busy || !release.enabled || activeApps.length === 0} onClick={() => { setGrantFor({ capabilityId: capability.id, version: release.version }); setGrantApp(''); setGrantLimit(1) }}>{t('capabilityCenter.grant.create')}</button></div>{activeGrants.map(grantItem => <div className={css.grant} key={grantItem.id}><span>{t('capabilityCenter.grant.value', { app: named(apps, grantItem.app_id), count: grantItem.max_concurrent })}</span><button type="button" data-danger disabled={busy} onClick={() => { void perform({ action: 'revoke_grant', grant_id: grantItem.id }, t('capabilityCenter.grant.revoked')) }}>{t('capabilityCenter.grant.revoke')}</button></div>)}</div>
              })}</div>
            </div>
          </details>)}</div>
        </section>
      </div>

      <section className={css.panel} aria-label={t('capabilityCenter.invocations.title')}>
        <header><div><strong>{t('capabilityCenter.invocations.title')}</strong><small>{t('capabilityCenter.invocations.help')}</small></div></header>
        {snapshot.invocations.length === 0 ? <p className={css.empty}>{t('capabilityCenter.invocations.empty')}</p> : <div className={css.invocations}>{snapshot.invocations.map(status => {
          const invocation = status.invocation
          const capability = named(capabilities, invocation.capability_id)
          return <details className={css.item} key={invocation.invocation_id}><summary><span><strong>{capability} · v{invocation.release_version}</strong><small>{named(apps, invocation.app_id)} · {timeLabel(invocation.accepted_at)}</small></span><em>{t(statusKey(executionStatusKeys, status.execution_status, 'capabilityCenter.status.unknown'))}</em></summary><div className={css.itemBody}><dl><div><dt>{t('capabilityCenter.invocation.request')}</dt><dd>{invocation.request_id}</dd></div><div><dt>{t('capabilityCenter.invocation.resultState')}</dt><dd>{t(statusKey(resultStatusKeys, status.result_availability, 'capabilityCenter.result.unavailable'))}</dd></div><div><dt>{t('capabilityCenter.invocation.deadline')}</dt><dd>{timeLabel(invocation.deadline_at)}</dd></div><div><dt>{t('capabilityCenter.invocation.cancellation')}</dt><dd>{t(statusKey(cancellationStatusKeys, status.cancellation_status, 'capabilityCenter.cancellation.not_requested'))}</dd></div></dl>{status.result === undefined ? null : <pre>{JSON.stringify(status.result, null, 2)}</pre>}{status.result_problems === undefined ? null : <p>{status.result_problems.map(problem => `${problem.path ?? ''} ${problem.code}`.trim()).join(' · ')}</p>}<div className={css.actions}><button type="button" disabled={busy || terminalStatuses.has(status.execution_status)} onClick={() => { setCancelFor(invocation.invocation_id); setCancelReason('') }}>{t('capabilityCenter.invocation.cancel')}</button></div>{cancelFor !== invocation.invocation_id ? null : <form className={css.inlineForm} onSubmit={(event) => { void cancel(event) }}><input autoFocus aria-label={t('capabilityCenter.invocation.reason')} value={cancelReason} maxLength={256} onChange={event => { setCancelReason(event.currentTarget.value) }} placeholder={t('capabilityCenter.invocation.reason.placeholder')} /><button type="button" onClick={() => { setCancelFor('') }}>{t('task.cancel')}</button><button type="submit" data-danger disabled={busy || cancelReason.trim() === ''}>{t('capabilityCenter.invocation.cancelConfirm')}</button></form>}</div></details>
        })}</div>}
      </section>
    </>}

    {credentialSecret === '' ? null : <div className={css.dialogBackdrop} role="presentation"><section className={css.dialog} role="dialog" aria-modal="true" aria-labelledby="capability-secret-title"><strong id="capability-secret-title">{t('capabilityCenter.secret.title')}</strong><p>{t('capabilityCenter.secret.notice')}</p><code>{credentialSecret}</code><div className={css.actions}><button type="button" onClick={() => { void navigator.clipboard.writeText(credentialSecret).then(() => { setNotice(t('capabilityCenter.secret.copied')) }, () => { setNotice(t('capabilityCenter.secret.copyFailed')) }) }}>{t('capabilityCenter.secret.copy')}</button><button type="button" data-primary onClick={() => { setCredentialSecret('') }}>{t('capabilityCenter.secret.saved')}</button></div></section></div>}
    {grantFor === null || snapshot === null ? null : <div className={css.dialogBackdrop} role="presentation"><form className={css.dialog} role="dialog" aria-modal="true" aria-labelledby="capability-grant-title" onSubmit={(event) => { void grant(event) }}><strong id="capability-grant-title">{t('capabilityCenter.grant.title')}</strong><label>{t('capabilityCenter.grant.app')}<select value={grantApp} onChange={event => { setGrantApp(event.currentTarget.value) }}><option value="">{t('capabilityCenter.grant.app.placeholder')}</option>{activeApps.map(app => <option key={app.id} value={app.id}>{app.name}</option>)}</select></label><label>{t('capabilityCenter.concurrency')}<input type="number" min={1} max={Math.max(1, apps.find(item => item.id === grantApp)?.max_concurrent_invocations ?? 1)} value={grantLimit} onChange={event => { setGrantLimit(Number(event.currentTarget.value)) }} /></label><div className={css.actions}><button type="button" onClick={() => { setGrantFor(null) }}>{t('task.cancel')}</button><button type="submit" data-primary disabled={busy || grantApp === ''}>{t('capabilityCenter.grant.confirm')}</button></div></form></div>}
  </section>
}

/** Published-capability management page owned by the shared Settings shell. */
export function CapabilitySettingsSection({ t }: Props & PropsRuntime<'settings.section'>) {
  return <CapabilityCenter t={t} />
}
