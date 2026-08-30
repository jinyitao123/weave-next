/** Weave Workbench occupants for the generic browser-brand slots. */
import type { Context as ClientContext } from '@deepseek-ai/cordis'
import type {} from '@deepseek-ai/dsh-client-ui-conversation/client'
import type {} from '@deepseek-ai/dsh-client-ui-renderer/client'
import type {} from '@deepseek-ai/dsh-client-ui-sidebar/client'
import { WorkbenchBrandMark, WorkbenchBrandName } from './Brand.tsx'

/** Required service: the UI slot registry. */
export const inject = ['slots']

/** Fill every shipped brand slot for a Workbench client build. */
export function apply(ctx: ClientContext): void {
  if (process.env.DSH_CLIENT_BUILD_PROFILE !== 'workbench') return
  ctx.slots.inject('sidebar.brand.mark', () =>
    ctx.slots.inject('sidebar.brand.name', () =>
      ctx.slots.inject('conversation.hero.brand.mark', function* () {
        yield ctx.slots.register({ name: 'sidebar.brand.mark' }, WorkbenchBrandMark)
        yield ctx.slots.register({ name: 'sidebar.brand.name' }, WorkbenchBrandName)
        yield ctx.slots.register({ name: 'conversation.hero.brand.mark' }, WorkbenchBrandMark)
      })))
}
