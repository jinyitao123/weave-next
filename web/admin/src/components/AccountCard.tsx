import { LogOut } from 'lucide-react'
import type { AdminSession } from '../lib/api'
import './account-card.css'

const roleLabel = (role: string) => ({ admin: '管理员', developer: '开发者', member: '成员' }[role] ?? '成员')
function initials(name: string): string {
  const words = name.trim().split(/\s+/)
  if (words.length > 1) return `${Array.from(words[0])[0]}${Array.from(words.at(-1)!)[0]}`.toUpperCase()
  return /\p{Script=Han}/u.test(name) ? Array.from(name)[0] : Array.from(name).slice(0, 2).join('').toUpperCase()
}

export function AccountCard({ session, onSignOut }: { session: AdminSession; onSignOut(): void }) {
  const name = session.name.trim() || roleLabel(session.role)
  return <section className="account-card" aria-label="登录账户">
    <div className="account-card__identity">
      <span className="account-card__avatar" aria-hidden="true">{initials(name)}</span>
      <div className="account-card__name"><strong title={name}>{name}</strong><span>{roleLabel(session.role)}</span></div>
      <button type="button" className="icon-button account-card__logout" aria-label="退出登录" title="退出登录" onClick={onSignOut}><LogOut size={15} aria-hidden="true" /></button>
    </div>
  </section>
}
