import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { DevelopmentDocument, DevelopmentMember } from '../../lib/teams'
import { MemberEditor } from './MemberEditor'

const member = (id: string, name: string, role: string, engine: string, extra: Partial<DevelopmentMember['configuration']> = {}, relation: Partial<DevelopmentMember['relationship']> = {}): DevelopmentMember => ({
  id,
  configuration: { display_name: name, role, engine, runtime_id: '', model: '', system_prompt: '工作方法', ...extra },
  relationship: { duty: '职责', result_requirement: '', enabled: true, ...relation },
})
const team = (): DevelopmentDocument => ({ name: '团队', objective: '', audience: [], workflows: [], members: [
  member('w-cli', '编码', 'worker', 'claude'),
  member('lead', '负责人员', 'avatar', 'loom'),
  member('w-loom', '整理', 'worker', 'loom', { model: 'deepseek-flash', permission_deny: ['bash'] }),
] })

function Controlled({ changed }: { changed?(document: DevelopmentDocument): void }) {
  const [document, setDocument] = useState(team())
  const apply = (next: DevelopmentDocument) => { setDocument(next); changed?.(next) }
  return <MemberEditor document={document} nodes={[]} accepting={{}}
    onConfig={(id, patch) => apply({ ...document, members: document.members.map((item) => item.id === id ? { ...item, configuration: { ...item.configuration, ...patch } } : item) })}
    onRelationship={(id, patch) => apply({ ...document, members: document.members.map((item) => item.id === id ? { ...item, relationship: { ...item.relationship, ...patch } } : item) })} />
}

const saved = (document: DevelopmentDocument | undefined, id: string) => document?.members.find((item) => item.id === id)

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ version: '1', models: ['deepseek-flash', 'qwen-max'] }), { status: 200 })))
})
afterEach(() => { cleanup(); vi.unstubAllGlobals() })

describe('member editor', () => {
  it('lists the lead first and keeps its engine and handoff settings out of reach', () => {
    render(<Controlled />)
    const items = screen.getAllByRole('button', { pressed: undefined }).filter((button) => button.className.includes('members__item'))
    expect(items[0].textContent).toContain('负责人员')
    expect(items[0].textContent).toContain('负责人')
    expect(screen.getByRole('button', { name: '负责人员的引擎' }).hasAttribute('disabled')).toBe(true)
    expect(screen.queryByRole('group', { name: '交接方式' })).toBeNull()
    expect(screen.queryByText('何时参与')).toBeNull()
  })

  it('shows skills, limits and memory only for built-in engines', () => {
    render(<Controlled />)
    fireEvent.click(screen.getByRole('button', { name: /^编码/ }))
    expect(screen.getByRole('button', { name: '编码的节点' })).toBeTruthy()
    expect(screen.queryByRole('region', { name: '技能' })).toBeNull()
    expect(screen.queryByRole('region', { name: '执行限制' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: /^整理/ }))
    expect(screen.queryByRole('button', { name: '整理的节点' })).toBeNull()
    expect(screen.getByRole('region', { name: '技能' })).toBeTruthy()
    expect(screen.getByRole('region', { name: '执行限制' })).toBeTruthy()
    expect(screen.getByRole('region', { name: '记忆' })).toBeTruthy()
  })

  it('writes duty to the relationship and keeps other tool denials when toggling deny-all', () => {
    let last: DevelopmentDocument | undefined
    render(<Controlled changed={(document) => { last = document }} />)
    fireEvent.click(screen.getByRole('button', { name: /^整理/ }))
    fireEvent.change(screen.getByRole('textbox', { name: '职责' }), { target: { value: '整理材料' } })
    expect(saved(last, 'w-loom')?.relationship.duty).toBe('整理材料')
    fireEvent.click(screen.getByRole('switch', { name: '禁止使用工具' }))
    expect(saved(last, 'w-loom')?.configuration.permission_deny).toEqual(['bash', '*'])
    fireEvent.click(screen.getByRole('switch', { name: '禁止使用工具' }))
    expect(saved(last, 'w-loom')?.configuration.permission_deny).toEqual(['bash'])
  })

  it('starts from the server default handoff kinds and keeps the default among the chosen ones', () => {
    let last: DevelopmentDocument | undefined
    render(<Controlled changed={(document) => { last = document }} />)
    fireEvent.click(screen.getByRole('button', { name: /^编码/ }))
    expect(screen.getByRole('button', { name: '咨询' }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByRole('button', { name: '派发' }).getAttribute('aria-pressed')).toBe('true')
    fireEvent.click(screen.getByRole('button', { name: '交接' }))
    expect(saved(last, 'w-cli')?.relationship).toMatchObject({ allowed_kinds: ['consult', 'dispatch', 'handoff'], default_kind: 'dispatch' })
    fireEvent.click(screen.getByRole('button', { name: '派发' }))
    expect(saved(last, 'w-cli')?.relationship).toMatchObject({ allowed_kinds: ['consult', 'handoff'], default_kind: 'consult' })
  })

  it('stores limits as whole numbers, cost as a decimal and empty as unlimited', () => {
    let last: DevelopmentDocument | undefined
    render(<Controlled changed={(document) => { last = document }} />)
    fireEvent.click(screen.getByRole('button', { name: /^整理/ }))
    fireEvent.change(screen.getByRole('textbox', { name: '最多执行步数' }), { target: { value: '12.7' } })
    expect(saved(last, 'w-loom')?.configuration.step_budget).toBe(12)
    fireEvent.change(screen.getByRole('textbox', { name: '费用上限（美元）' }), { target: { value: '0.5' } })
    expect(saved(last, 'w-loom')?.configuration.max_cost_usd).toBe(0.5)
    fireEvent.change(screen.getByRole('textbox', { name: '最多执行步数' }), { target: { value: '' } })
    expect(saved(last, 'w-loom')?.configuration.step_budget).toBe(0)
  })

  it('offers catalog models for built-in engines', async () => {
    render(<Controlled />)
    fireEvent.click(screen.getByRole('button', { name: /^整理/ }))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    fireEvent.click(screen.getByRole('button', { name: '整理的模型' }))
    expect(await screen.findByRole('option', { name: 'qwen-max' })).toBeTruthy()
  })
})
