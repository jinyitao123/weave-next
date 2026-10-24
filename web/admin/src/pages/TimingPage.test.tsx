import { cleanup, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { formatSeconds, TimingSection } from './TimingPage'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('time bounds in force', () => {
  it('shows each bound with its value and where it came from', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({
      first_output_median_seconds: null, sample: 0,
      timing: {
        member_step_limit: { seconds: 2700, source: 'fixed', scope: '' },
        provider_attempt_default: { seconds: 900, source: 'default', scope: '' },
        shutdown_drain: { seconds: 30, source: 'env', scope: '' },
        slow_run_alert: { seconds: 600, source: 'default', scope: '', overrides: { chat: 120 } },
      },
    }), { status: 200 })))
    render(<TimingSection />)
    const drain = (await screen.findByText('停机排空')).closest('tr')!
    expect(within(drain).getByText('30 秒')).toBeTruthy()
    expect(within(drain).getByText('部署设置')).toBeTruthy()
    const step = screen.getByText('成员单步上限').closest('tr')!
    expect(within(step).getByText('45 分钟')).toBeTruthy()
    expect(within(step).getByText('固定')).toBeTruthy()
    expect(screen.getByText('慢运行告警').closest('tr')!.textContent).toContain('不会结束运行')
    const chat = screen.getByText('慢运行告警：chat').closest('tr')!
    expect(within(chat).getByText('2 分钟')).toBeTruthy()
  })
  it('formats durations', () => {
    expect(formatSeconds(0)).toBe('0')
    expect(formatSeconds(90)).toBe('90 秒')
    expect(formatSeconds(600)).toBe('10 分钟')
  })
})
