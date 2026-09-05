// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, within } from '@testing-library/react'
import { DeliverablePreview } from '../src/client/DeliverablePreview.tsx'
import { deliverablePreviewLabels, zh } from '../src/client/preview-locales.ts'

const labels = deliverablePreviewLabels(key => zh[key])
const props = { title: '成员材料', imageUrl: '/api/weave.deliverable?id=known&mode=preview', labels }
afterEach(cleanup)

describe('structured deliverable reading', () => {
  it('preserves quoted cells, line breaks, and formula-like text without executing it', () => {
    const view = render(<DeliverablePreview {...props} contentType="text/csv"
      body={'\uFEFF姓名,说明,内容\r\n张三,"一、二\n第三行","<script>alert(1)</script>"\r\n李四,"引号""内容",=1+1'} />)
    const rows = within(view.getByRole('table')).getAllByRole('row')
    expect(rows).toHaveLength(3)
    expect(within(rows[1]!).getAllByRole('cell').map(cell => cell.textContent)).toEqual(['张三', '一、二\n第三行', '<script>alert(1)</script>'])
    expect(within(rows[2]!).getAllByRole('cell').map(cell => cell.textContent)).toEqual(['李四', '引号"内容', '=1+1'])
    expect(view.container.querySelector('script')).toBeNull()
  })

  it('bounds large table rendering and identifies the preview limit', () => {
    const body = Array.from({ length: 201 }, (_, row) => Array.from({ length: 41 }, (_, column) => `${row}:${column}`).join('\t')).join('\n')
    const view = render(<DeliverablePreview {...props} contentType="text/tab-separated-values" body={body} />)
    const rows = within(view.getByRole('table')).getAllByRole('row')
    expect(rows).toHaveLength(200)
    expect(within(rows[199]!).getAllByRole('cell')).toHaveLength(40)
    expect(view.getByRole('status').textContent).toBe(labels.tableLimited)
    expect(view.queryByText('200:0')).toBeNull()
  })

  it('zooms the existing authenticated image without opening a new view', () => {
    const view = render(<DeliverablePreview {...props} contentType="image/svg+xml" body="" />)
    const image = view.getByRole('img') as HTMLImageElement
    expect(view.getByRole('button', { name: labels.zoomIn }).hasAttribute('disabled')).toBe(true)
    fireEvent.load(image)
    fireEvent.click(view.getByRole('button', { name: labels.zoomIn }))
    expect(image.style.width).toBe('125%')
    fireEvent.click(view.getByRole('button', { name: labels.zoomReset }))
    expect(image.style.width).toBe('100%')
    expect(image.getAttribute('src')).toBe(props.imageUrl)
    fireEvent.error(image)
    expect(view.getByText(labels.imageUnavailable).getAttribute('role')).toBe('status')
    expect(view.getByRole('button', { name: labels.zoomIn }).hasAttribute('disabled')).toBe(true)
  })
})
