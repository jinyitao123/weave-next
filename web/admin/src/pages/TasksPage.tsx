import { createUUID } from '../lib/ids'
import { Bell, BellOff, Plus } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Badge, Dialog, EmptyState, InlineError, Select } from '../components/ui'
import { errorMessage } from '../lib/api'
import { relativeTime } from '../lib/format'
import { usePolling } from '../lib/polling'
import { listEnvironments, submitCodeTask, type Environment } from '../lib/environments'
import { executionLabel, executionTone, listTasks, listTeams, readMetrics, sourceLabel, taskSources, type TaskFilter, type TaskSource, type TaskSummary, type TeamOption } from '../lib/tasks'
import { disableNotifications, enableNotifications, notificationsEnabled } from '../lib/notify'
import { isCLIEngine, readDevelopment, type DevelopmentDocument } from '../lib/teams'

const filters: Array<{ value: TaskFilter; label: string }> = [
  { value: '', label: '全部' },
  { value: 'active', label: '进行中' },
  { value: 'attention', label: '需要处理' },
  { value: 'done', label: '已完成' },
]

const lastTeamKey = 'weave-admin:last-team'
const lastEnvironmentKey = 'weave-admin:last-environment'

function readLast(key: string): string {
  try { return localStorage.getItem(key) ?? '' } catch { return '' }
}

function writeLast(key: string, id: string) {
  try { localStorage.setItem(key, id) } catch { /* per-viewer convenience only */ }
}

// Every run of the workspace's teams the signed-in person may read, wherever
// it was started: the desktop, Feishu, a schedule or this console.
export function TasksPage({ navigate }: { navigate(path: string): void }) {
  const [filter, setFilter] = useState<TaskFilter>('')
  const [teamId, setTeamId] = useState('')
  const [source, setSource] = useState<TaskSource | ''>('')
  const [tasks, setTasks] = useState<TaskSummary[]>()
  const [error, setError] = useState('')
  const [teams, setTeams] = useState<TeamOption[]>()
  const [metric, setMetric] = useState<{ median: number; sample: number }>()
  const [notify, setNotify] = useState(notificationsEnabled)
  const [starting, setStarting] = useState(false)
  const refresh = useCallback(async () => {
    try {
      setTasks((await listTasks(filter, undefined, { team: teamId, source })).tasks)
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [filter, teamId, source])
  usePolling(refresh, 3000)
  useEffect(() => {
    void listTeams().then(setTeams).catch(() => setTeams([]))
    void readMetrics().then((body) => { if (body.first_output_median_seconds !== null) setMetric({ median: body.first_output_median_seconds, sample: body.sample }) }).catch(() => undefined)
  }, [])
  const narrowed = Boolean(filter || teamId || source)

  return <section className="page">
    <header className="page__header">
      <h1>运行</h1>
      <button type="button" className="button button--primary" onClick={() => setStarting(true)}><Plus size={15} />发起任务</button>
    </header>
    <div className="tasks-bar">
      <div className="segmented" role="group" aria-label="运行状态">
        {filters.map((item) => <button key={item.value} type="button" aria-pressed={filter === item.value} onClick={() => setFilter(item.value)}>{item.label}</button>)}
      </div>
      <Select label="团队" value={teamId} options={[{ value: '', label: '全部团队' }, ...(teams ?? []).map((team) => ({ value: team.id, label: team.display_name || team.name }))]} onChange={setTeamId} />
      <Select<TaskSource | ''> label="来源" value={source} options={[{ value: '', label: '全部来源' }, ...taskSources.map((item) => ({ value: item, label: sourceLabel(item) }))]} onChange={setSource} />
      {metric ? <span className="muted small">首个输出中位数 {Math.round(metric.median)} 秒（近 {metric.sample} 次）</span> : null}
      <button type="button" className="button tasks-bar__notify" onClick={() => { if (notify) { disableNotifications(); setNotify(false) } else void enableNotifications().then(setNotify) }}>{notify ? <><BellOff size={14} />关闭完成通知</> : <><Bell size={14} />开启完成通知</>}</button>
    </div>
    {error ? <InlineError message={error} onRetry={() => void refresh()} /> : null}
    {!tasks ? null : tasks.length === 0 ? (narrowed
      ? <EmptyState title="没有符合条件的运行" />
      : teams?.length === 0
        ? <EmptyState title="还没有运行记录" action={<button type="button" className="button button--primary" onClick={() => navigate('/teams')}>去建团队</button>}>先建一个团队并发布，它的每次运行都会出现在这里。</EmptyState>
        : <EmptyState title="还没有运行记录">团队发布后，员工从桌面或飞书发起的任务，以及在这里发起的任务，都会出现在这里。</EmptyState>)
      : <div className="task-list">{tasks.map((task) => <button key={task.run_id} type="button" className="task-row" onClick={() => navigate(`/tasks/${encodeURIComponent(task.run_id)}`)}>
        <span className="task-row__main">
          <strong>{task.title || task.team_name || '团队任务'}</strong>
          <span className="muted small">{[task.team_name || '团队', `第 ${task.workflow_version} 版`, sourceLabel(task.source), task.actor, relativeTime(task.created_at)].filter(Boolean).join(' · ')}</span>
        </span>
        <Badge tone={executionTone(task.status)}>{executionLabel(task.status, task.wait_kind)}</Badge>
      </button>)}</div>}
    {starting ? <StartDialog teams={teams} onClose={() => setStarting(false)} navigate={navigate} onSubmitted={(runId) => navigate(`/tasks/${encodeURIComponent(runId)}`)} /> : null}
  </section>
}

// Starting a task from the console follows the chosen team: a team that
// carries business actions needs the employee's own authorization and cannot
// be started here, and a code repository is offered only to teams that run on nodes.
function StartDialog({ teams, onClose, onSubmitted, navigate }: { teams?: TeamOption[]; onClose(): void; onSubmitted(runId: string): void; navigate(path: string): void }) {
  const [text, setText] = useState('')
  const [teamId, setTeamId] = useState(() => readLast(lastTeamKey))
  const [published, setPublished] = useState<DevelopmentDocument>()
  const [environments, setEnvironments] = useState<Environment[]>([])
  const [environmentId, setEnvironmentId] = useState(() => readLast(lastEnvironmentKey))
  const [branch, setBranch] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [requestId, setRequestId] = useState(() => createUUID())
  const team = teams?.find((item) => item.id === teamId) ?? teams?.[0]
  useEffect(() => { void listEnvironments().then(setEnvironments).catch(() => setEnvironments([])) }, [])
  useEffect(() => {
    setPublished(undefined)
    if (team) void readDevelopment(team.id).then((draft) => setPublished(draft.published_document ?? draft.document)).catch(() => setPublished(undefined))
  }, [team?.id])
  const members = useMemo(() => (published?.members ?? []).filter((member) => member.relationship.enabled !== false), [published])
  const business = members.some((member) => (member.configuration.business_capability_ids ?? []).length > 0)
  const usesNodes = members.some((member) => isCLIEngine(member.configuration.engine))
  const environment = usesNodes ? environments.find((item) => item.id === environmentId) : undefined

  const submit = async () => {
    if (!team) { setError('请先选择团队'); return }
    if (!text.trim()) { setError('请描述要做的事'); return }
    setBusy(true)
    setError('')
    try {
      // The same request id is reused until the submission succeeds, so a
      // retried click cannot create a second run.
      const result = await submitCodeTask({ teamId: team.id, environmentId: environment?.id, ref: environment ? (branch.trim() || environment.default_branch) : undefined, task: text.trim(), clientRequestId: requestId })
      writeLast(lastTeamKey, team.id)
      writeLast(lastEnvironmentKey, environment?.id ?? '')
      setRequestId(createUUID())
      onSubmitted(result.run_id)
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  return <Dialog title="发起任务" onClose={onClose} footer={business ? <>
    <button type="button" className="button" onClick={onClose}>关闭</button>
    <button type="button" className="button button--primary" onClick={() => team && navigate(`/teams/${encodeURIComponent(team.id)}/trial`)}>去试跑</button>
  </> : <>
    <button type="button" className="button" onClick={onClose}>取消</button>
    <button type="button" className="button button--primary" disabled={busy || !team} onClick={() => void submit()}>{busy ? '正在提交…' : '提交'}</button>
  </>}>
    <form className="stack" onSubmit={(event) => { event.preventDefault(); void submit() }}>
      <div className="field"><span>团队</span><Select label="团队" value={team?.id ?? ''} placeholder={teams && !teams.length ? '没有已发布的团队' : '选择团队'}
        options={(teams ?? []).map((item) => ({ value: item.id, label: item.display_name || item.name }))} onChange={(id) => { setTeamId(id); setError('') }} /></div>
      {business ? <p className="notice" role="status">这个团队会办理业务动作，需要员工本人的授权，不能从这里发起。要验证它，请到团队的“试跑”页；正式使用由员工从桌面或飞书发起。</p> : <>
        {usesNodes ? <div className="field"><span>代码仓库</span><Select label="代码仓库" value={environment?.id ?? ''} placeholder="不使用代码仓库"
          options={[{ value: '', label: '不使用代码仓库' }, ...environments.map((item) => ({ value: item.id, label: item.name, detail: item.repository_url }))]}
          onChange={(id) => { setEnvironmentId(id); setBranch(environments.find((item) => item.id === id)?.default_branch ?? '') }} /></div> : null}
        {environment ? <label className="field"><span>分支</span><input className="input mono" value={branch || environment.default_branch} onChange={(event) => setBranch(event.target.value)} spellCheck={false} /></label> : null}
        <label className="field"><span>任务内容</span><textarea className="input textarea" rows={5} value={text} placeholder="描述要做的事" onChange={(event) => { setText(event.target.value); setError('') }} autoFocus /></label>
      </>}
      {error ? <InlineError message={error} /> : null}
    </form>
  </Dialog>
}
