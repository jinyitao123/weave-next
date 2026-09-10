import { describe, expect, it } from 'vitest'
import { createUserMessage } from '@deepseek-ai/dsh-llm'
import type { UserMessage } from '@deepseek-ai/dsh-llm'
import {
  deriveBrowserTimeZoneContext,
  renderBrowserTimeZoneContext,
} from '../src/request-zone.ts'

function browserMessage(timeZone: string): UserMessage {
  return createUserMessage({
    content: [{ type: 'text', text: timeZone }],
    source: { kind: 'user', rpcId: `rpc-${timeZone}`, clientTimeZone: timeZone } as never,
  })
}

describe('browser request-zone context', () => {
  it('derives missing, unique, and sorted mixed zones from user-rpc messages only', () => {
    const plugin = createUserMessage({
      content: [{ type: 'text', text: 'plugin' }],
      source: { kind: 'plugin', plugin: 'test' },
    })
    expect(deriveBrowserTimeZoneContext([plugin])).toEqual({ kind: 'missing' })
    expect(deriveBrowserTimeZoneContext([
      browserMessage('Asia/Shanghai'),
      browserMessage('Asia/Shanghai'),
    ])).toEqual({ kind: 'resolved', timeZone: 'Asia/Shanghai' })
    expect(deriveBrowserTimeZoneContext([
      browserMessage('Asia/Shanghai'),
      browserMessage('America/New_York'),
    ])).toEqual({
      kind: 'mixed',
      timeZones: ['America/New_York', 'Asia/Shanghai'],
    })
  })

  it('validates every browser zone before classifying a mixed turn', () => {
    expect(() => deriveBrowserTimeZoneContext([
      browserMessage('+08:00'),
    ])).toThrow(/canonical UTC or IANA Area\/Location/)
    expect(() => deriveBrowserTimeZoneContext([
      browserMessage('Asia/Shanghai'),
      browserMessage('Not/A_Real_Zone'),
    ])).toThrow(/browser time zone is unsupported/)
    expect(() => deriveBrowserTimeZoneContext([
      browserMessage('Etc/UTC'),
    ])).toThrow(/browser time zone must be canonical/)
  })

  it('preserves a browser control turn zone without accepting metadata from unrelated plugins', () => {
    const control = createUserMessage({
      content: [{ type: 'text', text: '选用此团队' }],
      source: {
        kind: 'plugin', plugin: 'ui-control', form: 'relay',
        rpcId: 'control-rpc', clientTimeZone: 'Asia/Shanghai',
      } as never,
    })
    expect(deriveBrowserTimeZoneContext([control])).toEqual({
      kind: 'resolved', timeZone: 'Asia/Shanghai',
    })
    for (const source of [
      { kind: 'plugin', plugin: 'other', form: 'relay' },
      { kind: 'plugin', plugin: 'ui-control', form: 'inject' },
    ]) {
      const unrelated = createUserMessage({
        content: [{ type: 'text', text: 'plugin context' }],
        source: { ...source, rpcId: 'unrelated', clientTimeZone: 'Asia/Shanghai' } as never,
      })
      expect(deriveBrowserTimeZoneContext([unrelated])).toEqual({ kind: 'missing' })
    }
  })

  it('renders one explicit model policy for every context', () => {
    expect(renderBrowserTimeZoneContext({ kind: 'resolved', timeZone: 'Asia/Shanghai' }))
      .toContain('Interpret otherwise-unqualified dates and times in this zone.')
    expect(renderBrowserTimeZoneContext({
      kind: 'mixed', timeZones: ['America/New_York', 'Asia/Shanghai'],
    })).toContain('mixed ["America/New_York","Asia/Shanghai"]')
    expect(renderBrowserTimeZoneContext({ kind: 'missing' })).toContain('unavailable')
  })
})
