import type { Context as ClientContext } from '@deepseek-ai/cordis'
import type {} from '@deepseek-ai/dsh-client-locale/client'
import type {} from '@deepseek-ai/dsh-client-ui-renderer/client'
import { DeliverableRow } from './DeliverableRow.tsx'
import { TeamListRow } from './TeamListRow.tsx'
import { en, NS, zh, type WeaveKey } from './locales.ts'

declare module '@deepseek-ai/dsh-client-ui-slots' {
  interface LocaleNamespaceMap {
    weave: WeaveKey
  }
}

export const inject = ['slots', 'locale']

export function apply(ctx: ClientContext): void {
  ctx.effect(() => ctx.locale.register(NS, { zh, en }), 'ui-weave: dictionaries')
  ctx.slots.inject('tool.call.toolview', () => {
    const disposeTeamList = ctx.slots.register(
      { name: 'tool.call.toolview', key: 'mcp__weave__team_list', locale: NS },
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
}
