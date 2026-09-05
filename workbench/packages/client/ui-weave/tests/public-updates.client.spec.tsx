// @vitest-environment jsdom
import { cleanup, fireEvent, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { PublicUpdates } from '../src/client/PublicUpdates.tsx'
import type { WorkTaskReadingPosition } from '../src/client/view-store.ts'
import { createWorkTaskViewStore } from '../src/client/view-store.ts'
import { zh } from '../src/client/locales.ts'

const t = makeTranslate(zh, commonZh)
const update = (seq: number) => ({ taskId: 'attempt-1', eventId: `event-${seq}`, seq, text: `已核对材料 ${seq}`, occurredAt: '', truncated: false })
afterEach(() => { cleanup(); localStorage.clear(); vi.restoreAllMocks() })


function sceneGeometry() {
  const rectangle = (top: number, height: number) => ({
    top, bottom: top + height, height, left: 0, right: 400, width: 400, x: 0, y: top, toJSON: () => ({}),
  })
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    if (this.dataset.testid === 'scene') return rectangle(50, 300)
    const log = this.matches('[role="log"]') ? this : this.closest('[role="log"]')
    const scene = log?.closest<HTMLElement>('[data-testid="scene"]')
    if (log === null || scene === null || scene === undefined || log.closest('[hidden]') !== null) return rectangle(0, 0)
    const entries = Array.from(log.querySelectorAll('article'))
    const top = 150 - scene.scrollTop
    return this === log ? rectangle(top, entries.length * 200) : rectangle(top + entries.indexOf(this) * 200, 200)
  })
}

describe('member following', () => {
  it('preserves history reading until the user explicitly returns to the latest public record', () => {
    const view = render(<PublicUpdates active={true} updates={[update(1)]} truncated={false}
      position={undefined} remember={vi.fn()} t={t} />)
    const log = view.getByRole('log')
    Object.defineProperties(log, { scrollHeight: { value: 900, configurable: true }, clientHeight: { value: 300, configurable: true } })
    log.scrollTop = 50
    fireEvent.scroll(log)
    view.rerender(<PublicUpdates active={true} updates={[update(1), update(2)]} truncated={false}
      position={undefined} remember={vi.fn()} t={t} />)
    expect(log.scrollTop).toBe(50)
    expect(log.getAttribute('aria-live')).toBe('off')
    fireEvent.click(view.getByRole('button', { name: '有新记录，回到最新' }))
    expect(log.scrollTop).toBe(900)
    expect(document.activeElement).toBe(log)
    Object.defineProperty(log, 'scrollHeight', { value: 1100, configurable: true })
    view.rerender(<PublicUpdates active={true} updates={[update(1), update(2), update(3)]} truncated={true}
      position={undefined} remember={vi.fn()} t={t} />)
    expect(log.scrollTop).toBe(1100)
    expect(view.getByText('这里只保留最近的公开记录，较早内容请结合阶段产物查看。')).toBeTruthy()
  })

  it('restores a reader position after leaving and returning to the member', () => {
    const position = { top: 80, follow: false, lastEvent: 'attempt-1:1:9' }
    const view = render(<PublicUpdates active={true} updates={[update(1)]} truncated={false}
      position={position} remember={vi.fn()} t={t} />)
    expect(view.getByRole('log').scrollTop).toBe(80)
    view.unmount()
    const returned = render(<PublicUpdates active={true} updates={[update(1), update(2)]} truncated={false}
      position={position} remember={vi.fn()} t={t} />)
    expect(returned.getByRole('log').scrollTop).toBe(80)
    expect(returned.getByRole('button', { name: '有新记录，回到最新' })).toBeTruthy()
  })

  it('keeps the same text at the same viewport offset when earlier records expire', () => {
    const remember = vi.fn<(position: WorkTaskReadingPosition) => void>()
    const view = render(<PublicUpdates active={true} updates={[update(1), update(2), update(3)]} truncated={false}
      position={undefined} remember={remember} t={t} />)
    const log = view.getByRole('log')
    const rectangle = (top: number, height: number) => ({
      top, bottom: top + height, height, left: 0, right: 400, width: 400, x: 0, y: top, toJSON: () => ({}),
    })
    Object.defineProperties(log, {
      clientHeight: { value: 300 },
      scrollHeight: { get: () => log.querySelectorAll('article').length * 200 },
    })
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
      if (this === log) return rectangle(100, 300)
      const entries = Array.from(log.querySelectorAll('article'))
      const index = entries.indexOf(this)
      return index === -1 ? rectangle(0, 0) : rectangle(100 + index * 200 - log.scrollTop, 200)
    })
    log.scrollTop = 240
    log.focus()
    fireEvent.scroll(log)
    const retained = view.getByText('已核对材料 2').closest('article')!
    const before = retained.getBoundingClientRect().top
    view.rerender(<PublicUpdates active={true} updates={[update(2), update(3), update(4)]} truncated={true}
      position={undefined} remember={remember} t={t} />)
    expect(log.scrollTop).toBe(40)
    expect(retained.getBoundingClientRect().top).toBe(before)
    expect(document.activeElement).toBe(log)
    // Browsers dispatch a scroll event after adjusting scrollTop; it must keep history mode.
    fireEvent.scroll(log)
    expect(remember.mock.lastCall?.[0].follow).toBe(false)
    expect(view.getByRole('button', { name: '有新记录，回到最新' })).toBeTruthy()
    const saved = remember.mock.lastCall?.[0]
    expect(saved?.anchors?.[0]?.offset).toBe(-40)
  })

  it('restores the same record after switching members while the retained window advances', () => {
    const rectangle = (top: number, height: number) => ({
      top, bottom: top + height, height, left: 0, right: 400, width: 400, x: 0, y: top, toJSON: () => ({}),
    })
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
      if (this.getAttribute('role') === 'log') return rectangle(0, 300)
      const log = this.closest('[role="log"]')
      const index = Array.from(log?.querySelectorAll('article') ?? []).indexOf(this)
      return rectangle(index * 200 - (log?.scrollTop ?? 0), index === -1 ? 0 : 200)
    })
    const returned = render(<PublicUpdates active={true} updates={[update(2), update(3), update(4)]} truncated={true}
      position={{ top: 240, follow: false, lastEvent: '', anchors: [{ event: 'attempt-1:2', offset: -40 }, { event: 'attempt-1:3', offset: 160 }] }} remember={vi.fn()} t={t} />)
    expect(returned.getByRole('log').scrollTop).toBe(40)
    expect(returned.getByText('已核对材料 2').closest('article')!.getBoundingClientRect().top).toBe(-40)
  })

  it('starts long public records as excerpts and preserves expansion while changing members', () => {
    const longRecord = { ...update(1), text: '公开核对过程。'.repeat(180) + '保留的末尾说明。' }
    let position: WorkTaskReadingPosition | undefined
    const remember = (next: WorkTaskReadingPosition) => { position = next }
    const view = render(<PublicUpdates active={true} updates={[longRecord]}
      truncated={false} position={position} remember={remember} t={t} />)
    expect(view.queryByText(/保留的末尾说明/)).toBeNull()
    const expand = view.getByRole('button', { name: '展开本条记录' })
    expect(expand.getAttribute('aria-expanded')).toBe('false')
    fireEvent.click(expand)
    expect(view.getByText(/保留的末尾说明/)).toBeTruthy()
    expect(view.getByRole('button', { name: '回到最新记录' })).toBeTruthy()
    expect(position?.follow).toBe(false)
    view.unmount()
    const returned = render(<PublicUpdates active={true} updates={[longRecord]}
      truncated={false} position={position} remember={remember} t={t} />)
    expect(returned.getByText(/保留的末尾说明/)).toBeTruthy()
    fireEvent.click(returned.getByRole('button', { name: '收起本条记录' }))
    expect(returned.queryByText(/保留的末尾说明/)).toBeNull()
  })

  it('uses the scene viewport and retains the visible record when earlier records expire', () => {
    sceneGeometry()
    const remember = vi.fn<(position: WorkTaskReadingPosition) => void>()
    const renderScene = (records: ReturnType<typeof update>[]) => <div data-testid="scene" style={{ overflowY: 'auto' }}>
      <PublicUpdates active={true} updates={records} truncated={false} position={undefined} remember={remember} t={t} />
    </div>
    const view = render(renderScene([update(1), update(2), update(3)]))
    const scene = view.getByTestId('scene')
    const log = view.getByRole('log')
    scene.scrollTop = 340
    fireEvent.scroll(scene)
    expect(remember.mock.lastCall?.[0].anchors?.[0]).toEqual({ event: 'attempt-1:2', offset: -40 })
    view.rerender(renderScene([update(2), update(3), update(4)]))
    expect(scene.scrollTop).toBe(140)
    expect(log.scrollTop).toBe(0)
    expect(view.getByText('已核对材料 2').closest('article')!.getBoundingClientRect().top).toBe(10)
  })

  it('leaves initial scene positioning to its parent when returning to a member', () => {
    sceneGeometry()
    const renderScene = (top: number) => <div data-testid="scene" style={{ overflowY: 'auto' }}>
      <PublicUpdates active={true} updates={[update(2), update(3)]} truncated={false}
        position={{ top, follow: false, lastEvent: '', anchors: [{ event: 'attempt-1:2', offset: -40 }] }} remember={vi.fn()} t={t} />
    </div>
    const view = render(renderScene(340))
    const scene = view.getByTestId('scene')
    expect(scene.scrollTop).toBe(0)
    expect(view.getByRole('log').scrollTop).toBe(0)
    scene.scrollTop = 90
    view.rerender(renderScene(500))
    expect(scene.scrollTop).toBe(90)
  })

  it('follows newly appended visible records without moving to the bottom of the whole scene', () => {
    sceneGeometry()
    const renderScene = (records: ReturnType<typeof update>[]) => <div data-testid="scene" style={{ overflowY: 'auto' }}>
      <PublicUpdates active={true} updates={records} truncated={false} position={undefined} remember={vi.fn()} t={t} />
      <div>后续成员活动</div>
    </div>
    const view = render(renderScene([update(1)]))
    const scene = view.getByTestId('scene')
    Object.defineProperty(scene, 'scrollHeight', { value: 2000 })
    expect(scene.scrollTop).toBe(0)
    view.rerender(renderScene([update(1), update(2)]))
    expect(scene.scrollTop).toBe(200)
    expect(view.getByRole('log').scrollTop).toBe(0)
    scene.scrollTop = 1000
    fireEvent.scroll(scene)
    view.rerender(renderScene([update(1), update(2), update(3)]))
    expect(scene.scrollTop).toBe(1000)
  })

  it('does not scroll or save a hidden reader and leaves tab-return positioning to the scene', () => {
    sceneGeometry()
    const remember = vi.fn<(position: WorkTaskReadingPosition) => void>()
    const renderScene = (active: boolean, records: ReturnType<typeof update>[]) => <div data-testid="scene" style={{ overflowY: 'auto' }}>
      <div hidden={!active} data-weave-member-reader>
        <PublicUpdates active={active} updates={records} truncated={false} position={undefined} remember={remember} t={t} />
      </div>
    </div>
    const view = render(renderScene(true, [update(1)]))
    const scene = view.getByTestId('scene')
    view.rerender(renderScene(false, [update(1)]))
    scene.scrollTop = 430
    fireEvent.scroll(scene)
    view.rerender(renderScene(false, [update(1), update(2)]))
    expect(scene.scrollTop).toBe(430)
    expect(remember).not.toHaveBeenCalled()
    scene.scrollTop = 70
    view.rerender(renderScene(true, [update(1), update(2)]))
    expect(scene.scrollTop).toBe(70)
    expect(view.getByRole('button', { name: '有新记录，回到最新' })).toBeTruthy()
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
    const view = render(<PublicUpdates active={true} updates={[first]} truncated={false} position={position} remember={remember} t={t} />)
    view.rerender(<PublicUpdates active={true} updates={[first, update(2)]}
      truncated={false} position={position} remember={remember} t={t} />)
    const log = view.getByRole('log')
    Object.defineProperties(log, { scrollHeight: { value: 900 }, clientHeight: { value: 300 } })
    log.scrollTop = 40
    fireEvent.scroll(log)
    expect(position.lastEvent).toBe(`attempt-1:1:${first.text.length}`)
    view.unmount()
    const returned = render(<PublicUpdates active={true} updates={[first, update(2)]}
      truncated={false} position={position} remember={remember} t={t} />)
    expect(returned.getByRole('log').scrollTop).toBe(40)
    expect(returned.getByRole('button', { name: '有新记录，回到最新' })).toBeTruthy()
  })
})
