import { Boxes, FolderGit2, ListTodo, Plug, Server, type LucideIcon } from 'lucide-react'
import { useCallback, type ReactNode } from 'react'
import { AccountCard } from '../components/AccountCard'
import { trackTasks } from '../lib/notify'
import { usePolling } from '../lib/polling'
import { listTasks } from '../lib/tasks'
import type { AdminSession } from '../lib/api'
import { useRoute } from '../lib/router'
import { EnvironmentsPage } from './EnvironmentsPage'
import { IntegrationsPage } from './IntegrationsPage'
import { NodesPage } from './NodesPage'
import { TaskDetailPage } from './TaskDetailPage'
import { TasksPage } from './TasksPage'
import { TeamDetailPage } from './TeamDetailPage'
import { TeamsPage } from './TeamsPage'

interface RenderContext { session: AdminSession; path: string; navigate(path: string, options?: { force?: boolean }): void }

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
    ? <TeamDetailPage key={path} teamId={decodeURIComponent(path.slice('/teams/'.length).replace(/\/(trial|workflow)$/, ''))} initialTab={path.endsWith('/workflow') ? 'workflow' : path.endsWith('/trial') ? 'trial' : 'profile'} navigate={navigate} />
    : <TeamsPage session={session} navigate={navigate} /> },
  { path: '/nodes', label: '节点', icon: Server, render: ({ session }) => <NodesPage session={session} /> },
  { path: '/integrations', label: '集成', icon: Plug, render: ({ session }) => <IntegrationsPage session={session} /> },
]

export function Shell({ session, onSignOut }: { session: AdminSession; onSignOut(): void }) {
  const [path, navigate] = useRoute()
  // Finished tasks raise a notice on any page.
  const track = useCallback(async () => {
    const recent = await listTasks('').catch(() => undefined)
    if (recent) trackTasks(recent.tasks, (runId) => navigate(`/tasks/${encodeURIComponent(runId)}`))
  }, [navigate])
  usePolling(track, 5000)
  const active = sections.find((section) => section.path !== '/' && (path === section.path || path.startsWith(`${section.path}/`))) ?? sections[0]
  return <div className="shell">
    <nav className="nav" aria-label="主导航">
      <div className="nav__brand"><img src="/admin/favicon.svg" width={20} height={20} alt="" />Weave</div>
      {sections.map((section) => <button key={section.path} type="button" className="nav__item" aria-current={section === active ? 'page' : undefined} onClick={() => navigate(section.path)}>
        <section.icon size={16} aria-hidden="true" />{section.label}
      </button>)}
      <div className="nav__footer">
        <AccountCard session={session} onSignOut={onSignOut} />
      </div>
    </nav>
    <div className="main">{active.render({ session, path, navigate })}</div>
  </div>
}
