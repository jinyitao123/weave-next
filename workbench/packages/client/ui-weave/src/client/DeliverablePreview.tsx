/** In-place reading for retained tables and SVG drawings. */
import { useMemo, useState } from 'react'
import { csvParseRows, tsvParseRows } from 'd3-dsv'
import type { DeliverablePreviewLabels } from './preview-locales.ts'
import css from './DeliverablePreview.module.css'

interface Props {
  readonly contentType: string
  readonly body: string
  readonly title: string
  /** Same-origin authenticated image route bound to the current deliverable. */
  readonly imageUrl: string
  readonly labels: DeliverablePreviewLabels
}

/**
 * Read supported structured files without leaving the work scene.
 * @param props - Retained file contents, image route, and localized controls.
 * @returns A bounded table or zoomable image; unsupported types belong to the caller's document reader.
 */
export function DeliverablePreview({ contentType, body, title, imageUrl, labels }: Props) {
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
  if (table !== null) return <div className={css.tablePreview}>
    <div className={css.tableScroll} tabIndex={0} role="region" aria-label={labels.table}>
      <table><caption>{title}</caption><tbody>
        {table.rows.map((row, index) => <tr key={index}>{row.map((cell, column) => <td key={column}>{cell}</td>)}</tr>)}
      </tbody></table>
    </div>
    {table.limited ? <p role="status">{labels.tableLimited}</p> : null}
  </div>
  if (contentType !== 'image/svg+xml') return null
  return <div className={css.imagePreview}>
    <div className={css.controls}>
      <button type="button" disabled={zoom <= .5 || imageState !== 'ready'} onClick={() => { setZoom(value => Math.max(.5, value - .25)) }} aria-label={labels.zoomOut}>−</button>
      <output aria-live="polite">{Math.round(zoom * 100)}%</output>
      <button type="button" disabled={zoom >= 4 || imageState !== 'ready'} onClick={() => { setZoom(value => Math.min(4, value + .25)) }} aria-label={labels.zoomIn}>+</button>
      <button type="button" disabled={imageState !== 'ready'} onClick={() => { setZoom(1) }}>{labels.zoomReset}</button>
    </div>
    {imageState === 'loading' ? <p role="status">{labels.imageLoading}</p> : null}
    {imageState === 'failed' ? <p role="status">{labels.imageUnavailable}</p> : null}
    <div className={css.imageScroll} tabIndex={0} role="region" aria-label={title}>
      <img src={imageUrl} alt={title} style={{ width: `${zoom * 100}%` }}
        onLoad={() => { setImageState('ready') }} onError={() => { setImageState('failed') }} />
    </div>
  </div>
}
