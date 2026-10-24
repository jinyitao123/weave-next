import { describe, expect, it, vi } from 'vitest'
import { trackTasks } from './notify'
import { runStageState, stageTotals, waitText, type TaskSummary } from './tasks'

describe('waitText', () => {
  it('explains why a task waits without internal codes', () => {
    expect(waitText({ waiting: false })).toBe('')
    expect(waitText({ waiting: true, reason: 'team_pickup' })).toBe('等待团队接手')
    expect(waitText({ waiting: true, reason: 'node_busy', position: 3, node_name: 'mac' })).toBe('节点“mac”正忙，排在第 3 位')
    expect(waitText({ waiting: true, reason: 'runtime_offline', node_name: 'mac' })).toBe('等待节点“mac”：离线')
    expect(waitText({ waiting: true, reason: 'something_new' })).toBe('等待节点：暂不可接任务')
  })
})

describe('trackTasks', () => {
  it('counts tasks that finished while the window was in the background', () => {
    const task = (status: string): TaskSummary => ({ run_id: 'run-1', title: '修复', team_id: 't', team_name: '团队', workflow_id: 'f', workflow_version: 1, status, source_kind: '', created_at: '', updated_at: '' })
    vi.spyOn(document, 'hasFocus').mockReturnValue(false)
    const open = vi.fn()
    trackTasks([task('running')], open)
    trackTasks([task('running')], open)
    expect(document.title.startsWith('(')).toBe(false)
    trackTasks([task('succeeded')], open)
    expect(document.title.startsWith('(1)')).toBe(true)
    window.dispatchEvent(new Event('focus'))
    expect(document.title.startsWith('(')).toBe(false)
  })
})

describe('stageTotals', () => {
  it('keeps missing evidence and waiting or stopped steps distinct from steps not started', () => {
    expect(runStageState('')).toBe('unknown')
    expect(runStageState('waiting')).toBe('waiting')
    expect(runStageState('cancelled')).toBe('cancelled')
    expect(runStageState('pending')).toBe('pending')
    expect(runStageState('completed')).toBe('completed')
  })
  it('names rounds, tokens and the models a step was actually sent to', () => {
    expect(stageTotals({ rounds: 3, input_tokens: 8744, output_tokens: 747, models: ['deepseek-v4-pro'] })).toBe('3 轮 · 输入 8744 / 输出 747 tokens · 实际模型 deepseek-v4-pro')
    expect(stageTotals({ rounds: 1, input_tokens: 10, output_tokens: 2 })).toBe('输入 10 / 输出 2 tokens')
    expect(stageTotals({})).toBe('')
  })
})
