import type { InputTriggerServiceContract } from '@deepseek-ai/dsh-client-ui-input-trigger/client'
import { memberReference, memberReferenceSource, type WorkTaskMemberReference } from './member-reference.ts'
import type { BoundActions } from '@deepseek-ai/dsh-client-store'
import type {} from '@deepseek-ai/dsh-client-ui-workspace/client'
import type { SessionId } from '@deepseek-ai/dsh-session/types'
import type { Context as ClientContext } from '@deepseek-ai/cordis'
import type {} from '@deepseek-ai/dsh-api-session-controller/client'
import type {} from '@deepseek-ai/dsh-client-locale/client'
import type {} from '@deepseek-ai/dsh-client-ui-chat/client'
import type {} from '@deepseek-ai/dsh-client-ui-conversation/client'
import type {} from '@deepseek-ai/dsh-client-ui-layout/client'
import type {} from '@deepseek-ai/dsh-client-ui-renderer/client'
import type {} from '@deepseek-ai/dsh-client-ui-settings/client'
import { createWorkTaskViewStore } from './view-store.ts'
import { DeliverableRow } from './DeliverableRow.tsx'
import { ProjectActivity } from './ProjectActivity.tsx'
import { NS as projectNS, zh as projectZh, en as projectEn, type ProjectActivityKey } from './project-activity-locales.ts'
import { RuntimeSettingsSection } from './RuntimeCenter.tsx'
import { TeamListRow } from './TeamListRow.tsx'
import { WorkTaskCommandRow } from './WorkTaskCommandRow.tsx'
import { WorkTaskConversationCard, WorkTaskHeader, WorkTaskPanel } from './WorkTaskPanel.tsx'
import { en, NS, zh, type WeaveKey } from './locales.ts'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    weave: WeaveKey
    projectActivity: ProjectActivityKey
  }
}

export const inject = ['slots', 'locale', 'layout', 'sessions', 'inputTriggers']

export function apply(ctx: ClientContext): void {
  const t = ctx.locale.bind(NS)
  const taskView = createWorkTaskViewStore()
  let activeScene: { sessionId: SessionId; activate: () => void } | undefined
  let pendingTaskView: { sessionId: SessionId; runId: string; deliverableId: string } | undefined
  const requestTaskView = (sessionId: SessionId, runId = '', deliverableId = '') => {
    pendingTaskView = { sessionId, runId, deliverableId }
    const alreadyCurrent = ctx.sessions.list.getSnapshot().current === sessionId
    ctx.sessions.open(sessionId)
    if (alreadyCurrent && activeScene?.sessionId === sessionId) activeScene.activate()
  }
  const inputTriggers = ctx.get('inputTriggers') as InputTriggerServiceContract
  ctx.effect(() => inputTriggers.registerSource(memberReferenceSource(t('task.member.referenceIntent'))), 'ui-weave: member reference serialization')
  const taskAction = async (sessionId: string, body: object): Promise<string | null> => {
    try {
      const response = await fetch('/api/weave.task-action', {
        method: 'POST', headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: JSON.stringify({ sessionId, ...body }),
      })
      if (response.ok) return null
      await response.body?.cancel()
      return t('task.action.error')
    } catch { return t('task.action.offline') }
  }
  const taskInjected = (sessionId: SessionId, actions: BoundActions<typeof taskView>) => {
    const activateScene = () => {
      if (ctx.sessions.list.getSnapshot().current !== sessionId) return
      activeScene = { sessionId, activate: activateScene }
      if (pendingTaskView?.sessionId !== sessionId) return
      if (pendingTaskView.runId !== '') {
        if (pendingTaskView.deliverableId !== '') actions.showOutput(pendingTaskView.runId, pendingTaskView.deliverableId)
        else actions.selectTab(pendingTaskView.runId, 'outputs')
      }
      pendingTaskView = undefined
      ctx.layout.openDetails()
    }
    return ({
      activateScene,
      openDetails: () => { ctx.layout.openDetails() },
      expandDetails: () => { ctx.layout.openDetailsFocus() },
      beginMemberAdjustment: (member: WorkTaskMemberReference): Promise<void> => {
        const scoped = ctx.sessions.scope(sessionId)
        const conversation = scoped?.get('conversation')
        if (scoped === undefined || conversation === undefined) return Promise.reject(new Error('The current conversation is unavailable.'))
        const input = conversation.input.for(scoped)
        const state = input.state.getSnapshot()
        const reference = memberReference(member, t('task.member.referenceLabel', { name: member.memberName }))
        if (!state.occurrences.some(item => item.source === reference.source && item.ref === reference.ref)) {
          const end = state.draft.length - state.occurrences.reduce((removed, item) => removed + item.length - 1, 0)
          const accepted = scoped.bail(scoped, 'slash/input-insert-reference', {
            reference, span: { start: end, end, draftRev: state.draftRev },
          })
          if (accepted !== true) { input.notify('info', t('task.member.draftBusy')); return Promise.resolve() }
        }
        ctx.layout.closeDetails()
        return Promise.resolve()
      },
      returnToConversation: () => { ctx.layout.closeDetails() },
      requestDelivery: async () => {
        const scoped = ctx.sessions.scope(sessionId)
        const conversation = scoped?.get('conversation') as { send(text: string): Promise<void> } | undefined
        if (conversation === undefined) throw new Error('The current conversation is unavailable.')
        await conversation.send('请核对当前任务已有的阶段产物与最终交付，说明还缺什么、能否基于已完成内容补齐。不要重跑这次任务。')
        ctx.layout.closeDetails()
      },
      selectTeam: async (teamId: string, teamName: string) => {
        const scoped = ctx.sessions.scope(sessionId)
        const conversation = scoped?.get('conversation') as { send(text: string): Promise<void> } | undefined
        if (conversation === undefined) throw new Error('The current conversation is unavailable.')
        await conversation.send(`我选择团队 ${JSON.stringify(teamName)}（team_id: ${JSON.stringify(teamId)}）。请根据当前诉求整理完整任务简报和预期交付物，先让我确认，不要立即派发。`)
      },
      stopRun: async (runId: string) => {
        return await taskAction(sessionId, { action: 'stop', runId })
      },
      rerun: async (runId: string, brief: string) => {
        return await taskAction(sessionId, { action: 'rerun', runId, brief })
      },
      retryStage: async (runId: string, nodeId: string) => {
        return await taskAction(sessionId, { action: 'stage-retry', runId, nodeId })
      },
      requestCorrection: async (runId: string, targetKind: 'team' | 'member', targetMemberId: string, instruction: string) => {
        return await taskAction(sessionId, { action: 'correction-request', runId, targetKind, targetMemberId, instruction })
      },
      confirmCorrection: async (runId: string, correctionId: string, disposition: 'apply' | 'discard') => {
        return await taskAction(sessionId, { action: 'correction-confirm', runId, correctionId, disposition })
      },
      completeHumanTask: async (runId: string, interactionId: string, payload: unknown) => taskAction(sessionId, { action: 'human-complete', runId, interactionId, payload }),
      assessOutcome: async (runId: string, outcome: 'adopted' | 'needs-revision', note: string) => {
        return await taskAction(sessionId, { action: 'assess', runId, outcome, note })
      },
    })
  }
  ctx.effect(() => ctx.locale.register(projectNS, { zh: projectZh, en: projectEn }), 'ui-weave: project activity dictionaries')
  ctx.slots.inject('sidebar.workspaces.projectActivity', () => ctx.slots.register({
    name: 'sidebar.workspaces.projectActivity', locale: projectNS,
    inject: () => ({
      openTask: (sessionId: SessionId) => { requestTaskView(sessionId) },
      openTaskResults: (sessionId: SessionId, runId: string, deliverableId?: string) => {
        requestTaskView(sessionId, runId, deliverableId)
      },
    }),
  }, ProjectActivity))
  ctx.effect(() => ctx.locale.register(NS, { zh, en }), 'ui-weave: dictionaries')
  ctx.slots.inject('tool.call.toolview', () => {
    const disposeTeamList = ctx.slots.register(
      {
        name: 'tool.call.toolview', key: 'mcp__weave__team_list', locale: NS,
        inject: sessionId => ({
          selectTeam: async (teamId: string, teamName: string) => {
            const scoped = ctx.sessions.scope(sessionId)
            const conversation = scoped?.get('conversation') as { send(text: string): Promise<void> } | undefined
            if (conversation === undefined) throw new Error('The current conversation is unavailable.')
            await conversation.send(`我选择团队 ${JSON.stringify(teamName)}（team_id: ${JSON.stringify(teamId)}）。请根据当前诉求整理完整任务简报和预期交付物，先让我确认，不要立即派发。`)
          },
        }),
      },
      TeamListRow,
    )
    const disposeDeliverable = ctx.slots.register(
      { name: 'tool.call.toolview', key: 'mcp__weave__deliverable_get', locale: NS },
      DeliverableRow,
    )
    return () => {
      disposeDeliverable()
      disposeTeamList()
    }
  })
  ctx.slots.inject('conversation.chat.commandview', () => {
    const disposers = ['weave-stop', 'weave-rerun', 'weave-correct', 'weave-confirm-correction', 'weave-retry-stage', 'weave-assess']
      .map(key => ctx.slots.register({ name: 'conversation.chat.commandview', key, locale: NS }, WorkTaskCommandRow))
    return () => { for (const dispose of disposers.reverse()) dispose() }
  })
  ctx.slots.inject('conversation.session.header.actions', () => ctx.slots.register({
    name: 'conversation.session.header.actions',
    id: 'weave-work-task',
    order: -100,
    locale: NS,
    inject: () => ({ openDetails: () => { ctx.layout.openDetails() } }),
  }, WorkTaskHeader))
  ctx.slots.inject('conversation.details.summary', () => ctx.slots.register({
    name: 'conversation.details.summary',
    locale: NS, store: taskView,
    inject: taskInjected,
  }, WorkTaskPanel))
  ctx.slots.inject('conversation.input.dock', () => ctx.slots.register({
    name: 'conversation.input.dock', id: 'weave-task-action', order: -30, locale: NS, inject: taskInjected, store: taskView,
  }, WorkTaskConversationCard))
  ctx.slots.inject('settings.section', () => ctx.slots.register({
    name: 'settings.section', id: 'weave-runtimes', order: -20, label: () => t('runtimeCenter.title'), locale: NS,
  }, RuntimeSettingsSection))
}
