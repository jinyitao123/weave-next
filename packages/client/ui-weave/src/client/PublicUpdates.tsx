/** Public runtime updates remain distinct from final deliverables and private reasoning. */
import { useLayoutEffect, useRef, useState } from 'react'
import { MarkdownText } from '@deepseek-ai/dsh-client-ui-primitives'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import type { WorkTaskPublicUpdate } from './work-task-model.ts'
import type { WorkTaskReadingPosition } from './view-store.ts'
import css from './WorkTaskPanel.module.css'

interface Props extends PropsLocale<'weave'> { readonly updates: readonly WorkTaskPublicUpdate[]; readonly truncated: boolean; readonly position: WorkTaskReadingPosition | undefined; readonly remember: (position: WorkTaskReadingPosition) => void }

/**
 * Render current public text while following the latest entry; preserve the viewport during history reading.
 * @param props - Ordered, deduplicated runtime messages and labels.
 * @returns An in-place record with an explicit return-to-latest action.
 */
export function PublicUpdates({ updates, truncated, position, remember, t }: Props) {
  const scroll = useRef<HTMLDivElement>(null)
  const initialPosition = useRef(position)
  const follow = useRef(position?.follow ?? true)
  const restored = useRef(false)
  const [unread, setUnread] = useState(false)
  const latest = updates.at(-1)
  const revision = latest === undefined ? '' : `${latest.taskId}:${latest.seq}:${latest.text.length}`
  const lastRead = useRef(position?.lastEvent ?? revision)
  useLayoutEffect(() => {
    if (scroll.current === null) return
    if (!restored.current) {
      restored.current = true
      if (initialPosition.current !== undefined && !initialPosition.current.follow) scroll.current.scrollTop = initialPosition.current.top
    }
    if (follow.current) {
      scroll.current.scrollTop = scroll.current.scrollHeight
      lastRead.current = revision
    } else setUnread(lastRead.current !== revision)
  }, [revision])
  if (updates.length === 0) return null
  return <section className={css.publicUpdates} aria-label={t('task.updates.title')}>
    <div className={css.sectionHeader}><h3>{t('task.updates.title')}</h3><span>{t('task.updates.notDelivery')}</span></div>
    {truncated ? <p className={css.muted}>{t('task.updates.recent')}</p> : null}
    <div ref={scroll} className={css.publicUpdateLog} role="log" aria-live="off" tabIndex={0} aria-label={t('task.updates.record')} onScroll={(event) => {
      const element = event.currentTarget
      follow.current = element.scrollHeight - element.scrollTop - element.clientHeight < 24
      if (follow.current) { setUnread(false); lastRead.current = revision }
      remember({ top: element.scrollTop, follow: follow.current, lastEvent: lastRead.current })
    }}>
      {updates.map(update => <article key={`${update.taskId}:${update.seq}`}>
        {update.occurredAt === '' ? null : <time dateTime={update.occurredAt}>{new Date(update.occurredAt).toLocaleTimeString()}</time>}
        <MarkdownText text={update.text} labels={{ code: { copyLabel: t('task.document.copy'), copiedLabel: t('task.document.copied') }, footnotes: t('task.document.footnotes') }} />
        {update.truncated ? <p className={css.muted}>{t('task.updates.truncated')}</p> : null}
      </article>)}
    </div>
    {!unread ? null : <button type="button" className={css.secondaryButton} onClick={() => {
      follow.current = true; lastRead.current = revision; setUnread(false)
      if (scroll.current !== null) {
        scroll.current.scrollTop = scroll.current.scrollHeight
        remember({ top: scroll.current.scrollTop, follow: true, lastEvent: revision })
      }
    }}>{t('task.updates.latest')}</button>}
  </section>
}
