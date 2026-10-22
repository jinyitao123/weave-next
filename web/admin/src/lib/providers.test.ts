import { describe, expect, it } from 'vitest'
import { cleanModels, hasKey, keyPayload, providerBody, providerInput, validBaseURL } from './providers'

describe('provider payloads', () => {
  it('leaves the key out to keep it and sends an empty string to clear it', () => {
    expect(keyPayload({ kind: 'keep' })).toEqual({})
    expect(keyPayload({ kind: 'replace', value: 'sk-1' })).toEqual({ api_key: 'sk-1' })
    expect(keyPayload({ kind: 'clear' })).toEqual({ api_key: '' })
  })

  it('sends tuning options only for OpenAI-compatible providers', () => {
    const input = { ...providerInput(), name: 'jev', base_url: 'http://localhost:9000', models: ['jev'], json_object_mode: true }
    expect(providerBody(input, { kind: 'keep' })).toMatchObject({ protocol: 'openai', json_object_mode: true })
    expect(providerBody({ ...input, protocol: 'systemone' }, { kind: 'keep' })).toEqual({ name: 'jev', base_url: 'http://localhost:9000', protocol: 'systemone', models: ['jev'] })
  })

  it('reads the key state from the masked value or an explicit flag', () => {
    const provider = { id: 'p', name: 'p', base_url: 'https://x', models: [] }
    expect(hasKey({ ...provider, api_key: '••••••••' })).toBe(true)
    expect(hasKey({ ...provider, api_key: '' })).toBe(false)
    expect(hasKey(provider)).toBe(false)
    expect(hasKey({ ...provider, api_key: '••••••••', has_api_key: false })).toBe(false)
  })

  it('defaults a missing protocol to OpenAI-compatible', () => {
    expect(providerInput({ id: 'p', name: 'p', base_url: 'https://x', models: ['m'] }).protocol).toBe('openai')
  })

  it('accepts only http and https addresses', () => {
    expect(validBaseURL('https://api.example.com/v1')).toBe(true)
    expect(validBaseURL('http://127.0.0.1:11434')).toBe(true)
    expect(validBaseURL('ftp://example.com')).toBe(false)
    expect(validBaseURL('api.example.com')).toBe(false)
  })

  it('trims and de-duplicates model names', () => {
    expect(cleanModels([' a ', 'b', 'a', ''])).toEqual({ models: ['a', 'b'] })
    expect(cleanModels(['', ' ']).problem).toBe('请至少填写一个模型')
    expect(cleanModels(['a b']).problem).toBe('模型名称“a b”不能含空白')
  })
})
