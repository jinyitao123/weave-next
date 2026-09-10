import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { MarkdownText, Tooltip, IconChevronLeftOutline14, IconChevronRightOutline14, IconDownloadOutline16, IconFullscreenOutline16, IconPanelLeftOutline16, IconCloseOutline16 } from '@deepseek-ai/dsh-client-ui-primitives'
import type { CSSProperties } from 'react'
import type { InjectFace, PropsLocale, PropsRuntime, PropsStore } from '@deepseek-ai/dsh-client-ui-slots'
import type { WorkTaskDeliverable, WorkTaskMemberStage, WorkTaskMemberStatus, WorkTaskRuntime, WorkTaskStatus } from './work-task-model.ts'
import { projectedWorkTask, workTaskFactsStale, workTaskHasFinalDeliverable, workTaskHasUserVisibleCompletenessWarning, workTaskModel } from './work-task-model.ts'
import css from './WorkTaskPanel.module.css'
import { HumanTaskForm } from './HumanTaskForm.tsx'
import { DeliverablePreview } from './DeliverablePreview.tsx'
import { deliverablePreviewLabels } from './preview-locales.ts'
import { PublicUpdates } from './PublicUpdates.tsx'
import { WorkFileIcon, WorkSceneOverview } from './WorkSceneOverview.tsx'
import type { WorkTaskMemberReference } from './member-reference.ts'
import type { createWorkTaskViewStore } from './view-store.ts'
import { WorkTaskDeliveryState } from './WorkTaskDeliveryState.tsx'

interface WorkTaskInjected {
  readonly openDetails: () => void
  readonly activateScene?: () => void
  readonly returnToConversation?: () => void
  readonly beginMemberAdjustment?: (member: WorkTaskMemberReference) => Promise<void>
  readonly expandDetails?: () => void
  readonly requestDelivery?: (runId: string) => Promise<void>
  readonly selectTeam?: (teamId: string, teamName: string) => Promise<void>
  readonly stopRun?: (runId: string) => Promise<string | null>
  readonly rerun?: (runId: string, brief: string) => Promise<string | null>
  readonly retryStage?: (runId: string, nodeId: string, authorizedTotalRounds?: number) => Promise<string | null>
  readonly requestCorrection?: (runId: string, targetKind: 'team' | 'member', targetMemberId: string, instruction: string) => Promise<string | null>
  readonly confirmCorrection?: (runId: string, correctionId: string, disposition: 'apply' | 'discard') => Promise<string | null>
  readonly completeHumanTask?: (runId: string, interactionId: string, payload: unknown) => Promise<string | null>
  readonly assessOutcome?: (runId: string, deliveryRevisionId: string, outcome: 'adopted' | 'needs-revision', note: string) => Promise<string | null>
  readonly recheckDelivery?: (runId: string, deliveryRevisionId: string, contractDigest: string) => Promise<string | null>
}

type PanelProps =
  & PropsRuntime<'conversation.details.summary'>
  & InjectFace<WorkTaskInjected>
  & PropsLocale<'weave'>
  & PropsStore<ReturnType<typeof createWorkTaskViewStore>>
  & { readonly presentation?: 'conversation' }

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

function taskStatusKey(model: ReturnType<typeof workTaskModel>) {
  const displayKeys = {
    'preparing': 'task.state.preparing',
    'queued': 'task.state.queued',
    'running': 'task.state.running',
    'waiting': 'task.state.waiting',
    'stopping': 'task.state.stopping',
    'completed': 'task.state.completed',
    'failed': 'task.state.failed',
    'stopped': 'task.state.stopped',
    'outputsMissing': 'task.state.outputsMissing',
    'stopUnconfirmed': 'task.state.stopUnconfirmed',
    'human': 'task.state.human',
    'correction': 'task.state.correction',
    'runtimeStop': 'task.state.runtimeStop',
    'retryable': 'task.state.retryable',
    'parallel': 'task.state.parallel',
    'fanout': 'task.state.fanout',
    'timer': 'task.state.timer',
    'runtime': 'task.state.runtime',
    'buildSubmitting': 'task.state.buildSubmitting',
    'buildBuilding': 'task.state.buildBuilding',
    'buildReady': 'task.state.buildReady',
    'buildFailed': 'task.state.buildFailed',
    'buildUnknown': 'task.state.buildUnknown',
  } as const
  if (model.status === 'waiting' && model.members.some(member => member.stages.some(stage => stage.nodeId === model.waitNodeId && stage.budgetPause !== undefined))) return 'task.budget.title'
  if (model.displayState !== undefined && Object.hasOwn(displayKeys, model.displayState)) {
    return displayKeys[model.displayState as keyof typeof displayKeys]
  }
  if (model.pendingAction?.kind === 'stop') return 'task.status.stopping'
  if (model.actionError === 'stop_unconfirmed') return 'task.status.stopUnconfirmed'
  if (model.status === 'completed' && !workTaskHasFinalDeliverable(model)) return 'task.status.outputsMissing'
  if (model.status === 'waiting' && model.waitKind !== '') {
    if (interruptedInfrastructureStages(model).some(item => runtimeStopPending(item.stage))) return 'task.wait.runtimeStop'
    switch (model.waitKind) {
      case 'timer': return 'task.wait.timer'
      case 'fanout': return 'task.wait.fanout'
      case 'human': return 'task.wait.human'
      case 'correction': return 'task.wait.correction'
      case 'runtime': return 'task.wait.runtime'
    }
  }
  return STATUS_KEYS[model.status]
}

function statusKey(status: WorkTaskStatus): typeof STATUS_KEYS[WorkTaskStatus] {
  return STATUS_KEYS[status]
}

function stageFailureKey(stage: WorkTaskMemberStage, retryable: boolean) {
  if (stage.failureReason === 'tool outcome requires reconciliation before continuing') return 'task.failure.reason.toolUnknown'
  if (stage.memberRunId && retryable) return 'task.failure.reason.memberInterrupted'
  if (stage.failureReason === 'the referenced result file was not saved as a deliverable') return 'task.failure.reason.fileMissing'
  if (stage.failureClass === 'infrastructure') return retryable ? 'task.failure.reason.infrastructure' : 'task.failure.reason.infrastructureUnavailable'
  if (stage.failureClass === 'verification') return 'task.failure.reason.verification'
  return 'task.failure.reason.recorded'
}

function runtimeDisplayStatus(runtime: WorkTaskRuntime, runtimes: readonly WorkTaskRuntime[], members: ReturnType<typeof workTaskModel>['members']): WorkTaskStatus {
  const activeMembers = members.filter(member => member.status === 'running')
  if (activeMembers.length === 0) return runtime.status
  if (runtimes.length === 1 || activeMembers.some(member => member.runtime === runtime.name || member.runtime.startsWith(`${runtime.name} · `))) {
    return 'running'
  }
  return runtime.status
}

const MEMBER_STATUS_KEYS = {
 waiting: 'task.status.waiting',
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

function timestampLabel(value: string, t: PanelProps['t']): string {
  if (value === '') return ''
  const timestamp = new Date(value)
  if (Number.isNaN(timestamp.getTime())) return ''
  const minutes = Math.max(0, Math.floor((Date.now() - timestamp.getTime()) / 60_000))
  if (minutes < 1) return t('runtimeCenter.justNow')
  if (minutes < 60) return t('runtimeCenter.minutesAgo', { count: minutes })
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return t('runtimeCenter.hoursAgo', { count: hours })
  return t('runtimeCenter.daysAgo', { count: Math.floor(hours / 24) })
}

function stageLabel(value: string, t: PanelProps['t']): string {
  if (/[㐀-鿿]/u.test(value)) return value.trim()
  const stage = value.trim().toLowerCase()
  if (/^(parallel[-_ ]|coordinate|collect results|join)/u.test(stage)) return t(stage.startsWith('parallel') ? 'task.stage.parallel' : 'task.stage.coordinate')
  if (/(research|analysis|analy[sz]e|investigate|evidence|调研|研究|分析|证据)/u.test(stage)) return t('task.stage.research')
  if (/(review|verify|audit|check|test|validate|复核|审核|校验|验证|验收)/u.test(stage)) return t('task.stage.verify')
  if (/(draft|write|create|design|build|generate|撰写|设计|生成|制作|初稿)/u.test(stage)) return t('task.stage.create')
  if (/(edit|refine|final|deliver|summary|synthesi[sz]e|汇总|定稿|交付|编辑)/u.test(stage)) return t('task.stage.deliver')
  if (/[㐀-鿿]/u.test(value) || /\s/u.test(value.trim())) return value.trim()
  return t('task.stage.generic')
}

function toolLabel(value: string, t: PanelProps['t']): string {
  const tool = value.trim().toLowerCase()
  if (/(read|search|find|list|get|fetch|open|browse)/u.test(tool)) return t('task.member.tool.read')
  if (/(write|edit|create|save|patch|render)/u.test(tool)) return t('task.member.tool.write')
  if (/(exec|run|shell|bash|test|valid|build|compile)/u.test(tool)) return t('task.member.tool.execute')
  if (/(delegate|dispatch|agent|message|handoff|wait)/u.test(tool)) return t('task.member.tool.coordinate')
  return t('task.member.tool.other')
}

function inputSourceLabel(value: string, t: PanelProps['t']): string {
  const source = value.trim().toLowerCase()
  if (/(node|stage|upstream|workflow)/u.test(source)) return t('task.member.input.upstream')
  if (/(file|path|attachment|artifact|deliverable|workspace)/u.test(source)) return t('task.member.input.file')
  if (/(request|task|brief|user|prompt)/u.test(source)) return t('task.member.input.request')
  return t('task.member.input.other')
}

function deliverableTypeLabel(value: string, t: PanelProps['t']): string {
  const kind = value.trim().toLowerCase()
  if (kind.includes('html')) return t('task.deliverables.type.web')
  if (kind.includes('csv') || kind.includes('spreadsheet') || kind.includes('excel')) return t('task.deliverables.type.table')
  if (kind.includes('json') || kind.includes('yaml') || kind.includes('xml')) return t('task.deliverables.type.data')
  if (kind.startsWith('image/')) return t('task.deliverables.type.image')
  if (kind.includes('markdown') || kind.includes('text') || kind.includes('pdf') || kind.includes('word')) return t('task.deliverables.type.document')
  return t('task.deliverables.type.file')
}

function teamDisplayName(value: string, fallback: string): string {
  const name = value.trim()
  if (name === '' || /^[0-9a-f]{8}-[0-9a-f-]{27,}$/iu.test(name)) return fallback
  if (!/^[a-z0-9_-]+$/u.test(name) || (!name.includes('-') && !name.includes('_'))) return name
  return name.split(/[-_]+/u).filter(Boolean).map(word => word.charAt(0).toUpperCase() + word.slice(1)).join(' ')
}

function runtimeNameLabel(value: string, t: PanelProps['t']): string {
  if (value.trim() === '') return t('task.member.runtimeUnknown')
  return value.split(' · ').map((part, index) => {
    if (index > 0 && part.toLowerCase() === 'codex') return t('runtimeCenter.engine.codex')
    if (index > 0 && (part.toLowerCase() === 'claude' || part.toLowerCase() === 'claude-code')) return t('runtimeCenter.engine.claude')
    if (index > 0 && part.toLowerCase() === 'opencode') return t('runtimeCenter.engine.opencode')
    return teamDisplayName(part, t('task.runtime.assigned'))
  }).join(' · ')
}

function runtimeDetailLabel(value: string, t: PanelProps['t']): string {
  if (value.trim() === '') return t('task.runtime.detailPending')
  return value.split(' · ').map((part) => {
    const normalized = part.trim().toLowerCase()
    if (normalized === 'codex') return t('runtimeCenter.engine.codex')
    if (normalized === 'claude' || normalized === 'claude-code') return t('runtimeCenter.engine.claude')
    if (normalized === 'opencode') return t('runtimeCenter.engine.opencode')
    if (normalized === 'openai') return t('runtimeCenter.provider.openai')
    return part
  }).join(' · ')
}

function deliverableTitle(value: string, t: PanelProps['t']): string {
  const title = value.trim()
  if (/^(最终产物|阶段产物) · /u.test(title)) {
    const stage = title.split(' · ').slice(1).join(' · ')
    return t(title.startsWith('最终') ? 'task.deliverables.finalTitle' : 'task.deliverables.stageTitle', { stage: stageLabel(stage, t) })
  }
  return title === '' || /^[0-9a-f]{8}-[0-9a-f-]{27,}$/iu.test(title) ? t('deliverable.untitled') : title
}

function deliverableUrl(item: WorkTaskDeliverable, sessionId: string, runId: string, preview = false): string {
  return `/api/weave.deliverable?${new URLSearchParams({ sessionId, runId, id: item.id, mode: preview ? 'preview' : 'download' })}`
}

function DeliverableBody({ item, sessionId, runId, t }: {
  readonly item: WorkTaskDeliverable
  readonly sessionId: string
  readonly runId: string
  readonly t: PanelProps['t']
}) {
  const type = item.contentType.toLowerCase()
  const body = item.content || item.preview
  if (type === 'text/csv' || type === 'text/tab-separated-values' || type === 'image/svg+xml') return <DeliverablePreview key={item.id} contentType={type} body={body} title={deliverableTitle(item.title, t)} imageUrl={deliverableUrl(item, sessionId, runId, true)} labels={deliverablePreviewLabels(t)} download={{ url: deliverableUrl(item, sessionId, runId), label: t('task.deliverables.download'), notice: item.truncated ? t('task.deliverables.truncated') : '' }} />
  if (body === '') return <p className={css.muted}>{t('task.deliverables.noPreview')}</p>
  if (type.includes('json') && item.kind === 'summary') {
    let review: unknown
    try { review = JSON.parse(body) } catch { /* Keep malformed and unrelated files in the original-text reader. */ }
    if (review !== null && typeof review === 'object' && !Array.isArray(review)) {
      const record = review as Record<string, unknown>
      // Exact response format of Weave's human-final-review template.
      if (Object.keys(record).length === 2 && typeof record.comments === 'string'
        && (record.decision === 'approve' || record.decision === 'reject')) {
        return <div className={css.reviewResponse}>
          <strong>{t(record.decision === 'approve' ? 'task.review.approve' : 'task.review.reject')}</strong>
          {record.comments === '' ? null : <p>{record.comments}</p>}
          <details><summary>{t('task.review.original')}</summary><pre className={css.documentBody}>{body}</pre></details>
        </div>
      }
    }
  }
  if (type.includes('html')) return <iframe className={css.documentFrame} title={deliverableTitle(item.title, t)} sandbox=""
    srcDoc={`<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'">${body}`} />
  if (type.includes('markdown')) return <div className={css.documentBody}><MarkdownText text={body} labels={{
    code: { copyLabel: t('task.document.copy'), copiedLabel: t('task.document.copied') }, footnotes: t('task.document.footnotes'),
  }} /></div>
  return <pre className={css.documentBody}>{body}</pre>
}

function memberDisplayNames(members: ReturnType<typeof workTaskModel>['members'], t: PanelProps['t']) {
  const labels = members.map((member, index) => {
    const name = member.name.trim()
    const internalName = name === '' || name === member.agentId
      || /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(name)
    if (!internalName) return name
    return member.role === 'lead' ? t('task.member.lead') : t('task.member.numbered', { number: index + 1 })
  })
  return members.map((member, index) => {
    const name = labels[index] ?? t('task.member.numbered', { number: index + 1 })
    return { ...member, name: labels.filter(label => label === name).length > 1
      ? t('task.member.distinguished', { name, number: index + 1 }) : name }
  })
}

interface InterruptedStage {
  readonly memberId: string
  readonly memberName: string
  readonly stage: WorkTaskMemberStage
}

function runtimeStopPending(stage: WorkTaskMemberStage): boolean {
  return !stage.retryable
    && stage.failureReason === 'Reconnect the runtime and wait for the previous execution to confirm it has stopped.'
}

function interruptedInfrastructureStages(model: ReturnType<typeof workTaskModel>): readonly InterruptedStage[] {
  if (model.status !== 'waiting' || (model.waitKind !== 'runtime' && model.waitKind !== 'fanout')) return []
  const stages = new Map<string, InterruptedStage>()
  for (const member of model.members) {
    for (const stage of member.stages) {
      if (model.waitKind === 'runtime' && model.waitNodeId !== stage.nodeId) continue
      if (!((stage.status === 'failed' && stage.failureClass === 'infrastructure') || (stage.status === 'waiting' && stage.budgetPause !== undefined))
        || (!stage.retryable && !runtimeStopPending(stage)) || stages.has(stage.nodeId)) continue
      stages.set(stage.nodeId, { memberId: member.agentId, memberName: member.name, stage })
    }
  }
  return [...stages.values()].sort((left, right) => {
    const leftTime = Date.parse(left.stage.completedAt || left.stage.startedAt)
    const rightTime = Date.parse(right.stage.completedAt || right.stage.startedAt)
    return (Number.isNaN(rightTime) ? 0 : rightTime) - (Number.isNaN(leftTime) ? 0 : leftTime)
  })
}

function humanResponseText(value: unknown, t: PanelProps['t']): string {
  if (typeof value === 'boolean') return t(value ? 'task.human.yes' : 'task.human.no')
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'bigint' || typeof value === 'symbol') return String(value)
  if (value === null || value === undefined) return ''
  if (Array.isArray(value)) return value.map(item => humanResponseText(item, t)).join(' · ')
  if (typeof value === 'object') {
    const record = value as Record<string, unknown>
    if (Object.keys(record).length === 2 && typeof record.comments === 'string'
      && (record.decision === 'approve' || record.decision === 'reject')) {
      return `${t(record.decision === 'approve' ? 'task.review.approve' : 'task.review.reject')}\n${record.comments}`
    }
  }
  if (typeof value === 'object') return Object.entries(value).map(([key, item]) => `${key.replaceAll('_', ' ')} · ${humanResponseText(item, t)}`).join('\n')
  if (typeof value === 'function') return value.toString()
  return ''
}

function useFreshnessDeadline(model: ReturnType<typeof workTaskModel>): void {
  const [, refresh] = useState(0)
  useEffect(() => {
    if (model.observedAt <= 0 || ['completed', 'failed', 'stopped'].includes(model.status)) return
    const timeout = setTimeout(() => { refresh(value => value + 1) }, Math.max(1, model.observedAt + 45_001 - Date.now()))
    return () => { clearTimeout(timeout) }
  }, [model.observedAt, model.status])
}

function taskIsExecuting(model: ReturnType<typeof workTaskModel>): boolean {
  if (workTaskFactsStale(model.status, model.observedAt) || model.pendingAction?.kind === 'stop'
    || model.actionError === 'stop_unconfirmed' || interruptedInfrastructureStages(model).some(item => runtimeStopPending(item.stage))) return false
  return model.status === 'running'
    || (model.status === 'waiting' && model.waitKind === 'fanout' && model.members.some(member => member.status === 'running'))
}

interface DeliverableReading {
  readonly expandedIds: readonly string[]
  readonly selection: { readonly id: string; readonly request: number } | undefined
  readonly toggleOutput: (id: string, expanded: boolean) => void
}

function DeliverableItems({ items, sessionId, runId, expandedIds, selection, toggleOutput, t }: DeliverableReading & { readonly items: readonly WorkTaskDeliverable[]; readonly sessionId: string; readonly runId: string; readonly t: PanelProps['t'] }) {
  const visited = useRef(new Set<string>())
  for (const id of expandedIds) visited.current.add(id)
  const itemsById = useRef(new Map<string, HTMLDetailsElement>())
  const handledSelection = useRef<string>()
  useEffect(() => {
    if (selection === undefined) return
    const requestKey = `${runId}:${selection.id}:${selection.request}`
    if (requestKey === handledSelection.current) return
    const target = itemsById.current.get(selection.id)
    if (target === undefined) return
    handledSelection.current = requestKey
    let parent: HTMLElement | null = target.parentElement
    while (parent !== null) { if (parent instanceof HTMLDetailsElement) parent.open = true; parent = parent.parentElement }
    target.open = true
    target.querySelector('summary')?.focus()
    target.scrollIntoView({ block: 'nearest' })
  }, [selection, runId, items])
  const latestIds = new Set<string>()
  for (const item of items) {
    if (!items.some(candidate => candidate.title === item.title && latestIds.has(candidate.id))) latestIds.add(item.id)
  }
  return (
    <div className={css.deliverableList}>
      {items.map((item) => {
        const revisions = items.filter(candidate => candidate.title === item.title).length
        const timestamp = timestampLabel(item.createdAt, t)
        const revisionLabel = revisions < 2 ? '' : t(latestIds.has(item.id)
          ? 'task.deliverables.currentRevision'
          : 'task.deliverables.previousRevision')
        const metadata = [revisionLabel, timestamp].filter(Boolean).join(' · ')
        const expanded = expandedIds.includes(item.id)
        const structured = ['text/csv', 'text/tab-separated-values', 'image/svg+xml'].includes(item.contentType.toLowerCase())
        return (
          <details className={css.deliverable} data-kind={item.kind} data-deliverable-id={item.id} key={item.id} open={expanded}
            ref={(element) => { if (element === null) itemsById.current.delete(item.id); else itemsById.current.set(item.id, element) }}
            onToggle={(event) => { if (event.currentTarget.open !== expanded) toggleOutput(item.id, event.currentTarget.open) }}>
            <summary>
              <WorkFileIcon contentType={item.contentType} title={item.title} />
              <span className={css.deliverableIdentity}>
                <strong>{deliverableTitle(item.title, t)}</strong>
                <span className={css.deliverableMetadata}><span>{deliverableTypeLabel(item.contentType, t)}</span>
                  {metadata === '' ? null : <span>{metadata}</span>}</span>
              </span>
              <span className={css.deliverableKind}>{t(item.kind === 'final'
                ? 'task.deliverables.final'
                : item.kind === 'summary'
                  ? 'task.deliverables.summary'
                  : 'task.deliverables.stage')}</span>
            </summary>
            {visited.current.has(item.id) ? (
              <div className={css.deliverableBody} data-structured={structured || undefined}>
                {structured ? null : <div className={css.deliverableActions}>
                  <Tooltip label={t('task.deliverables.download')}><a className={css.sceneIconButton} href={deliverableUrl(item, sessionId, runId)} download aria-label={t('task.deliverables.download')}><IconDownloadOutline16 /></a></Tooltip>
                  {item.truncated ? <span>{t('task.deliverables.truncated')}</span> : null}
                </div>}
                <DeliverableBody item={item} sessionId={sessionId} runId={runId} t={t} />
              </div>
            ) : null}
          </details>
        )
      })}
    </div>
  )
}

type WorkTab = 'overview' | 'progress' | 'outputs'

function WorkTaskTabs({ selected, select, id, t }: { readonly selected: WorkTab; readonly select: (tab: WorkTab) => void; readonly id: string; readonly t: PanelProps['t'] }) {
  const buttons = useRef(new Map<WorkTab, HTMLButtonElement>())
  return <div className={css.tabs} role="tablist" aria-label={t('task.workScene')}>
    {(['overview', 'progress', 'outputs'] as const).map(tab => <button key={tab} ref={(button) => {
      if (button === null) buttons.current.delete(tab); else buttons.current.set(tab, button)
    }}
    id={`${id}-${tab}-tab`} role="tab" aria-selected={selected === tab} aria-controls={`${id}-${tab}-panel`} tabIndex={selected === tab ? 0 : -1}
    onClick={() => { select(tab) }} onKeyDown={(event) => {
      const order = ['overview', 'progress', 'outputs'] as const
      const index = order.indexOf(tab)
      const next = event.key === 'Home' ? 'overview' : event.key === 'End' ? 'outputs'
        : event.key === 'ArrowLeft' ? order[(index + 2) % 3] : event.key === 'ArrowRight' ? order[(index + 1) % 3] : null
      if (next == null) return
      event.preventDefault()
      select(next)
      buttons.current.get(next)?.focus()
    }}>{t(tab === 'overview' ? 'task.tab.overview' : tab === 'progress' ? 'task.tab.progress' : 'task.tab.outputs')}</button>)}
  </div>
}

/** One projection-backed conversation card; durable user-action receipts survive refreshes. */
export function WorkTaskConversationCard(props: PropsRuntime<'conversation.input.dock'> & InjectFace<WorkTaskInjected> & PropsLocale<'weave'> & PropsStore<ReturnType<typeof createWorkTaskViewStore>>) {
  const conversation = props.useChat(snapshot => workTaskModel(snapshot.nodes.values()))
  const model = projectedWorkTask(conversation, props.useProjection('workTask'))
  if (!model.detected) return null
  return <section className={css.conversationCard} data-weave-task-card={model.runId}>
    <WorkTaskPanel {...props} presentation="conversation" />
  </section>
}

/** Compact live status beside the Session title. */
export function WorkTaskHeader({ useChat, useProjection, openDetails, t }: HeaderProps) {
  const conversationModel = useChat(snapshot => workTaskModel(snapshot.nodes.values()))
  const projection = useProjection('workTask')
  const model = projectedWorkTask(conversationModel, projection)
  useFreshnessDeadline(model)
  if (!model.detected) return (
    <button className={css.headerPill} type="button" onClick={openDetails} aria-label={t('task.open')}>
      <span>{t('task.header.scene')}</span><IconChevronRightOutline14 size={14} />
    </button>
  )
  const progress = model.totalStages > 0
    ? t('task.progress.count', { completed: model.completedStages, total: model.totalStages })
    : model.completedStages > 0
      ? t('task.progress.completedCount', { completed: model.completedStages })
      : null
  return (
    <button className={css.headerPill} type="button" onClick={openDetails} aria-label={t('task.open')} title={[teamDisplayName(model.teamName, model.teamName), progress].filter(Boolean).join(' · ')}>
      <span className={css.statusDot} data-executing={taskIsExecuting(model) || undefined} data-status={model.status === 'completed' && !workTaskHasFinalDeliverable(model) ? 'attention' : model.status} aria-hidden />
      <span className={css.headerLabel}>{t('task.header.scene')}</span>
      <span className={css.headerState}>{t(taskStatusKey(model))}</span>
      <IconChevronRightOutline14 size={14} className={css.headerChevron} />
    </button>
  )
}

/** Persistent Weave task, team, runtime, and deliverable projection. */
export function WorkTaskPanel({
  useChat, useProjection, useSessions, useStore, actions, sessionId, selectTeam,
  stopRun, rerun, retryStage, requestCorrection, confirmCorrection, completeHumanTask, assessOutcome, recheckDelivery, returnToConversation,
  requestDelivery, beginMemberAdjustment, expandDetails, presentation, openDetails, activateScene, t,
}: PanelProps) {
  const conversationModel = useChat(snapshot => workTaskModel(snapshot.nodes.values()))
  const projection = useProjection('workTask')
  const projected = projectedWorkTask(conversationModel, projection)
  const model = { ...projected, members: memberDisplayNames(projected.members, t) }
  useFreshnessDeadline(model)
  const followed = useStore(state => state.followed[model.runId] ?? [])
  const selectedTab = useStore(state => state.tabs[model.runId])
  const initialTabs = useRef(new Map<string, WorkTab>())
  if (!initialTabs.current.has(model.runId)) initialTabs.current.set(model.runId, model.status === 'completed' ? workTaskHasFinalDeliverable(model) ? 'overview' : 'outputs' : model.status === 'stopped' && model.deliverables.length > 0 ? 'overview' : 'progress')
  const selectedMemberId = useStore(state => state.selectedMembers[model.runId] ?? '')
  const reading = useStore(state => state.reading)
  const expandedIds = useStore(state => state.expandedOutputs[model.runId] ?? [])
  const outputSelection = useStore(state => state.outputSelection[model.runId])
  const deliverableView: DeliverableReading = { expandedIds, selection: presentation === 'conversation' || (selectedTab ?? initialTabs.current.get(model.runId)) !== 'outputs' ? undefined : outputSelection, toggleOutput: (id, expanded) => { actions.expandOutput(model.runId, id, expanded) } }
  const setSelectedMemberId = (id: string) => { saveScenePosition(); actions.selectMember(model.runId, id) }
  const panelId = `${sessionId}-${presentation === 'conversation' ? 'conversation' : 'scene'}`
  const sceneRoot = useRef<HTMLDivElement>(null)
  const sceneKey = `${model.runId}:${selectedTab ?? initialTabs.current.get(model.runId)}:${selectedTab === 'outputs' ? '' : selectedMemberId}`
  const sceneScroller = () => {
    let element = sceneRoot.current?.parentElement ?? null
    while (element !== null) {
      if (/auto|scroll/u.test(getComputedStyle(element).overflowY)) return element
      element = element.parentElement
    }
    return null
  }
  const saveScenePosition = () => {
    const element = sceneScroller()
    if (element !== null) actions.rememberReading(`scene:${sceneKey}`, { top: element.scrollTop, follow: false, lastEvent: '' })
  }
  useLayoutEffect(() => {
    const element = sceneScroller()
    if (element === null) return
    const targetTop = reading[`scene:${sceneKey}`]?.top ?? 0
    let observer: ResizeObserver | undefined
    const restore = () => {
      element.scrollTop = targetTop
      return element.scrollTop + 1 < targetTop
    }
    let restoring = restore()
    // Projection and Markdown content can finish mounting after the scene itself.
    // Do not persist a browser-clamped position while its retained target is still loading.
    if (restoring && sceneRoot.current !== null && typeof ResizeObserver !== 'undefined') {
      observer = new ResizeObserver(() => {
        restoring = restore()
        if (!restoring) observer?.disconnect()
      })
      observer.observe(sceneRoot.current)
    }
    const stopRestoring = () => { restoring = false; observer?.disconnect() }
    const remember = () => {
      if (!restoring) actions.rememberReading(`scene:${sceneKey}`, { top: element.scrollTop, follow: false, lastEvent: '' })
    }
    element.addEventListener('scroll', remember, { passive: true })
    for (const type of ['wheel', 'touchstart', 'pointerdown', 'keydown']) element.addEventListener(type, stopRestoring, { passive: true })
    return () => {
      observer?.disconnect()
      element.removeEventListener('scroll', remember)
      for (const type of ['wheel', 'touchstart', 'pointerdown', 'keydown']) element.removeEventListener(type, stopRestoring)
    }
  }, [sceneKey])
  const taskTitle = useSessions(snapshot => snapshot.byId[sessionId]?.title ?? '')
  useEffect(() => { if (presentation !== 'conversation') activateScene?.() }, [activateScene, presentation])
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
  useEffect(() => { setAssessmentNote('') }, [model.runId, model.delivery?.revisionId])
  const [outputQuery, setOutputQuery] = useState('')
  useEffect(() => { setOutputQuery('') }, [model.runId, outputSelection?.request])
  const [retryNodeId, setRetryNodeId] = useState('')
  const [budgetCeiling, setBudgetCeiling] = useState('')
  const backButton = useRef<HTMLButtonElement>(null)
  const correctionButton = useRef<HTMLButtonElement>(null)
  const correctionRegion = useRef<HTMLElement>(null)
  const correctionWasOpen = useRef(false)
  const followCorrection = useRef(false)
  useEffect(() => {
    if (correctionWasOpen.current && !correctionOpen) correctionButton.current?.focus()
    correctionWasOpen.current = correctionOpen
  }, [correctionOpen])
  useEffect(() => {
    if (followCorrection.current) correctionRegion.current?.focus()
  }, [model.corrections])
  const memberButtons = useRef(new Map<string, HTMLButtonElement>())
  const returnFocus = useRef('')
  useEffect(() => {
    if (selectedMemberId !== '') backButton.current?.focus({ preventScroll: true })
    else if (returnFocus.current !== '') {
      memberButtons.current.get(returnFocus.current)?.focus({ preventScroll: true })
      returnFocus.current = ''
    }
  }, [selectedMemberId])

  if (!model.detected) return (
    <div className={css.panel}>
      <div className={css.emptyScene}>
        <strong>{t('task.empty.title')}</strong>
        <span>{t('task.empty.description')}</span>
      </div>
    </div>
  )
  if (model.runId === '' && model.preparation !== undefined) return (
    <section className={`${css.panel} ${presentation === 'conversation' ? css.dockContent : css.preparationPanel}`} data-weave-preparation={model.preparation.state}>
      <div className={css.eyebrow}>{teamDisplayName(model.teamName, t('task.team.pending'))}</div>
      <h2 className={css.preparationTitle}>{t(taskStatusKey(model))}</h2>
      {model.brief === '' || presentation === 'conversation' ? null : <details className={css.briefDetails}><summary>{t('task.brief')}</summary><p>{model.brief}</p></details>}
      <p className={css.muted}>{t(model.preparation.state === 'unknown' ? 'build.unknownHelp' : model.preparation.state === 'failed' ? 'build.failedHelp' : 'build.help')}</p>
      {!model.preparation.steps?.length ? null : <details><summary>{t('build.steps')}</summary><ol>
        {model.preparation.steps.map(step => <li key={step.id}><strong>{step.label}</strong> · {t(step.status === 'succeeded' ? 'task.status.completed' : step.status === 'failed' ? 'task.status.failed' : step.status === 'running' ? 'task.status.running' : 'task.status.waiting')}
          {step.attempt > 1 ? <span> · {t('build.attempt', { count: step.attempt })}</span> : null}</li>)}
      </ol></details>}
      {presentation === 'conversation' ? <button type="button" className={css.secondaryButton} onClick={openDetails}>{t('task.card.openScene')}</button> : returnToConversation === undefined ? null : <Tooltip label={t('build.back')} side="bottom"><button type="button" className={css.sceneIconButton} aria-label={t('build.back')} onClick={returnToConversation}><IconChevronLeftOutline14 size={16} /></button></Tooltip>}
      {model.preparation.error === '' ? null : <details className={css.diagnostics}><summary>{t('build.details')}</summary><pre>{model.preparation.error}</pre></details>}
    </section>
  )
  const progressPercent = model.totalStages === 0
    ? 0
    : Math.min(100, Math.round(model.completedStages / model.totalStages * 100))
  const finalDeliverables = model.deliverables.filter(item => item.kind === 'final')
  const summaryDeliverables = model.deliverables.filter(item => item.kind === 'summary')
  const stageDeliverables = model.deliverables.filter(item => item.kind === 'stage')
  const matchesOutput = (item: WorkTaskDeliverable) => item.title.toLocaleLowerCase().includes(outputQuery.trim().toLocaleLowerCase())
  const visibleFinal = [...finalDeliverables, ...summaryDeliverables].filter(matchesOutput)
  const visibleStages = stageDeliverables.filter(matchesOutput)
  const terminal = model.status === 'completed' || model.status === 'failed' || model.status === 'stopped'
  const compact = presentation === 'conversation'
  const activeTab = selectedTab ?? initialTabs.current.get(model.runId) ?? 'overview'
  const selectTab = (tab: WorkTab) => { saveScenePosition(); actions.selectTab(model.runId, tab) }
  const tabId = `weave-task-${panelId}`
  const stale = workTaskFactsStale(model.status, model.observedAt)
  const executing = taskIsExecuting(model)
  const activeMemberNames = executing ? model.members.filter(member => member.status === 'running').map(member => member.name).join('、') : ''
  const activityHint = activeMemberNames === '' ? '' : t('task.activity.members', { names: activeMemberNames })
  const incomplete = workTaskHasUserVisibleCompletenessWarning(model)
  const activeCorrection = model.corrections.find(item => item.status === 'requested' || item.status === 'ready' || item.status === 'confirmed')
  const displayedRuntimes = model.runtimes.map(runtime => ({
    ...runtime,
    status: runtimeDisplayStatus(runtime, model.runtimes, model.members),
  }))
  const selectedMember = !compact ? model.members.find(member => member.agentId === selectedMemberId) : undefined
  const hasFinal = workTaskHasFinalDeliverable(model)
  const canAssessDelivery = (model.delivery?.revisionId ?? '') !== '' && model.delivery?.available === true
  const actionDisabled = actionPending || model.pendingAction !== null
  const canCorrect = model.runId !== '' && !terminal && model.status !== 'stopping'
    && activeCorrection === undefined && requestCorrection !== undefined && model.pendingAction === null
  const closeMember = () => {
    returnFocus.current = selectedMemberId
    setSelectedMemberId('')
    setCorrectionOpen(false)
    setCorrectionInstruction('')
  }
  const nodeLabel = (nodeId: string) => {
    const owner = model.members.find(member => member.stages.some(stage => stage.nodeId === nodeId))
    const stage = owner?.stages.find(item => item.nodeId === nodeId)
    return owner === undefined ? stageLabel(nodeId, t) : `${owner.name} · ${stageLabel(stage?.name ?? nodeId, t)}`
  }
  const interruptedStages = interruptedInfrastructureStages(model)
  const budgetStage = model.status === 'waiting' ? model.members.flatMap(member => member.stages).find(stage => stage.nodeId === model.waitNodeId && stage.budgetPause !== undefined) : undefined
  const recoverableStages = retryStage === undefined ? [] : interruptedStages.filter(item => item.stage.retryable)
  const pendingStopStages = interruptedStages.filter(item => runtimeStopPending(item.stage))
  const pendingStopNotice = pendingStopStages.length === 0 ? null : <div className={css.stageFailure}>
    <strong>{pendingStopStages.map(item => nodeLabel(item.stage.nodeId)).join(' · ')}</strong>
    <span>{t('task.wait.runtimeStopHelp')}</span>
  </div>
  const failedMember = model.status === 'failed' ? model.members.find(member => member.stages.some(stage => stage.status === 'failed')) : undefined
  const failedStage = failedMember?.stages.find(stage => stage.status === 'failed')
  const failureNotice = failedStage === undefined ? null : <div className={css.stageFailure}>
    <strong>{nodeLabel(failedStage.nodeId)}</strong>
    <span>{t(stageFailureKey(failedStage, false))}</span>
    {failedStage.failureReason === '' ? null : <details><summary>{t('task.failure.details')}</summary><p>{failedStage.failureReason}</p></details>}
  </div>

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
  const submitStageRetry = async () => {
    if (retryStage === undefined || actionPending || retryNodeId === '') return
    setActionPending(true)
    setActionError(null)
    const budget = recoverableStages.find(item => item.stage.nodeId === retryNodeId)?.stage.budgetPause
    const ceiling = budget?.reason === 'total_limit' ? Number(budgetCeiling) : undefined
    if (ceiling !== undefined && (!Number.isSafeInteger(ceiling) || ceiling <= budget!.authorizedTotalRounds)) {
      setActionPending(false); setActionError(t('task.budget.invalid')); return
    }
    const error = ceiling === undefined ? await retryStage(model.runId, retryNodeId) : await retryStage(model.runId, retryNodeId, ceiling)
    setActionPending(false)
    if (error !== null) setActionError(error)
    else setRetryNodeId('')
  }
  const submitCorrection = async () => {
    if (requestCorrection === undefined || actionPending || correctionInstruction.trim() === '') return
    setActionPending(true)
    setActionError(null)
    followCorrection.current = true
    const targetKind = correctionTarget === 'team' ? 'team' : 'member'
    const error = await requestCorrection(model.runId, targetKind, targetKind === 'member' ? correctionTarget : '', correctionInstruction.trim())
    setActionPending(false)
    if (error !== null) setActionError(error)
    else { setCorrectionOpen(false); setCorrectionInstruction('') }
  }
  const submitCorrectionDecision = async (disposition: 'apply' | 'discard') => {
    if (confirmCorrection === undefined || actionDisabled || terminal || activeCorrection?.status !== 'ready') return
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
  const submitDeliveryReview = async () => {
    if (requestDelivery === undefined || actionDisabled) return
    setActionPending(true)
    setActionError(null)
    try { await requestDelivery(model.runId) }
    catch { setActionError(t('task.action.offline')) }
    finally { setActionPending(false) }
  }
  const submitAssessment = async (outcome: 'adopted' | 'needs-revision') => {
    const deliveryRevisionId = model.delivery?.revisionId ?? ''
    if (assessOutcome === undefined || actionPending || !canAssessDelivery) return
    setActionPending(true)
    setActionError(null)
    try {
      const error = await assessOutcome(model.runId, deliveryRevisionId, outcome, assessmentNote.trim())
      if (error !== null) setActionError(error)
    } catch { setActionError(t('task.action.offline')) }
    finally { setActionPending(false) }
  }

  const correctionView = (memberId = '') => {
    const correction = model.corrections.find(item => memberId === '' || item.targetKind === 'member' && item.targetMemberId === memberId)
    if (correction === undefined) return null
    const ready = correction.status === 'ready' && !terminal && model.status !== 'stopping'
    return <section ref={correctionRegion} tabIndex={-1} className={css.correctionPlan} aria-label={t('task.correction.impact')}>
      <div className={css.sectionHeader}><h3>{t('task.correction.impact')}</h3><span>{t(
        correction.status === 'applied' ? 'task.correction.applied' : correction.status === 'discarded' ? 'task.correction.discarded'
          : ready ? 'task.correction.ready' : terminal ? 'task.correction.ended' : correction.status === 'confirmed' ? 'task.correction.resuming' : 'task.correction.awaitingSafePoint',
      )}</span></div>
      <p>{correction.instruction}</p>
      {correction.affectedNodeIds.length === 0 && correction.restartNodeId === '' ? null : <>
        <div className={css.impactRoute}>
          {correction.restartNodeId === '' ? null : <span>{t('task.correction.restartAt')} <strong>{nodeLabel(correction.restartNodeId)}</strong></span>}
        </div>
        <div className={css.impactColumns}>
          <div><span>{t('task.correction.affected')}</span>{correction.affectedNodeIds.map(id => <strong key={id}>{nodeLabel(id)}</strong>)}</div>
          <div><span>{t('task.correction.preserved')}</span>{correction.preservedNodeIds.length === 0 ? <em>{t('task.correction.none')}</em> : correction.preservedNodeIds.map(id => <strong key={id}>{nodeLabel(id)}</strong>)}</div>
        </div>
      </>}
      {!ready || confirmCorrection === undefined ? null : <div className={css.controlActions}>
        <button type="button" className={css.secondaryButton} disabled={actionDisabled} onClick={() => { void submitCorrectionDecision('discard') }}>{t('task.correction.discard')}</button>
        <button type="button" className={css.primaryButton} disabled={actionDisabled} onClick={() => { void submitCorrectionDecision('apply') }}>{t('task.correction.apply')}</button>
      </div>}
    </section>
  }

  const recoverySection = recoverableStages.length === 0 ? null : (
    <section className={css.recoveryCard} aria-label={t(budgetStage === undefined ? 'task.recovery.title' : 'task.budget.title')}>
      <div className={css.recoveryHeading}>
        <span className={css.recoveryMark} aria-hidden />
        <div>
          <strong>{t(budgetStage === undefined ? 'task.recovery.title' : 'task.budget.title')}</strong>
          <span>{t(budgetStage === undefined ? 'task.recovery.description' : 'task.budget.help')}</span>
        </div>
      </div>
      <div className={css.recoveryStages}>
        {recoverableStages.map(({ memberId, memberName, stage }) => (
          <div className={css.recoveryStage} key={stage.nodeId}>
            <button type="button" className={css.recoveryIdentity} onClick={() => { setSelectedMemberId(memberId); if (compact) openDetails() }}>
              <strong>{nodeLabel(stage.nodeId)}</strong>
              <span>{t('task.recovery.owner', { name: memberName })}</span>
            </button>
            {retryNodeId === stage.nodeId ? (
              <div className={css.retryConfirm}>
                <span>{t(stage.memberRunId ? 'task.retry.memberImpact' : 'task.retry.impact')}</span>
                {stage.budgetPause?.reason !== 'total_limit' ? null : <label>{t('task.budget.ceiling')}<input type="number" min={stage.budgetPause.authorizedTotalRounds + 1} max={Number.MAX_SAFE_INTEGER} step={1} value={budgetCeiling} onChange={event => { setBudgetCeiling(event.currentTarget.value) }} /></label>}
                <div className={css.controlActions}>
                  <button type="button" className={css.secondaryButton} onClick={() => { setRetryNodeId('') }}>{t('task.cancel')}</button>
                  <button type="button" className={css.primaryButton} disabled={actionPending || model.pendingAction !== null} onClick={() => { void submitStageRetry() }}>{t('task.retry.confirm')}</button>
                </div>
              </div>
            ) : (
              <button type="button" className={css.primaryButton} disabled={actionPending || model.pendingAction !== null}
                onClick={() => { setBudgetCeiling(''); setRetryNodeId(stage.nodeId) }}>{t(stage.memberRunId ? 'task.retry.memberContinue' : 'task.recovery.retry')}</button>
            )}
          </div>
        ))}
      </div>
      <span className={css.recoveryImpact}>{t(recoverableStages.some(item => item.stage.memberRunId) ? 'task.retry.memberImpact' : 'task.retry.impact')}</span>
    </section>
  )

  const correctionControls = !canCorrect ? null : (
    <section className={css.controlSection} aria-label={t('task.correction')}>
      {correctionOpen ? (
        <div className={css.correctionComposer}>
          <label htmlFor={`weave-correction-target-${panelId}`}>{t('task.correction.target')}</label>
          <select id={`weave-correction-target-${panelId}`} value={correctionTarget} onChange={(event) => { setCorrectionTarget(event.currentTarget.value) }}>
            <option value="team">{t('task.correction.team')}</option>
            {model.members.map(member => <option key={member.agentId} value={member.agentId}>{member.name}</option>)}
          </select>
          <label htmlFor={`weave-correction-instruction-${panelId}`}>{t('task.correction.instruction')}</label>
          <textarea autoFocus id={`weave-correction-instruction-${panelId}`} value={correctionInstruction}
            onChange={(event) => { setCorrectionInstruction(event.currentTarget.value) }} placeholder={t('task.correction.placeholder')} />
          <span>{t('task.correction.notice')}</span>
          <div className={css.controlActions}>
            <button type="button" className={css.secondaryButton} onClick={() => { setCorrectionOpen(false) }}>{t('task.cancel')}</button>
            <button type="button" className={css.primaryButton} disabled={actionPending || correctionInstruction.trim() === ''} onClick={() => { void submitCorrection() }}>{t('task.correction.submit')}</button>
          </div>
        </div>
      ) : <button ref={correctionButton} type="button" className={css.primaryButton} onClick={() => { setCorrectionTarget('team'); setCorrectionOpen(true) }}>{t('task.correction')}</button>}
    </section>
  )

  const humanSection = model.humanTask === null || completeHumanTask === undefined || model.status !== 'waiting' || model.waitKind !== 'human' ? null : <HumanTaskForm key={model.humanTask.interactionId} task={model.humanTask} id={`${panelId}-human`} disabled={actionDisabled} rejectionId={model.actionHistory.findLast(receipt => receipt.action.kind === 'human-complete' && receipt.outcome === 'rejected')?.id ?? ''} submit={payload => completeHumanTask(model.runId, model.humanTask?.interactionId ?? '', payload)} discuss={returnToConversation} t={t} />

  const stopControls = !terminal ? <div className={css.secondaryActions}>{model.runId === '' || model.status === 'stopping' ? null : (
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
      ) : <button type="button" disabled={actionDisabled || stopRun === undefined} className={css.secondaryButton} onClick={() => { setConfirmStop(true) }}>{t('task.stop')}</button>}
    </section>
  )}</div> : null

  const memberView = selectedMember === undefined ? null : (
    <div key={selectedMember.agentId} data-weave-member-reader>
      <Tooltip label={t('task.member.back')} side="bottom"><button ref={backButton} type="button" className={`${css.sceneIconButton} ${css.backButton}`} aria-label={t('task.member.back')} onClick={closeMember}><IconChevronLeftOutline14 size={16} /></button></Tooltip>
      <header className={css.memberWorkspaceHeader}>
        <div className={css.memberWorkspaceIdentity}>
          <span className={css.memberStatus} data-executing={(executing && selectedMember.status === 'running') || undefined} data-status={selectedMember.status} aria-hidden />
          <div>
            <h2>{selectedMember.name}</h2>
            {selectedMember.duty.length > 80 ? <details className={css.memberDuty}>
              <summary>{t(selectedMember.role === 'lead' ? 'task.member.lead' : 'task.member.worker')}</summary>
              <p>{selectedMember.duty}</p>
            </details> : <p>{selectedMember.duty === ''
              ? t(selectedMember.role === 'lead' ? 'task.member.lead' : 'task.member.worker')
              : selectedMember.duty}</p>}
          </div>
        </div>
        <div className={css.memberWorkspaceMeta}>
          <strong>{t(memberStatusKey(selectedMember.status))}</strong>
          <details><summary>{t('task.diagnostics')}</summary>
            <p>{t('task.member.runtime')} · {runtimeNameLabel(selectedMember.runtime, t)}</p>
            {displayedRuntimes.filter(runtime => selectedMember.runtime.startsWith(runtime.name) || displayedRuntimes.length === 1)
              .map(runtime =>
                <p key={runtime.name}>{runtimeDetailLabel(runtime.detail, t)} · {t(statusKey(runtime.status))}</p>)}
            <dl><dt>{t('task.member.identifier')}</dt><dd>{selectedMember.agentId}</dd></dl>
          </details>
        </div>
      </header>

      <div className={css.memberToolbar}>
        <div className={css.followControls}>
          <span>{t(terminal ? 'task.updates.recorded' : selectedMember.updateMode === 'live' ? 'task.updates.live' : 'task.updates.onCompletion')}</span>
          <Tooltip label={t(followed.includes(selectedMember.agentId) ? 'task.member.unfollow' : 'task.member.follow')}><button type="button" className={css.sceneIconButton} aria-label={t(followed.includes(selectedMember.agentId) ? 'task.member.unfollow' : 'task.member.follow')} aria-pressed={followed.includes(selectedMember.agentId)} disabled={!followed.includes(selectedMember.agentId) && followed.length >= 3} onClick={() => { actions.toggleFollow(model.runId, selectedMember.agentId) }}>
            <svg className={css.followIcon} width="17" height="17" viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinejoin="round" aria-hidden="true"><path d="m10 2.5 2.3 4.65 5.2.75-3.75 3.65.88 5.15L10 14.27 5.37 16.7l.88-5.15L2.5 7.9l5.2-.75Z" /></svg>
          </button></Tooltip>
        </div>
        <section className={css.memberWorkspaceActions} aria-label={t('task.correction.member')}>
          {actionError === null ? null : <div className={css.actionError} role="alert">{actionError}</div>}
          {canCorrect && beginMemberAdjustment !== undefined ? <button type="button" className={css.secondaryButton} onClick={() => {
            void beginMemberAdjustment({ runId: model.runId, memberId: selectedMember.agentId, memberName: selectedMember.name, stages: selectedMember.stages.map(stage => ({ nodeId: stage.nodeId, name: stageLabel(stage.name, t), outputIds: stage.outputRefs, outputTitles: model.deliverables.filter(item => stage.outputRefs.includes(item.id)).map(item => deliverableTitle(item.title, t)) })) }).catch(() => { setActionError(t('task.action.offline')) })
          }}>{t('task.member.proposeAdjustment')}</button> : activeCorrection !== undefined || model.pendingAction !== null ? null : <p className={css.muted}>{t(terminal ? 'task.member.recordOnly' : 'task.member.correctionUnavailable')}</p>}
          {model.pendingAction?.kind !== 'correction-request' && model.pendingAction?.kind !== 'correction-confirm' ? null : <p role="status">{t(model.pendingAction.kind === 'correction-request' ? 'task.action.correctionPending' : 'task.action.correctionConfirmPending')}</p>}
          {model.actionError === '' ? null : <p role="alert">{t(model.actionError === 'stop_unconfirmed' ? 'task.stop.unconfirmedHelp' : 'task.action.rejected')}</p>}
          {correctionView(selectedMember.agentId)}
        </section>
      </div>

      <section className={css.memberWorkspaceActivity} aria-label={t('task.member.activity')}>
        <div className={css.sectionHeader}>
          <span>{t('task.member.activity')}</span>
          <span>{selectedMember.stages.length}</span>
        </div>
        {selectedMember.stages.length === 0 ? <p className={css.muted}>{t('task.member.stages.empty')}</p> : (
          <ol className={css.memberStages}>
            {selectedMember.stages.map(stage => (
              <li key={stage.nodeId} data-status={stage.status} data-executing={(executing && stage.status === 'running') || undefined}>
                <div className={css.memberStageHeading}>
                  <strong>{nodeLabel(stage.nodeId)}</strong>
                  <span>{t(memberStatusKey(stage.status))}</span>
                </div>
                {!stage.checkpointSavedAt ? null : <p className={css.muted}>{t('task.member.progressSaved')} <time dateTime={stage.checkpointSavedAt}>{new Date(stage.checkpointSavedAt).toLocaleTimeString()}</time></p>}
                {stage.status !== 'failed' || stage.failureClass === '' ? null : (
                  <div className={css.stageFailure} data-class={stage.failureClass}>
                    <strong>{t(stage.failureClass === 'infrastructure'
                      ? 'task.failure.infrastructure'
                      : stage.failureClass === 'verification'
                        ? 'task.failure.verification'
                        : 'task.failure.work')}</strong>
                    {stage.failureReason === '' ? null : <><span>{t(pendingStopStages.some(item => item.stage.nodeId === stage.nodeId)
                      ? 'task.wait.runtimeStopHelp' : stageFailureKey(stage, recoverableStages.some(item => item.stage.nodeId === stage.nodeId)))}</span>
                    <details><summary>{t('task.failure.details')}</summary><p>{stage.failureReason}</p></details></>}
                    {!recoverableStages.some(item => item.stage.nodeId === stage.nodeId) ? null : retryNodeId === stage.nodeId ? (
                      <div className={css.retryConfirm}>
                        <span>{t(stage.memberRunId ? 'task.retry.memberImpact' : 'task.retry.impact')}</span>
                        <div className={css.controlActions}>
                          <button type="button" className={css.secondaryButton} onClick={() => { setRetryNodeId('') }}>{t('task.cancel')}</button>
                          <button type="button" className={css.primaryButton} disabled={actionPending} onClick={() => { void submitStageRetry() }}>{t('task.retry.confirm')}</button>
                        </div>
                      </div>
                    ) : <button type="button" className={css.memberCorrectionButton} onClick={() => { setBudgetCeiling(''); setRetryNodeId(stage.nodeId) }}>{t(stage.memberRunId ? 'task.retry.memberContinue' : 'task.retry.stage')}</button>}
                  </div>
                )}
                {stage.startedAt === '' && stage.durationMs === 0 && stage.toolCalls === 0 ? null : (
                  <div className={css.memberStageMetrics}>
                    {stage.startedAt === '' ? null : <span>{t('task.member.started')} <time dateTime={stage.startedAt}>{new Date(stage.startedAt).toLocaleTimeString()}</time></span>}
                    {stage.durationMs === 0 ? null : <span>{t('task.member.duration')} {durationLabel(stage.durationMs, t)}</span>}
                    <span>{t('task.member.tools')} {stage.toolCalls}</span>
                  </div>
                )}
                <PublicUpdates active={activeTab === 'progress'} updates={stage.publicUpdates} truncated={stage.publicUpdatesTruncated || stage.publicUpdatesState === 'partial'} position={reading[`${model.runId}:${selectedMember.agentId}:${stage.nodeId}`]} remember={(position) => { actions.rememberReading(`${model.runId}:${selectedMember.agentId}:${stage.nodeId}`, position) }} t={t} />
                {stage.tools.length === 0 ? null : (
                  <div className={css.toolTimeline}>
                    <span>{t('task.member.toolActivity')}</span>
                    {stage.tools.map((tool, toolIndex) => (
                      <div key={`${tool.callId}-${toolIndex}`} data-status={tool.status}>
                        <span className={css.toolState} aria-hidden />
                        <strong>{toolLabel(tool.name, t)}</strong>
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
                  <details className={css.inputDetails}>
                    <summary>{t('task.member.input.details')}</summary>
                    <div className={css.memberStageFacts}>
                      {stage.inputs.map((input, inputIndex) => (
                        <div className={css.inputFact} key={`${input.name}-${inputIndex}`}>
                          <strong>{inputSourceLabel(input.source, t)}</strong>
                          {input.summary === '' ? null : <div className={css.documentBody}><MarkdownText text={input.summary} labels={{ code: { copyLabel: t('task.document.copy'), copiedLabel: t('task.document.copied') }, footnotes: t('task.document.footnotes') }} /></div>}
                          {input.nodeId === '' ? null : <DeliverableItems items={model.deliverables.filter(item => model.members.some(member => member.stages.some(prior => prior.nodeId === input.nodeId && prior.outputRefs.includes(item.id))))} sessionId={sessionId} runId={model.runId} {...deliverableView} selection={undefined} t={t} />}
                        </div>
                      ))}
                    </div>
                  </details>
                )}
                {stage.outputRefs.length === 0 ? null : (
                  <div className={css.memberStageFacts}>
                    <span>{t('task.member.outputs')}</span>
                    <DeliverableItems items={model.deliverables.filter(item => stage.outputRefs.includes(item.id))}
                      sessionId={sessionId} runId={model.runId} {...deliverableView} selection={undefined} t={t} />
                    {stage.outputRefs.some(ref => !model.deliverables.some(item => item.id === ref)) ? <p className={css.muted}>{t('task.member.outputPending')}</p> : null}
                  </div>
                )}
              </li>
            ))}
          </ol>
        )}
      </section>
    </div>
  )

  const historySection = model.actionHistory.length === 0 && model.corrections.length < 2 ? null : <details className={css.actionHistory}>
    <summary>{t('task.history.title')}</summary>
    <ol>{model.actionHistory.map((receipt) => {
      const action = receipt.action
      const label = t(action.kind === 'human-complete' ? 'task.human.submit' : action.kind === 'stop' ? 'task.stop' : action.kind === 'rerun' ? 'task.rerun' : action.kind === 'stage-retry' ? 'task.retry.stage' : action.kind === 'correction-request' ? 'task.correction' : 'task.correction.impact')
      return <li key={receipt.id}><strong>{t(receipt.outcome === 'accepted' ? 'task.history.accepted' : 'task.history.rejected', { action: label })}</strong>
        {action.instruction === '' ? null : <p>{action.instruction}</p>}{action.humanPayload === undefined ? null : <details><summary>{t('task.human.response')}</summary><p style={{ whiteSpace: 'pre-wrap' }}>{humanResponseText(action.humanPayload, t)}</p></details>}<time dateTime={new Date(receipt.resolvedAt).toISOString()}>{new Date(receipt.resolvedAt).toLocaleString()}</time></li>
    })}</ol>
    {model.corrections.slice(1).map(correction => <article key={correction.correctionId}><strong>{t(correction.status === 'applied' ? 'task.correction.applied' : correction.status === 'discarded' ? 'task.correction.discarded' : 'task.correction.ended')}</strong><p>{correction.instruction}</p><p>{t('task.correction.affected')} · {correction.affectedNodeIds.map(nodeLabel).join('、')}</p><p>{t('task.correction.preserved')} · {correction.preservedNodeIds.map(nodeLabel).join('、')}</p></article>)}
  </details>

  const openOutputs = (id = '') => {
    if (!compact) saveScenePosition()
    setOutputQuery('')
    if (id === '') actions.selectTab(model.runId, 'outputs')
    else actions.showOutput(model.runId, id)
    if (compact) openDetails()
  }

  if (compact) {
    const finalOutput = [...finalDeliverables, ...summaryDeliverables][0]
    const context = pendingStopStages.length > 0 ? '' : model.humanTask?.title || activeCorrection?.instruction || activityHint
      || (terminal || model.latestStage === '' ? '' : stageLabel(model.latestStage, t))
    return <div className={`${css.panel} ${css.dockContent}`} data-weave-task-receipt data-status={model.status}>
      <div className={css.dockHeading} role="status">
        <span className={css.statusDot} data-executing={taskIsExecuting(model) || undefined} data-status={model.status === 'completed' && !hasFinal ? 'attention' : model.status} aria-hidden />
        <strong>{t(taskStatusKey(model))}</strong>
        {context === '' ? null : <span title={context}>{context}</span>}
      </div>
      <WorkTaskDeliveryState model={model} executionLabel={t(statusKey(model.status))} compact t={t} />
      {model.pendingAction === null ? null : <p className={css.dockNotice}>{t(
        model.pendingAction.kind === 'human-complete' ? 'task.human.recorded' : model.pendingAction.kind === 'stop' ? 'task.action.stopPending'
          : model.pendingAction.kind === 'stage-retry' ? 'task.action.stageRetryPending' : model.pendingAction.kind === 'correction-request' ? 'task.action.correctionPending'
            : model.pendingAction.kind === 'correction-confirm' ? 'task.action.correctionConfirmPending' : 'task.action.rerunPending',
      )}</p>}
      {actionError === null ? null : <p className={css.actionError} role="alert">{actionError}</p>}
      {model.actionError === '' ? null : <p className={css.actionError} role="alert">{t(model.actionError === 'stop_unconfirmed' ? 'task.stop.unconfirmedHelp' : 'task.action.rejected')}</p>}
      {stale ? <p className={css.dockNotice}>{t('task.freshness.stale')}</p> : null}
      {failureNotice}
      {pendingStopStages.length === 0 ? null : <p className={css.dockNotice}>{t('task.wait.runtimeStopHelp')}</p>}
      {budgetStage?.budgetPause === undefined ? null : <p className={css.dockNotice}>{t('task.budget.usage', { used: budgetStage.budgetPause.roundsUsed, total: budgetStage.budgetPause.authorizedTotalRounds })} {t(budgetStage.budgetPause.reason === 'total_limit' ? 'task.budget.total' : budgetStage.budgetPause.reason === 'slice_limit' ? 'task.budget.slice' : 'task.budget.blocked')}</p>}
      {recoverySection}
      {humanSection === null ? null : <details className={css.dockForm}>
        <summary>{t('task.card.answer')}</summary>{humanSection}
      </details>}
      {activeCorrection === undefined ? null : <details className={css.dockForm}>
        <summary>{t(activeCorrection.status === 'ready' ? 'task.correction.impact' : 'task.card.correctionProgress')}</summary>
        {correctionView()}
      </details>}
      {correctionOpen ? correctionControls : null}
      {confirmStop ? stopControls : null}
      <div className={css.dockActions}>
        {finalOutput === undefined ? null : <button type="button" className={`${css.dockLink} ${css.dockPrimary}`}
          onClick={() => { openOutputs(finalOutput.id) }}>{deliverableTitle(finalOutput.title, t)}</button>}
        {stageDeliverables.length === 0 ? null : <button type="button" className={css.dockLink}
          onClick={() => { openOutputs() }}>{t('task.deliverables.stageGroup', { count: stageDeliverables.length })}</button>}
        <button type="button" className={`${css.dockLink} ${finalOutput === undefined ? css.dockPrimary : ''}`} onClick={openDetails}>{t('task.card.openScene')}</button>
        {model.status !== 'completed' || hasFinal || requestDelivery === undefined ? null : <button type="button"
          className={css.dockLink} disabled={actionDisabled} onClick={() => { void submitDeliveryReview() }}>{t('task.deliverables.reviewMissing')}</button>}
        {!canCorrect || correctionOpen ? null : <button ref={correctionButton} type="button" className={css.dockLink}
          onClick={() => { setCorrectionTarget('team'); setCorrectionOpen(true) }}>{t('task.correction')}</button>}
        {terminal || model.status === 'stopping' || stopRun === undefined || confirmStop ? null : <button type="button"
          className={css.dockLink} disabled={actionDisabled} onClick={() => { setConfirmStop(true) }}>{t('task.stop')}</button>}
      </div>
    </div>
  }

  const membersSection = (<section className={css.section} aria-label={t('task.members')}>
    <div className={css.sectionHeader}><h2>{t('task.members')}</h2><span>{t('task.overview.completedMembers', { completed: model.members.filter(member => member.status === 'completed').length, total: model.members.length })}</span></div>
    {followed.length === 0 ? null : <nav className={css.followedMembers} aria-label={t('task.member.following')}>{model.members.filter(member => followed.includes(member.agentId)).map(member => <button type="button" key={member.agentId} className={css.secondaryButton} onClick={() => { setSelectedMemberId(member.agentId) }}>{member.name}<span>{t(memberStatusKey(member.status))}</span></button>)}</nav>}
    {model.members.length === 0 ? <p className={css.muted}>{t(terminal ? 'task.members.notRecorded' : 'task.members.pending')}</p> : (
      <div className={css.memberList}>
        {model.members.map((member, index) => (
          <button className={css.member} type="button" key={member.agentId}
            ref={(button) => {
              if (button === null) memberButtons.current.delete(member.agentId); else memberButtons.current.set(member.agentId, button)
            }}
            aria-label={t('task.member.open', { name: member.name })}
            onClick={() => { setSelectedMemberId(member.agentId) }}>
            <span className={css.memberAvatar} data-tone={index % 3} aria-hidden>{member.name.slice(0, 1)}<span className={css.memberStatus} data-executing={(executing && member.status === 'running') || undefined} data-status={member.status} /></span>
            <span className={css.memberIdentity}>
              <strong>{member.name}</strong>
              <span>{member.stages.some(stage => stage.status === 'running') ? stageLabel(member.stages.find(stage => stage.status === 'running')?.name ?? '', t) : member.duty === '' ? t(member.role === 'lead' ? 'task.member.lead' : 'task.member.worker') : member.duty}</span>
            </span>
            <span className={css.memberState}>{t(memberStatusKey(member.status))}</span>
            <IconChevronRightOutline14 size={16} className={css.memberChevron} />
          </button>
        ))}
      </div>
    )}
  </section>)
  const deliverySection = <section className={css.deliverySection} aria-label={t('task.deliverables')}>
    <div className={css.sectionHeader}><h2>{t('task.overview.outputs')}</h2>
      <span>{hasFinal ? finalDeliverables.length > 0 ? t('task.deliverables.finalReady', { count: finalDeliverables.length }) : t('task.deliverables.summaryReady') : t('task.deliverables.noFinal')}</span>
    </div>
    {model.deliverables.length < 5 ? null : <div className={css.outputSearch}>
      <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden><circle cx="8.5" cy="8.5" r="5.5" /><path d="m13 13 4 4" /></svg>
      <input type="search" aria-label={t('task.overview.findOutput')} placeholder={t('task.overview.findOutput')} value={outputQuery} onChange={(event) => { setOutputQuery(event.currentTarget.value) }} />
      {outputQuery === '' ? null : <Tooltip label={t('task.overview.clearSearch')}><button type="button" className={css.sceneIconButton} aria-label={t('task.overview.clearSearch')} onClick={() => { setOutputQuery('') }}><IconCloseOutline16 /></button></Tooltip>}
    </div>}
    {model.deliverables.length === 0 ? <p className={css.muted}>{t('task.deliverables.empty')}</p> : <>
      {visibleFinal.length + visibleStages.length > 0 ? null : <p className={css.resourceEmpty} role="status">{t('task.overview.noMatch')}</p>}
      <DeliverableItems items={visibleFinal} sessionId={sessionId} runId={model.runId} {...deliverableView} t={t} />
      {visibleStages.length === 0 ? null : visibleStages.length === 1 || outputQuery.trim() !== '' ? <DeliverableItems
        items={visibleStages} sessionId={sessionId} runId={model.runId}
        {...deliverableView} t={t} /> : <details className={css.stageDeliverables}>
        <summary>{t('task.deliverables.stageGroup', { count: visibleStages.length })}</summary>
        <DeliverableItems items={visibleStages} sessionId={sessionId} runId={model.runId} {...deliverableView} t={t} />
      </details>}
    </>}
  </section>

  return (
    <div ref={sceneRoot} className={css.panel} data-weave-work-task onKeyDown={(event) => { if (event.key === 'Escape' && selectedMember !== undefined && activeTab === 'progress') { if (correctionOpen) setCorrectionOpen(false); else closeMember() } }} data-status={model.status} data-delivery={hasFinal ? 'ready' : 'missing'}>
      <div className={css.viewControls}><WorkTaskTabs selected={activeTab} select={selectTab} id={tabId} t={t} />
        {expandDetails === undefined ? null : <Tooltip label={t('task.fullWidth')} side="bottom"><button type="button" className={`${css.sceneIconButton} ${css.fullWidthButton}`} aria-label={t('task.fullWidth')} onClick={expandDetails}><IconFullscreenOutline16 /></button></Tooltip>}
        <Tooltip label={t('task.sideBySide')} side="bottom"><button type="button" className={`${css.sceneIconButton} ${css.sideBySideButton}`} aria-label={t('task.sideBySide')} onClick={openDetails}><IconPanelLeftOutline16 /></button></Tooltip>
        {returnToConversation === undefined ? null : <Tooltip label={t('task.card.returnConversation')} side="bottom"><button type="button" className={`${css.sceneIconButton} ${css.conversationReturn}`} aria-label={t('task.card.returnConversation')} onClick={returnToConversation}><IconChevronLeftOutline14 size={16} /></button></Tooltip>}
      </div>
      <header className={css.hero} hidden={activeTab === 'progress' && selectedMember !== undefined}>
        <div className={css.eyebrow}>{teamDisplayName(model.teamName, t(model.runId === '' && model.status === 'preparing' ? 'task.team.pending' : 'task.team.unknown'))}</div>
        <h1 className={css.taskTitle}>{taskTitle || t('task.workScene')}</h1>
        <div className={css.statusLine} role="status">
          <span className={css.statusDot} data-executing={taskIsExecuting(model) || undefined} data-status={model.status === 'completed' && !hasFinal ? 'attention' : model.status} aria-hidden />
          <strong>{t(taskStatusKey(model))}</strong>
          {model.totalStages === 0 ? null : <span>{t('task.progress.count', { completed: model.completedStages, total: model.totalStages })}</span>}
          {activityHint === '' ? null : <span className={css.activityHint} title={activityHint}>{activityHint}</span>}
        </div>
        <WorkTaskDeliveryState model={model} executionLabel={t(statusKey(model.status))}
          recheckDelivery={recheckDelivery} disabled={actionDisabled} t={t} />
        {failureNotice}
        {pendingStopNotice}
        {budgetStage?.budgetPause === undefined ? null : <p>{t('task.budget.usage', { used: budgetStage.budgetPause.roundsUsed, total: budgetStage.budgetPause.authorizedTotalRounds })} {t(budgetStage.budgetPause.reason === 'total_limit' ? 'task.budget.total' : budgetStage.budgetPause.reason === 'slice_limit' ? 'task.budget.slice' : 'task.budget.blocked')}</p>}
        {model.status !== 'waiting' || pendingStopStages.length > 0 || budgetStage !== undefined ? null : <p>{t(model.waitKind === 'human' ? 'task.wait.humanHelp'
          : model.waitKind === 'correction' ? 'task.wait.correctionHelp' : model.waitKind === 'runtime' ? 'task.wait.runtimeHelp'
            : model.waitKind === 'timer' ? 'task.wait.timerHelp' : model.waitKind === 'fanout' ? 'task.wait.fanoutHelp' : 'task.wait.unknownHelp')}</p>}
        {model.status !== 'completed' || hasFinal ? null : <div className={css.deliveryGap}>
          <p>{t('task.deliverables.missingFinal')}</p>
          {requestDelivery === undefined ? null : <button type="button" className={css.primaryButton} disabled={actionDisabled} onClick={() => { void submitDeliveryReview() }}>{t('task.deliverables.reviewMissing')}</button>}
        </div>}
        {model.status !== 'waiting' || model.waitKind !== 'human' || model.humanTask !== null || returnToConversation === undefined ? null : <button type="button" className={css.primaryButton} onClick={returnToConversation}>{t('task.human.respond')}</button>}
        {model.brief === '' || activeTab === 'overview' ? null : <details className={css.briefDetails}><summary>{t('task.brief')}</summary><p>{model.brief}</p></details>}
        {!stale && !incomplete ? null : <div className={css.truthLine}>
          {stale ? <span>{t('task.freshness.stale')}</span> : null}
          {incomplete ? <span>{t('task.completeness.partial')}</span> : null}
        </div>}
        {model.pendingAction === null ? null : <div className={css.pendingAction} role="status">{t(
          model.pendingAction.kind === 'human-complete' ? 'task.human.recorded' : model.pendingAction.kind === 'stop' ? 'task.action.stopPending' : model.pendingAction.kind === 'rerun' ? 'task.action.rerunPending'
            : model.pendingAction.kind === 'stage-retry' ? 'task.action.stageRetryPending' : model.pendingAction.kind === 'correction-request' ? 'task.action.correctionPending' : 'task.action.correctionConfirmPending',
        )}</div>}
      </header>

      <div role="tabpanel" id={`${tabId}-overview-panel`} aria-labelledby={`${tabId}-overview-tab`} hidden={activeTab !== 'overview'}>
        {model.status !== 'waiting' && model.status !== 'failed' && model.blocker === 'none' && activeCorrection === undefined ? null : <button type="button" className={css.attentionLink} onClick={() => { selectTab('progress'); setSelectedMemberId('') }}>
          <span>{t('task.overview.attention')}</span><IconChevronRightOutline14 />
        </button>}
        <WorkSceneOverview deliverables={model.deliverables.map(item => ({ ...item, title: deliverableTitle(item.title, t) }))} members={model.members} brief={model.brief} openOutput={(id) => { openOutputs(id) }} openOutputs={() => { openOutputs() }} openMembers={() => { actions.rememberReading(`scene:${model.runId}:progress:`, { top: 0, follow: false, lastEvent: '' }); setSelectedMemberId('') }} t={t} />
      </div>

      <div role="tabpanel" id={`${tabId}-progress-panel`} aria-labelledby={`${tabId}-progress-tab`} hidden={activeTab !== 'progress'}>
        {memberView}
        <div hidden={selectedMember !== undefined}>
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
          {model.blocker === 'none' || model.actionError === 'stop_unconfirmed' ? null : (
            <section className={css.issue} role="status">
              <strong>{t('task.blocker')}</strong>
              <span>{t(model.blocker === 'runtime-missing'
                ? 'task.blocker.runtimeMissing'
                : model.blocker === 'failed'
                  ? 'task.blocker.failed'
                  : 'task.blocker.queued')}</span>
            </section>
          )}

          {humanSection}
          {model.humanTaskCount === 0 || model.humanTask !== null ? null : (
            <section className={css.attention}>
              <strong>{t('task.human')}</strong>
              <span>{t('task.human.count', { count: model.humanTaskCount })}</span>
            </section>
          )}
          {actionError === null ? null : <div className={css.actionError} role="alert">{actionError}</div>}
          {model.actionError === '' ? null : <div className={css.actionError} role="alert">{t(model.actionError === 'stop_unconfirmed' ? 'task.stop.unconfirmedHelp' : 'task.action.rejected')}</div>}
          {recoverySection}
          {selectedMember !== undefined ? null : activeCorrection !== undefined ? correctionView() : model.corrections.length === 0 ? null : <details className={css.pastCorrection}><summary>{t('task.overview.pastCorrection')}</summary>{correctionView()}</details>}
          {selectedMember === undefined ? correctionControls : null}
          {membersSection}
          {historySection}
        </div>
      </div>
      <div role="tabpanel" id={`${tabId}-outputs-panel`} aria-labelledby={`${tabId}-outputs-tab`} hidden={activeTab !== 'outputs'}>
        {deliverySection}
        {model.status !== 'completed' || !hasFinal || assessOutcome === undefined ? null : <details className={css.assessment}>
          <summary>{t('task.assessment')}</summary>
          <p>{t('task.assessment.notice')}</p>
          {canAssessDelivery ? null : <p>{t((model.delivery?.revisionId ?? '') === '' ? 'task.assessment.revisionUnavailable' : 'task.assessment.deliveryUnavailable')}</p>}
          {actionError === null ? null : <p role="alert">{actionError}</p>}
          <textarea aria-label={t('task.assessment.placeholder')} disabled={!canAssessDelivery} value={assessmentNote} onChange={(event) => { setAssessmentNote(event.currentTarget.value) }} placeholder={model.outcomeRevisionId === model.delivery?.revisionId && model.outcomeNote || t('task.assessment.placeholder')} />
          <div className={css.controlActions}>
            <button type="button" className={css.secondaryButton} disabled={actionDisabled || !canAssessDelivery} onClick={() => { void submitAssessment('needs-revision') }}>{t('task.assessment.needsRevision')}</button>
            <button type="button" className={css.primaryButton} disabled={actionDisabled || !canAssessDelivery} onClick={() => { void submitAssessment('adopted') }}>{t('task.assessment.adopted')}</button>
          </div>
        </details>}
      </div>
      <div className={css.sceneSupplement} hidden={activeTab === 'overview' || activeTab === 'progress' && selectedMember !== undefined}>
        <details hidden={activeTab !== 'progress' || model.totalStages === 0} className={css.section} aria-label={t('task.progress')}>
          <summary>{t('task.progress')} · {t('task.progress.count', { completed: model.completedStages, total: model.totalStages })}</summary>
          {model.totalStages > 0 ? (
            <div className={css.progressTrack} aria-valuemin={0} aria-valuemax={100} aria-valuenow={progressPercent} role="progressbar" aria-label={t('task.progress')}>
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
              <strong>{stageLabel(model.latestStage, t)}</strong>
            </div>
          )}
          {model.stages.length === 0 ? null : (
            <ol className={css.stages}>
              {model.stages.map((stage, index) => (
                <li key={`${stage.name}-${index}`} data-status={stage.status}>
                  <span className={css.stageMark} aria-hidden />
                  <span>{stageLabel(stage.name, t)}</span>
                </li>
              ))}
            </ol>
          )}
        </details>
        {stopControls}
        {!terminal || rerun === undefined ? null : <details className={css.secondaryActions}><summary>{t('task.moreActions')}</summary>
          <section className={css.controlSection} aria-label={t('task.rerun')}>
            {revisionOpen ? (
              <div className={css.revisionBox}>
                <label htmlFor={`weave-revision-${panelId}`}>{t('task.rerun.brief')}</label>
                <textarea
                  id={`weave-revision-${panelId}`}
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
              <button type="button" className={css.secondaryButton} onClick={() => { setRevisedBrief(model.brief); setRevisionOpen(true) }}>{t('task.rerun')}</button>
            )}
          </section>
        </details>}
        {model.attempts.length < 2 ? null : (
          <section className={css.section} aria-label={t('task.attempts')}>
            <div className={css.sectionHeader}><span>{t('task.attempts')}</span><span>{model.attempts.length}</span></div>
            <ol className={css.attemptList}>
              {model.attempts.map((attempt, index) => (
                <li key={attempt.clientRequestId || attempt.runId} data-current={attempt.runId === model.runId || undefined}>
                  <span>{t('task.attempt', { index: index + 1 })}</span>
                  <strong>{t(statusKey(attempt.status))}</strong>
                  {attempt.brief === '' ? null : (
                    <details className={css.attemptBrief}>
                      <summary>{t('task.attempt.brief')}</summary>
                      <p>{attempt.brief}</p>
                    </details>
                  )}
                </li>
              ))}
            </ol>
          </section>
        )}
        {model.runId === '' ? null : <details className={css.diagnostics}>
          <summary>{t('task.diagnostics')}</summary>
          <section className={css.section} aria-label={t('task.runtimes')}>
            <div className={css.sectionHeader}><span>{t('task.runtimes')}</span><span>{model.runtimes.length}</span></div>
            {model.runtimes.length === 0 ? <p className={css.muted}>{t(terminal ? 'task.runtimes.notRecorded' : 'task.runtimes.empty')}</p> : (
              <div className={css.runtimeList}>
                {displayedRuntimes.map((runtime, index) => (
                  <div className={css.runtime} key={`${runtime.name}-${index}`}>
                    <span className={css.statusDot} data-status={runtime.status} aria-hidden />
                    <span className={css.runtimeIdentity}>
                      <strong>{runtimeNameLabel(runtime.name, t)}</strong>
                      <span>{runtimeDetailLabel(runtime.detail, t)}</span>
                    </span>
                    <span className={css.runtimeStatus}>{t(statusKey(runtime.status))}</span>
                  </div>
                ))}
              </div>
            )}
          </section>
          <dl><dt>{t('task.runId')}</dt><dd>{model.runId}</dd><dt>{t('task.workflow')}</dt><dd>{model.workflowName}</dd></dl>
          <dl>{model.members.map(member => <div key={member.agentId}>
            <dt>{member.name} · {t('task.member.identifier')}</dt><dd>{member.agentId}</dd>
          </div>)}</dl>
          {model.tokensIn + model.tokensOut === 0 && model.costUSD === 0 ? null : <dl><dt>{t('task.usage')}</dt><dd>{model.tokensIn + model.tokensOut}</dd><dt>{t('task.cost')}</dt><dd>{model.costUSD.toFixed(4)}</dd></dl>}
        </details>}
      </div>
    </div>
  )
}
