import { Plus, Upload } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { Badge, Dialog, EmptyState, InlineError } from '../components/ui'
import { errorMessage, type AdminSession } from '../lib/api'
import { relativeTime } from '../lib/format'
import { createUUID } from '../lib/ids'
import { createTeam, importTeam, listTeamCards, readTeamPackage, type TeamCard, type TeamImported, type TeamKind, type TeamPackage } from '../lib/teams'

const canDevelop = (session: AdminSession) => ['developer', 'admin', 'owner'].includes(session.role)

export function TeamsPage({ session, navigate }: { session: AdminSession; navigate(path: string): void }) {
  const [teams, setTeams] = useState<TeamCard[]>()
  const [error, setError] = useState('')
  const [creating, setCreating] = useState(false)
  const [importing, setImporting] = useState(false)
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
      {canDevelop(session) ? <div className="page__actions">
        <button type="button" className="button" onClick={() => setImporting(true)}><Upload size={15} />导入团队</button>
        <button type="button" className="button button--primary" onClick={() => setCreating(true)}><Plus size={15} />新建团队</button>
      </div> : null}
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
    {importing ? <ImportTeamDialog onClose={() => setImporting(false)} onOpen={(id) => navigate(`/teams/${encodeURIComponent(id)}`)} /> : null}
    {creating ? <CreateTeamDrawer onClose={() => setCreating(false)} onCreated={(id) => navigate(`/teams/${encodeURIComponent(id)}`)} /> : null}
  </section>
}

function CreateTeamDrawer({ onClose, onCreated }: { onClose(): void; onCreated(id: string): void }) {
  const [name, setName] = useState('')
  const [objective, setObjective] = useState('')
  const [kind, setKind] = useState<TeamKind>('business')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // One request id for this dialog: pressing create again after a lost answer
  // returns the team the first attempt made instead of making another.
  const request = useRef({ id: createUUID(), content: '' })
  const create = async () => {
    if (!name.trim() || !objective.trim()) {
      setError('请填写团队名称和目标')
      return
    }
    setBusy(true)
    setError('')
    try {
      const content = `${kind}\n${name.trim()}\n${objective.trim()}`
      if (request.current.content !== content) request.current = { id: createUUID(), content }
      onCreated(await createTeam(request.current.id, name.trim(), objective.trim(), kind))
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

function ImportTeamDialog({ onClose, onOpen }: { onClose(): void; onOpen(id: string): void }) {
  const [file, setFile] = useState<{ name: string; pkg: TeamPackage }>()
  const [imported, setImported] = useState<TeamImported>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const picker = useRef<HTMLInputElement>(null)
  // One request id per chosen file: importing again after a lost answer
  // returns the team the first attempt made.
  const request = useRef(createUUID())
  const choose = async (chosen: File | undefined) => {
    if (!chosen) return
    setError('')
    setFile(undefined)
    let text: string
    try {
      text = await readText(chosen)
    } catch {
      setError('无法读取这个文件')
      return
    }
    try {
      setFile({ name: chosen.name, pkg: readTeamPackage(text) })
      request.current = createUUID()
    } catch (failure) {
      setError((failure as Error).message)
    }
  }
  const submit = async () => {
    if (!file) return
    setBusy(true)
    setError('')
    try {
      const team = await importTeam(request.current, file.pkg)
      if (team.adjustments?.length) setImported(team)
      else onOpen(team.team_id)
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  if (imported) return <Dialog title="已导入团队" onClose={onClose} footer={<>
    <button type="button" className="button" onClick={onClose}>关闭</button>
    <button type="button" className="button button--primary" onClick={() => onOpen(imported.team_id)}>打开团队</button>
  </>}>
    <div className="stack">
      <p>“{imported.name}”已创建为未发布的草稿。以下内容在本工作区没有对应项，已调整：</p>
      <ul className="readiness">{imported.adjustments!.map((line) => <li key={line}>{line}</li>)}</ul>
    </div>
  </Dialog>
  return <Dialog title="导入团队" onClose={onClose} footer={<>
    <button type="button" className="button" onClick={onClose}>取消</button>
    <button type="button" className="button button--primary" disabled={busy || !file} onClick={() => void submit()}>{busy ? '正在导入…' : '导入'}</button>
  </>}>
    <div className="stack">
      <input ref={picker} type="file" accept=".json,application/json" hidden aria-hidden="true" tabIndex={-1} onChange={(event) => { void choose(event.target.files?.[0]); event.target.value = '' }} />
      <div className="field"><span>团队导出文件</span><div className="file-choice">
        <button type="button" className="button" onClick={() => picker.current?.click()}><Upload size={15} />{file ? '重新选择' : '选择文件'}</button>
        {file ? <span className="small">{file.pkg.team.name}<span className="muted">（{file.name}）</span></span> : null}
      </div></div>
      <p className="small muted">导入后创建一个新的未发布团队，不会覆盖已有团队。</p>
      {error ? <InlineError message={error} /> : null}
    </div>
  </Dialog>
}

const readText = (file: File) => new Promise<string>((resolve, reject) => {
  const reader = new FileReader()
  reader.onload = () => resolve(String(reader.result))
  reader.onerror = () => reject(reader.error)
  reader.readAsText(file)
})
