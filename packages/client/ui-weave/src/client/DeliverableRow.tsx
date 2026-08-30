import type { ReactNode } from 'react'
import { IconDownloadOutline16, IconSparkle16, StateDot } from '@deepseek-ai/dsh-client-ui-primitives'
import type { ToolCallViewProps } from '@deepseek-ai/dsh-client-ui-tool/client'
import type { PropsLocale } from '@deepseek-ai/dsh-client-ui-slots'
import css from './DeliverableRow.module.css'

type DeliverableProps = ToolCallViewProps & PropsLocale<'weave'>
type DeliverableState = 'running' | 'ok' | 'error' | 'stopped' | 'invalid'

interface Deliverable {
  readonly id: string
  readonly title: string
  readonly content: string
  readonly contentType: string
  readonly filename: string
}

interface DeliverableModel {
  readonly state: DeliverableState
  readonly deliverable: Deliverable | null
  readonly detail: string | null
}

function resultText(block: ToolCallViewProps['block']): string | null {
  if (!('kind' in block)) return null
  const parts = block.content.map(item => item.type === 'text' ? item.text : JSON.stringify(item, null, 2))
  if (parts.length === 0 && block.error !== undefined) parts.push(`${block.error.name}: ${block.error.code}`)
  return parts.join('\n') || null
}

function stringField(record: Record<string, unknown>, key: string): string {
  const value = record[key]
  return typeof value === 'string' ? value : ''
}

function extension(contentType: string, content: string): string {
  const normalized = contentType.toLowerCase()
  if (normalized.includes('json')) return 'json'
  if (normalized.includes('html') || /^\s*<!doctype html/i.test(content)) return 'html'
  if (normalized.includes('svg') || /^\s*<svg/i.test(content)) return 'svg'
  if (normalized.includes('plain')) return 'txt'
  return 'md'
}

function safeFilename(title: string, id: string, suffix: string): string {
  const stem = (title || id || 'weave-deliverable')
    .replace(/[\\/:*?"<>|\u0000-\u001f]/g, '-')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, 100)
  return `${stem || 'weave-deliverable'}.${suffix}`
}

function callDeliverableId(block: ToolCallViewProps['block']): string {
  const argsRaw = 'kind' in block ? block.call?.argsRaw ?? '' : block.argsRaw
  try {
    const parsed = JSON.parse(argsRaw) as unknown
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return ''
    const record = parsed as Record<string, unknown>
    return stringField(record, 'id').trim()
  } catch {
    return ''
  }
}

function markdownTitle(content: string): string {
  const heading = content.match(/^\s*#{1,6}\s+(.+)$/m)?.[1]?.trim() ?? ''
  return heading.replace(/^《|》$/g, '').trim()
}

export function deliverableModel(block: ToolCallViewProps['block']): DeliverableModel {
  if (!('kind' in block)) return { state: 'running', deliverable: null, detail: null }
  const detail = resultText(block)
  if (block.error?.code === 'interrupted') return { state: 'stopped', deliverable: null, detail }
  if (block.isError) return { state: 'error', deliverable: null, detail }
  if (detail === null) return { state: 'invalid', deliverable: null, detail: null }
  try {
    const parsed = JSON.parse(detail) as unknown
    if (typeof parsed === 'string') {
      const id = callDeliverableId(block)
      if (id === '' || parsed === '') {
        return { state: 'invalid', deliverable: null, detail }
      }
      const title = markdownTitle(parsed) || id
      return {
        state: 'ok', detail: null,
        deliverable: {
          id, title, content: parsed, contentType: 'text/markdown',
          filename: safeFilename(title, id, 'md'),
        },
      }
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      return { state: 'invalid', deliverable: null, detail }
    }
    const record = parsed as Record<string, unknown>
    const id = stringField(record, 'id').trim()
    const title = stringField(record, 'title').trim()
    const content = stringField(record, 'content')
    const contentType = stringField(record, 'content_type').trim() || 'text/markdown'
    if (id === '' || content === '') return { state: 'invalid', deliverable: null, detail }
    return {
      state: 'ok', detail: null,
      deliverable: {
        id, title: title || id, content, contentType,
        filename: safeFilename(title, id, extension(contentType, content)),
      },
    }
  } catch {
    return { state: 'invalid', deliverable: null, detail }
  }
}

function leading(state: DeliverableState): ReactNode {
  if (state === 'error' || state === 'invalid') return <StateDot state="error" />
  if (state === 'stopped') return <StateDot state="warning" />
  return <IconSparkle16 size={14} />
}

function summary(state: DeliverableState, t: DeliverableProps['t']): string {
  switch (state) {
    case 'running': return t('deliverable.running')
    case 'ok': return t('deliverable.ready')
    case 'error': return t('deliverable.failed')
    case 'stopped': return t('deliverable.stopped')
    case 'invalid': return t('deliverable.invalid')
  }
}

function download(deliverable: Deliverable): void {
  const blob = new Blob([deliverable.content], { type: `${deliverable.contentType};charset=utf-8` })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = deliverable.filename
  anchor.click()
  URL.revokeObjectURL(url)
}

/** Render a Weave final deliverable as a visible, downloadable file. */
export function DeliverableRow({ block, t }: DeliverableProps) {
  const model = deliverableModel(block)
  const deliverable = model.deliverable
  return (
    <section className={css.card} data-tool="mcp__weave__deliverable_get" data-state={model.state}>
      <header className={css.header}>
        <span className={css.leading}>{leading(model.state)}</span>
        <span className={css.title}>{t('deliverable.title')}</span>
        <span className={css.separator} aria-hidden />
        <span className={css.summary}>{summary(model.state, t)}</span>
      </header>
      {deliverable !== null ? (
        <article className={css.file}>
          <div className={css.fileHeader}>
            <div className={css.identity}>
              <strong>{deliverable.title}</strong>
              <span>{deliverable.filename}</span>
            </div>
            <button className={css.download} type="button" onClick={() => download(deliverable)}>
              <IconDownloadOutline16 size={14} />
              {t('deliverable.download')}
            </button>
          </div>
          <div className={css.previewLabel}>{t('deliverable.preview')}</div>
          <pre className={css.preview}>{deliverable.content}</pre>
        </article>
      ) : null}
      {(model.state === 'error' || model.state === 'invalid') && model.detail !== null ? (
        <pre className={css.errorDetail}>{model.detail}</pre>
      ) : null}
    </section>
  )
}
