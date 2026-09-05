import type { SessionListState } from '@deepseek-ai/dsh-api-session-controller/client'
import type { WorkspaceView } from '@deepseek-ai/dsh-api-workspace-controller/client'
import type { SessionId } from '@deepseek-ai/dsh-session/types'
import type { WorkTaskProjection, WorkTaskPublicUpdate } from './work-task-model.ts'

/** One current task retained by a Session explicitly assigned to this Workspace. */
export interface ProjectActivityTask {
  readonly sessionId: SessionId
  readonly title: string
  readonly task: WorkTaskProjection
}

/** Read-only project projection; absent Session snapshots remain visibly incomplete. */
export interface ProjectActivityModel {
  readonly tasks: readonly ProjectActivityTask[]
  readonly missingSessions: number
  readonly teamCount: number
}

/**
 * Latest public runtime message already retained in the current task projection.
 * @param task - current task, including older persisted projections.
 * @returns latest public message, or an empty string when none was recorded.
 */
export function latestProjectMessage(task: WorkTaskProjection): string {
  let latestAt = Number.NEGATIVE_INFINITY
  let text = ''
  for (const member of task.members) {
    for (const stage of member.stages) {
      // Older persisted Session-index stages predate this optional field.
      const recorded: { readonly publicUpdates?: readonly WorkTaskPublicUpdate[] } = stage
      for (const update of recorded.publicUpdates ?? []) {
        const at = Date.parse(update.occurredAt)
        if (at > latestAt) { latestAt = at; text = update.text }
      }
    }
  }
  return text
}

/**
 * Read only explicit Workspace membership and Host-persisted task projections.
 * @param workspace - current Workspace record, absent after deletion.
 * @param sessions - framework Session index including durable task projections.
 * @param archivedIds - Sessions hidden by the Workspace registry.
 * @returns current tasks, without path-based grouping or historical-attempt promotion.
 */
export function projectActivityModel(
  workspace: WorkspaceView | undefined,
  sessions: SessionListState,
  archivedIds: readonly SessionId[],
): ProjectActivityModel {
  const archived = new Set(archivedIds)
  const tasks: ProjectActivityTask[] = []
  let missingSessions = 0
  for (const sessionId of new Set(workspace?.sessionIds ?? [])) {
    if (archived.has(sessionId)) continue
    const session = sessions.byId[sessionId]
    if (session === undefined) { missingSessions++; continue }
    const task = session.projectionValues?.workTask
    if (task === undefined && session.projectionUnavailableKeys?.includes('workTask') === true) { missingSessions++; continue }
    if (task == null) continue
    tasks.push({ sessionId, title: task.brief || session.displayTitle, task })
  }
  tasks.sort((left, right) => right.task.observedAt - left.task.observedAt
    || right.task.updatedAt - left.task.updatedAt || left.sessionId.localeCompare(right.sessionId))
  const seen = new Set<string>()
  const current = tasks.filter(({ sessionId, task }) => {
    const identity = task.runId || task.clientRequestId || sessionId
    if (seen.has(identity)) return false
    seen.add(identity)
    return true
  })
  const teams = new Set(current.map(({ task }) => task.teamId || task.teamName).filter(Boolean))
  return { tasks: current, missingSessions, teamCount: teams.size }
}
