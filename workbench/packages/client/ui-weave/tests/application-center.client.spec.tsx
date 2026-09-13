// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { ApplicationSettingsSection } from '../src/client/ApplicationCenter.tsx'
import { zh, type WeaveKey } from '../src/client/locales.ts'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

it('issues and hides a key while retaining the application identity', async () => {
  const actions: Record<string, unknown>[] = []
  let issued = false
  let revoked = false
  vi.stubGlobal('fetch', vi.fn<typeof fetch>(async (_url, init) => {
    if (init?.method === 'POST') {
      const body = JSON.parse(String(init.body)) as Record<string, unknown>
      actions.push(body)
      if (body.action === 'issue') { issued = true; return Response.json({ key: 'wv_cap_one_time' }, { status: 201 }) }
      if (body.action === 'revoke') { revoked = true; return new Response(null, { status: 204 }) }
    }
    return Response.json({
      apps: [{ id: 'app-stable', name: 'My app', enabled: true }], grants: [], versions: [],
      credentials: issued ? [{ id: 'key-new', app_id: 'app-stable', name: 'Replacement', revoked_at: revoked ? '2026-09-13' : null }] : [],
    })
  }))
  render(<ApplicationSettingsSection {...{ t: (key: WeaveKey) => zh[key] } as Parameters<typeof ApplicationSettingsSection>[0]} />)
  await screen.findByText('My app')
  fireEvent.change(screen.getByLabelText(zh['app.select']), { target: { value: 'app-stable' } })
  fireEvent.change(screen.getByLabelText(zh['app.credentialName']), { target: { value: 'Replacement' } })
  fireEvent.click(screen.getByText(zh['app.issue']))
  await screen.findByText('wv_cap_one_time')
  expect(actions[0]).toMatchObject({ app_id: 'app-stable', action: 'issue', scopes: ['invoke', 'read', 'cancel'] })
  fireEvent.click(screen.getByText(zh['app.hide']))
  expect(screen.queryByText('wv_cap_one_time')).toBeNull()
  const row = await screen.findByRole('group', { name: 'Replacement' })
  fireEvent.click(within(row).getByText(zh['app.revoke']))
  await within(row).findByText(zh['app.revoked'])
  expect(actions[1]).toMatchObject({ action: 'revoke', app_id: 'app-stable', credential_id: 'key-new' })
})

it('grants the exact selected capability version', async () => {
  const actions: Record<string, unknown>[] = []
  vi.stubGlobal('fetch', vi.fn<typeof fetch>(async (_url, init) => {
    if (init?.method === 'POST') { actions.push(JSON.parse(String(init.body)) as Record<string, unknown>); return new Response(null, { status: 204 }) }
    return Response.json({ apps: [{ id: 'app', name: 'Consumer', enabled: true }], credentials: [], grants: [], versions: [{ capability_id: 'review', name: 'Review', revision: 2 }] })
  }))
  render(<ApplicationSettingsSection {...{ t: (key: WeaveKey) => zh[key] } as Parameters<typeof ApplicationSettingsSection>[0]} />)
  await screen.findByText('Consumer')
  fireEvent.change(screen.getByLabelText(zh['app.select']), { target: { value: 'app' } })
  fireEvent.change(screen.getByLabelText(zh['app.version']), { target: { value: JSON.stringify(['review', 2]) } })
  fireEvent.click(screen.getByText(zh['app.grant']))
  expect(actions[0]).toEqual({ action: 'grant', app_id: 'app', capability_id: 'review', revision: 2, enabled: true })
})
