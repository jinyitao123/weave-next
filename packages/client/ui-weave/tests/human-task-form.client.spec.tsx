// @vitest-environment jsdom
import { cleanup, fireEvent, render, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { makeTranslate } from '@deepseek-ai/dsh-client-test-runtime'
import { zh as commonZh } from '@deepseek-ai/dsh-client-locale/src/locales/zh.ts'
import { HumanTaskForm } from '../src/client/HumanTaskForm.tsx'
import { zh } from '../src/client/locales.ts'

const t = makeTranslate(zh, commonZh)
const task = { interactionId: 'run:review:time', nodeId: 'review', title: '确认交付范围', instructions: '请先阅读**已完成的材料**。', resumeSchema: { type: 'object', properties: { decision: { type: 'string', title: '处理意见', enum: ['继续', '修改'] }, publish: { type: 'boolean', title: '允许发布' }, copies: { type: 'integer', title: '份数' } }, required: ['decision', 'publish', 'copies'] } }
afterEach(cleanup)

describe('human task response form', () => {
  it('submits declared names and primitive types only after an explicit answer action', async () => {
    const submit = vi.fn(async () => null)
    const view = render(<HumanTaskForm task={task} id="question" disabled={false} rejectionId="" submit={submit} t={t} />)
    expect(submit).not.toHaveBeenCalled()
    fireEvent.change(view.getByLabelText('处理意见 · 必填'), { target: { value: '"继续"' } })
    fireEvent.change(view.getByLabelText('允许发布 · 必填'), { target: { value: 'false' } })
    fireEvent.change(view.getByLabelText('份数 · 必填'), { target: { value: '2' } })
    fireEvent.click(view.getByRole('button', { name: '提交答复并继续' }))
    await waitFor(() =>{  expect(submit).toHaveBeenCalledWith({ decision: '继续', publish: false, copies: 2 }) })
    expect((view.getByRole('button', { name: '答复已记录，等待处理' }) as HTMLButtonElement).closest('fieldset')?.disabled).toBe(true)
    view.rerender(<HumanTaskForm task={task} id="question" disabled={false} rejectionId="rejected-1" submit={submit} t={t} />)
    await waitFor(() =>{  expect(view.getByRole('button', { name: '提交答复并继续' })).toBeTruthy() })
    expect((view.getByLabelText('份数 · 必填') as HTMLInputElement).value).toBe('2')
  })

  it('retains nested declared constants in the actual submitted payload', async () => {
    const submit = vi.fn(async () => null)
    const nested = { ...task, resumeSchema: { type: 'object', properties: { approval: { type: 'object', properties: { version: { const: 2 }, decision: { type: 'string', title: '决定' } }, required: ['version', 'decision'] } }, required: ['approval'] } }
    const view = render(<HumanTaskForm task={nested} id="nested" disabled={false} rejectionId="" submit={submit} t={t} />)
    fireEvent.change(view.getByLabelText('决定 · 必填'), { target: { value: '继续' } })
    fireEvent.click(view.getByRole('button', { name: '提交答复并继续' }))
    await waitFor(() =>{  expect(submit).toHaveBeenCalledWith({ approval: { version: 2, decision: '继续' } }) })
  })

  it('keeps complex schema requirements in the conversation instead of inventing an answer field', () => {
    const discuss = vi.fn()
    const submit = vi.fn(async () => null)
    const view = render(<HumanTaskForm task={{ ...task, resumeSchema: { oneOf: [{ type: 'object' }, { type: 'array' }] } }} id="complex" disabled={false} rejectionId="" discuss={discuss} submit={submit} t={t} />)
    expect(view.queryByRole('button', { name: '提交答复并继续' })).toBeNull()
    fireEvent.click(view.getByRole('button', { name: '先在主对话讨论' }))
    expect(discuss).toHaveBeenCalledOnce()
    expect(submit).not.toHaveBeenCalled()
  })
})
