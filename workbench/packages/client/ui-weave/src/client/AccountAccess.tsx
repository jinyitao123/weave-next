/** Account presentation; the Host remains the authority for browser access. */
import { useLayoutEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { createPortal } from 'react-dom'
import type { PropsLocale, PropsRuntime } from '@deepseek-ai/dsh-client-ui-slots'
import type {} from '@deepseek-ai/dsh-client-ui-sidebar/client'
import type { AccountProps } from './account-controller.ts'
import css from './AccountAccess.module.css'

type GateProps = PropsRuntime<'shell.access'> & PropsLocale<'weave'> & AccountProps
type ButtonProps = PropsRuntime<'sidebar.footer.action'> & PropsLocale<'weave'> & AccountProps

/** Render account access and report whether the shell may mount business views. */
export function AccountAccess({ useAccount, login, retry, onAccessChange, t }: GateProps) {
  const view = useAccount(value => value)
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [tenant, setTenant] = useState('')
  const user = view.status === 'authenticated' ? view.user : null
  const identity = user === null ? null : JSON.stringify([user.workspace_id, user.id])
  useLayoutEffect(() => {
    onAccessChange(identity)
    return () => { onAccessChange(null) }
  }, [identity, onAccessChange])

  if (user !== null) return null
  const pending = ['loading', 'changing', 'signing-out'].includes(view.status)
  const unavailable = view.status === 'error' || view.status === 'logout-failed'
  const submit = (event: FormEvent): void => {
    event.preventDefault()
    if (username.trim() === '' || password === '' || view.status === 'signing-in') return
    const input = { username, password, ...(tenant.trim() === '' ? {} : { tenant }) }
    setPassword('')
    void login(input)
  }
  return <main className={css.page}>
    <div className={css.card}>
      <div className={css.wordmark} aria-hidden="true"><span className={css.mark}>{t('account.mark')}</span><span>{t('account.brand')}</span></div>
      {pending ? <div className={css.pending} role="status">
        <span className={css.spinner} aria-hidden="true" />
        {t(view.status === 'signing-out' ? 'account.signingOut' : view.status === 'changing' ? 'account.changing' : 'account.loading')}
      </div> : <>
        <h1>{t('account.title')}</h1>
        <p className={css.description}>{t('account.description')}</p>
        {view.error !== null && <p className={css.error} role="alert">{t(`account.error.${view.error}`)}</p>}
        {unavailable ? <button className={css.primary} type="button" onClick={() => { void retry() }}>
          {t(view.status === 'logout-failed' ? 'account.retryLogout' : 'account.retry')}
        </button> : <form className={css.form} onSubmit={submit}>
          <label>{t('account.username')}
            <input name="username" autoComplete="username" autoCapitalize="none" spellCheck={false}
              value={username} required autoFocus disabled={view.status === 'signing-in'}
              placeholder={t('account.username.placeholder')} onChange={event => { setUsername(event.target.value) }} />
          </label>
          <label>{t('account.password')}
            <input name="password" type="password" autoComplete="current-password" value={password} required
              disabled={view.status === 'signing-in'} placeholder={t('account.password.placeholder')}
              onChange={event => { setPassword(event.target.value) }} />
          </label>
          <details className={css.workspace}>
            <summary>{t('account.workspace.options')}</summary>
            <label>{t('account.workspace')}
              <input name="tenant" autoComplete="off" value={tenant} disabled={view.status === 'signing-in'}
                placeholder={t('account.workspace.placeholder')} onChange={event => { setTenant(event.target.value) }} />
            </label>
            <small>{t('account.workspace.help')}</small>
          </details>
          <button className={css.primary} type="submit" disabled={view.status === 'signing-in' || username.trim() === '' || password === ''}>
            {t(view.status === 'signing-in' ? 'account.signingIn' : 'account.login')}
          </button>
        </form>}
      </>}
    </div>
  </main>
}

/** Sidebar account disclosure with a direct sign-out action. */
export function AccountButton({ useAccount, logout, wide, t }: ButtonProps) {
  const view = useAccount(value => value)
  const [open, setOpen] = useState(false)
  const [position, setPosition] = useState({ left: 12, bottom: 80 })
  const trigger = useRef<HTMLButtonElement>(null)
  const user = view.user
  if (user === null || view.status !== 'authenticated') return null
  const name = user.display_name || user.username
  const close = (): void => { setOpen(false); trigger.current?.focus() }
  return <div className={css.account} onKeyDown={event => { if (event.key === 'Escape') close() }}>
    <button ref={trigger} type="button" className={css.accountButton} aria-label={t('account.open', { name })}
      aria-expanded={open} aria-haspopup="dialog" onClick={() => {
        const rect = trigger.current!.getBoundingClientRect()
        setPosition({ left: Math.max(12, rect.left), bottom: Math.max(12, window.innerHeight - rect.top + 8) })
        setOpen(!open)
      }} title={name}>
      <span className={css.avatar} aria-hidden="true">{Array.from(name)[0]}</span>
      {wide && <span className={css.accountLabel}>{name}</span>}
    </button>
    {open && createPortal(<>
      <button type="button" className={css.dismiss} aria-label={t('account.close')} onClick={close} tabIndex={-1} />
      <section className={css.popover} style={position} role="dialog" aria-label={t('account.current')}>
        <div className={css.popoverHeading}><span>{t('account.current')}</span>
          <button type="button" autoFocus aria-label={t('account.close')} onClick={close}>×</button>
        </div>
        <strong>{name}</strong>
        <span className={css.username}>{user.username}</span>
        <span className={css.role}>{t(user.role === 'admin' ? 'account.role.admin' : 'account.role.user')}</span>
        <p>{t('account.logoutHelp')}</p>
        <button type="button" className={css.logout} onClick={() => { setOpen(false); void logout() }}>{t('account.logout')}</button>
      </section>
    </>, document.body)}
  </div>
}
