import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { Dialog, Drawer } from './ui'

afterEach(cleanup)

function EditableModal({ kind, onClosed }: { kind: 'dialog' | 'drawer'; onClosed(value: string): void }) {
  const [open, setOpen] = useState(false)
  const [name, setName] = useState('')
  const [requirement, setRequirement] = useState('')
  const Panel = kind === 'dialog' ? Dialog : Drawer
  return <>
    <button type="button" onClick={() => setOpen(true)}>打开编辑</button>
    {open ? <Panel title="编辑步骤" onClose={() => { onClosed(`${name}\n${requirement}`); setOpen(false) }}>
      <label>步骤名称<input value={name} onChange={event => setName(event.target.value)} /></label>
      <label>工作要求<textarea value={requirement} onChange={event => setRequirement(event.target.value)} /></label>
    </Panel> : null}
  </>
}

describe('editable modal focus', () => {
  it.each(['dialog', 'drawer'] as const)('keeps typing focused in a %s and restores focus only when closed', kind => {
    const onClosed = vi.fn()
    render(<EditableModal kind={kind} onClosed={onClosed} />)
    const trigger = screen.getByRole('button', { name: '打开编辑' })
    trigger.focus()
    fireEvent.click(trigger)
    expect(document.activeElement).toBe(screen.getByRole('dialog'))
    const name = screen.getByRole('textbox', { name: '步骤名称' })
    name.focus()
    for (const value of ['h', 'he', 'hello', '合同核对']) {
      fireEvent.change(name, { target: { value } })
      expect(document.activeElement).toBe(name)
    }
    const requirement = screen.getByRole('textbox', { name: '工作要求' })
    requirement.focus()
    for (const value of ['核', '核对材料\n保留依据']) {
      fireEvent.change(requirement, { target: { value } })
      expect(document.activeElement).toBe(requirement)
    }
    fireEvent.keyDown(requirement, { key: 'Escape' })
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(trigger)
    expect(onClosed).toHaveBeenCalledWith('合同核对\n核对材料\n保留依据')
  })
})
