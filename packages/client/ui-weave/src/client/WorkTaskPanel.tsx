import { useEffect, useState } from 'react'
import type { CSSProperties } from 'react'
import type { InjectFace, PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import type { WorkTaskDeliverable, WorkTaskMemberStatus, WorkTaskRuntime, WorkTaskStatus } from './work-task-model.ts'
import { projectedWorkTask, workTaskFactsStale, workTaskHasUserVisibleCompletenessWarning, workTaskModel } from './work-task-model.ts'
import css from './WorkTaskPanel.module.css'

interface WorkTaskInjected {
  readonly openDetails: () => void
  readonly selectTeam?: (teamId: string, teamName: string) => Promise<void>
  readonly stopRun?: (runId: string) => Promise<string | null>
  readonly rerun?: (runId: string, brief: string) => Promise<string | null>
  readonly requestCorrection?: (runId: string, targetKind: 'team' | 'member', targetMemberId: string, instruction: string) => Promise<string | null>
  readonly confirmCorrection?: (runId: string, correctionId: string, disposition: 'apply' | 'discard') => Promise<string | null>
  readonly assessOutcome?: (runId: string, outcome: 'adopted' | 'needs-revision', note: string) => Promise<string | null>
}

type PanelProps =
  & PropsRuntime<'conversation.details.summary'>
  & InjectFace<WorkTaskInjected>
  & PropsLocale<'weave'>

type HeaderProps =
  & PropsRuntime<'conversation.session.header.actions'>
  & InjectFace<WorkTaskInjected>
  & PropsLocale<'weave'>

const STATUS_KEYS = {
  preparing: 'task.status.preparing',
  queued: 'task.status.queued',
  running: 'task.status.running',
  waiting: 'task.status.waiting',
  stopping: 'task.status.stopping',
  completed: 'task.status.completed',
  failed: 'task.status.failed',
  stopped: 'task.status.stopped',
} as const

function statusKey(status: WorkTaskStatus): typeof STATUS_KEYS[WorkTaskStatus] {
  return STATUS_KEYS[status]
}

function runtimeDisplayStatus(runtime: WorkTaskRuntime, runtimes: readonly WorkTaskRuntime[], members: ReturnType<typeof workTaskModel>['members']): WorkTaskStatus {
  const activeMember = members.find(member => member.status === 'running')
  if (activeMember === undefined) return runtime.status
  if (runtimes.length === 1 || activeMember.runtime === runtime.name || activeMember.runtime.startsWith(`${runtime.name} · `)) {
    return 'running'
  }
  return runtime.status
}

const MEMBER_STATUS_KEYS = {
  pending: 'task.member.status.pending',
  running: 'task.member.status.running',
  'partially-completed': 'task.member.status.partiallyCompleted',
  completed: 'task.member.status.completed',
  failed: 'task.member.status.failed',
  stopped: 'task.member.status.stopped',
  'not-recorded': 'task.member.status.notRecorded',
} as const

function memberStatusKey(status: WorkTaskMemberStatus): typeof MEMBER_STATUS_KEYS[WorkTaskMemberStatus] {
  return MEMBER_STATUS_KEYS[status]
}

function durationLabel(milliseconds: number, t: PanelProps['t']): string {
  if (milliseconds < 1_000) return t('task.duration.milliseconds', { count: milliseconds })
  const seconds = Math.round(milliseconds / 100) / 10
  if (seconds < 60) return t('task.duration.seconds', { count: seconds })
  return t('task.duration.minutes', { minutes: Math.floor(seconds / 60), seconds: Math.round(seconds % 60) })
}

function timestampLabel(value: string): string {
  if (value === '') return ''
  const timestamp = new Date(value)
  return Number.isNaN(timestamp.getTime()) ? '' : timestamp.toLocaleString()
}

function downloadDeliverable(item: WorkTaskDeliverable): void {
  const blob = new Blob([item.content], { type: `${item.contentType};charset=utf-8` })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = item.title || 'weave-deliverable'
  anchor.click()
  URL.revokeObjectURL(url)
}

function DeliverableItems({ items, t }: { readonly items: readonly WorkTaskDeliverable[]; readonly t: PanelProps['t'] }) {
  const latestIds = new Set<string>()
  for (const item of items) {
    if (!items.some(candidate => candidate.title === item.title && latestIds.has(candidate.id))) latestIds.add(item.id)
  }
  return (
    <div className={css.deliverableList}>
      {items.map((item) => {
        const revisions = items.filter(candidate => candidate.title === item.title).length
        const timestamp = timestampLabel(item.createdAt)
        const revisionLabel = revisions < 2 ? '' : t(latestIds.has(item.id)
          ? 'task.deliverables.currentRevision'
          : 'task.deliverables.previousRevision')
        const metadata = [revisionLabel, timestamp].filter(Boolean).join(' · ')
        return (
          <details className={css.deliverable} data-kind={item.kind} key={item.id} open={item.kind === 'final'}>
            <summary>
              <span className={css.fileMark} aria-hidden />
              <span className={css.deliverableIdentity}>
                <strong>{item.title}</strong>
                <span>{item.contentType}</span>
                {metadata === '' ? null : <span>{metadata}</span>}
              </span>
              <span className={css.deliverableKind}>{t(item.kind === 'final' ? 'task.deliverables.final' : 'task.deliverables.stage')}</span>
            </summary>
            <div className={css.deliverableBody}>
              {item.preview === ''
                ? <p className={css.muted}>{t('task.deliverables.noPreview')}</p>
                : <pre>{item.preview}</pre>}
              <div className={css.deliverableActions}>
                {item.content === '' || item.truncated ? null : (
                  <button type="button" onClick={() => downloadDeliverable(item)}>{t('task.deliverables.download')}</button>
                )}
                {item.truncated ? <span>{t('task.deliverables.truncated')}</span> : null}
              </div>
            </div>
          </details>
        )
      })}
    </div>
  )
}

/** Compact live status beside the Session title. */
export function WorkTaskHeader({ useChat, useProjection, openDetails, t }: HeaderProps) {
  const conversationModel = useChat(snapshot => workTaskModel(snapshot.nodes.values()))
  const projection = useProjection('workTask')
  const model = projectedWorkTask(conversationModel, projection)
  if (!model.detected) return null
  const progress = model.totalStages > 0
    ? t('task.progress.count', { completed: model.completedStages, total: model.totalStages })
    : model.completedStages > 0
      ? t('task.progress.completedCount', { completed: model.completedStages })
      : null
  return (
    <button className={css.headerPill} type="button" onClick={openDetails} aria-label={t('task.open')}>
      <span className={css.statusDot} data-status={model.status} aria-hidden />
      <span>{t(statusKey(model.status))}</span>
      {progress === null ? null : <span className={css.headerProgress}>{progress}</span>}
    </button>
  )
}

/** Persistent Weave task, team, runtime, and deliverable projection. */
export function WorkTaskPanel({
  useChat, useProjection, useSessions, sessionId, openDetails, selectTeam,
  stopRun, rerun, requestCorrection, confirmCorrection, assessOutcome, t,
}: PanelProps) {
  const conversationModel = useChat(snapshot => workTaskModel(snapshot.nodes.values()))
  const projection = useProjection('workTask')
  const model = projectedWorkTask(conversationModel, projection)
  const taskTitle = useSessions(snapshot => snapshot.byId[sessionId]?.title ?? '')
  const [confirmStop, setConfirmStop] = useState(false)
  const [revisionOpen, setRevisionOpen] = useState(false)
  const [revisedBrief, setRevisedBrief] = useState('')
  const [actionPending, setActionPending] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)
  const [selectingTeamId, setSelectingTeamId] = useState('')
  const [correctionOpen, setCorrectionOpen] = useState(false)
  const [correctionTarget, setCorrectionTarget] = useState('team')
  const [correctionInstruction, setCorrectionInstruction] = useState('')
  const [assessmentNote, setAssessmentNote] = useState('')

  useEffect(() => {
    if (model.detected) openDetails()
  }, [model.detected, openDetails])

  if (!model.detected) return null
  const progressPercent = model.totalStages === 0
    ? 0
    : Math.min(100, Math.round(model.completedStages / model.totalStages * 100))
  const finalDeliverables = model.deliverables.filter(item => item.kind === 'final')
  const stageDeliverables = model.deliverables.filter(item => item.kind === 'stage')
  const terminal = model.status === 'completed' || model.status === 'failed' || model.status === 'stopped'
  const stale = workTaskFactsStale(model.status, model.observedAt)
  const incomplete = workTaskHasUserVisibleCompletenessWarning(model)
  const activeCorrection = model.corrections.find(item => item.status === 'requested' || item.status === 'ready' || item.status === 'confirmed')
  const displayedRuntimes = model.runtimes.map(runtime => ({
    ...runtime,
    status: runtimeDisplayStatus(runtime, model.runtimes, model.members),
  }))
  const activeRuntimeCount = displayedRuntimes.filter(runtime => runtime.status === 'running').length
  const visibleRuntimeCount = model.runtimes.length === 0
    ? model.members.filter(member => member.runtime !== '').length
    : model.runtimes.length

  const submitStop = async () => {
    if (stopRun === undefined || actionPending) return
    setActionPending(true)
    setActionError(null)
    const error = await stopRun(model.runId)
    setActionPending(false)
    if (error !== null) setActionError(error)
    else setConfirmStop(false)
  }
  const submitRerun = async () => {
    if (rerun === undefined || actionPending || revisedBrief.trim() === '') return
    setActionPending(true)
    setActionError(null)
    const error = await rerun(model.runId, revisedBrief.trim())
    setActionPending(false)
    if (error !== null) setActionError(error)
    else setRevisionOpen(false)
  }
  const submitCorrection = async () => {
    if (requestCorrection === undefined || actionPending || correctionInstruction.trim() === '') return
    setActionPending(true)
    setActionError(null)
    const targetKind = correctionTarget === 'team' ? 'team' : 'member'
    const error = await requestCorrection(model.runId, targetKind, targetKind === 'member' ? correctionTarget : '', correctionInstruction.trim())
    setActionPending(false)
    if (error !== null) setActionError(error)
    else { setCorrectionOpen(false); setCorrectionInstruction('') }
  }
  const submitCorrectionDecision = async (disposition: 'apply' | 'discard') => {
    if (confirmCorrection === undefined || actionPending || activeCorrection?.status !== 'ready') return
    setActionPending(true)
    setActionError(null)
    const error = await confirmCorrection(model.runId, activeCorrection.correctionId, disposition)
    setActionPending(false)
    if (error !== null) setActionError(error)
  }
  const submitTeamSelection = async (teamId: string, teamName: string) => {
    if (selectTeam === undefined || selectingTeamId !== '') return
    setSelectingTeamId(teamId)
    setActionError(null)
    try { await selectTeam(teamId, teamName) }
    catch (error) { setActionError(error instanceof Error ? error.message : String(error)) }
    finally { setSelectingTeamId('') }
  }
  const submitAssessment = async (outcome: 'adopted' | 'needs-revision') => {
    if (assessOutcome === undefined || actionPending) return
    setActionPending(true)
    setActionError(null)
    const error = await assessOutcome(model.runId, outcome, assessmentNote.trim())
    setActionPending(false)
    if (error !== null) setActionError(error)
  }

  return (
    <div className={css.panel} data-weave-work-task data-status={model.status}>
      <section className={css.hero}>
        <div className={css.eyebrow}>{t('task.workScene')}</div>
        <div className={css.taskTitle}>{taskTitle}</div>
        <div className={css.statusLine}>
          <span className={css.statusDot} data-status={model.status} aria-hidden />
          <strong>{t(statusKey(model.status))}</strong>
        </div>
        {model.brief === '' ? null : <p className={css.brief}>{model.brief}</p>}
        <div className={css.truthLine}>
          {stale ? <span data-tone="warning">{t('task.freshness.stale')}</span> : <span>{t('task.freshness.current')}</span>}
          {incomplete ? <span data-tone="warning">{t('task.completeness.partial')}</span> : null}
        </div>
        {model.pendingAction === null ? null : (
          <div className={css.pendingAction} role="status">
            {t(model.pendingAction.kind === 'stop'
              ? 'task.action.stopPending'
              : model.pendingAction.kind === 'rerun'
                ? 'task.action.rerunPending'
                : model.pendingAction.kind === 'correction-request'
                  ? 'task.action.correctionPending'
                  : 'task.action.correctionConfirmPending')}
          </div>
        )}
      </section>

      <section className={css.sceneBoard} aria-label={t('task.workScene')}>
        <div className={css.sceneCard}>
          <span>{t('task.team')}</span>
          <strong>{model.teamName || t('task.team.pending')}</strong>
          {model.workflowName === '' ? null : <small>{t('task.workflow')} · {model.workflowName}</small>}
        </div>
        <div className={css.sceneCard}>
          <span>{t('task.progress')}</span>
          <strong>{model.totalStages > 0
            ? t('task.progress.count', { completed: model.completedStages, total: model.totalStages })
            : t('task.progress.completedCount', { completed: model.completedStages })}</strong>
          {model.latestStage === '' ? null : <small>{model.latestStage}</small>}
        </div>
        <div className={css.sceneCard}>
          <span>{t('task.members')}</span>
          <strong>{t('task.members.count', { count: model.members.length })}</strong>
          <small>{t('task.runtimes.summary', { active: activeRuntimeCount, total: visibleRuntimeCount })}</small>
        </div>
        <div className={css.sceneCard}>
          <span>{t('task.deliverables')}</span>
          <strong>{t('task.deliverables.count', { count: model.deliverableCount })}</strong>
          <small>{finalDeliverables.length === 0 ? t('task.deliverables.noFinal') : t('task.deliverables.finalReady', { count: finalDeliverables.length })}</small>
        </div>
      </section>

      {model.runId !== '' || model.teamCandidates.length === 0 ? null : (
        <section className={css.teamChooser} aria-label={t('task.teamChooser')}>
          <div className={css.sectionHeader}>
            <span>{t('task.teamChooser')}</span>
            <span>{t('task.teamChooser.count', { count: model.teamCandidates.length })}</span>
          </div>
          <p className={css.muted}>{t('task.teamChooser.notice')}</p>
          <div className={css.teamCandidateList}>
            {model.teamCandidates.map((team) => {
              const dispatchable = team.status === 'active' && team.workflowAvailable
              return (
                <div className={css.teamCandidate} data-dispatchable={dispatchable || undefined} key={team.teamId}>
                  <div><strong>{team.name}</strong><span>{dispatchable ? t('teamList.dispatchable') : t('teamList.noWorkflow')}</span></div>
                  {team.objective === '' ? null : <p>{team.objective}</p>}
                  {!dispatchable || selectTeam === undefined ? null : (
                    <button type="button" className={css.primaryButton} disabled={selectingTeamId !== ''}
                      onClick={() => { void submitTeamSelection(team.teamId, team.name) }}>
                      {selectingTeamId === team.teamId ? t('task.teamChooser.selecting') : t('teamList.select')}
                    </button>
                  )}
                </div>
              )
            })}
          </div>
        </section>
      )}

      {model.runId === '' || terminal || model.status === 'stopping' ? null : (
        <section className={css.controlSection} aria-label={t('task.controls')}>
          {confirmStop ? (
            <div className={css.confirmBox}>
              <strong>{t('task.stop.confirmTitle')}</strong>
              <span>{t('task.stop.confirmBody')}</span>
              <div className={css.controlActions}>
                <button type="button" className={css.secondaryButton} onClick={() => { setConfirmStop(false) }}>{t('task.cancel')}</button>
                <button type="button" className={css.dangerButton} disabled={actionPending} onClick={() => { void submitStop() }}>{t('task.stop.confirm')}</button>
              </div>
            </div>
          ) : <button type="button" className={css.secondaryButton} onClick={() => { setConfirmStop(true) }}>{t('task.stop')}</button>}
        </section>
      )}

      {!terminal ? null : (
        <section className={css.controlSection} aria-label={t('task.rerun')}>
          {revisionOpen ? (
            <div className={css.revisionBox}>
              <label htmlFor={`weave-revision-${sessionId}`}>{t('task.rerun.brief')}</label>
              <textarea
                id={`weave-revision-${sessionId}`}
                value={revisedBrief}
                onChange={(event) => { setRevisedBrief(event.currentTarget.value) }}
                placeholder={t('task.rerun.placeholder')}
              />
              <span>{t('task.rerun.notice')}</span>
              <div className={css.controlActions}>
                <button type="button" className={css.secondaryButton} onClick={() => { setRevisionOpen(false) }}>{t('task.cancel')}</button>
                <button type="button" className={css.primaryButton} disabled={actionPending || revisedBrief.trim() === ''} onClick={() => { void submitRerun() }}>{t('task.rerun.confirm')}</button>
              </div>
            </div>
          ) : (
            <button type="button" className={css.primaryButton} onClick={() => { setRevisedBrief(model.brief); setRevisionOpen(true) }}>{t('task.rerun')}</button>
          )}
        </section>
      )}
      {actionError === null ? null : <div className={css.actionError} role="alert">{actionError}</div>}
      {model.actionError === '' ? null : <div className={css.actionError} role="alert">{model.actionError}</div>}

      {activeCorrection?.status === 'ready' ? (
        <section className={css.correctionPlan} aria-label={t('task.correction.impact')}>
          <div className={css.sectionHeader}><span>{t('task.correction.impact')}</span><span>{t('task.correction.ready')}</span></div>
          <strong>{activeCorrection.instruction}</strong>
          <div className={css.impactRoute}>
            <span>{t('task.correction.safePoint')} <code>{activeCorrection.safeNodeId}</code></span>
            <span>{t('task.correction.restartAt')} <code>{activeCorrection.restartNodeId}</code></span>
          </div>
          <div className={css.impactColumns}>
            <div><span>{t('task.correction.affected')}</span>{activeCorrection.affectedNodeIds.map(nodeId => <code key={nodeId}>{nodeId}</code>)}</div>
            <div><span>{t('task.correction.preserved')}</span>{activeCorrection.preservedNodeIds.length === 0 ? <em>{t('task.correction.none')}</em> : activeCorrection.preservedNodeIds.map(nodeId => <code key={nodeId}>{nodeId}</code>)}</div>
          </div>
          <div className={css.controlActions}>
            <button type="button" className={css.secondaryButton} disabled={actionPending} onClick={() => { void submitCorrectionDecision('discard') }}>{t('task.correction.discard')}</button>
            <button type="button" className={css.primaryButton} disabled={actionPending} onClick={() => { void submitCorrectionDecision('apply') }}>{t('task.correction.apply')}</button>
          </div>
        </section>
      ) : activeCorrection?.status === 'requested' || activeCorrection?.status === 'confirmed' ? (
        <div className={css.pendingAction} role="status">{t(activeCorrection.status === 'requested' ? 'task.correction.awaitingSafePoint' : 'task.correction.resuming')}</div>
      ) : null}

      {model.runId === '' || !(model.status === 'running' || (model.status === 'waiting' && model.members.some(member => member.status === 'running'))) || activeCorrection !== undefined ? null : (
        <section className={css.controlSection} aria-label={t('task.correction')}>
          {correctionOpen ? (
            <div className={css.correctionComposer}>
              <label htmlFor={`weave-correction-target-${sessionId}`}>{t('task.correction.target')}</label>
              <select id={`weave-correction-target-${sessionId}`} value={correctionTarget} onChange={(event) => { setCorrectionTarget(event.currentTarget.value) }}>
                <option value="team">{t('task.correction.team')}</option>
                {model.members.map(member => <option key={member.agentId} value={member.agentId}>{member.name}</option>)}
              </select>
              <label htmlFor={`weave-correction-instruction-${sessionId}`}>{t('task.correction.instruction')}</label>
              <textarea id={`weave-correction-instruction-${sessionId}`} value={correctionInstruction}
                onChange={(event) => { setCorrectionInstruction(event.currentTarget.value) }} placeholder={t('task.correction.placeholder')} />
              <span>{t('task.correction.notice')}</span>
              <div className={css.controlActions}>
                <button type="button" className={css.secondaryButton} onClick={() => { setCorrectionOpen(false) }}>{t('task.cancel')}</button>
                <button type="button" className={css.primaryButton} disabled={actionPending || correctionInstruction.trim() === ''} onClick={() => { void submitCorrection() }}>{t('task.correction.submit')}</button>
              </div>
            </div>
          ) : <button type="button" className={css.primaryButton} onClick={() => { setCorrectionTarget('team'); setCorrectionOpen(true) }}>{t('task.correction')}</button>}
        </section>
      )}

      <section className={css.section} aria-label={t('task.deliverables')}>
        <div className={css.sectionHeader}>
          <span>{t('task.deliverables')}</span>
          <span>{model.deliverableCount}</span>
        </div>
        {model.deliverables.length === 0 ? <p className={css.muted}>{t('task.deliverables.empty')}</p> : (
          <div className={css.deliverableGroups}>
            {finalDeliverables.length === 0 ? null : (
              <div className={css.deliverableGroup}>
                <div className={css.deliverableGroupTitle}>{t('task.deliverables.finalGroup')}</div>
                <DeliverableItems items={finalDeliverables} t={t} />
              </div>
            )}
            {stageDeliverables.length === 0 ? null : (
              <details className={css.stageDeliverables}>
                <summary>{t('task.deliverables.stageGroup', { count: stageDeliverables.length })}</summary>
                <DeliverableItems items={stageDeliverables} t={t} />
              </details>
            )}
          </div>
        )}
      </section>

      {model.status !== 'completed' || finalDeliverables.length === 0 ? null : (
        <section className={css.assessment} aria-label={t('task.assessment')}>
          <div className={css.sectionHeader}>
            <span>{t('task.assessment')}</span>
            {model.outcome === 'unrated' ? null : (
              <span data-outcome={model.outcome}>{t(model.outcome === 'adopted' ? 'task.assessment.adopted' : 'task.assessment.needsRevision')}</span>
            )}
          </div>
          <p>{t('task.assessment.notice')}</p>
          <textarea value={assessmentNote} onChange={(event) => { setAssessmentNote(event.currentTarget.value) }}
            placeholder={model.outcomeNote || t('task.assessment.placeholder')} />
          <div className={css.controlActions}>
            <button type="button" className={css.secondaryButton} disabled={actionPending}
              onClick={() => { void submitAssessment('needs-revision') }}>{t('task.assessment.needsRevision')}</button>
            <button type="button" className={css.primaryButton} disabled={actionPending}
              onClick={() => { void submitAssessment('adopted') }}>{t('task.assessment.adopted')}</button>
          </div>
        </section>
      )}

      <section className={css.section} aria-label={t('task.progress')}>
        <div className={css.sectionHeader}>
          <span>{t('task.progress')}</span>
          {model.totalStages > 0
            ? <span>{t('task.progress.count', { completed: model.completedStages, total: model.totalStages })}</span>
            : null}
        </div>
        {model.totalStages > 0 ? (
          <div className={css.progressTrack} aria-valuemin={0} aria-valuemax={100} aria-valuenow={progressPercent} role="progressbar">
            <span style={{ '--weave-progress': `${progressPercent}%` } as CSSProperties} />
          </div>
        ) : <p className={css.muted}>{t(
          model.completedStages === 0
            ? 'task.progress.pending'
            : terminal
              ? 'task.progress.recordedComplete'
              : 'task.progress.partial',
          { completed: model.completedStages },
        )}</p>}
        {model.latestStage === '' ? null : (
          <div className={css.fact}>
            <span>{t(terminal ? 'task.lastStage' : 'task.currentStage')}</span>
            <strong>{model.latestStage}</strong>
          </div>
        )}
        {model.stages.length === 0 ? null : (
          <ol className={css.stages}>
            {model.stages.map((stage, index) => (
              <li key={`${stage.name}-${index}`} data-status={stage.status}>
                <span className={css.stageMark} aria-hidden />
                <span>{stage.name}</span>
              </li>
            ))}
          </ol>
        )}
      </section>

      <section className={css.section} aria-label={t('task.team')}>
        <div className={css.sectionHeader}><span>{t('task.team')}</span></div>
        <div className={css.primaryCard}>
          <strong>{model.teamName || t('task.team.pending')}</strong>
          {model.workflowName === '' ? null : <span>{t('task.workflow')} · {model.workflowName}</span>}
        </div>
      </section>

      <section className={css.section} aria-label={t('task.members')}>
        <div className={css.sectionHeader}><span>{t('task.members')}</span><span>{model.members.length}</span></div>
        {model.members.length === 0 ? <p className={css.muted}>{t(terminal ? 'task.members.notRecorded' : 'task.members.pending')}</p> : (
          <div className={css.memberList}>
            {model.members.map(member => (
              <details className={css.member} key={member.agentId}>
                <summary>
                  <span className={css.memberStatus} data-status={member.status} aria-hidden />
                  <span className={css.memberIdentity}>
                    <strong>{member.name}</strong>
                    <span>{member.duty === '' ? t(member.role === 'lead' ? 'task.member.lead' : 'task.member.worker') : member.duty}</span>
                  </span>
                  <span className={css.memberRuntimeChip}>{member.runtime || t('task.member.runtimeUnknown')}</span>
                  <span className={css.memberState}>{t(memberStatusKey(member.status))}</span>
                </summary>
                <div className={css.memberBody}>
                  {member.duty === '' ? null : <p>{member.duty}</p>}
                  <div className={css.memberRuntime}>
                    <span>{t('task.member.runtime')}</span>
                    <strong>{member.runtime || t('task.member.runtimeUnknown')}</strong>
                  </div>
                  {model.status !== 'running' || activeCorrection !== undefined ? null : (
                    <button type="button" className={css.memberCorrectionButton} onClick={() => {
                      setCorrectionTarget(member.agentId); setCorrectionOpen(true)
                    }}>{t('task.correction.member')}</button>
                  )}
                  {member.stages.length === 0 ? <p className={css.muted}>{t('task.member.stages.empty')}</p> : (
                    <ol className={css.memberStages}>
                      {member.stages.map(stage => (
                        <li key={stage.nodeId} data-status={stage.status}>
                          <div className={css.memberStageHeading}>
                            <strong>{stage.name}</strong>
                            <span>{t(memberStatusKey(stage.status))}</span>
                          </div>
                          {stage.startedAt === '' && stage.durationMs === 0 && stage.toolCalls === 0 ? null : (
                            <div className={css.memberStageMetrics}>
                              {stage.startedAt === '' ? null : <span>{t('task.member.started')} <time dateTime={stage.startedAt}>{new Date(stage.startedAt).toLocaleTimeString()}</time></span>}
                              {stage.durationMs === 0 ? null : <span>{t('task.member.duration')} {durationLabel(stage.durationMs, t)}</span>}
                              <span>{t('task.member.tools')} {stage.toolCalls}</span>
                            </div>
                          )}
                          {stage.tools.length === 0 ? null : (
                            <div className={css.toolTimeline}>
                              <span>{t('task.member.toolActivity')}</span>
                              {stage.tools.map((tool, toolIndex) => (
                                <div key={`${tool.callId}-${toolIndex}`} data-status={tool.status}>
                                  <span className={css.toolState} aria-hidden />
                                  <code>{tool.name}</code>
                                  <small>{t(tool.status === 'running' ? 'task.member.tool.running' : tool.status === 'error' ? 'task.member.tool.error' : 'task.member.tool.ok')}</small>
                                  {tool.input === '' && tool.output === '' ? null : (
                                    <details className={css.toolDetail}>
                                      <summary>{t('task.member.tool.details')}</summary>
                                      {tool.input === '' ? null : <><span>{t('task.member.tool.input')}</span><pre>{tool.input}</pre></>}
                                      {tool.output === '' ? null : <><span>{t('task.member.tool.output')}</span><pre>{tool.output}</pre></>}
                                    </details>
                                  )}
                                </div>
                              ))}
                            </div>
                          )}
                          {stage.inputs.length === 0 ? null : (
                            <div className={css.memberStageFacts}>
                              <span>{t('task.member.inputs')}</span>
                              {stage.inputs.map(input => (
                                <div className={css.inputFact} key={input.name}>
                                  <code>{input.name}: {input.source}{input.nodeId === '' ? '' : ` / ${input.nodeId}`}{input.path === '' ? '' : ` ${input.path}`}</code>
                                  {input.summary === '' ? null : <span>{input.summary}</span>}
                                </div>
                              ))}
                            </div>
                          )}
                          {stage.outputRefs.length === 0 ? null : (
                            <div className={css.memberStageFacts}>
                              <span>{t('task.member.outputs')}</span>
                              {stage.outputRefs.map(outputRef => (
                                <code key={outputRef}>{model.deliverables.find(item => item.id === outputRef)?.title ?? outputRef}</code>
                              ))}
                            </div>
                          )}
                        </li>
                      ))}
                    </ol>
                  )}
                </div>
              </details>
            ))}
          </div>
        )}
      </section>

      <section className={css.section} aria-label={t('task.runtimes')}>
        <div className={css.sectionHeader}><span>{t('task.runtimes')}</span><span>{model.runtimes.length}</span></div>
        {model.runtimes.length === 0 ? <p className={css.muted}>{t(terminal ? 'task.runtimes.notRecorded' : 'task.runtimes.empty')}</p> : (
          <div className={css.runtimeList}>
            {displayedRuntimes.map((runtime, index) => (
              <div className={css.runtime} key={`${runtime.name}-${index}`}>
                <span className={css.statusDot} data-status={runtime.status} aria-hidden />
                <span className={css.runtimeIdentity}>
                  <strong>{runtime.name}</strong>
                  <span>{runtime.detail || t('task.runtime.detailPending')}</span>
                </span>
                <span className={css.runtimeStatus}>{t(statusKey(runtime.status))}</span>
              </div>
            ))}
          </div>
        )}
      </section>

      {model.blocker === 'none' ? null : (
        <section className={css.issue} role="status">
          <strong>{t('task.blocker')}</strong>
          <span>{t(model.blocker === 'runtime-missing'
            ? 'task.blocker.runtimeMissing'
            : model.blocker === 'failed'
              ? 'task.blocker.failed'
              : 'task.blocker.queued')}</span>
        </section>
      )}

      {model.humanTaskCount === 0 ? null : (
        <section className={css.attention}>
          <strong>{t('task.human')}</strong>
          <span>{t('task.human.count', { count: model.humanTaskCount })}</span>
        </section>
      )}

      {model.attempts.length < 2 ? null : (
        <section className={css.section} aria-label={t('task.attempts')}>
          <div className={css.sectionHeader}><span>{t('task.attempts')}</span><span>{model.attempts.length}</span></div>
          <ol className={css.attemptList}>
            {model.attempts.map((attempt, index) => (
              <li key={attempt.clientRequestId || attempt.runId} data-current={attempt.runId === model.runId || undefined}>
                <span>{t('task.attempt', { index: index + 1 })}</span>
                <strong>{t(statusKey(attempt.status))}</strong>
                <small>{attempt.brief}</small>
              </li>
            ))}
          </ol>
        </section>
      )}

      {model.runId === '' ? null : (
        <details className={css.diagnostics}>
          <summary>{t('task.diagnostics')}</summary>
          <div><span>{t('task.runId')}</span><code>{model.runId}</code></div>
        </details>
      )}
    </div>
  )
}
