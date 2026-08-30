import { Context } from '@deepseek-ai/cordis'
import { describe, expect, it } from 'vitest'
import { SlotRegistry } from '@deepseek-ai/dsh-client-ui-renderer/client'
import { apply, inject } from '../src/client/index.ts'
import { TeamListRow } from '../src/client/TeamListRow.tsx'

describe('ui-weave browser plugin', () => {
  it('registers the Weave team-list wire name and dictionaries', async () => {
    const ctx = new Context()
    const slots = new SlotRegistry(ctx)
    slots.register({
      name: 'root', children: { 'tool.call.toolview': { kind: 'keyed', scope: 'session' } },
    } as never, () => null)
    const dictionaries: unknown[] = []
    ctx.provide('locale', {
      register(namespace: string, value: unknown) {
        dictionaries.push({ namespace, value })
        return () => {}
      },
    })
    await ctx.plugin({ inject: [...inject], apply }).await()
    const entry = slots.entries('tool.call.toolview')[0]
    expect(entry?.options).toMatchObject({ key: 'mcp__weave__team_list' })
    expect(entry?.locale).toBe('weave')
    expect(entry?.component).toBe(TeamListRow)
    expect(dictionaries).toHaveLength(1)
  })
})
