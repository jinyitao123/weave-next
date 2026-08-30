import { describe, expect, it } from 'vitest'
import { apply, inject, name } from '../src/invariant.ts'

describe('Workbench brand invariant companion', () => {
  it('registers package ownership with no mutable runtime state', async () => {
    const registrations: string[] = []
    const dispose = () => {}
    const ctx = {
      invariants: { register: (pkg: string) => { registrations.push(pkg); return dispose } },
    }
    expect(name).toBe('client-ui-brand-workbench-invariant')
    expect(inject).toEqual(['invariants'])
    await expect(apply(ctx as never)).resolves.toBe(dispose)
    expect(registrations).toEqual(['@deepseek-ai/dsh-client-ui-brand-workbench'])
  })
})
