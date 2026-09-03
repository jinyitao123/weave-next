/** First-use and settings presentation for the host-owned Weave readiness probe. */

import { useCallback, useEffect, useState } from 'react'
import type { PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import css from './ReadinessPanel.module.css'

type ReadinessStatus = 'ready' | 'attention' | 'unconfigured'
type CheckId = 'credential' | 'service' | 'access' | 'teams' | 'runtimes'

interface ReadinessCheck {
  readonly id: CheckId
  readonly tone: 'pass' | 'warning' | 'fail'
  readonly detail: string
}

interface Readiness {
  readonly status: ReadinessStatus
  readonly checkedAt: string
  readonly serviceVersion: string
  readonly teamCount: number
  readonly dispatchableTeamCount: number
  readonly runtimeCount: number
  readonly healthyRuntimeCount: number
  readonly checks: readonly ReadinessCheck[]
}

type StatusState =
  | { readonly phase: 'loading'; readonly value: null; readonly error: string }
  | { readonly phase: 'ready'; readonly value: Readiness; readonly error: string }
  | { readonly phase: 'error'; readonly value: null; readonly error: string }

const ACK_KEY = 'weave-workbench:onboarding:v1'

function validCheck(value: unknown): value is ReadinessCheck {
  if (typeof value !== 'object' || value === null) return false
  const item = value as Partial<ReadinessCheck>
  return ['credential', 'service', 'access', 'teams', 'runtimes'].includes(item.id ?? '')
    && ['pass', 'warning', 'fail'].includes(item.tone ?? '') && typeof item.detail === 'string'
}

function readiness(value: unknown): Readiness | null {
  if (typeof value !== 'object' || value === null) return null
  const item = value as Partial<Readiness>
  if (!['ready', 'attention', 'unconfigured'].includes(item.status ?? '') || !Array.isArray(item.checks)
    || !item.checks.every(validCheck)) return null
  return {
    status: item.status as ReadinessStatus,
    checkedAt: typeof item.checkedAt === 'string' ? item.checkedAt : '',
    serviceVersion: typeof item.serviceVersion === 'string' ? item.serviceVersion : '',
    teamCount: typeof item.teamCount === 'number' ? item.teamCount : 0,
    dispatchableTeamCount: typeof item.dispatchableTeamCount === 'number' ? item.dispatchableTeamCount : 0,
    runtimeCount: typeof item.runtimeCount === 'number' ? item.runtimeCount : 0,
    healthyRuntimeCount: typeof item.healthyRuntimeCount === 'number' ? item.healthyRuntimeCount : 0,
    checks: item.checks,
  }
}

function useReadiness(): readonly [StatusState, () => Promise<void>] {
  const [state, setState] = useState<StatusState>({ phase: 'loading', value: null, error: '' })
  const reload = useCallback(async () => {
    setState({ phase: 'loading', value: null, error: '' })
    try {
      const response = await fetch('/api/weave.status', { headers: { Accept: 'application/json' }, cache: 'no-store' })
      if (!response.ok) throw new Error(`HTTP ${String(response.status)}`)
      const value = readiness(await response.json() as unknown)
      if (value === null) throw new Error('invalid readiness response')
      setState({ phase: 'ready', value, error: '' })
    } catch (error: unknown) {
      setState({ phase: 'error', value: null, error: error instanceof Error ? error.message : String(error) })
    }
  }, [])
  useEffect(() => { void reload() }, [reload])
  return [state, reload]
}

function checkLabel(id: CheckId, t: ReadinessProps['t']): string {
  return t(`readiness.check.${id}` as const)
}

function checkDetail(check: ReadinessCheck, value: Readiness, t: ReadinessProps['t']): string {
  if (check.id === 'credential') {
    return t(check.tone === 'pass' ? 'readiness.detail.credential.ready' : 'readiness.detail.credential.missing')
  }
  if (check.id === 'service') {
    if (check.tone === 'warning') return t('readiness.detail.service.pending')
    if (check.tone === 'fail') return t('readiness.detail.service.offline')
    return value.serviceVersion === ''
      ? t('readiness.detail.service.ready')
      : t('readiness.detail.service.version', { version: value.serviceVersion })
  }
  if (check.id === 'access') {
    if (check.tone === 'warning') return t('readiness.detail.access.pending')
    return t(check.tone === 'pass' ? 'readiness.detail.access.ready' : 'readiness.detail.access.denied')
  }
  if (check.id === 'teams') {
    if (check.tone === 'warning') return t('readiness.detail.teams.pending')
    return t('readiness.detail.teams.count', { available: value.dispatchableTeamCount, total: value.teamCount })
  }
  if (check.tone === 'warning') return t('readiness.detail.runtimes.pending')
  return t('readiness.detail.runtimes.count', { available: value.healthyRuntimeCount, total: value.runtimeCount })
}

function StatusContents({ state, reload, t }: {
  readonly state: StatusState
  readonly reload: () => Promise<void>
  readonly t: ReadinessProps['t']
}) {
  if (state.phase === 'loading') return <div className={css.loading}>{t('readiness.loading')}</div>
  if (state.phase === 'error') return (
    <div className={css.error} role="alert">
      <strong>{t('readiness.unavailable')}</strong>
      <button type="button" onClick={() => { void reload() }}>{t('readiness.retry')}</button>
    </div>
  )
  const value = state.value
  return (
    <>
      <div className={css.summary} data-status={value.status}>
        <span className={css.statusMark} aria-hidden />
        <div>
          <strong>{t(value.status === 'ready' ? 'readiness.ready' : value.status === 'unconfigured' ? 'readiness.unconfigured' : 'readiness.attention')}</strong>
          <span>{t('readiness.summary', { teams: value.dispatchableTeamCount, runtimes: value.healthyRuntimeCount })}</span>
        </div>
        <button type="button" onClick={() => { void reload() }}>{t('readiness.retry')}</button>
      </div>
      <div className={css.checks}>
        {value.checks.map(check => (
          <div className={css.check} data-tone={check.tone} key={check.id}>
            <span className={css.checkMark} aria-hidden />
            <div><strong>{checkLabel(check.id, t)}</strong><span>{checkDetail(check, value, t)}</span></div>
          </div>
        ))}
      </div>
    </>
  )
}

type ReadinessProps = PropsLocale<'weave'>

/** Persistent Weave settings page with truthful host and runtime status. */
export function ReadinessSection({ t }: ReadinessProps & PropsRuntime<'settings.section'>) {
  const [state, reload] = useReadiness()
  const downloadReport = () => {
    const anchor = document.createElement('a')
    anchor.href = '/api/weave.pilot-report'
    anchor.download = 'weave-pilot-report.json'
    anchor.click()
  }
  return (
    <div className={css.section}>
      <header><span>{t('readiness.eyebrow')}</span><h2>{t('readiness.title')}</h2><p>{t('readiness.description')}</p></header>
      <StatusContents state={state} reload={reload} t={t} />
      <div className={css.firstRun}>
        <strong>{t('readiness.firstRun')}</strong>
        <ol><li>{t('readiness.step.request')}</li><li>{t('readiness.step.team')}</li><li>{t('readiness.step.observe')}</li><li>{t('readiness.step.deliver')}</li></ol>
      </div>
      <div className={css.dataCard}>
        <div><strong>{t('readiness.report')}</strong><span>{t('readiness.report.notice')}</span></div>
        <button type="button" onClick={downloadReport}>{t('readiness.report.download')}</button>
      </div>
      <p className={css.privacy}>{t('readiness.privacy')}</p>
    </div>
  )
}

/** One-time first-use dialog; unavailable installations remain skippable and honest. */
export function ReadinessOnboarding({ complete, openSection, t }: ReadinessProps & PropsRuntime<'settings.onboarding'>) {
  const [state, reload] = useReadiness()
  const acknowledged = typeof localStorage !== 'undefined' && localStorage.getItem(ACK_KEY) === 'done'
  useEffect(() => { if (acknowledged) complete() }, [acknowledged, complete])
  if (acknowledged) return null
  const finish = () => {
    localStorage.setItem(ACK_KEY, 'done')
    complete()
  }
  return (
    <div className={css.modalBackdrop} role="presentation">
      <section className={css.modal} role="dialog" aria-modal="true" aria-labelledby="weave-readiness-title">
        <span className={css.modalEyebrow}>{t('readiness.eyebrow')}</span>
        <h2 id="weave-readiness-title">{t('readiness.welcome')}</h2>
        <p>{t('readiness.welcome.notice')}</p>
        <StatusContents state={state} reload={reload} t={t} />
        <div className={css.modalActions}>
          <button type="button" onClick={() => { openSection('weave'); complete() }}>{t('readiness.openSettings')}</button>
          <button type="button" className={css.primary} onClick={finish}>{t(state.phase === 'ready' && state.value.status === 'ready' ? 'readiness.start' : 'readiness.later')}</button>
        </div>
      </section>
    </div>
  )
}
