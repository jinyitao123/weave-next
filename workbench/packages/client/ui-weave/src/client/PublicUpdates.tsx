/** Public runtime updates remain distinct from final deliverables and private reasoning. */
import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import { MarkdownText } from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import type { WorkTaskPublicUpdate } from './work-task-model.ts'
import type { WorkTaskReadingPosition } from './view-store.ts'
import css from './WorkTaskPanel.module.css'

interface Props extends PropsLocale<'weave'> { readonly active: boolean; readonly updates: readonly WorkTaskPublicUpdate[]; readonly truncated: boolean; readonly position: WorkTaskReadingPosition | undefined; readonly remember: (position: WorkTaskReadingPosition) => void }
type ReadingAnchor = NonNullable<WorkTaskReadingPosition['anchors']>[number]

function visibleAnchors(log: HTMLDivElement, element: HTMLElement): ReadingAnchor[] {
  const viewport = element.getBoundingClientRect()
  return Array.from(log.querySelectorAll<HTMLElement>('article[data-record]')).flatMap((article) => {
    const bounds = article.getBoundingClientRect()
    const event = article.dataset.record
    return event !== undefined && bounds.bottom > viewport.top && bounds.top < viewport.bottom
      ? [{ event, offset: bounds.top - viewport.top }]
      : []
  })
}

function restoreAnchor(log: HTMLDivElement, element: HTMLElement, anchors: readonly ReadingAnchor[]): boolean {
  const records = Array.from(log.querySelectorAll<HTMLElement>('article[data-record]'))
  for (const anchor of anchors) {
    const article = records.find(record => record.dataset.record === anchor.event)
    if (article !== undefined) {
      element.scrollTop = Math.max(
        0, element.scrollTop + article.getBoundingClientRect().top - element.getBoundingClientRect().top - anchor.offset,
      )
      return true
    }
  }
  return false
}

/** The scene owns scrolling; isolated readers retain their local fallback. */
function scrollOwner(log: HTMLDivElement): HTMLElement {
  let parent = log.parentElement
  while (parent !== null) {
    if (/^(auto|scroll)$/u.test(getComputedStyle(parent).overflowY)) return parent
    parent = parent.parentElement
  }
  return log
}

function readerVisible(log: HTMLDivElement, element: HTMLElement): boolean {
  if (log.closest('[hidden], [inert], [aria-hidden="true"]') !== null) return false
  if (log === element) return true
  const bounds = log.getBoundingClientRect()
  const viewport = element.getBoundingClientRect()
  return bounds.height > 0 && viewport.height > 0 && bounds.bottom > viewport.top && bounds.top < viewport.bottom
}

function nearLatest(log: HTMLDivElement, element: HTMLElement): boolean {
  return log === element
    ? element.scrollHeight - element.scrollTop - element.clientHeight < 24
    : log.getBoundingClientRect().bottom - element.getBoundingClientRect().bottom < 24
}

function showLatest(log: HTMLDivElement, element: HTMLElement): void {
  element.scrollTop = log === element ? element.scrollHeight : Math.max(0,
    element.scrollTop + Math.max(0, log.getBoundingClientRect().bottom - element.getBoundingClientRect().bottom),
  )
}

/**
 * Follow visible public text until the reader moves into history, using the scene's main scrollport.
 * @param props - Ordered public messages, active-view state, and retained reading preferences.
 * @returns Expandable records that share the scene's reading surface.
 */
export function PublicUpdates({ active, updates, truncated, position, remember, t }: Props) {
  const contentId = useId()
  const scroll = useRef<HTMLDivElement>(null)
  const initialPosition = useRef(position)
  const follow = useRef(position?.follow ?? true)
  const restored = useRef(false)
  const wasActive = useRef(false)
  const wasVisible = useRef(false)
  const anchors = useRef<readonly ReadingAnchor[]>(position?.anchors ?? [])
  const adjustedTop = useRef<number>()
  const [readingHistory, setReadingHistory] = useState(!follow.current)
  const [unread, setUnread] = useState(false)
  const [expanded, setExpanded] = useState<readonly string[]>(position?.expandedRecords ?? [])
  const latest = updates.at(-1)
  const revision = latest === undefined ? '' : `${latest.taskId}:${latest.seq}:${latest.text.length}`
  const lastRead = useRef(position?.lastEvent ?? revision)
  const priorRevision = useRef(revision)
  const priorContent = useRef({ updates, expanded })
  const hasUpdates = updates.length > 0
  const rememberPosition = (log: HTMLDivElement, element: HTMLElement, expandedRecords = expanded) => {
    anchors.current = visibleAnchors(log, element)
    remember({ top: element.scrollTop, follow: follow.current, lastEvent: lastRead.current, anchors: anchors.current, expandedRecords })
  }
  const onScroll = useRef<(log: HTMLDivElement, element: HTMLElement) => void>(() => {})
  onScroll.current = (log, element) => {
    if (!active) return
    wasVisible.current = readerVisible(log, element)
    if (!wasVisible.current) return
    // Layout corrections must not turn a history reader into a latest-entry follower.
    if (adjustedTop.current !== element.scrollTop) {
      follow.current = nearLatest(log, element)
      setReadingHistory(!follow.current)
      if (follow.current) { setUnread(false); lastRead.current = revision }
    }
    adjustedTop.current = undefined
    rememberPosition(log, element)
  }
  useLayoutEffect(() => {
    const log = scroll.current
    if (!active || log === null) return
    const element = scrollOwner(log)
    const handleScroll = () => { onScroll.current(log, element) }
    element.addEventListener('scroll', handleScroll, { passive: true })
    return () => { element.removeEventListener('scroll', handleScroll) }
  }, [active, hasUpdates])
  useLayoutEffect(() => {
    const log = scroll.current
    if (!active) { wasActive.current = false; return }
    if (log === null) return
    const element = scrollOwner(log)
    if (log.closest('[hidden], [inert], [aria-hidden="true"]') !== null) { wasActive.current = false; return }
    const previousTop = element.scrollTop
    const reactivated = restored.current && !wasActive.current
    const visible = readerVisible(log, element)
    const contentChanged = priorContent.current.expanded !== expanded
      || priorContent.current.updates.length !== updates.length
      || updates.some((update, index) => {
        const previous = priorContent.current.updates[index]
        return previous === undefined || previous.taskId !== update.taskId || previous.seq !== update.seq || previous.text !== update.text
      })
    if (!restored.current) {
      restored.current = true
      if (log === element && initialPosition.current !== undefined && !follow.current) {
        if (!restoreAnchor(log, element, anchors.current)) element.scrollTop = initialPosition.current.top
      } else if (log === element) showLatest(log, element)
      else if (visible) {
        follow.current = nearLatest(log, element)
        setReadingHistory(!follow.current)
      }
    } else if (wasActive.current && contentChanged && (wasVisible.current || log === element)) {
      if (!follow.current) restoreAnchor(log, element, anchors.current)
      else if (revision !== priorRevision.current) showLatest(log, element)
    }
    // The scene restores its own position when a mounted tab becomes active again.
    // Refresh anchors here without replaying the hidden tab's old scroll position.
    const nowVisible = readerVisible(log, element)
    if (reactivated && nowVisible && follow.current && !nearLatest(log, element)) {
      follow.current = false
      setReadingHistory(true)
    }
    if (follow.current && nowVisible && nearLatest(log, element)) { lastRead.current = revision; setUnread(false) }
    else if (!follow.current) setUnread(lastRead.current !== revision)
    if (element.scrollTop !== previousTop) adjustedTop.current = element.scrollTop
    if (nowVisible) anchors.current = visibleAnchors(log, element)
    wasVisible.current = nowVisible
    wasActive.current = true
    priorRevision.current = revision
    priorContent.current = { updates, expanded }
  }, [active, updates, expanded, revision])
  useEffect(() => {
    const log = scroll.current
    if (!active || log === null) return
    const element = scrollOwner(log)
    // Parent layout effects restore the scene after child layout effects run.
    // Start subsequent content anchoring from that committed viewport.
    wasVisible.current = readerVisible(log, element)
    if (wasVisible.current) anchors.current = visibleAnchors(log, element)
  }, [active, hasUpdates])
  if (updates.length === 0) return null
  return <section className={css.publicUpdates} aria-label={t('task.updates.title')}>
    <div className={css.sectionHeader}><h3>{t('task.updates.title')}</h3><span>{t('task.updates.notDelivery')}</span></div>
    {truncated ? <p className={css.muted}>{t('task.updates.recent')}</p> : null}
    <div ref={scroll} className={css.publicUpdateLog} role="log" aria-live="off" tabIndex={0} aria-label={t('task.updates.record')}>
      {updates.map((update, index) => {
        const key = `${update.taskId}:${update.seq}`
        const isLong = update.text.length > 1200 || update.text.split('\n').length > 12
        const isExpanded = expanded.includes(key)
        const excerpt = update.text.split(/\n\s*\n/u).find(paragraph => paragraph.trim() !== '' && !/^#{1,6}\s/u.test(paragraph.trim())) ?? update.text
        return <article key={key} data-record={key}>
          {update.occurredAt === '' ? null : <time dateTime={update.occurredAt}>{new Date(update.occurredAt).toLocaleTimeString()}</time>}
          {isLong ? <button type="button" className={css.publicUpdateToggle} aria-expanded={isExpanded} aria-controls={`${contentId}-${index}`} onClick={() => {
            if (!active) return
            const retainedExpanded = expanded.filter(item => updates.some(record => `${record.taskId}:${record.seq}` === item))
            const nextExpanded = isExpanded ? retainedExpanded.filter(item => item !== key) : [...retainedExpanded, key]
            follow.current = false
            setReadingHistory(true)
            if (scroll.current !== null) rememberPosition(scroll.current, scrollOwner(scroll.current), nextExpanded)
            setExpanded(nextExpanded)
          }}>{t(isExpanded ? 'task.updates.collapse' : 'task.updates.expand')}</button> : null}
          <div id={`${contentId}-${index}`}>
            {isLong && !isExpanded
              ? <div className={css.publicUpdatePreview}><MarkdownText text={`${excerpt.slice(0, 240)}…`} labels={{ code: { copyLabel: t('task.document.copy'), copiedLabel: t('task.document.copied') }, footnotes: t('task.document.footnotes') }} /></div>
              : <MarkdownText text={update.text} labels={{ code: { copyLabel: t('task.document.copy'), copiedLabel: t('task.document.copied') }, footnotes: t('task.document.footnotes') }} />}
          </div>
          {update.truncated ? <p className={css.muted}>{t('task.updates.truncated')}</p> : null}
        </article>
      })}
    </div>
    {!readingHistory ? null : <button type="button" className={css.secondaryButton} onClick={() => {
      const log = scroll.current
      if (!active || log === null) return
      const element = scrollOwner(log)
      follow.current = true; lastRead.current = revision; setUnread(false); setReadingHistory(false)
      showLatest(log, element)
      log.focus({ preventScroll: true })
      rememberPosition(log, element)
    }}>{t(unread ? 'task.updates.latest' : 'task.updates.returnLatest')}</button>}
  </section>
}
