import { Context } from '@deepseek-ai/cordis'
import { describe, expect, it, vi } from 'vitest'
import { SlotRegistry } from '@deepseek-ai/dsh-client-ui-renderer/client'
import { apply, inject } from '../src/client/index.ts'
import { DeliverableRow } from '../src/client/DeliverableRow.tsx'
import { TeamListRow } from '../src/client/TeamListRow.tsx'
import { WorkTaskHeader, WorkTaskPanel } from '../src/client/WorkTaskPanel.tsx'

describe('ui-weave browser plugin', () => {
  it('registers the Weave team-list wire name and dictionaries', async () => {
    const ctx = new Context()
    const slots = new SlotRegistry(ctx)
    slots.register({
      name: 'root', children: {
        'tool.call.toolview': { kind: 'keyed', scope: 'session' },
        'conversation.session.header.actions': { kind: 'list', scope: 'session' },
        'conversation.details.summary': { kind: 'single', scope: 'session' },
      },
    } as never, () => null)
    const dictionaries: unknown[] = []
    ctx.provide('locale', {
      register(namespace: string, value: unknown) {
        dictionaries.push({ namespace, value })
        return () => {}
      },
      bind: () => ((key: string) => key),
    })
    ctx.provide('layout', { openDetails() {}, closeDetails() {}, toggleSidebar() {} })
    const execute = vi.fn(() => Promise.resolve({ ok: true, value: { commandId: 'command-1', result: { kind: 'success' } } }))
    const send = vi.fn(() => Promise.resolve())
    ctx.provide('sessions', { scope: () => ({ get: (name: string) => name === 'conversation' ? { send } : undefined }) })
    ctx.provide('remote', { commands: { execute } })
    ctx.provide('remote.commands', { execute })
    await ctx.plugin({ inject: [...inject], apply }).await()
    const entries = slots.entries('tool.call.toolview')
    expect(entries).toHaveLength(2)
    expect(entries[0]?.options).toMatchObject({ key: 'mcp__weave__team_list' })
    expect(entries[0]?.locale).toBe('weave')
    expect(entries[0]?.component).toBe(TeamListRow)
    const teamInjected = (entries[0]?.inject as (sessionId: string) => {
      selectTeam(teamId: string, teamName: string): Promise<void>
    })('session-1')
    await teamInjected.selectTeam('team-1', '日冕推演团队')
    expect(send).toHaveBeenCalledWith(expect.stringContaining('team-1'))
    expect(entries[1]?.options).toMatchObject({ key: 'mcp__weave__deliverable_get' })
    expect(entries[1]?.locale).toBe('weave')
    expect(entries[1]?.component).toBe(DeliverableRow)
    const header = slots.entries('conversation.session.header.actions')
    expect(header).toHaveLength(1)
    expect(header[0]?.options).toMatchObject({ id: 'weave-work-task', order: -100 })
    expect(header[0]?.component).toBe(WorkTaskHeader)
    const summary = slots.entries('conversation.details.summary')
    expect(summary).toHaveLength(1)
    expect(summary[0]?.component).toBe(WorkTaskPanel)
    const injected = (summary[0]?.inject as (sessionId: string) => {
      stopRun(runId: string): Promise<string | null>
      rerun(runId: string, brief: string): Promise<string | null>
    })('session-1')
    await expect(injected.stopRun('run-1')).resolves.toBeNull()
    await expect(injected.rerun('run-1', 'revised brief')).resolves.toBeNull()
    expect(execute).toHaveBeenCalledTimes(2)
    expect(dictionaries).toHaveLength(1)
  })
})
