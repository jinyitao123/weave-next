// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { CapabilitySettingsSection } from '../src/client/CapabilityCenter.tsx'
import { zh, type WeaveKey } from '../src/client/locales.ts'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

it('saves the edited draft before publishing the selected revision', async () => {
  const commands: Record<string, unknown>[] = []
  vi.stubGlobal('fetch', vi.fn<typeof fetch>(async (_url, init) => {
    if (init?.method === 'POST') {
      const command = JSON.parse(String(init.body)) as Record<string, unknown>
      commands.push(command)
      return Response.json(command.action === 'save' ? { status: 'draft_saved' } : { revision: 3 })
    }
    return Response.json({ drafts: [] })
  }))
  render(<CapabilitySettingsSection {...{ t: (key: WeaveKey) => zh[key] } as Parameters<typeof CapabilitySettingsSection>[0]} />)
  fireEvent.change(screen.getByLabelText(zh['cap.revision']), { target: { value: '3' } })
  fireEvent.click(screen.getByText(zh['cap.publish']))
  await screen.findByText(zh['cap.published'])
  expect(commands.map(item => item.action)).toEqual(['save', 'publish'])
  expect(commands[1]).toMatchObject({ id: 'my-capability', revision: 3 })
})

it('retries an uncertain submission with the original request identity and input', async () => {
  const commands: Record<string, unknown>[] = []
  let failed = false
  vi.stubGlobal('fetch', vi.fn<typeof fetch>(async (_url, init) => {
    if (init?.method !== 'POST') return Response.json({ drafts: [] })
    const command = JSON.parse(String(init.body)) as Record<string, unknown>
    commands.push(command)
    if (command.action === 'invoke' && !failed) { failed = true; throw new Error('connection lost') }
    return Response.json({ invocation_id: 'inv', status: 'completed', result: { ok: true } })
  }))
  render(<CapabilitySettingsSection {...{ t: (key: WeaveKey) => zh[key] } as Parameters<typeof CapabilitySettingsSection>[0]} />)
  fireEvent.click(screen.getByText(zh['cap.invoke']))
  await screen.findByRole('alert')
  fireEvent.change(screen.getByLabelText(zh['cap.input']), { target: { value: '{"changed":true}' } })
  fireEvent.click(screen.getByText(zh['cap.retry']))
  await waitFor(() => { expect(commands.filter(item => item.action === 'invoke')).toHaveLength(2) })
  const invocations = commands.filter(item => item.action === 'invoke')
  expect(invocations[1]).toEqual(invocations[0])
})
