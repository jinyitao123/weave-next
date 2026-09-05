// @vitest-environment jsdom
import { cleanup, fireEvent, render, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import type { SessionListState, SessionSummary } from '@deepseek-ai/dsh-api-session-controller/client'
import type { WorkspaceId, WorkspaceView } from '@deepseek-ai/dsh-api-workspace-controller/client'
import type { SessionId } from '@deepseek-ai/dsh-session/types'
import { ProjectActivity } from '../src/client/ProjectActivity.tsx'
import { latestProjectMessage, projectActivityModel } from '../src/client/project-activity.ts'
import { zh } from '../src/client/project-activity-locales.ts'
import { workTaskModel, type WorkTaskProjection } from '../src/client/work-task-model.ts'

afterEach(cleanup)
const sid = (value: string) => value as SessionId
const task = (runId: string, overrides: Partial<WorkTaskProjection> = {}): WorkTaskProjection => ({
  ...workTaskModel([]), clientRequestId: `request-${runId}`, runId, teamId: 'team', teamName: '研究团队',
  brief: `任务 ${runId}`, updatedAt: 10, observedAt: 10, status: 'running', ...overrides,
})
const session = (id: string, projection?: WorkTaskProjection): SessionSummary => ({
  id: sid(id), displayTitle: id, running: false, blank: false, updatedAt: 1, cwd: '/shared',
  ...(projection === undefined ? {} : { projectionValues: { workTask: projection } }),
})
const sessions = (...items: SessionSummary[]): SessionListState => ({
  ids: items.map(item => item.id), byId: Object.fromEntries(items.map(item => [item.id, item])),
  current: undefined, phase: 'ready', subagentsByParent: {}, jobsBySession: {}, currentAddress: undefined,
})
const workspace = (...ids: string[]): WorkspaceView => ({
  workspaceId: 'project' as WorkspaceId, path: '/shared', title: '活动筹备', sessionIds: ids.map(sid), createdAt: '0', updatedAt: '0',
})
function props(project: WorkspaceView, list: SessionListState): Parameters<typeof ProjectActivity>[0] {
  return {
    workspaceId: project.workspaceId, onClose: vi.fn(), openTask: vi.fn(), openTaskResults: vi.fn(),
    useSessions: (select: (value: SessionListState) => unknown) => select(list),
    useWorkspaces: (select: (value: unknown) => unknown) => select({ items: [project], archivedSessionIds: [], phase: 'ready' }),
    t: makeTranslate(zh),
  } as unknown as Parameters<typeof ProjectActivity>[0]
}

describe('project activity from explicit Session membership', () => {
  it('counts an unavailable task projection without inventing tasks for ordinary Sessions', () => {
    const cold = { ...session('cold'), projectionUnavailableKeys: ['workTask'] }
    const ordinary = { ...session('ordinary'), projectionValues: { workTask: null }, projectionUnavailableKeys: ['workTask'] }
    const result = projectActivityModel(workspace('cold', 'ordinary', 'no-task'), sessions(cold, ordinary, session('no-task')), [])
    expect(result.missingSessions).toBe(1)
    expect(result.tasks).toEqual([])
  })
  it('excludes same-path strangers and archived tasks; deduplicates old views of the same physical run', () => {
    const list = sessions(session('old-view', task('run', { observedAt: 1 })), session('current-view', task('run', { observedAt: 20 })),
      session('archived', task('archived')), session('same-path', task('stranger')), session('ordinary'))
    const result = projectActivityModel(workspace('old-view', 'current-view', 'current-view', 'archived', 'ordinary', 'missing'), list, [sid('archived')])
    expect(result.tasks.map(item => item.sessionId)).toEqual([sid('current-view')])
    expect(result.missingSessions).toBe(1)
    expect(result.teamCount).toBe(1)
    expect(projectActivityModel(undefined, list, []).tasks).toEqual([])
  })

  it('uses the latest public update and tolerates older stages without public updates', () => {
    const projection = task('updates', { members: [{
      agentId: 'writer', name: '撰稿员', duty: '', role: 'worker', status: 'running', runtime: '', updateMode: 'live',
      stages: [
        { publicUpdates: [
          { text: '最后一条记录', occurredAt: '2026-09-05T01:00:00Z' },
          { text: '更早的记录', occurredAt: '2026-09-05T00:00:00Z' },
          { text: '无效日期', occurredAt: 'unknown' },
        ] },
        {},
      ] as unknown as WorkTaskProjection['members'][number]['stages'],
    }] })
    expect(latestProjectMessage(projection)).toBe('最后一条记录')
    const view = render(<ProjectActivity {...props(workspace('work'), sessions(session('work', projection)))} />)
    expect(view.getByText('最后一条记录')).toBeTruthy()
    expect(view.getByText(/记录时间 .*（UTC）/u)).toBeTruthy()
    expect(latestProjectMessage(task('empty'))).toBe('')
  })

  it('shows current task, members, location, human wait and saves; both links return to the same task', () => {
    const projection = task('run-1', {
      status: 'waiting', waitKind: 'human', latestStage: '核对活动方案',
      humanTask: { interactionId: 'question', nodeId: 'review', title: '请确认活动日期', instructions: '', resumeSchema: {} },
      runtimes: [{ name: '协作电脑', detail: '远程执行', status: 'waiting' }],
      members: [{ agentId: 'writer', name: '撰稿员', duty: '整理活动安排', role: 'worker', status: 'completed', runtime: '协作电脑', updateMode: 'on_completion', stages: [] }],
      deliverables: [{ id: 'brief', title: '活动安排.md', kind: 'stage', contentType: 'text/markdown', content: '安排', preview: '', truncated: false, createdAt: '' }],
    })
    const input = props(workspace('work'), sessions(session('work', projection)))
    const view = render(<ProjectActivity {...input} />)
    expect(view.getByText(/研究团队/u)).toBeTruthy()
    expect(view.getByText('撰稿员')).toBeTruthy()
    expect(view.getByText('核对活动方案')).toBeTruthy()
    expect(view.getByText('请确认活动日期')).toBeTruthy()
    expect(view.getByText('协作电脑 · 远程执行')).toBeTruthy()
    fireEvent.click(view.getByRole('button', { name: '活动安排.md' }))
    expect(input.openTaskResults).toHaveBeenCalledWith('work', 'run-1', 'brief')
    fireEvent.click(view.getByRole('button', { name: '查看成果' }))
    expect(input.openTaskResults).toHaveBeenLastCalledWith('work', 'run-1', undefined)
    fireEvent.click(view.getByRole('button', { name: '打开工作对话' }))
    expect(input.openTask).toHaveBeenCalledWith('work')
    expect(input.onClose).toHaveBeenCalledTimes(3)
  })

  it('does not count a finished run with only stage outputs as final delivery or revive an old human wait', () => {
    const stage = { id: 'stage', title: '阶段记录', kind: 'stage' as const, contentType: 'text/plain', content: '记录', preview: '', truncated: false, createdAt: '' }
    const finished = task('finished', { status: 'completed', deliverables: [stage] })
    const stopped = task('stopped', { status: 'stopped', waitKind: 'human', humanTaskCount: 1 })
    const view = render(<ProjectActivity {...props(workspace('finished', 'stopped'), sessions(session('finished', finished), session('stopped', stopped)))} />)
    expect(within(view.getByRole('article', { name: '任务 finished' })).getAllByText('执行已结束，最终成果待核实').length).toBeGreaterThan(0)
    expect(within(view.getByRole('article', { name: '任务 stopped' })).getByText('当前没有需要你处理的事项')).toBeTruthy()
    expect(view.queryByText('回答团队的问题')).toBeNull()
    expect(view.getByText('1 份可用成果')).toBeTruthy()
  })

  it('keeps incomplete membership visible and does not claim recorded output quality is accepted', () => {
    const projection = task('done', { status: 'completed', deliverables: [{
      id: 'final', title: '最终方案', kind: 'final', contentType: 'text/plain', content: '方案', preview: '', truncated: false, createdAt: '',
    }] })
    const view = render(<ProjectActivity {...props(workspace('work', 'missing'), sessions(session('work', projection)))} />)
    expect(view.getByRole('status').textContent).toBe('部分任务记录尚未加载，当前汇总不完整。')
    expect(view.getByText('成果是否合格仍需确认')).toBeTruthy()
    expect(view.getByText('查看成果并确认是否采用')).toBeTruthy()
  })
})
