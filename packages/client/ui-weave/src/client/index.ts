import type { Context as ClientContext } from '@deepseek-ai/cordis'
import type {} from '@deepseek-ai/dsh-api-remotes/client'
import type {} from '@deepseek-ai/dsh-api-session-controller/client'
import type {} from '@deepseek-ai/dsh-client-locale/client'
import type {} from '@deepseek-ai/dsh-client-ui-chat/client'
import type {} from '@deepseek-ai/dsh-client-ui-conversation/client'
import type {} from '@deepseek-ai/dsh-client-ui-layout/client'
import type {} from '@deepseek-ai/dsh-client-ui-renderer/client'
import type {} from '@deepseek-ai/dsh-client-ui-settings/client'
import { DeliverableRow } from './DeliverableRow.tsx'
import { TeamListRow } from './TeamListRow.tsx'
import { WorkTaskHeader, WorkTaskPanel } from './WorkTaskPanel.tsx'
import { ReadinessOnboarding, ReadinessSection } from './ReadinessPanel.tsx'
import { en, NS, zh, type WeaveKey } from './locales.ts'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    weave: WeaveKey
  }
}

export const inject = ['slots', 'locale', 'layout', 'sessions', 'remote', 'remote.commands']

export function apply(ctx: ClientContext): void {
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
        const result = await ctx.remote.commands.execute(sessionId, `/weave-stop ${JSON.stringify({ runId })}`, [])
        if (!result.ok) return result.error.message
        if (result.value === undefined) return 'The Weave stop command is unavailable.'
        return result.value.result.kind === 'error' ? result.value.result.text : null
      },
      rerun: async (runId: string, brief: string) => {
        const result = await ctx.remote.commands.execute(sessionId, `/weave-rerun ${JSON.stringify({ runId, brief })}`, [])
        if (!result.ok) return result.error.message
        if (result.value === undefined) return 'The Weave rerun command is unavailable.'
        return result.value.result.kind === 'error' ? result.value.result.text : null
      },
      retryStage: async (runId: string, nodeId: string) => {
        const result = await ctx.remote.commands.execute(sessionId, `/weave-retry-stage ${JSON.stringify({ runId, nodeId })}`, [])
        if (!result.ok) return result.error.message
        if (result.value === undefined) return 'The Weave stage retry command is unavailable.'
        return result.value.result.kind === 'error' ? result.value.result.text : null
      },
      requestCorrection: async (runId: string, targetKind: 'team' | 'member', targetMemberId: string, instruction: string) => {
        const result = await ctx.remote.commands.execute(sessionId, `/weave-correct ${JSON.stringify({ runId, targetKind, targetMemberId, instruction })}`, [])
        if (!result.ok) return result.error.message
        if (result.value === undefined) return 'The Weave correction command is unavailable.'
        return result.value.result.kind === 'error' ? result.value.result.text : null
      },
      confirmCorrection: async (runId: string, correctionId: string, disposition: 'apply' | 'discard') => {
        const result = await ctx.remote.commands.execute(sessionId, `/weave-confirm-correction ${JSON.stringify({ runId, correctionId, disposition })}`, [])
        if (!result.ok) return result.error.message
        if (result.value === undefined) return 'The Weave correction confirmation is unavailable.'
        return result.value.result.kind === 'error' ? result.value.result.text : null
      },
      assessOutcome: async (runId: string, outcome: 'adopted' | 'needs-revision', note: string) => {
        const result = await ctx.remote.commands.execute(sessionId, `/weave-assess ${JSON.stringify({ runId, outcome, note })}`, [])
        if (!result.ok) return result.error.message
        if (result.value === undefined) return 'The Weave outcome command is unavailable.'
        return result.value.result.kind === 'error' ? result.value.result.text : null
      },
    }),
  }, WorkTaskPanel))
  const t = ctx.locale.bind(NS)
  ctx.slots.inject('settings.section', () => ctx.slots.register({
    name: 'settings.section', id: 'weave', order: -20, label: () => t('readiness.title'), locale: NS,
  }, ReadinessSection))
  ctx.slots.inject('settings.onboarding', () => ctx.slots.register({
    name: 'settings.onboarding', id: 'weave-readiness', order: -200, locale: NS,
  }, ReadinessOnboarding))
}
