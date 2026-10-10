import { Activity, Boxes, ChevronDown, ChevronRight, FolderGit2, Plug, Server, type LucideIcon } from 'lucide-react'
import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { AccountCard } from '../components/AccountCard'
import { listEnvironments } from '../lib/environments'
import { listNodes } from '../lib/nodes'
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
import { TeamDetailPage, teamTabs, type TeamTab } from './TeamDetailPage'
import { TeamsPage } from './TeamsPage'

interface RenderContext { session: AdminSession; path: string; navigate(path: string, options?: { force?: boolean }): void }

interface Section {
  path: string
  label: string
  icon: LucideIcon
  /** Other addresses that belong to this section. */
  owns?(path: string): boolean
  render(context: RenderContext): ReactNode
}

// Teams come first: everything else is about what a team did or needs.
const sections: Section[] = [
  { path: '/teams', label: '团队', icon: Boxes, owns: (path) => path === '/', render: ({ session, path, navigate }) => {
    if (!path.startsWith('/teams/')) return <TeamsPage session={session} navigate={navigate} />
    // /teams/<id>/<tab>: the tab is part of the address, the page is keyed by the team.
    const [id, suffix] = path.slice('/teams/'.length).split('/')
    const tab = teamTabs.some(([value]) => value === suffix) ? suffix as TeamTab : 'profile'
    return <TeamDetailPage key={id} teamId={decodeURIComponent(id)} tab={tab} navigate={navigate} />
  } },
  { path: '/runs', label: '运行', icon: Activity, owns: (path) => path.startsWith('/tasks/'), render: ({ path, navigate }) => path.startsWith('/tasks/')
    ? <TaskDetailPage key={path} runId={decodeURIComponent(path.slice('/tasks/'.length))} navigate={navigate} />
    : <TasksPage navigate={navigate} /> },
  { path: '/integrations', label: '集成', icon: Plug, render: ({ session }) => <IntegrationsPage session={session} /> },
]

// Only teams whose members run on nodes need these two.
const codeSections: Section[] = [
  { path: '/environments', label: '代码仓库', icon: FolderGit2, render: ({ session }) => <EnvironmentsPage session={session} /> },
  { path: '/nodes', label: '节点', icon: Server, render: ({ session }) => <NodesPage session={session} /> },
]

const codeGroupKey = 'weave-admin:code-group'
const matches = (section: Section, path: string) => path === section.path || path.startsWith(`${section.path}/`) || Boolean(section.owns?.(path))

export function Shell({ session, onSignOut }: { session: AdminSession; onSignOut(): void }) {
  const [path, navigate] = useRoute()
  // The group opens by itself once the workspace has a node or a repository;
  // after that it stays as the person left it.
  const [codeOpen, setCodeOpen] = useState(() => { try { return localStorage.getItem(codeGroupKey) === 'open' } catch { return false } })
  useEffect(() => {
    let stored: string | null = null
    try { stored = localStorage.getItem(codeGroupKey) } catch { /* per-viewer convenience only */ }
    if (stored) return
    void Promise.all([listNodes().catch(() => []), listEnvironments().catch(() => [])]).then(([nodes, environments]) => { if (nodes.length || environments.length) setCodeOpen(true) })
  }, [])
  const toggleCode = () => setCodeOpen((open) => {
    try { localStorage.setItem(codeGroupKey, open ? 'closed' : 'open') } catch { /* per-viewer convenience only */ }
    return !open
  })
  // Finished tasks raise a notice on any page; only the person's own tasks do.
  const track = useCallback(async () => {
    const recent = await listTasks('').catch(() => undefined)
    if (recent) trackTasks(recent.tasks.filter((task) => task.mine !== false), (runId) => navigate(`/tasks/${encodeURIComponent(runId)}`))
  }, [navigate])
  usePolling(track, 5000)
  const active = [...sections, ...codeSections].find((section) => matches(section, path)) ?? sections[0]
  const codeShown = codeOpen || codeSections.includes(active)
  const item = (section: Section) => <button key={section.path} type="button" className="nav__item" aria-current={section === active ? 'page' : undefined} onClick={() => navigate(section.path)}>
    <section.icon size={16} aria-hidden="true" />{section.label}
  </button>
  return <div className="shell">
    <nav className="nav" aria-label="主导航">
      <div className="nav__brand"><img src="/admin/favicon.svg" width={20} height={20} alt="" />Weave</div>
      {sections.map(item)}
      <button type="button" className="nav__group" aria-expanded={codeShown} onClick={toggleCode}>
        {codeShown ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}代码任务
      </button>
      {codeShown ? codeSections.map(item) : null}
      <div className="nav__footer">
        <AccountCard session={session} onSignOut={onSignOut} />
      </div>
    </nav>
    <div className="main">{active.render({ session, path, navigate })}</div>
  </div>
}
