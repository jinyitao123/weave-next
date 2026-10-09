import { afterEach, describe, expect, it, vi } from 'vitest'
import { createUUID } from './ids'

afterEach(() => vi.unstubAllGlobals())

describe('HTTP-compatible UUIDs', () => {
  it('keeps the native secure-context generator', () => {
    const randomUUID = vi.fn(() => 'native-uuid')
    vi.stubGlobal('crypto', { randomUUID })
    expect(createUUID()).toBe('native-uuid')
    expect(randomUUID).toHaveBeenCalledOnce()
  })

  it('uses random bytes with UUID v4 bits when HTTP has no randomUUID', () => {
    const getRandomValues = vi.fn((bytes: Uint8Array) => {
      bytes.set(Array.from({ length: 16 }, (_, index) => index * 17))
      return bytes
    })
    vi.stubGlobal('crypto', { getRandomValues })
    expect(createUUID()).toBe('00112233-4455-4677-8899-aabbccddeeff')
    expect(getRandomValues).toHaveBeenCalledOnce()
  })
})
