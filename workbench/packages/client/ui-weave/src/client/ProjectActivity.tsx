import { useMemo } from 'react'
import { Modal } from '@deepseek-ai/dsh-client-ui-primitives'
import type { InjectFace, PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import type {} from '@deepseek-ai/dsh-client-ui-workspace/client'
import type { SessionId } from '@deepseek-ai/dsh-session/types'
import { zh, type ProjectActivityKey } from './project-activity-locales.ts'
import { latestProjectMessage, projectActivityModel } from './project-activity.ts'
import { workTaskFactsStale, workTaskHasFinalDeliverable } from './work-task-model.ts'
import type { WorkTaskMemberStatus, WorkTaskProjection } from './work-task-model.ts'
import css from './ProjectActivity.module.css'

type ProjectActivityInjected = {
  openTask: (sessionId: SessionId) => void
  openTaskResults: (sessionId: SessionId, runId: string, deliverableId?: string) => void
}
type ProjectActivityProps = PropsRuntime<'sidebar.workspaces.projectActivity'>
  & PropsLocale<'projectActivity'> & InjectFace<ProjectActivityInjected>

const MEMBER_STATUS: Record<WorkTaskMemberStatus, ProjectActivityKey> = {
  waiting: 'waiting',
  pending: 'memberPending', running: 'memberRunning', 'partially-completed': 'memberPartial',
  completed: 'memberCompleted', failed: 'memberFailed', stopped: 'memberStopped', 'not-recorded': 'memberUnknown',
}

function statusKey(task: WorkTaskProjection): ProjectActivityKey {
  const key = `task.state.${task.displayState ?? ''}`
  if (Object.hasOwn(zh, key)) return key as ProjectActivityKey
  if (task.actionError === 'stop_unconfirmed') return 'stopUnconfirmed'
  if (task.status === 'completed' && !workTaskHasFinalDeliverable(task)) return 'missingOutput'
  return task.status === 'failed' ? 'failedStatus' : task.status
}

function todoKey(task: WorkTaskProjection): ProjectActivityKey | null {
  if (task.status === 'waiting' && task.waitKind === 'human') return 'human'
  if (task.status === 'waiting' && task.waitKind === 'correction') return 'correction'
  if (task.status === 'failed' || task.actionError === 'stop_unconfirmed') return 'failed'
  if (task.status === 'completed' && !workTaskHasFinalDeliverable(task)) return 'missingOutput'
  if (task.outcome === 'needs-revision') return 'revision'
  if (task.status === 'completed' && task.outcome === 'unrated') return 'review'
  return null
}

/** Project overview reads the Session index; actions only navigate to the original task. */
export function ProjectActivity({ workspaceId, onClose, useWorkspaces, useSessions, openTask, openTaskResults, t }: ProjectActivityProps) {
  const workspaces = useWorkspaces(value => value)
  const sessions = useSessions(value => value)
  const workspace = workspaces.items.find(item => item.workspaceId === workspaceId)
  const model = useMemo(() => projectActivityModel(workspace, sessions, workspaces.archivedSessionIds),
    [workspace, sessions, workspaces.archivedSessionIds])
  const outputCount = model.tasks.reduce((count, { task }) => count + task.deliverables.length, 0)
  const attentionCount = model.tasks.filter(({ task }) => todoKey(task) !== null).length
  const navigate = (sessionId: SessionId, runId?: string, deliverableId?: string): void => {
    onClose()
    if (runId === undefined) openTask(sessionId)
    else openTaskResults(sessionId, runId, deliverableId)
  }
  return <Modal open onClose={onClose} closeLabel={t('close')}
    title={t('title', { name: workspace?.title || workspace?.path || '' })}
    description={t('scope')} className={css.dialog ?? ''} contentClassName={css.content ?? ''}>
    {workspace === undefined ? <p role="status">{t('unavailable')}</p> : <>
      <div className={css.summary}>
        <strong>{t('tasks', { n: model.tasks.length })}</strong><span>{t('teams', { n: model.teamCount })}</span>
        <span>{t('attention', { n: attentionCount })}</span><span>{t('outputs', { n: outputCount })}</span>
      </div>
      {(model.missingSessions > 0 || sessions.phase !== 'ready' || workspaces.phase !== 'ready')
        && <p role="status">{t('incomplete')}</p>}
      {model.tasks.length === 0 && <p>{t('empty')}</p>}
      <div className={css.tasks}>{model.tasks.map(({ sessionId, title, task }) => {
        const todo = todoKey(task)
        const message = latestProjectMessage(task)
        return <article key={sessionId} className={css.task} aria-label={title}>
          <header><h3>{title}</h3><span>{t(statusKey(task))}</span></header>
          <div className={css.metadata}><span>{t('team')}{' · '}{task.teamName || t('teamUnknown')}</span>
            {task.totalStages > 0 && <span>{t('progress', { done: task.completedStages, total: task.totalStages })}</span>}
          </div>
          <dl className={css.facts}>
            <div><dt>{t('activity')}</dt><dd>{task.latestStage || t('noActivity')}</dd>
              {message !== '' && <dd className={css.message}>{message}</dd>}
              <dd className={css.muted}>{task.observedAt > 0
                ? t('updated', { time: new Date(task.observedAt).toISOString().replace('T', ' ').slice(0, 19) })
                : t('neverUpdated')}</dd></div>
            <div><dt>{t('location')}</dt><dd>{task.runtimes.length === 0 ? t('locationUnknown')
              : task.runtimes.map(runtime => runtime.name + (runtime.detail ? ` · ${runtime.detail}` : '')).join(' / ')}</dd></div>
            <div><dt>{t('todo')}</dt><dd>{todo === null ? t('noTodo') : todo === 'human' && task.humanTask !== null
              ? task.humanTask.title || t(todo) : t(todo)}</dd></div>
          </dl>
          {workTaskFactsStale(task.status, task.observedAt) && <p className={css.muted}>{t('stale')}</p>}
          <details open><summary>{t('members')}</summary>
            {task.members.length === 0 ? <p>{t('membersUnknown')}</p> : <ul className={css.members}>
              {task.members.map(member => <li key={member.agentId}><strong>{member.name}</strong>
                <span>{member.duty}</span><span>{t(MEMBER_STATUS[member.status])}</span>
                <span className={css.muted}>{member.runtime || t('locationUnknown')}</span></li>)}
            </ul>}
          </details>
          <section aria-label={t('saved')}>
            {task.deliverables.length === 0 ? <p className={css.muted}>{t('noOutputs')}</p>
              : <ul className={css.outputs}>{task.deliverables.map(item => <li key={item.id}>
                <button type="button" onClick={() => { navigate(sessionId, task.runId, item.id) }}>{item.title}</button>
                <span>{t(item.kind)}</span>
              </li>)}</ul>}
            {workTaskHasFinalDeliverable(task) && task.outcome !== 'adopted' && <p className={css.muted}>{t('unreviewed')}</p>}
          </section>
          <footer><button type="button" onClick={() => { navigate(sessionId) }}>{t('openTask')}</button>
            {task.deliverables.length > 0 && <button type="button" onClick={() => { navigate(sessionId, task.runId) }}>{t('openResults')}</button>}
          </footer>
        </article>
      })}</div>
    </>}
  </Modal>
}
