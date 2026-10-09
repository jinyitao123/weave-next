import { createUUID } from '../lib/ids'
import { ArrowUp } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState, type KeyboardEvent } from 'react'
import { Badge, EmptyState, InlineError, Select } from '../components/ui'
import { errorMessage } from '../lib/api'
import { relativeTime } from '../lib/format'
import { listNodes, type RuntimeNode } from '../lib/nodes'
import { usePolling } from '../lib/polling'
import { listEnvironments, submitCodeTask, type Environment } from '../lib/environments'
import { executionLabel, executionTone, listTasks, listTeams, readMetrics, type TaskFilter, type TaskSummary, type TeamOption } from '../lib/tasks'
import { disableNotifications, enableNotifications, notificationsEnabled } from '../lib/notify'
import { Bell, BellOff } from 'lucide-react'

const filters: Array<{ value: TaskFilter; label: string }> = [
  { value: 'active', label: '进行中' },
  { value: 'attention', label: '需要我看' },
  { value: 'done', label: '已完成' },
  { value: '', label: '全部' },
]

const lastTeamKey = 'weave-admin:last-team'
const lastEnvironmentKey = 'weave-admin:last-environment'

function readLastEnvironment(): string {
  try { return localStorage.getItem(lastEnvironmentKey) ?? '' } catch { return '' }
}

function writeLastEnvironment(id: string) {
  try { localStorage.setItem(lastEnvironmentKey, id) } catch { /* per-viewer convenience only */ }
}

function readLastTeam(): string {
  try { return localStorage.getItem(lastTeamKey) ?? '' } catch { return '' }
}

function writeLastTeam(id: string) {
  try { localStorage.setItem(lastTeamKey, id) } catch { /* per-viewer convenience only */ }
}

export function TasksPage({ navigate }: { navigate(path: string): void }) {
  const [filter, setFilter] = useState<TaskFilter>('active')
  const [tasks, setTasks] = useState<TaskSummary[]>()
  const [error, setError] = useState('')
  const [teams, setTeams] = useState<TeamOption[]>()
  const [nodes, setNodes] = useState<RuntimeNode[]>()
  const [environments, setEnvironments] = useState<Environment[]>()
  const [metric, setMetric] = useState<{ median: number; sample: number }>()
  const [notify, setNotify] = useState(notificationsEnabled)
  const refresh = useCallback(async () => {
    try {
      setTasks((await listTasks(filter)).tasks)
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [filter])
  usePolling(refresh, 3000)
  useEffect(() => {
    void listTeams().then(setTeams).catch(() => setTeams([]))
    void listNodes().then(setNodes).catch(() => setNodes([]))
    void listEnvironments().then(setEnvironments).catch(() => setEnvironments([]))
    void readMetrics().then((body) => { if (body.first_output_median_seconds !== null) setMetric({ median: body.first_output_median_seconds, sample: body.sample }) }).catch(() => undefined)
  }, [])

  const firstRun = tasks !== undefined && tasks.length === 0 && filter === ''
  return <section className="page">
    <Composer teams={teams} nodes={nodes} environments={environments} onSubmitted={(runId) => navigate(`/tasks/${encodeURIComponent(runId)}`)} navigate={navigate} />
    <div className="tasks-bar">
    <div className="segmented" role="group" aria-label="任务筛选">
      {filters.map((item) => <button key={item.value} type="button" aria-pressed={filter === item.value} onClick={() => setFilter(item.value)}>{item.label}</button>)}
    </div>
    {metric ? <span className="muted small">首个输出中位数 {Math.round(metric.median)} 秒（近 {metric.sample} 个任务）</span> : null}
    <button type="button" className="button tasks-bar__notify" onClick={() => { if (notify) { disableNotifications(); setNotify(false) } else void enableNotifications().then(setNotify) }}>{notify ? <><BellOff size={14} />关闭完成通知</> : <><Bell size={14} />开启完成通知</>}</button>
    </div>
    {error ? <InlineError message={error} onRetry={() => void refresh()} /> : null}
    {!tasks ? null : tasks.length === 0 ? (firstRun || (teams?.length === 0) ? <Onboarding teams={teams} nodes={nodes} environments={environments} navigate={navigate} /> : <EmptyState title={filter === 'attention' ? '没有需要处理的任务' : filter === 'active' ? '没有进行中的任务' : '没有任务'} />)
      : <div className="task-list">{tasks.map((task) => <button key={task.run_id} type="button" className="task-row" onClick={() => navigate(`/tasks/${encodeURIComponent(task.run_id)}`)}>
        <span className="task-row__main">
          <strong>{task.title || task.team_name || '团队任务'}</strong>
          <span className="muted small">{task.team_name || '团队'} · 第 {task.workflow_version} 版 · {relativeTime(task.created_at)}</span>
        </span>
        <Badge tone={executionTone(task.status)}>{executionLabel(task.status, task.wait_kind)}</Badge>
      </button>)}</div>}
  </section>
}

function Composer({ teams, nodes, environments, onSubmitted, navigate }: { teams?: TeamOption[]; nodes?: RuntimeNode[]; environments?: Environment[]; onSubmitted(runId: string): void; navigate(path: string): void }) {
  const [text, setText] = useState('')
  const [teamId, setTeamId] = useState(readLastTeam)
  const [environmentId, setEnvironmentId] = useState(readLastEnvironment)
  const [branch, setBranch] = useState('')
  const environment = environments?.find((item) => item.id === environmentId)
  const chooseEnvironment = (id: string) => {
    setEnvironmentId(id)
    const chosen = environments?.find((item) => item.id === id)
    setBranch(chosen?.default_branch ?? '')
    if (chosen?.default_team_id) setTeamId(chosen.default_team_id)
  }
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [requestId, setRequestId] = useState(() => createUUID())
  const team = teams?.find((item) => item.id === teamId) ?? teams?.[0]
  const accepting = useMemo(() => nodes?.filter((node) => node.accepting).length ?? 0, [nodes])

  const submit = async () => {
    if (!team) {
      setError('请先选择团队')
      return
    }
    if (!text.trim()) {
      setError('请描述要做的事')
      return
    }
    setBusy(true)
    setError('')
    try {
      // The same request id is reused until the submission succeeds, so a
      // retried click cannot create a second run.
      const result = await submitCodeTask({ teamId: team.id, environmentId: environment?.id, ref: environment ? (branch.trim() || environment.default_branch) : undefined, task: text.trim(), clientRequestId: requestId })
      writeLastTeam(team.id)
      writeLastEnvironment(environment?.id ?? '')
      setText('')
      setRequestId(createUUID())
      onSubmitted(result.run_id)
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
      event.preventDefault()
      void submit()
    }
  }
  return <form className="composer" onSubmit={(event) => { event.preventDefault(); void submit() }}>
    <label className="sr-only" htmlFor="task-text">任务内容</label>
    <textarea id="task-text" className="composer__input" rows={3} placeholder="描述要做的事" value={text} onChange={(event) => { setText(event.target.value); setError('') }} onKeyDown={onKeyDown} />
    <div className="composer__bar">
      <Select label="环境" value={environment?.id ?? ''} placeholder="不使用代码仓库"
        options={[{ value: '', label: '不使用代码仓库' }, ...(environments ?? []).map((item) => ({ value: item.id, label: item.name, detail: item.repository_url }))]} onChange={chooseEnvironment} />
      {environment ? <label className="composer__branch"><span className="sr-only">分支</span><input className="input mono" value={branch || environment.default_branch} onChange={(event) => setBranch(event.target.value)} aria-label="分支" spellCheck={false} /></label> : null}
      <Select label="团队" value={team?.id ?? ''} placeholder={teams && !teams.length ? '没有可用团队' : '选择团队'}
        options={(teams ?? []).map((item) => ({ value: item.id, label: item.display_name || item.name }))} onChange={setTeamId} />
      <span className="composer__hint muted small">{nodes && accepting === 0 ? <button type="button" className="link-button" onClick={() => navigate('/nodes')}>当前没有可接任务的节点</button> : 'Ctrl/⌘ + Enter 提交'}</span>
      <button type="submit" className="button button--primary" disabled={busy} aria-label="提交任务"><ArrowUp size={15} />{busy ? '正在提交…' : '提交'}</button>
    </div>
    {error ? <InlineError message={error} /> : null}
  </form>
}

function Onboarding({ teams, nodes, environments, navigate }: { teams?: TeamOption[]; nodes?: RuntimeNode[]; environments?: Environment[]; navigate(path: string): void }) {
  const steps = [
    { done: Boolean(nodes?.some((node) => node.accepting)), label: '接入一个可接任务的节点', path: '/nodes' },
    { done: Boolean(environments?.length), label: '建一个环境（仓库与验证命令）', path: '/environments' },
    { done: Boolean(teams?.length), label: '配置并发布一个团队', path: '/teams' },
    { done: false, label: '提交第一个任务', path: '/' },
  ]
  return <ol className="checklist">{steps.map((step) => <li key={step.label} className={step.done ? 'is-done' : ''}>
    <span className="checklist__mark" aria-hidden="true">{step.done ? '✓' : ''}</span>
    {step.path === '/' ? <span>{step.label}</span> : <button type="button" className="link-button" onClick={() => navigate(step.path)}>{step.label}</button>}
  </li>)}</ol>
}
