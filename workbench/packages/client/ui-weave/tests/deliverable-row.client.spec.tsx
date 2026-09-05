// @vitest-environment jsdom

import { cleanup, fireEvent, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { RunningToolCall, ToolResultNode } from '@deepseek-ai/dsh-client-ui-chat/client'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { DeliverableRow, deliverableModel } from '../src/client/DeliverableRow.tsx'
import { zh } from '../src/client/locales.ts'

type Props = Parameters<typeof DeliverableRow>[0]
const t: Props['t'] = makeTranslate(zh, commonZh)

afterEach(cleanup)

function settled(text: string, over: Partial<ToolResultNode> = {}, argsRaw = '{}'): ToolResultNode {
  return {
    kind: 'tool-result', seq: 3, time: 3_000, callId: 'call-deliverable',
    call: { name: 'mcp__weave__deliverable_get', argsRaw }, callTime: 2_000,
    content: [{ type: 'text', text }], isError: false, subCalls: [], ...over,
  }
}

function running(): RunningToolCall {
  return {
    callId: 'call-deliverable', name: 'mcp__weave__deliverable_get', argsRaw: '{}',
    turn: 1, step: 1, time: 2_000, subCalls: [],
  }
}

function props(block: Props['block']): Props {
  return {
    callId: block.callId, toolName: 'mcp__weave__deliverable_get', block,
    openFile: vi.fn(), t,
  } as unknown as Props
}

describe('DeliverableRow', () => {
  it('shows the final file contents without exposing raw envelope fields', () => {
    const payload = JSON.stringify({
      id: 'deliverable-secret-id', title: '日冕共同论证基线',
      content: '# 共同基线\n\n这是可见的最终正文。', content_type: 'text/markdown',
      workspace_id: 'internal-workspace', metadata: { source: 'published_workflow' },
    })
    const view = render(<DeliverableRow {...props(settled(payload))} />)
    expect(view.container.textContent).toContain('最终交付物')
    expect(view.container.textContent).toContain('文件已就绪')
    expect(view.container.textContent).toContain('日冕共同论证基线.md')
    expect(view.container.textContent).toContain('这是可见的最终正文。')
    expect(view.container.textContent).not.toContain('internal-workspace')
    expect(view.container.textContent).not.toContain('published_workflow')
  })

  it('downloads the visible content as a file', () => {
    const createObjectURL = vi.fn(() => 'blob:deliverable')
    const revokeObjectURL = vi.fn()
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL })
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: revokeObjectURL })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const payload = JSON.stringify({
      id: 'd1', title: '分析报告', content: '{"answer":42}', content_type: 'application/json',
    })
    const view = render(<DeliverableRow {...props(settled(payload))} />)
    fireEvent.click(view.getByRole('button', { name: /下载文件/ }))
    expect(createObjectURL).toHaveBeenCalledOnce()
    expect(click).toHaveBeenCalledOnce()
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:deliverable')
    click.mockRestore()
  })

  it('renders a content-path result as the requested deliverable file', () => {
    const content = '# 《日冕计划共同论证基线 v0.2》\n\n正文直接来自 /content。'
    const view = render(<DeliverableRow {...props(settled(
      JSON.stringify(content), {}, JSON.stringify({ id: 'deliverable-1', path: '/content' }),
    ))} />)
    expect(view.container.textContent).toContain('文件已就绪')
    expect(view.container.textContent).toContain('日冕计划共同论证基线 v0.2.md')
    expect(view.container.textContent).toContain('正文直接来自 /content。')
  })

  it('bounds a large preview while retaining the complete download', () => {
    const content = `begin-${'x'.repeat(12_000)}-complete-tail`
    const createObjectURL = vi.fn<(blob: Blob) => string>(() => 'blob:large-deliverable')
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL })
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: vi.fn() })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const payload = JSON.stringify({ id: 'large', title: '大文件', content, content_type: 'text/plain' })
    const view = render(<DeliverableRow {...props(settled(payload))} />)

    expect(view.container.textContent).toContain('文件较大，仅展示开头')
    expect(view.container.textContent).not.toContain('complete-tail')
    fireEvent.click(view.getByRole('button', { name: /下载文件/ }))
    expect(createObjectURL).toHaveBeenCalledWith(expect.any(Blob))
    expect(createObjectURL.mock.calls[0]?.[0].size).toBe(new Blob([content]).size)
    click.mockRestore()
  })

  it('keeps running and failures honest', () => {
    expect(deliverableModel(running()).state).toBe('running')
    expect(deliverableModel(settled('bad json')).state).toBe('invalid')
    expect(deliverableModel(settled('http_403', { isError: true })).state).toBe('error')
    expect(deliverableModel(settled('', {
      content: [], error: { name: 'InterruptedError', code: 'interrupted' },
    }))).toMatchObject({ state: 'stopped' })
  })
})
