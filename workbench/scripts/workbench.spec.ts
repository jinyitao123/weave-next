import { describe, expect, it, vi } from 'vitest'
import { keychainCredential, validateWeaveCommand, workbenchEnvironment, WORKBENCH_KEYCHAIN_SERVICE } from './workbench.ts'

describe('Workbench launcher credential boundary', () => {
  it('keeps an explicit API key without consulting credential storage', () => {
    const read = vi.fn(() => 'wv_sk_stored')
    const resolved = workbenchEnvironment({ PATH: '/bin', WEAVE_API_KEY: 'wv_sk_explicit' }, read)
    expect(resolved).toEqual({ PATH: '/bin', WEAVE_API_KEY: 'wv_sk_explicit' })
    expect(read).not.toHaveBeenCalled()
  })

  it('adds a stored API key without adding a server secret', () => {
    const resolved = workbenchEnvironment({ PATH: '/bin' }, () => 'wv_sk_stored')
    expect(resolved).toEqual({ PATH: '/bin', WEAVE_API_KEY: 'wv_sk_stored' })
    expect(resolved).not.toHaveProperty('WEAVE_SECRET_KEY')
    expect(resolved).not.toHaveProperty('WEAVE_SECRET_KEY_FILE')
  })

  it('leaves non-macOS environments unchanged when no explicit key exists', () => {
    expect(keychainCredential('linux')).toBeUndefined()
    expect(workbenchEnvironment({ PATH: '/bin' }, () => undefined)).toEqual({ PATH: '/bin' })
    expect(WORKBENCH_KEYCHAIN_SERVICE).toBe('weave-workbench-api-key')
  })
})

describe('Workbench launcher MCP command boundary', () => {
  it('normalizes an executable relative path before starting the profile', () => {
    expect(validateWeaveCommand({ WEAVE_COMMAND: '../bin/sh' }, '/tmp')).toEqual({ WEAVE_COMMAND: '/bin/sh' })
  })

  it('rejects a missing explicit command instead of opening without discovery tools', () => {
    expect(() => validateWeaveCommand({ WEAVE_COMMAND: '../missing-weave' }, '/tmp'))
      .toThrow('WEAVE_COMMAND is not an executable file: /missing-weave')
  })
})
