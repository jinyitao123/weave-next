import { useState } from 'react'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import type { WorkTaskModel } from './work-task-model.ts'
import css from './WorkTaskPanel.module.css'

const VERIFICATION_KEYS = {
  pending: 'task.verification.pending',
  passed: 'task.verification.passed',
  failed: 'task.verification.failed',
  unknown: 'task.verification.unknown',
} as const

function verificationReason(reason: string, t: PropsLocale<'weave'>['t']): string {
  switch (reason) {
    case 'delivery_verification_unavailable': return t('task.verification.unavailable')
    case 'delivery_verification_unsupported': return t('task.verification.unavailable')
    case 'delivery_report_missing': return t('task.verification.reportMissing')
    case 'awaiting_delivery': return t('task.verification.awaitingDelivery')
    case 'delivery_contract_missing': case 'contract_missing': case 'output_contract_missing': return t('task.verification.requirementsMissing')
    case 'delivery_binding_mismatch': case 'delivery_report_mismatch': return t('task.verification.recordMismatch')
    case 'requirements_not_explicit': case 'unsupported_requirement': return t('task.verification.requirementsIncomplete')
    case 'explicit_requirements': return t('task.verification.requirementsRecorded')
    case 'required_file_missing': return t('task.verification.fileMissing')
    case 'required_file_collection_limited': return t('task.verification.collectionLimited')
    case 'file_conditions_satisfied': return t('task.verification.fileSatisfied')
    case 'file_content_type_mismatch': case 'file_digest_mismatch': case 'file_content_condition_missing': return t('task.verification.fileMismatch')
    case 'output_type_valid': return t('task.verification.outputValid')
    case 'output_type_invalid': return t('task.verification.outputInvalid')
    case 'file_source_missing': case 'output_source_missing': return t('task.verification.sourceMissing')
    case 'source_identity_invalid': case 'frozen_value_selection_invalid': return t('task.verification.sourceMismatch')
    case 'external_effects_scope_unverified': return t('task.verification.effectsUnverified')
    case 'verifier_unavailable': case 'external_effects_verifier_unavailable': return t('task.verification.checkUnavailable')
    case 'verifier_observation_unavailable': case 'verifier_evidence_missing': return t('task.verification.evidenceUnavailable')
    case 'verifier_result_invalid': return t('task.verification.checkInvalid')
    default: return reason || t('task.verification.reasonUnavailable')
  }
}

/** Execution, saved delivery checks, and the user's revision-specific assessment remain independent. */
export function WorkTaskDeliveryState({ model, executionLabel, compact = false, recheckDelivery, disabled = false, t }: PropsLocale<'weave'> & {
  readonly model: WorkTaskModel
  readonly executionLabel: string
  readonly compact?: boolean
  readonly recheckDelivery?: ((runId: string, deliveryRevisionId: string, contractDigest: string) => Promise<string | null>) | undefined
  readonly disabled?: boolean
}) {
  const [recheck, setRecheck] = useState({ pending: false, error: '' })
  const delivery = model.delivery
  const verification = delivery?.verificationStatus ?? 'unknown'
  const outcome = delivery?.revisionId && model.outcomeRevisionId === delivery.revisionId ? model.outcome : 'unrated'
  const reason = delivery?.reason ?? ''
  const canRecheck = delivery?.available === true && delivery.revisionId !== '' && delivery.contractDigest !== ''
  const submitRecheck = async () => {
    if (!canRecheck || recheckDelivery === undefined || disabled || recheck.pending) return
    const { revisionId, contractDigest } = delivery
    setRecheck({ pending: true, error: '' })
    try {
      const error = await recheckDelivery(model.runId, revisionId, contractDigest)
      setRecheck({ pending: false, error: error ?? '' })
    } catch { setRecheck({ pending: false, error: t('task.action.offline') }) }
  }
  return <section className={css.deliveryState} aria-label={t('task.results.states')}>
    <dl className={css.resultDimensions}>
      <div><dt>{t('task.results.execution')}</dt><dd>{executionLabel}</dd></div>
      <div><dt>{t('task.results.verification')}</dt><dd data-verification-status={verification}>{t(VERIFICATION_KEYS[verification])}</dd></div>
      <div><dt>{t('task.results.assessment')}</dt><dd>{t(outcome === 'adopted' ? 'task.assessment.adopted' : outcome === 'needs-revision' ? 'task.assessment.needsRevision' : 'task.assessment.unrated')}</dd></div>
    </dl>
    {compact ? null : <>
      <p className={css.muted}>{t('task.verification.notice')}</p>
      {delivery === undefined ? <p>{t('task.verification.unavailable')}</p>
        : reason === '' ? null : <p title={reason}>{verificationReason(reason, t)}</p>}
      {outcome === 'unrated' || model.outcomeNote === '' ? null : <p>{t('task.assessment.savedNote', { note: model.outcomeNote })}</p>}
      <details className={css.verificationDetails}>
        <summary>{t('task.verification.checks', { count: delivery?.checks.length ?? 0 })}</summary>
        {!delivery?.revisionId ? null : <p className={css.deliveryRevision}>{t('task.verification.revision', { revision: delivery.revisionId })}</p>}
        {delivery?.evidenceCompleteness === 'complete' ? null : <p>{t('task.verification.evidenceUnavailable')}</p>}
        {!delivery?.checks.length ? <p>{t('task.verification.noChecks')}</p> : <ul className={css.verificationChecks}>
          {delivery.checks.map(check => <li key={check.checkId}>
            <div><strong>{check.checkId}</strong>
              <span data-verification-status={check.status}>{t(VERIFICATION_KEYS[check.status])}</span></div>
            <p title={check.reason}>{verificationReason(check.reason, t)}</p>
          </li>)}
        </ul>}
        {recheckDelivery === undefined ? null : <>
          <p>{t('task.verification.recheckNotice')}</p>
          {canRecheck ? null : <p>{t('task.verification.recheckUnavailable')}</p>}
          {recheck.error === '' ? null : <p className={css.actionError} role="alert">{recheck.error}</p>}
          <button type="button" className={css.secondaryButton} disabled={!canRecheck || disabled || recheck.pending}
            onClick={() => { void submitRecheck() }}>{t(recheck.pending ? 'task.verification.rechecking' : 'task.verification.recheck')}</button>
        </>}
      </details>
    </>}
  </section>
}
