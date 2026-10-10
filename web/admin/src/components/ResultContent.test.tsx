import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ResultContent } from './ResultContent'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('result reading', () => {
  it('decodes JSON text and material lists while copying the exact original', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    const content = JSON.stringify({ disposition: '材料不足', summary: '第一段\n第二段', missing_items: ['技术协议', '对应报价'], detail: { amount: 2100, confirmed: false }, notes: [] })
    const { container } = render(<ResultContent content={content} />)
    expect(screen.getByText('处理结论')).toBeTruthy()
    expect(screen.getByText('汇总')).toBeTruthy()
    expect(screen.getByText('待补材料')).toBeTruthy()
    expect(container.querySelector('dd p')?.textContent).toBe('材料不足')
    expect(screen.getByText('第一段 第二段').textContent).toBe('第一段\n第二段')
    expect(container.querySelectorAll('li')).toHaveLength(2)
    expect(screen.getByText('2100')).toBeTruthy()
    expect(screen.getByText('false')).toBeTruthy()
    expect(screen.queryByRole('group', { name: '结果显示' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '复制原文' }))
    await screen.findByRole('button', { name: '已复制原文' })
    expect(writeText).toHaveBeenCalledWith(content)
    expect(screen.getByText('技术协议')).toBeTruthy()
  })

  it('renders Markdown headings, lists, tables and code as readable blocks', () => {
    const { container } = render(<ResultContent content={'## 核对结果\n\n- **技术协议**缺失\n\n| 项目 | 金额 |\n| --- | --- |\n| 设备 | 1600 |\n\n```json\n{"amount":1600}\n```'} />)
    expect(screen.getByRole('heading', { name: '核对结果' })).toBeTruthy()
    expect(screen.getByRole('table')).toBeTruthy()
    expect(screen.getByRole('cell', { name: '1600' })).toBeTruthy()
    expect(container.querySelector('strong')?.textContent).toBe('技术协议')
    expect(container.querySelector('pre code')?.textContent).toBe('{"amount":1600}\n')
  })

  it('reads fenced JSON and preserves unknown fields and empty values', () => {
    render(<ResultContent content={'```json\n{"other":{"items":[],"data":{},"value":null}}\n```'} />)
    for (const text of ['other', 'items', 'data', 'value', '[]', '{}', 'null']) expect(screen.getByText(text)).toBeTruthy()
  })

  it('keeps malformed or truncated JSON readable and copies it without alteration', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    const content = '{"summary":"材料不全",'
    const { container } = render(<ResultContent content={content} />)
    expect(container.textContent).toContain(content)
    fireEvent.click(screen.getByRole('button', { name: '复制原文' }))
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(content))
  })

  it('reports a failed copy and permits retry without claiming success', async () => {
    const writeText = vi.fn().mockRejectedValueOnce(new Error('denied')).mockResolvedValueOnce(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    render(<ResultContent content={'  原始文本\n第二行  '} />)
    fireEvent.click(screen.getByRole('button', { name: '复制原文' }))
    fireEvent.click(await screen.findByRole('button', { name: '复制失败，点击重试' }))
    await screen.findByRole('button', { name: '已复制原文' })
    expect(writeText).toHaveBeenLastCalledWith('  原始文本\n第二行  ')
  })

  it('does not execute embedded HTML, unsafe links or remote images', () => {
    const { container } = render(<ResultContent content={'<script>alert(1)</script>\n\n[危险链接](javascript:alert%281%29)\n\n![图片说明](https://example.test/image.png)\n\n[出处](https://example.test/reference)'} />)
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('img')).toBeNull()
    expect(screen.getByText('危险链接').closest('a')).toBeNull()
    expect(screen.getByText('图片说明')).toBeTruthy()
    expect(screen.getByRole('link', { name: '出处' }).getAttribute('rel')).toBe('noopener noreferrer')
  })
})
