import { Boxes, FolderGit2, ListTodo, Lock, LockOpen, LogOut, Server, type LucideIcon } from 'lucide-react'
import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { CommandPalette } from '../components/CommandPalette'
import { trackTasks } from '../lib/notify'
import { usePolling } from '../lib/polling'
import { listTasks } from '../lib/tasks'
import type { AdminConfig, AdminSession } from '../lib/api'
import { useRoute } from '../lib/router'
import { EnvironmentsPage } from './EnvironmentsPage'
import { NodesPage } from './NodesPage'
import { TaskDetailPage } from './TaskDetailPage'
import { TasksPage } from './TasksPage'
import { TeamDetailPage } from './TeamDetailPage'
import { TeamsPage } from './TeamsPage'

interface RenderContext { session: AdminSession; path: string; navigate(path: string): void }

interface Section {
  path: string
  label: string
  icon: LucideIcon
  render(context: RenderContext): ReactNode
}

const sections: Section[] = [
  { path: '/', label: '任务', icon: ListTodo, render: ({ path, navigate }) => path.startsWith('/tasks/')
    ? <TaskDetailPage key={path} runId={decodeURIComponent(path.slice('/tasks/'.length))} navigate={navigate} />
    : <TasksPage navigate={navigate} /> },
  { path: '/environments', label: '环境', icon: FolderGit2, render: ({ session }) => <EnvironmentsPage session={session} /> },
  { path: '/teams', label: '团队', icon: Boxes, render: ({ session, path, navigate }) => path.startsWith('/teams/')
    ? <TeamDetailPage key={path} teamId={decodeURIComponent(path.slice('/teams/'.length))} navigate={navigate} />
    : <TeamsPage session={session} navigate={navigate} /> },
  { path: '/nodes', label: '节点', icon: Server, render: ({ session }) => <NodesPage session={session} /> },
]

const roleLabel = (role: string) => ({ admin: '管理员', developer: '开发者', member: '成员' }[role] ?? '成员')

export function Shell({ config, session, onSignOut }: { config: AdminConfig; session: AdminSession; onSignOut(): void }) {
  const [path, navigate] = useRoute()
  const [palette, setPalette] = useState(false)
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') { event.preventDefault(); setPalette((open) => !open) }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])
  // Finished tasks raise a notice on any page.
  const track = useCallback(async () => {
    const recent = await listTasks('').catch(() => undefined)
    if (recent) trackTasks(recent.tasks, (runId) => navigate(`/tasks/${encodeURIComponent(runId)}`))
  }, [navigate])
  usePolling(track, 5000)
  const active = sections.find((section) => section.path !== '/' && (path === section.path || path.startsWith(`${section.path}/`))) ?? sections[0]
  return <div className="shell">
    <nav className="nav" aria-label="主导航">
      <div className="nav__brand"><img src="/admin/favicon.svg" width={20} height={20} alt="" />Weave<button type="button" className="nav__palette" aria-label="快速跳转（Ctrl/⌘ + K）" title="快速跳转（Ctrl/⌘ + K）" onClick={() => setPalette(true)}>⌘K</button></div>
      {sections.map((section) => <button key={section.path} type="button" className="nav__item" aria-current={section === active ? 'page' : undefined} onClick={() => navigate(section.path)}>
        <section.icon size={16} aria-hidden="true" />{section.label}
      </button>)}
      <div className="nav__footer">
        <div className="nav__account">
          <span><strong>{session.name || roleLabel(session.role)}</strong><br /><span className="muted">{roleLabel(session.role)}</span></span>
          <button type="button" className="icon-button" aria-label="退出登录" title="退出登录" onClick={onSignOut}><LogOut size={15} /></button>
        </div>
        <span className="nav__connection">
          {config.secure ? <Lock size={11} aria-hidden="true" /> : <LockOpen size={11} aria-hidden="true" />}
          {config.secure ? 'HTTPS 连接' : 'HTTP 连接'}
        </span>
      </div>
    </nav>
    <div className="main">{active.render({ session, path, navigate })}</div>
    {palette ? <CommandPalette navigate={navigate} onClose={() => setPalette(false)} /> : null}
  </div>
}
