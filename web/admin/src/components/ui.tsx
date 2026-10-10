import { Check, ChevronDown, Copy, X } from 'lucide-react'
import { useEffect, useId, useRef, useState, type ReactNode } from 'react'

export type Tone = 'neutral' | 'accent' | 'success' | 'warning' | 'danger' | 'claimed'

export function Badge({ tone = 'neutral', children, title }: { tone?: Tone; children: ReactNode; title?: string }) {
  return <span className={`badge badge--${tone}`} title={title}>{children}</span>
}

export function Dot({ on, label }: { on: boolean; label: string }) {
  return <span className="dot-label"><span className={`dot ${on ? 'dot--on' : ''}`} aria-hidden="true" />{label}</span>
}

// Focus moves into a modal panel on open and back on close; Escape closes it.
function useModalPanel(onClose: () => void) {
  const panel = useRef<HTMLElement>(null)
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null
    panel.current?.focus()
    const onKey = (event: KeyboardEvent) => { if (event.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('keydown', onKey)
      previous?.focus?.()
    }
  }, [onClose])
  return panel
}

// Centered panel for a short form or a confirmation; its buttons sit right
// under the content.
export function Dialog({ title, onClose, children, footer }: { title: string; onClose(): void; children: ReactNode; footer?: ReactNode }) {
  const panel = useModalPanel(onClose)
  const titleId = useId()
  return <div className="dialog-layer">
    <button type="button" className="drawer-scrim" aria-label="关闭" onClick={onClose} />
    <section ref={panel} className="dialog" role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1}>
      <header className="dialog__header"><h2 id={titleId}>{title}</h2><button type="button" className="icon-button" aria-label="关闭" onClick={onClose}><X size={16} /></button></header>
      <div className="dialog__body">{children}</div>
      {footer ? <footer className="dialog__footer">{footer}</footer> : null}
    </section>
  </div>
}

// Right-side panel for longer forms.
export function Drawer({ title, onClose, children, footer }: { title: string; onClose(): void; children: ReactNode; footer?: ReactNode }) {
  const panel = useModalPanel(onClose)
  const titleId = useId()
  return <div className="drawer-layer">
    <button type="button" className="drawer-scrim" aria-label="关闭" onClick={onClose} />
    <aside ref={panel} className="drawer" role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1}>
      <header className="drawer__header"><h2 id={titleId}>{title}</h2><button type="button" className="icon-button" aria-label="关闭" onClick={onClose}><X size={16} /></button></header>
      <div className="drawer__body">{children}</div>
      {footer ? <footer className="drawer__footer">{footer}</footer> : null}
    </aside>
  </div>
}

export function CopyField({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      setCopied(false)
    }
  }
  return <div className="copy-field">
    <span className="copy-field__label">{label}</span>
    <div className="copy-field__row">
      <code>{value}</code>
      <button type="button" className="icon-button" aria-label={`复制${label}`} onClick={() => void copy()}>{copied ? <Check size={14} /> : <Copy size={14} />}</button>
    </div>
  </div>
}

export function EmptyState({ title, children, action }: { title: string; children?: ReactNode; action?: ReactNode }) {
  return <div className="empty"><h2>{title}</h2>{children ? <p className="muted">{children}</p> : null}{action}</div>
}

export function InlineError({ message, onRetry }: { message: string; onRetry?(): void }) {
  return <p className="alert" role="alert">{message}{onRetry ? <> <button type="button" className="link-button" onClick={onRetry}>重试</button></> : null}</p>
}

export interface SelectOption<T extends string> { value: T; label: string; detail?: string }

// Product select: a button with a keyboard-operable listbox, used instead of
// the browser's native control.
export function Select<T extends string>({ value, options, onChange, label, placeholder = '请选择', disabled }: {
  value: T | ''
  options: Array<SelectOption<T>>
  onChange(value: T): void
  label: string
  placeholder?: string
  disabled?: boolean
}) {
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(0)
  const root = useRef<HTMLDivElement>(null)
  const listId = useId()
  const selected = options.find((option) => option.value === value)
  useEffect(() => {
    if (!open) return
    const close = (event: MouseEvent) => { if (!root.current?.contains(event.target as Node)) setOpen(false) }
    window.addEventListener('mousedown', close)
    return () => window.removeEventListener('mousedown', close)
  }, [open])
  const openList = () => {
    if (disabled || !options.length) return
    setActive(Math.max(0, options.findIndex((option) => option.value === value)))
    setOpen(true)
  }
  const choose = (index: number) => {
    const option = options[index]
    if (option) onChange(option.value)
    setOpen(false)
  }
  const onKeyDown = (event: React.KeyboardEvent) => {
    if (!open) {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(event.key)) { event.preventDefault(); openList() }
      return
    }
    if (event.key === 'Escape') { event.preventDefault(); setOpen(false) }
    else if (event.key === 'ArrowDown') { event.preventDefault(); setActive((index) => Math.min(options.length - 1, index + 1)) }
    else if (event.key === 'ArrowUp') { event.preventDefault(); setActive((index) => Math.max(0, index - 1)) }
    else if (event.key === 'Home') { event.preventDefault(); setActive(0) }
    else if (event.key === 'End') { event.preventDefault(); setActive(options.length - 1) }
    else if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); choose(active) }
    else if (event.key === 'Tab') setOpen(false)
  }
  return <div className="select" ref={root}>
    <button type="button" className="select__button" aria-label={label} aria-haspopup="listbox" aria-expanded={open} aria-controls={open ? listId : undefined}
      aria-activedescendant={open ? `${listId}-${active}` : undefined} disabled={disabled || !options.length} onClick={() => (open ? setOpen(false) : openList())} onKeyDown={onKeyDown}>
      <span className={selected ? '' : 'muted'}>{selected?.label ?? placeholder}</span><ChevronDown size={14} aria-hidden="true" />
    </button>
    {open ? <ul className="select__list" role="listbox" id={listId} aria-label={label}>
      {options.map((option, index) => <li key={option.value} id={`${listId}-${index}`} role="option" aria-selected={option.value === value}
        className={`select__option${index === active ? ' is-active' : ''}`} onMouseEnter={() => setActive(index)} onMouseDown={(event) => { event.preventDefault(); choose(index) }}>
        <span>{option.label}</span>{option.detail ? <span className="muted small">{option.detail}</span> : null}
      </li>)}
    </ul> : null}
  </div>
}

export function Switch({ checked, label, onChange, disabled }: { checked: boolean; label: string; onChange(value: boolean): void; disabled?: boolean }) {
  return <button type="button" role="switch" aria-checked={checked} disabled={disabled} className={`switch${checked ? ' is-on' : ''}`} onClick={() => onChange(!checked)}>
    <span className="switch__track" aria-hidden="true"><span className="switch__thumb" /></span>
    <span>{label}</span>
  </button>
}
