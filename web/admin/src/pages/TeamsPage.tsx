import { Plus } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { Badge, Dialog, EmptyState, InlineError } from '../components/ui'
import { errorMessage, type AdminSession } from '../lib/api'
import { relativeTime } from '../lib/format'
import { createStarterTeam, listTeamCards, prepareNewTeam, type TeamCard, type TeamKind } from '../lib/teams'

const canDevelop = (session: AdminSession) => ['developer', 'admin', 'owner'].includes(session.role)

export function TeamsPage({ session, navigate }: { session: AdminSession; navigate(path: string): void }) {
  const [teams, setTeams] = useState<TeamCard[]>()
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const refresh = useCallback(async () => {
    try {
      setTeams(await listTeamCards())
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
      团队由一位负责人和若干成员组成，按流程一步步完成任务。
    </EmptyState> : <div className="team-cards">{teams.map((team) => {
      const published = team.status === 'active' && Boolean(team.default_workflow_id)
      return <button key={team.id} type="button" className="team-card" onClick={() => navigate(`/teams/${encodeURIComponent(team.id)}`)}>
        <span className="team-card__head"><strong>{team.display_name || team.name}</strong><Badge tone={published ? 'success' : 'neutral'}>{published ? '已发布' : '未发布'}</Badge></span>
        <span className="team-card__goal">{team.objective || '未填写目标'}</span>
        <span className="team-card__meta muted small">{team.members.length ? <span>{team.members.length} 位成员：{team.members.join('、')}</span> : null}<span>更新于 {relativeTime(team.updated_at)}</span></span>
      </button>
    })}</div>}
    {creating ? <CreateTeamDrawer onClose={() => setCreating(false)} onCreated={(id) => navigate(`/teams/${encodeURIComponent(id)}`)} /> : null}
  </section>
}

function CreateTeamDrawer({ onClose, onCreated }: { onClose(): void; onCreated(id: string): void }) {
  const [name, setName] = useState('')
  const [objective, setObjective] = useState('')
  const [kind, setKind] = useState<TeamKind>('business')
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
      const id = await createStarterTeam(name.trim(), objective.trim(), kind)
      // The starting flow is saved with the team; if that fails the page still
      // opens and offers to save it.
      await prepareNewTeam(id).catch(() => undefined)
      onCreated(id)
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const code = kind === 'code'
  return <Dialog title="新建团队" onClose={onClose} footer={<>
    <button type="button" className="button" onClick={onClose}>取消</button>
    <button type="button" className="button button--primary" disabled={busy} onClick={() => void create()}>{busy ? '正在创建…' : '创建团队'}</button>
  </>}>
    <form className="stack" onSubmit={(event) => { event.preventDefault(); void create() }}>
      <div className="field"><span>团队类型</span><div className="segmented" role="group" aria-label="团队类型">
        <button type="button" aria-pressed={!code} onClick={() => setKind('business')}>业务团队</button>
        <button type="button" aria-pressed={code} onClick={() => setKind('code')}>代码团队</button>
      </div></div>
      <label className="field"><span>名称</span><input className="input" value={name} maxLength={80} onChange={(event) => setName(event.target.value)} placeholder={code ? '支付服务开发' : '线索跟进团队'} autoFocus /></label>
      <label className="field"><span>目标</span><textarea className="input textarea" rows={3} value={objective} maxLength={2000} onChange={(event) => setObjective(event.target.value)} placeholder={code ? '为支付服务实现需求，并在合入前通过测试' : '整理销售线索材料，判断是否值得跟进'} /></label>
      <p className="small muted">{code ? '预置成员：负责人、编码（Claude）、验证（Codex），需要先接入节点。' : '预置成员：负责人和一位业务办理员，创建后可以改名、添加成员。'}</p>
      {error ? <InlineError message={error} /> : null}
    </form>
  </Dialog>
}
