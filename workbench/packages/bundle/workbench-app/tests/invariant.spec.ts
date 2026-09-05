import { describe, expect, it } from 'vitest'
import { apply, inject, name } from '../src/invariant.ts'

describe('Workbench bundle invariant companion', () => {
  it('registers package ownership for the static patch bundle', async () => {
    const registrations: string[] = []
    const dispose = () => {}
    const ctx = {
      invariants: { register: (pkg: string) => { registrations.push(pkg); return dispose } },
    }
    expect(name).toBe('workbench-app-invariant')
    expect(inject).toEqual(['invariants'])
    await expect(apply(ctx as never)).resolves.toBe(dispose)
    expect(registrations).toEqual(['@deepseek-ai/dsh-workbench-app'])
  })
})
