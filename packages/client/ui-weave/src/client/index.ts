import type { Context as ClientContext } from '@deepseek-ai/cordis'
import type {} from '@deepseek-ai/dsh-api-session-controller/client'
import type {} from '@deepseek-ai/dsh-client-locale/client'
import type {} from '@deepseek-ai/dsh-client-ui-chat/client'
import type {} from '@deepseek-ai/dsh-client-ui-conversation/client'
import type {} from '@deepseek-ai/dsh-client-ui-layout/client'
import type {} from '@deepseek-ai/dsh-client-ui-renderer/client'
import type {} from '@deepseek-ai/dsh-client-ui-settings/client'
import { DeliverableRow } from './DeliverableRow.tsx'
import { RuntimeHomeEntry } from './RuntimeCenter.tsx'
import { TeamListRow } from './TeamListRow.tsx'
import { WorkTaskCommandRow } from './WorkTaskCommandRow.tsx'
import { WorkTaskHeader, WorkTaskPanel } from './WorkTaskPanel.tsx'
import { ReadinessOnboarding, ReadinessSection } from './ReadinessPanel.tsx'
import { en, NS, zh, type WeaveKey } from './locales.ts'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    weave: WeaveKey
  }
}

export const inject = ['slots', 'locale', 'layout', 'sessions']

export function apply(ctx: ClientContext): void {
  const t = ctx.locale.bind(NS)
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
    locale: NS,
    inject: sessionId => ({
      openDetails: () => { ctx.layout.openDetails() },
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
      assessOutcome: async (runId: string, outcome: 'adopted' | 'needs-revision', note: string) => {
        return await taskAction(sessionId, { action: 'assess', runId, outcome, note })
      },
    }),
  }, WorkTaskPanel))
  ctx.slots.inject('conversation.input.dock', () => ctx.slots.register({
    name: 'conversation.input.dock', id: 'weave-runtime-home', order: -100, locale: NS,
  }, RuntimeHomeEntry))
  ctx.slots.inject('settings.section', () => ctx.slots.register({
    name: 'settings.section', id: 'weave', order: -20, label: () => t('readiness.title'), locale: NS,
  }, ReadinessSection))
  ctx.slots.inject('settings.onboarding', () => ctx.slots.register({
    name: 'settings.onboarding', id: 'weave-readiness', order: -200, locale: NS,
  }, ReadinessOnboarding))
}
