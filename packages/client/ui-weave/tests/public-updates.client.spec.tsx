// @vitest-environment jsdom
import { cleanup, fireEvent, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { PublicUpdates } from '../src/client/PublicUpdates.tsx'
import { createWorkTaskViewStore } from '../src/client/view-store.ts'
import { zh } from '../src/client/locales.ts'

const t = makeTranslate(zh, commonZh)
const update = (seq: number) => ({ taskId: 'attempt-1', eventId: `event-${seq}`, seq, text: `已核对材料 ${seq}`, occurredAt: '', truncated: false })
afterEach(() => { cleanup(); localStorage.clear() })

describe('member following', () => {
  it('preserves history reading until the user explicitly returns to the latest public record', () => {
    const view = render(<PublicUpdates updates={[update(1)]} truncated={false}
      position={undefined} remember={vi.fn()} t={t} />)
    const log = view.getByRole('log')
    Object.defineProperties(log, { scrollHeight: { value: 900, configurable: true }, clientHeight: { value: 300, configurable: true } })
    log.scrollTop = 50
    fireEvent.scroll(log)
    view.rerender(<PublicUpdates updates={[update(1), update(2)]} truncated={false}
      position={undefined} remember={vi.fn()} t={t} />)
    expect(log.scrollTop).toBe(50)
    expect(log.getAttribute('aria-live')).toBe('off')
    fireEvent.click(view.getByRole('button', { name: '有新记录，回到最新' }))
    expect(log.scrollTop).toBe(900)
    Object.defineProperty(log, 'scrollHeight', { value: 1100, configurable: true })
    view.rerender(<PublicUpdates updates={[update(1), update(2), update(3)]} truncated={true}
      position={undefined} remember={vi.fn()} t={t} />)
    expect(log.scrollTop).toBe(1100)
    expect(view.getByText('这里只保留最近的公开记录，较早内容请结合阶段产物查看。')).toBeTruthy()
  })

  it('restores a reader position after leaving and returning to the member', () => {
    const position = { top: 80, follow: false, lastEvent: 'attempt-1:1:9' }
    const view = render(<PublicUpdates updates={[update(1)]} truncated={false}
      position={position} remember={vi.fn()} t={t} />)
    expect(view.getByRole('log').scrollTop).toBe(80)
    view.unmount()
    const returned = render(<PublicUpdates updates={[update(1), update(2)]} truncated={false}
      position={position} remember={vi.fn()} t={t} />)
    expect(returned.getByRole('log').scrollTop).toBe(80)
    expect(returned.getByRole('button', { name: '有新记录，回到最新' })).toBeTruthy()
  })

  it('remembers at most three followed members without changing execution', () => {
    const handle = createWorkTaskViewStore()
    const store = handle.create('session')
    for (const id of ['research', 'review', 'deliver', 'extra']) store.actions.toggleFollow('run', id)
    expect(store.getSnapshot().followed.run).toEqual(['research', 'review', 'deliver'])
    store.actions.selectTab('run', 'outputs')
    const restored = handle.create('session')
    expect(restored.getSnapshot()).toEqual(store.getSnapshot())
    store.actions.toggleFollow('run', 'review')
    expect(store.getSnapshot().followed.run).toEqual(['research', 'deliver'])
  })

  it('keeps unseen updates marked unread after scrolling farther up and returning', () => {
    const first = update(1)
    let position = { top: 120, follow: false, lastEvent: `attempt-1:1:${first.text.length}` }
    const remember = (next: typeof position) => { position = next }
    const view = render(<PublicUpdates updates={[first]} truncated={false} position={position} remember={remember} t={t} />)
    view.rerender(<PublicUpdates updates={[first, update(2)]} truncated={false} position={position} remember={remember} t={t} />)
    const log = view.getByRole('log')
    Object.defineProperties(log, { scrollHeight: { value: 900 }, clientHeight: { value: 300 } })
    log.scrollTop = 40
    fireEvent.scroll(log)
    expect(position.lastEvent).toBe(`attempt-1:1:${first.text.length}`)
    view.unmount()
    const returned = render(<PublicUpdates updates={[first, update(2)]} truncated={false} position={position} remember={remember} t={t} />)
    expect(returned.getByRole('log').scrollTop).toBe(40)
    expect(returned.getByRole('button', { name: '有新记录，回到最新' })).toBeTruthy()
  })
})
