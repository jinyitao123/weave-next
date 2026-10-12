// @vitest-environment node
import { afterEach, expect, it, vi } from 'vitest'
import { readOrganizationConnection } from './organization-connection'

afterEach(() => vi.unstubAllGlobals())
it('preserves the server bytes rather than reserializing JSON in the browser', async () => {
  const content = '{\n  "version": 1,\n  "forgeOrigin": "http://forge.example.test:8080",\n  "weaveOrigin": "https://teams.example.test"\n}\n'
  const request = vi.fn(async (_input: RequestInfo | URL) => new Response(content, { headers: { 'Content-Type': 'application/json' } }))
  vi.stubGlobal('fetch', request)
  const result = await readOrganizationConnection()
  expect(await result.file.text()).toBe(content)
  expect(result.config.version).toBe(1)
  expect(request.mock.calls[0][0]).toBe('/v1/admin/organization-connection')
})
