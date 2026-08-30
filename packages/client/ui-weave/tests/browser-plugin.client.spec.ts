import { Context } from '@deepseek-ai/cordis'
import { describe, expect, it } from 'vitest'
import { SlotRegistry } from '@deepseek-ai/dsh-client-ui-renderer/client'
import { apply, inject } from '../src/client/index.ts'
import { DeliverableRow } from '../src/client/DeliverableRow.tsx'
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
    const entries = slots.entries('tool.call.toolview')
    expect(entries).toHaveLength(2)
    expect(entries[0]?.options).toMatchObject({ key: 'mcp__weave__team_list' })
    expect(entries[0]?.locale).toBe('weave')
    expect(entries[0]?.component).toBe(TeamListRow)
    expect(entries[1]?.options).toMatchObject({ key: 'mcp__weave__deliverable_get' })
    expect(entries[1]?.locale).toBe('weave')
    expect(entries[1]?.component).toBe(DeliverableRow)
    expect(dictionaries).toHaveLength(1)
  })
})
