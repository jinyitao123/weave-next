import { useId } from 'react'
import type { AdminSession } from '../lib/api'
import { DeploymentConnectionSection } from './DeploymentConnectionPage'
import { ModelsSection } from './ModelsPage'
import { NodesSection } from './NodesPage'
import { TimingSection } from './TimingPage'

// One page for what the deployment provides: how business systems connect,
// the nodes that run engines, and the models members can use. Each part keeps
// its own permission rule; the connection part is for administrators only.
export const deploymentTabs = [['connection', '连接'], ['nodes', '执行节点'], ['models', '模型'], ['timing', '时间上限']] as const
export type DeploymentTab = typeof deploymentTabs[number][0]

const administers = (session: AdminSession) => ['admin', 'owner'].includes(session.role)

export const visibleDeploymentTabs = (session: AdminSession) => deploymentTabs.filter(([value]) => value !== 'connection' || administers(session))

// Addresses of the pages this one replaced, so bookmarks keep working.
export function legacyDeploymentPath(path: string): string | undefined {
  if (path === '/deployment-connection' || path.startsWith('/deployment-connection/')) return '/deployment/connection'
  if (path === '/nodes' || path.startsWith('/nodes/')) return '/deployment/nodes'
  if (path === '/models' || path.startsWith('/models/')) return '/deployment/models'
  return undefined
}

export function DeploymentPage({ session, tab, navigate }: { session: AdminSession; tab?: string; navigate(path: string, options?: { force?: boolean }): void }) {
  const tabs = visibleDeploymentTabs(session)
  const active = tabs.find(([value]) => value === tab)?.[0] ?? tabs[0][0]
  const id = useId()
  return <section className="page">
    <header className="page__header"><h1>部署</h1></header>
    <div className="tabs" role="tablist" aria-label="部署">
      {tabs.map(([value, label]) => <button key={value} id={`${id}-${value}`} type="button" role="tab" aria-selected={active === value} aria-controls={`${id}-panel`}
        onClick={() => navigate(`/deployment/${value}`)}>{label}</button>)}
    </div>
    <div id={`${id}-panel`} role="tabpanel" aria-labelledby={`${id}-${active}`}>
      {active === 'connection' ? <DeploymentConnectionSection session={session} />
        : active === 'nodes' ? <NodesSection session={session} />
          : active === 'timing' ? <TimingSection />
            : <ModelsSection session={session} />}
    </div>
  </section>
}
