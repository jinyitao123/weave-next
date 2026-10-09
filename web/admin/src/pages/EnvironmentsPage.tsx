import { Plus } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { Drawer, EmptyState, InlineError, Select, Switch } from '../components/ui'
import { errorMessage, type AdminSession } from '../lib/api'
import { archiveEnvironment, environmentInput, listEnvironments, saveEnvironment, type Environment, type EnvironmentInput } from '../lib/environments'
import { listTeamRecords, type TeamRecord } from '../lib/teams'

const canEdit = (session: AdminSession) => ['developer', 'admin', 'owner'].includes(session.role)

const blank: EnvironmentInput = { name: '', repository_url: '', default_branch: 'main', setup_script: '', verify_commands: [], push_branches: false, default_team_id: '', git_username: '' }

export function EnvironmentsPage({ session }: { session: AdminSession }) {
  const [environments, setEnvironments] = useState<Environment[]>()
  const [teams, setTeams] = useState<TeamRecord[]>([])
  const [error, setError] = useState('')
  const [editing, setEditing] = useState<Environment | 'new'>()
  const refresh = useCallback(async () => {
    try {
      setEnvironments(await listEnvironments())
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [])
  useEffect(() => {
    void refresh()
    void listTeamRecords().then(setTeams).catch(() => setTeams([]))
  }, [refresh])
  const teamName = (id: string) => teams.find((team) => team.id === id)?.display_name || teams.find((team) => team.id === id)?.name || ''

  return <section className="page">
    <header className="page__header">
      <h1>环境</h1>
      {canEdit(session) ? <button type="button" className="button button--primary" onClick={() => setEditing('new')}><Plus size={15} />新建环境</button> : null}
    </header>
    {error ? <InlineError message={error} onRetry={() => void refresh()} /> : null}
    {!environments ? null : environments.length === 0 ? <EmptyState title="还没有环境" action={canEdit(session) ? <button type="button" className="button button--primary" onClick={() => setEditing('new')}><Plus size={15} />新建环境</button> : undefined}>
      环境记录团队要处理的代码仓库、默认分支和验证命令。
    </EmptyState> : <div className="task-list">{environments.map((environment) => <button key={environment.id} type="button" className="task-row" onClick={() => setEditing(environment)}>
      <span className="task-row__main">
        <strong>{environment.name}</strong>
        <span className="muted small mono">{environment.repository_url} · {environment.default_branch}</span>
      </span>
      <span className="muted small">{environment.verify_commands.length ? `${environment.verify_commands.length} 条验证命令` : '未声明验证命令'}{environment.default_team_id ? ` · ${teamName(environment.default_team_id)}` : ''}</span>
    </button>)}</div>}
    {editing ? <EnvironmentDrawer environment={editing === 'new' ? undefined : editing} teams={teams} editable={canEdit(session)} onClose={() => setEditing(undefined)} onSaved={() => { setEditing(undefined); void refresh() }} /> : null}
  </section>
}

function EnvironmentDrawer({ environment, teams, editable, onClose, onSaved }: { environment?: Environment; teams: TeamRecord[]; editable: boolean; onClose(): void; onSaved(): void }) {
  const [form, setForm] = useState<EnvironmentInput>(() => environment ? environmentInput(environment) : { ...blank })
  const [token, setToken] = useState('')
  const [clearToken, setClearToken] = useState(false)
  const hasToken = Boolean(environment?.git_credential) && !clearToken
  const [commands, setCommands] = useState(() => (environment?.verify_commands ?? []).join('\n'))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [confirmArchive, setConfirmArchive] = useState(false)
  const set = <K extends keyof EnvironmentInput>(key: K, value: EnvironmentInput[K]) => { setForm((current) => ({ ...current, [key]: value })); setError('') }
  const submit = async () => {
    if (!form.name.trim() || !form.repository_url.trim() || !form.default_branch.trim()) {
      setError('请填写名称、仓库地址和默认分支')
      return
    }
    setBusy(true)
    try {
      const git_token = token.trim() ? token.trim() : clearToken ? '' : undefined
      await saveEnvironment({ ...form, git_token, verify_commands: commands.split('\n').map((line) => line.trim()).filter(Boolean) }, environment?.id)
      onSaved()
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const archive = async () => {
    if (!environment) return
    setBusy(true)
    try {
      await archiveEnvironment(environment.id)
      onSaved()
    } catch (failure) {
      setError(errorMessage(failure))
      setBusy(false)
    }
  }
  return <Drawer title={environment ? environment.name : '新建环境'} onClose={onClose} footer={editable ? <div className="toolbar">
    <button type="button" className="button button--primary" disabled={busy} onClick={() => void submit()}>{busy ? '正在保存…' : '保存'}</button>
    {environment ? <button type="button" className="button button--danger" disabled={busy} onClick={() => setConfirmArchive(true)}>归档</button> : null}
  </div> : undefined}>
    <form className="stack" onSubmit={(event) => { event.preventDefault(); void submit() }}>
      <label className="field"><span>名称</span><input className="input" value={form.name} maxLength={80} disabled={!editable} onChange={(event) => set('name', event.target.value)} placeholder="支付服务" autoFocus /></label>
      <label className="field"><span>仓库地址</span><input className="input mono" value={form.repository_url} disabled={!editable} onChange={(event) => set('repository_url', event.target.value)} placeholder="git@github.com:team/payments.git" spellCheck={false} /></label>
      <label className="field"><span>默认分支</span><input className="input mono" value={form.default_branch} disabled={!editable} onChange={(event) => set('default_branch', event.target.value)} spellCheck={false} /></label>
      <label className="field"><span>启动脚本</span><textarea className="input textarea mono" rows={2} value={form.setup_script} disabled={!editable} onChange={(event) => set('setup_script', event.target.value)} placeholder="go mod download" spellCheck={false} /></label>
      <label className="field"><span>验证命令（每行一条）</span><textarea className="input textarea mono" rows={4} value={commands} disabled={!editable} onChange={(event) => { setCommands(event.target.value); setError('') }} placeholder={'go test ./...\ngo vet ./...'} spellCheck={false} /></label>
      <div className="field"><span>默认团队</span><Select label="默认团队" value={form.default_team_id} placeholder="不指定" disabled={!editable}
        options={[{ value: '', label: '不指定' }, ...teams.map((team) => ({ value: team.id, label: team.display_name || team.name }))]} onChange={(value) => set('default_team_id', value)} /></div>
      <Switch checked={form.push_branches} label="把每个任务的结果推送到 weave/ 开头的分支" onChange={(value) => editable && set('push_branches', value)} />
      <label className="field"><span>访问令牌</span><input className="input mono" type="password" autoComplete="new-password" value={token} disabled={!editable}
        onChange={(event) => { setToken(event.target.value); setError('') }} placeholder={hasToken ? '已保存，留空保持不变' : '不填则节点使用自己的 git 登录'} spellCheck={false} /></label>
      {token.trim() || hasToken ? <label className="field"><span>git 用户名</span><input className="input mono" value={form.git_username} maxLength={100} disabled={!editable}
        onChange={(event) => set('git_username', event.target.value)} placeholder="x-access-token" spellCheck={false} /></label> : null}
      {hasToken && editable ? <div className="toolbar">
        <button type="button" className="button" disabled={busy} onClick={() => { setClearToken(true); setToken(''); setError('') }}>清除令牌</button></div> : null}
      {clearToken && !token.trim() ? <p className="muted small">保存后清除令牌</p> : null}
      {confirmArchive ? <div className="confirm" role="alertdialog" aria-label="确认归档环境">
        <p>归档后不能再用它提交新任务，已提交的任务不受影响。</p>
        <div className="toolbar"><button type="button" className="button" onClick={() => setConfirmArchive(false)}>取消</button><button type="button" className="button button--danger" disabled={busy} onClick={() => void archive()}>归档</button></div>
      </div> : null}
      {error ? <InlineError message={error} /> : null}
    </form>
  </Drawer>
}
