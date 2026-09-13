/** The shared Host's conversation can coordinate work without executing Host code. */
import type { Context } from '@deepseek-ai/cordis'
import type { Agent } from '@deepseek-ai/dsh-agent'
import type {} from '@deepseek-ai/dsh-tools'

function allowed(name: string): boolean {
  return name === 'weave_dispatch' || name === 'ask_user_question' || name.startsWith('mcp__weave__')
}

/** Reuse the tool registry's scope mask and final guard; neither starts another loop. */
export function installForegroundTools(ctx: Context, activeSession: (id: string) => boolean): void {
  const scoped = new Map<Agent, { names: string; dispose: () => void }>()
  let updating = false
  // This guard is installed before any Agent is published. An incomplete scope,
  // a late local tool, and PTC's reserved transport all fail closed.
  ctx.effect(() => ctx.tools.guard(exec => {
    if (exec.agent === undefined || !scoped.has(exec.agent) || !activeSession(String(exec.agent.session.id))) return 'workbench_account_required'
    return allowed(exec.name) ? undefined : 'workbench_dispatch_required'
  }), 'workbench foreground execution boundary')
  const refresh = (agent: Agent): void => {
    const names = ctx.tools.schemas().map(tool => tool.name).filter(allowed).sort()
    const signature = JSON.stringify(names)
    const previous = scoped.get(agent)
    if (previous?.names === signature) return
    const dispose = agent.ctx.tools.restrict({ allow: names })
    scoped.set(agent, { names: signature, dispose })
    previous?.dispose()
  }
  const attach = (agent: Agent): void => {
    updating = true
    try {
      // Workbench uses native product tools. A conflicting preset prevents
      // publication instead of leaving a code transport visible.
      agent.ctx.tools.presentAs('native')
      refresh(agent)
    } finally { updating = false }
  }
  ctx.on('agent/created', ({ agent }) => { attach(agent) })
  ctx.on('agent/disposed', ({ agent }) => { scoped.delete(agent) })
  ctx.on('tools/change', () => {
    if (updating) return
    updating = true
    try { for (const agent of scoped.keys()) refresh(agent) } finally { updating = false }
  })
  for (const agent of ctx.agents.list()) attach(agent)
  ctx.effect(() => () => { scoped.clear() }, 'workbench foreground scope lifecycle')
}
