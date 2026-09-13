import { Context } from '@deepseek-ai/cordis'
import { describe, expect, it, vi } from 'vitest'
import { SlotRegistry } from '@deepseek-ai/dsh-client-ui-renderer/client'
import { apply, inject } from '../src/client/index.ts'
import { DeliverableRow } from '../src/client/DeliverableRow.tsx'
import { RuntimeSettingsSection } from '../src/client/RuntimeCenter.tsx'
import { ApplicationSettingsSection } from '../src/client/ApplicationCenter.tsx'
import { TeamListRow } from '../src/client/TeamListRow.tsx'
import { WorkTaskConversationCard, WorkTaskHeader, WorkTaskPanel } from '../src/client/WorkTaskPanel.tsx'
import type { InputTriggerSource } from '@deepseek-ai/dsh-client-ui-input-trigger/client'
import type { WorkTaskMemberReference } from '../src/client/member-reference.ts'
import { WorkTaskCommandRow } from '../src/client/WorkTaskCommandRow.tsx'

describe('ui-weave browser plugin', () => {
  it('registers the Weave team-list wire name and dictionaries', async () => {
    const ctx = new Context()
    const slots = new SlotRegistry(ctx)
    slots.register({
      name: 'root', children: {
        'tool.call.toolview': { kind: 'keyed', scope: 'session' },
        'conversation.session.header.actions': { kind: 'list', scope: 'session' },
        'conversation.input.dock': { kind: 'list', scope: 'session' },
        'conversation.details.summary': { kind: 'single', scope: 'session' },
        'sidebar.workspaces.projectActivity': { kind: 'single', scope: 'root' },
        'sidebar.footer.action': { kind: 'list', scope: 'root' },
        'settings.section': { kind: 'list', scope: 'root' },
        'shell.overlay': { kind: 'list', scope: 'root' },
        'conversation.chat.commandview': { kind: 'keyed', scope: 'session' },
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
    const openDetails = vi.fn()
    ctx.provide('layout', { openDetails, openDetailsFocus() {}, closeDetails() {}, toggleSidebar() {} })
    const fetcher = vi.fn<typeof fetch>(() => Promise.resolve(new Response(null, { status: 204 })))
    vi.stubGlobal('fetch', fetcher)
    const send = vi.fn(() => Promise.resolve())
    const insert = vi.fn(() => true)
    const notify = vi.fn()
    const draft = { draft: '保留这段已有草稿', draftRev: 7, occurrences: [] as { source: string; ref: string; length: number }[] }
    let source: InputTriggerSource | undefined
    const disposeSource = vi.fn()
    ctx.provide('inputTriggers', { registerSource: (value: InputTriggerSource) => { source = value; return disposeSource } })
    const scoped = { bail: insert, get: (name: string) => name === 'conversation' ? { send, input: { for: () => ({ state: { getSnapshot: () => draft }, notify }) } } : undefined }
    const openSession = vi.fn()
    const selection = { current: 'session-1' }
    ctx.provide('sessions', { scope: () => scoped, open: openSession, list: { getSnapshot: () => selection } })
    const fiber = ctx.plugin({ inject: [...inject], apply })
    await fiber.await()
    const entries = slots.entries('tool.call.toolview')
    expect(entries).toHaveLength(2)
    expect(entries[0]?.options).toMatchObject({ key: 'mcp__weave__team_list' })
    expect(entries[0]?.locale).toBe('weave')
    expect(entries[0]?.component).toBe(TeamListRow)
    const teamInjected = (entries[0]?.inject as (sessionId: string) => {
      selectTeam(teamId: string, teamName: string): Promise<void>
    })('session-1')
    await teamInjected.selectTeam('team-1', '日冕推演团队')
    expect(send).toHaveBeenCalledWith(expect.stringContaining('team-1'), 'ui-control')
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
    expect(slots.entries('conversation.input.dock')[0]?.component).toBe(WorkTaskConversationCard)
    expect(slots.entries('sidebar.footer.action')).toHaveLength(0)
    const settings = slots.entries('settings.section')
    expect(settings).toHaveLength(2)
    expect(settings[0]?.options).toMatchObject({ id: 'weave-runtimes', order: -20 })
    expect(settings[0]?.component).toBe(RuntimeSettingsSection)
    expect(settings[1]?.options).toMatchObject({ id: 'weave-capability-apps', order: -5 })
    expect(settings[1]?.component).toBe(ApplicationSettingsSection)
    expect(slots.entries('shell.overlay')).toHaveLength(0)
    const commands = slots.entries('conversation.chat.commandview')
    expect(commands).toHaveLength(6)
    expect(commands.map(entry => entry.options.key)).toEqual([
      'weave-stop', 'weave-rerun', 'weave-correct', 'weave-confirm-correction', 'weave-retry-stage', 'weave-assess',
    ])
    expect(commands.every(entry => entry.component === WorkTaskCommandRow)).toBe(true)
    const selectTab = vi.fn()
    const showOutput = vi.fn()
    const injected = (summary[0]?.inject as (
      sessionId: string, actions: { selectTab: typeof selectTab; showOutput: typeof showOutput },
    ) => {
      activateScene(): void
      stopRun(runId: string): Promise<string | null>
      rerun(runId: string, brief: string): Promise<string | null>
      retryStage(runId: string, nodeId: string): Promise<string | null>
      beginMemberAdjustment(member: WorkTaskMemberReference): Promise<void>
      selectTeam(teamId: string, teamName: string): Promise<void>
      requestDelivery(): Promise<void>
      assessOutcome(runId: string, deliveryRevisionId: string, outcome: 'adopted' | 'needs-revision', note: string): Promise<string | null>
      recheckDelivery(runId: string, deliveryRevisionId: string, contractDigest: string): Promise<string | null>
    })('session-1', { selectTab, showOutput })
    await expect(injected.stopRun('run-1')).resolves.toBeNull()
    await expect(injected.rerun('run-1', 'revised brief')).resolves.toBeNull()
    await expect(injected.retryStage('run-1', 'physics')).resolves.toBeNull()
    expect(fetcher).toHaveBeenCalledTimes(3)
    const member = { runId: 'run-1', memberId: 'reviewer', memberName: '复核员', stages: [{ nodeId: 'review', name: '复核阶段', outputIds: ['report-1'], outputTitles: ['复核报告'] }] }
    await injected.beginMemberAdjustment(member)
    expect(insert).toHaveBeenCalledWith(scoped, 'slash/input-insert-reference', {
      reference: expect.objectContaining({ source: 'weave-member', ref: JSON.stringify(member) }) as unknown, span: { start: draft.draft.length, end: draft.draft.length, draftRev: 7 },
    })
    expect(await source?.codec?.serialize(JSON.stringify(member), new AbortController().signal)).toContain('report-1')
    expect(await source?.candidates({} as never, {} as never)).toEqual([])
    expect(draft.draft).toBe('保留这段已有草稿')
    expect(send).toHaveBeenCalledTimes(1)
    expect(fetcher).toHaveBeenCalledTimes(3)
    draft.occurrences = [{ source: 'file', ref: '/notes.md', length: 4 }]
    await injected.beginMemberAdjustment(member)
    expect(insert).toHaveBeenLastCalledWith(scoped, 'slash/input-insert-reference', expect.objectContaining({ span: { start: draft.draft.length - 3, end: draft.draft.length - 3, draftRev: 7 } }))
    const callsBeforeDuplicate = insert.mock.calls.length
    draft.occurrences.push({ source: 'weave-member', ref: JSON.stringify(member), length: 2 })
    await injected.beginMemberAdjustment(member)
    expect(insert).toHaveBeenCalledTimes(callsBeforeDuplicate)
    draft.occurrences = []
    insert.mockReturnValueOnce(false)
    await injected.beginMemberAdjustment(member)
    expect(notify).toHaveBeenCalledWith('info', 'task.member.draftBusy')
    expect(send).toHaveBeenCalledTimes(1)
    const bodies = fetcher.mock.calls.map(([, init]) => {
      if (typeof init?.body !== 'string') throw new Error('expected a JSON request body')
      return JSON.parse(init.body) as unknown
    })
    expect(bodies).toEqual([
      { sessionId: 'session-1', action: 'stop', runId: 'run-1' },
      { sessionId: 'session-1', action: 'rerun', runId: 'run-1', brief: 'revised brief' },
      { sessionId: 'session-1', action: 'stage-retry', runId: 'run-1', nodeId: 'physics' },
    ])
    await expect(injected.assessOutcome('run-1', 'revision-shown', 'adopted', '可用')).resolves.toBeNull()
    expect(JSON.parse(fetcher.mock.calls.at(-1)?.[1]?.body as string)).toEqual({
      sessionId: 'session-1', action: 'assess', runId: 'run-1', deliveryRevisionId: 'revision-shown', outcome: 'adopted', note: '可用',
    })
    await expect(injected.recheckDelivery('run-1', 'revision-shown', 'contract-shown')).resolves.toBeNull()
    expect(JSON.parse(fetcher.mock.calls.at(-1)?.[1]?.body as string)).toEqual({
      sessionId: 'session-1', action: 'recheck', runId: 'run-1', deliveryRevisionId: 'revision-shown', contractDigest: 'contract-shown',
    })
    await injected.selectTeam('team-2', '复核团队')
    expect(send).toHaveBeenLastCalledWith(expect.stringContaining('team-2'), 'ui-control')
    await injected.requestDelivery()
    expect(send).toHaveBeenLastCalledWith(expect.stringContaining('不要重跑这次任务'), 'ui-control')
    expect(dictionaries).toHaveLength(2)
    const project = slots.entries('sidebar.workspaces.projectActivity')
    expect(project).toHaveLength(1)
    const projectInjected = (project[0]?.inject as () => {
      openTaskResults(sessionId: string, runId: string, deliverableId?: string): void
    })()
    injected.activateScene()
    projectInjected.openTaskResults('session-1', 'run-1')
    expect(openSession).toHaveBeenCalledWith('session-1')
    expect(selectTab).toHaveBeenCalledWith('run-1', 'outputs')
    openDetails.mockClear(); selectTab.mockClear()
    projectInjected.openTaskResults('session-2', 'run-2')
    expect(openDetails).not.toHaveBeenCalled()
    const target = (summary[0]?.inject as (sessionId: string, actions: { selectTab: typeof selectTab; showOutput: typeof showOutput }) => { activateScene(): void })('session-2', { selectTab, showOutput })
    target.activateScene()
    expect(openDetails).not.toHaveBeenCalled()
    selection.current = 'session-2'
    target.activateScene()
    expect(openDetails).toHaveBeenCalledOnce()
    expect(selectTab).toHaveBeenCalledWith('run-2', 'outputs')
    projectInjected.openTaskResults('session-2', 'run-2', 'artifact-2')
    expect(showOutput).toHaveBeenCalledWith('run-2', 'artifact-2')
    await fiber.dispose()
    expect(disposeSource).toHaveBeenCalledOnce()
    expect(slots.entries('conversation.input.dock')).toHaveLength(0)
    expect(slots.entries('sidebar.workspaces.projectActivity')).toHaveLength(0)
    vi.unstubAllGlobals()
  })
})
