import { Plus } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { Badge, Drawer, EmptyState, InlineError } from '../components/ui'
import { errorMessage, type AdminSession } from '../lib/api'
import { relativeTime } from '../lib/format'
import { createStarterTeam, listTeamRecords, type TeamRecord } from '../lib/teams'

const canDevelop = (session: AdminSession) => ['developer', 'admin', 'owner'].includes(session.role)

export function TeamsPage({ session, navigate }: { session: AdminSession; navigate(path: string): void }) {
  const [teams, setTeams] = useState<TeamRecord[]>()
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const refresh = useCallback(async () => {
    try {
      setTeams(await listTeamRecords())
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [])
  useEffect(() => { void refresh() }, [refresh])

  return <section className="page">
    <header className="page__header">
      <h1>团队</h1>
      {canDevelop(session) ? <button type="button" className="button button--primary" onClick={() => setCreating(true)}><Plus size={15} />新建团队</button> : null}
    </header>
    {error ? <InlineError message={error} onRetry={() => void refresh()} /> : null}
    {!teams ? null : teams.length === 0 ? <EmptyState title="还没有团队" action={canDevelop(session) ? <button type="button" className="button button--primary" onClick={() => setCreating(true)}><Plus size={15} />新建团队</button> : undefined}>
      新团队预置“编码”和“验证”两名成员，由不同引擎分别完成。
    </EmptyState> : <div className="task-list">{teams.map((team) => <button key={team.id} type="button" className="task-row" onClick={() => navigate(`/teams/${encodeURIComponent(team.id)}`)}>
      <span className="task-row__main"><strong>{team.display_name || team.name}</strong><span className="muted small">{team.objective || '未填写目标'} · 更新于 {relativeTime(team.updated_at)}</span></span>
      <Badge tone={team.status === 'active' && team.default_workflow_id ? 'success' : 'neutral'}>{team.status === 'active' && team.default_workflow_id ? '已发布' : '未发布'}</Badge>
    </button>)}</div>}
    {creating ? <CreateTeamDrawer onClose={() => setCreating(false)} onCreated={(id) => navigate(`/teams/${encodeURIComponent(id)}`)} /> : null}
  </section>
}

function CreateTeamDrawer({ onClose, onCreated }: { onClose(): void; onCreated(id: string): void }) {
  const [name, setName] = useState('')
  const [objective, setObjective] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const create = async () => {
    if (!name.trim() || !objective.trim()) {
      setError('请填写团队名称和目标')
      return
    }
    setBusy(true)
    setError('')
    try {
      onCreated(await createStarterTeam(name.trim(), objective.trim()))
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  return <Drawer title="新建团队" onClose={onClose}>
    <form className="stack" onSubmit={(event) => { event.preventDefault(); void create() }}>
      <label className="field"><span>名称</span><input className="input" value={name} maxLength={80} onChange={(event) => setName(event.target.value)} placeholder="支付服务开发" autoFocus /></label>
      <label className="field"><span>目标</span><textarea className="input textarea" rows={3} value={objective} maxLength={2000} onChange={(event) => setObjective(event.target.value)} placeholder="为支付服务实现需求，并在合入前通过测试" /></label>
      <div className="stack small muted"><span>预置成员：编码（Claude）、验证（Codex）。创建后可调整。</span></div>
      {error ? <InlineError message={error} /> : null}
      <button type="submit" className="button button--primary" disabled={busy}>{busy ? '正在创建…' : '创建团队'}</button>
    </form>
  </Drawer>
}
