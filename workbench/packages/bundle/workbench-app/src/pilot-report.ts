/** Commercial-pilot evidence derived only from durable Workbench task projections. */

import type { Session } from '@deepseek-ai/dsh-session'
import type { WorkTaskProjection } from './index.ts'

interface TaskSource {
  list(): readonly Session[]
}

interface ProjectionSource {
  stateOf(session: Session, key: 'workTask'): { readonly task: WorkTaskProjection | null } | undefined
}

/** One task outcome included in the bounded commercial-pilot report. */
export interface PilotReportTask {
  readonly team: string
  readonly status: WorkTaskProjection['status']
  readonly startedAt: string
  readonly finishedAt: string
  readonly durationMs: number
  readonly costUSD: number
  readonly tokensIn: number
  readonly tokensOut: number
  readonly corrections: number
  readonly reruns: number
  readonly finalDeliverables: number
  readonly verificationStatus: 'pending' | 'passed' | 'failed' | 'unknown'
  readonly outcome: WorkTaskProjection['outcome']
  readonly outcomeNote: string
}

/** Aggregate commercial-pilot evidence and its contributing task rows. */
export interface PilotReport {
  readonly generatedAt: string
  readonly summary: {
    readonly tasks: number
    readonly completed: number
    readonly executionCompletionRate: number
    readonly verification: Readonly<Record<'pending' | 'passed' | 'failed' | 'unknown', number>>
    readonly adopted: number
    readonly needsRevision: number
    readonly unrated: number
    readonly totalCostUSD: number
    readonly averageDurationMs: number
    readonly corrections: number
    readonly reruns: number
  }
  readonly tasks: readonly PilotReportTask[]
}

/**
 * Build a bounded, secretless report suitable for a design-partner review.
 * @param sessions - Session inventory used to locate candidate work tasks.
 * @param projections - Durable WorkTask projection reader.
 * @returns The aggregate pilot report and its bounded task rows.
 */
export function buildPilotReport(sessions: TaskSource, projections: ProjectionSource): PilotReport {
  const tasks = sessions.list().flatMap((session): PilotReportTask[] => {
    const task = projections.stateOf(session, 'workTask')?.task
    if (task === null || task === undefined || task.runId === '') return []
    const started = Date.parse(task.startedAt)
    const finished = Date.parse(task.finishedAt)
    const durationMs = Number.isFinite(started) && Number.isFinite(finished) && finished >= started
      ? finished - started
      : Math.max(0, task.updatedAt - (task.attempts[0]?.createdAt ?? task.updatedAt))
    return [{
      team: task.teamName || task.teamId,
      status: task.status,
      startedAt: task.startedAt,
      finishedAt: task.finishedAt,
      durationMs,
      costUSD: task.costUSD,
      tokensIn: task.tokensIn,
      tokensOut: task.tokensOut,
      corrections: task.corrections.length,
      reruns: Math.max(0, task.attempts.length - 1),
      finalDeliverables: task.deliverables.filter(item => item.kind === 'final').length,
      verificationStatus: task.delivery?.verificationStatus ?? 'unknown',
      outcome: task.outcome,
      outcomeNote: task.outcomeNote,
    }]
  })
  const completed = tasks.filter(task => task.status === 'completed').length
  const terminalDurations = tasks.filter(task => task.durationMs > 0)
  const sum = (values: readonly number[]): number => values.reduce((total, value) => total + value, 0)
  const totalCostUSD = Math.round(sum(tasks.map(task => task.costUSD)) * 1_000_000) / 1_000_000
  return {
    generatedAt: new Date().toISOString(),
    summary: {
      tasks: tasks.length,
      completed,
      executionCompletionRate: tasks.length === 0 ? 0 : Math.round(completed / tasks.length * 10_000) / 100,
      verification: {
        pending: tasks.filter(task => task.verificationStatus === 'pending').length,
        passed: tasks.filter(task => task.verificationStatus === 'passed').length,
        failed: tasks.filter(task => task.verificationStatus === 'failed').length,
        unknown: tasks.filter(task => task.verificationStatus === 'unknown').length,
      },
      adopted: tasks.filter(task => task.outcome === 'adopted').length,
      needsRevision: tasks.filter(task => task.outcome === 'needs-revision').length,
      unrated: tasks.filter(task => task.outcome === 'unrated').length,
      totalCostUSD,
      averageDurationMs: terminalDurations.length === 0
        ? 0
        : Math.round(sum(terminalDurations.map(task => task.durationMs)) / terminalDurations.length),
      corrections: sum(tasks.map(task => task.corrections)),
      reruns: sum(tasks.map(task => task.reruns)),
    },
    tasks,
  }
}
