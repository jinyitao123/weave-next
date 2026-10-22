import { Activity, Boxes, Plug, Settings, type LucideIcon } from 'lucide-react'
import { useCallback, useEffect, type ReactNode } from 'react'
import { AccountCard } from '../components/AccountCard'
import { trackTasks } from '../lib/notify'
import { usePolling } from '../lib/polling'
import { listTasks } from '../lib/tasks'
import type { AdminSession } from '../lib/api'
import { useRoute } from '../lib/router'
import { IntegrationsPage } from './IntegrationsPage'
import { DeploymentPage, legacyDeploymentPath } from './DeploymentPage'
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
  { path: '/deployment', label: '部署', icon: Settings, owns: (path) => Boolean(legacyDeploymentPath(path)), render: ({ session, path, navigate }) =>
    <DeploymentPage session={session} tab={(legacyDeploymentPath(path) ?? path).split('/')[2]} navigate={navigate} /> },
]

const matches = (section: Section, path: string) => path === section.path || path.startsWith(`${section.path}/`) || Boolean(section.owns?.(path))

export function Shell({ session, onSignOut }: { session: AdminSession; onSignOut(): void }) {
  const [path, navigate] = useRoute()
  const available = sections
  useEffect(() => {
    const redirected = legacyDeploymentPath(path)
    if (redirected) navigate(redirected, { replace: true })
  }, [path, navigate])
  // Finished tasks raise a notice on any page; only the person's own tasks do.
  const track = useCallback(async () => {
    const recent = await listTasks('').catch(() => undefined)
    if (recent) trackTasks(recent.tasks.filter((task) => task.mine !== false), (runId) => navigate(`/tasks/${encodeURIComponent(runId)}`))
  }, [navigate])
  usePolling(track, 5000)
  const active = available.find((section) => matches(section, path)) ?? sections[0]
  const item = (section: Section) => <button key={section.path} type="button" className="nav__item" aria-current={section === active ? 'page' : undefined} onClick={() => navigate(section.path)}>
    <section.icon size={16} aria-hidden="true" />{section.label}
  </button>
  return <div className="shell">
    <nav className="nav" aria-label="主导航">
      <div className="nav__brand"><img src="/admin/favicon.svg" width={20} height={20} alt="" />Weave</div>
      {available.map(item)}
      <div className="nav__footer">
        <AccountCard session={session} onSignOut={onSignOut} />
      </div>
    </nav>
    <div className="main">
      {session.default_key ? <p className="notice shell__warning" role="status">当前用默认密钥登录。默认密钥人人都知道，能访问这个地址的人都可以用它以管理员身份进入；请在部署配置中设置自己的登录密钥。</p> : null}
      {active.render({ session, path, navigate })}
    </div>
  </div>
}
