/** In-place reading for retained tables and SVG drawings. */
import { type CSSProperties, useMemo, useState } from 'react'
import { csvParseRows, tsvParseRows } from 'd3-dsv'
import { IconDownloadOutline16, IconRefreshOutline16, Tooltip } from '@deepseek-ai/dsh-client-ui-primitives'
import type { DeliverablePreviewLabels } from './preview-locales.ts'
import css from './DeliverablePreview.module.css'

interface Props {
  readonly contentType: string
  readonly body: string
  readonly title: string
  /** Same-origin authenticated image route bound to the current deliverable. */
  readonly imageUrl: string
  readonly download?: { readonly url: string; readonly label: string; readonly notice: string }
  readonly labels: DeliverablePreviewLabels
}

/**
 * Read supported structured files without leaving the work scene.
 * @param props - Retained file contents, image route, and localized controls.
 * @returns A bounded table or zoomable image; unsupported types belong to the caller's document reader.
 */
export function DeliverablePreview(props: Props) {
  return <FilePreview key={JSON.stringify([props.contentType, props.imageUrl])} {...props} />
}

function ZoomIcon({ direction }: { direction: 'in' | 'out' }) {
  return <svg viewBox="0 0 16 16" width="16" height="16" fill="none" aria-hidden="true">
    <path d={direction === 'in' ? 'M3.5 8h9M8 3.5v9' : 'M3.5 8h9'} stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
  </svg>
}

function FilePreview({ contentType, body, title, imageUrl, labels, download }: Props) {
  const [zoom, setZoom] = useState(1)
  const [imageState, setImageState] = useState<'loading' | 'ready' | 'failed'>('loading')
  const table = useMemo(() => {
    if (contentType !== 'text/csv' && contentType !== 'text/tab-separated-values') return null
    let limited = false
    const parse = contentType === 'text/csv' ? csvParseRows : tsvParseRows
    const rows = parse(body.replace(/^\uFEFF/u, ''), (row, index) => {
      if (index >= 200) { limited = true; return null }
      if (row.length > 40) limited = true
      return row.slice(0, 40)
    })
    return { rows, limited }
  }, [body, contentType])
  if (table !== null) return <div className={css.preview}>
    <div className={css.toolbar}>
      <span>{labels.table}</span>
      {download === undefined ? null : <Tooltip label={download.label}>
        <a className={css.iconButton} href={download.url} download aria-label={download.label}><IconDownloadOutline16 /></a>
      </Tooltip>}
    </div>
    <div className={css.tableScroll} tabIndex={0} role="region" aria-label={labels.table}>
      <table><caption>{title}</caption><tbody>
        {table.rows.map((row, index) => <tr key={index}>{row.map((cell, column) => <td key={column}>{cell}</td>)}</tr>)}
      </tbody></table>
    </div>
    {download?.notice ? <p className={css.notice}>{download.notice}</p> : null}
    {table.limited ? <p className={css.notice} role="status">{labels.tableLimited}</p> : null}
  </div>
  if (contentType !== 'image/svg+xml') return null
  return <div className={css.preview}>
    <div className={css.toolbar}>
      {download === undefined ? <span>{labels.image}</span>
        : <Tooltip label={download.label}>
          <a className={css.iconButton} href={download.url} download aria-label={download.label}><IconDownloadOutline16 /></a>
        </Tooltip>}
      <div className={css.controls} role="group" aria-label={labels.zoomLevel}>
        <Tooltip label={labels.zoomOut}><button type="button" disabled={zoom <= .5 || imageState !== 'ready'} onClick={() => { setZoom(value => Math.max(.5, value - .25)) }} aria-label={labels.zoomOut}><ZoomIcon direction="out" /></button></Tooltip>
        <output aria-live="polite" aria-label={labels.zoomLevel}>{Math.round(zoom * 100)}%</output>
        <Tooltip label={labels.zoomIn}><button type="button" disabled={zoom >= 4 || imageState !== 'ready'} onClick={() => { setZoom(value => Math.min(4, value + .25)) }} aria-label={labels.zoomIn}><ZoomIcon direction="in" /></button></Tooltip>
        <Tooltip label={labels.zoomReset}><button type="button" disabled={imageState !== 'ready'} onClick={() => { setZoom(1) }} aria-label={labels.zoomReset}><IconRefreshOutline16 /></button></Tooltip>
      </div>
    </div>
    {download?.notice ? <p className={css.notice}>{download.notice}</p> : null}
    {imageState === 'loading' ? <p className={css.notice} role="status">{labels.imageLoading}</p> : null}
    {imageState === 'failed' ? <p className={css.notice} role="status">{labels.imageUnavailable}</p> : null}
    <div className={css.imageScroll} tabIndex={0} role="region" aria-label={title} aria-busy={imageState === 'loading'} hidden={imageState === 'failed'}>
      <img src={imageUrl} alt={title} style={{ '--weave-preview-width': `${zoom * 100}%` } as CSSProperties}
        onLoad={() => { setImageState('ready') }} onError={() => { setImageState('failed') }} />
    </div>
  </div>
}
