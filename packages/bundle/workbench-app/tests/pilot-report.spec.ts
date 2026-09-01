import { describe, expect, it } from 'vitest'
import type { Session } from '@deepseek-ai/dsh-session'
import { buildPilotReport } from '../src/pilot-report.ts'
import type { WorkTaskProjection } from '../src/index.ts'

function task(overrides: Partial<WorkTaskProjection> = {}): WorkTaskProjection {
  return {
    brief: 'reconcile commerce orders', clientRequestId: 'request-1', runId: 'run-1',
    teamId: 'team-1', teamName: 'Commerce reconciliation', workflowName: 'default', status: 'completed',
    completedStages: 3, totalStages: 3, latestStage: 'deliver', members: [], corrections: [], runtimes: [],
    humanTaskCount: 0, deliverableCount: 1,
    deliverables: [{ id: 'file-1', title: 'report.md', kind: 'final', contentType: 'text/markdown',
      preview: '# done', content: '# done', truncated: false, createdAt: '2026-09-01T00:01:00Z' }],
    blocker: 'none', attempts: [{ clientRequestId: 'request-1', runId: 'run-1', brief: 'reconcile commerce orders',
      status: 'completed', completedStages: 3, totalStages: 3, latestStage: 'deliver', deliverableCount: 1,
      deliverables: [], createdAt: 1_000, updatedAt: 61_000 }],
    pendingAction: null, actionError: '', completeness: { run: 'complete' },
    startedAt: '2026-09-01T00:00:00Z', finishedAt: '2026-09-01T00:01:00Z',
    tokensIn: 1_000, tokensOut: 500, costUSD: 0.25, outcome: 'adopted', outcomeNote: 'used by finance',
    observedAt: 61_000, updatedAt: 61_000,
    ...overrides,
  }
}

describe('Workbench pilot report', () => {
  it('summarizes durable task outcomes without task prompts, run ids, or credentials', () => {
    const session = {} as Session
    const report = buildPilotReport(
      { list: () => [session] },
      { stateOf: () => ({ task: task() }) },
    )
    expect(report.summary).toMatchObject({
      tasks: 1, completed: 1, successRate: 100, adopted: 1, totalCostUSD: 0.25, averageDurationMs: 60_000,
    })
    expect(report.tasks[0]).toMatchObject({ team: 'Commerce reconciliation', outcome: 'adopted', finalDeliverables: 1 })
    const serialized = JSON.stringify(report)
    expect(serialized).not.toContain('reconcile commerce orders')
    expect(serialized).not.toContain('run-1')
    expect(serialized).not.toContain('request-1')
  })
})
