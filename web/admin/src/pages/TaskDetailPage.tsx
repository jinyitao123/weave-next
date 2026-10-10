import { createUUID } from '../lib/ids'
import { ArrowLeft, CircleCheck, Download, CircleDashed, CircleX, LoaderCircle, Square } from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Badge, InlineError } from '../components/ui'
import { ResultContent } from '../components/ResultContent'
import { ApiError, errorMessage } from '../lib/api'
import { checkReason, checkTitle, engineName, relativeTime } from '../lib/format'
import { usePolling } from '../lib/polling'
import { readTaskCode, shortSHA, verdictLabel, type TaskCode } from '../lib/environments'
import { CodeChanges, CodeEvidencePanel, CodeSummary, CodeVerdict } from './TaskCodePanels'
import { FollowUpComposer, TaskThread } from './TaskThread'
import { LogViewer } from './LogViewer'
import {
  duration, executionLabel, failureText, readQueue, subscribeTask, waitText, type TaskWait, executionTone, readActivity, readTask, retryStage, sourceLabel, stageLabel, stopTask, terminal, verificationLabel,
  type Activity, type ActivityMember, type MemberStage, type TaskSummary,
} from '../lib/tasks'

interface FlatStage { member: ActivityMember; stage: MemberStage; key: string }

function flatten(activity?: Activity): FlatStage[] {
  const stages = (activity?.members ?? []).flatMap((member) => (member.stages ?? []).map((stage) => ({ member, stage, key: `${member.agent_id}:${stage.node_id}` })))
  return stages.sort((left, right) => {
    const a = left.stage.started_at ? Date.parse(left.stage.started_at) : Number.MAX_SAFE_INTEGER
    const b = right.stage.started_at ? Date.parse(right.stage.started_at) : Number.MAX_SAFE_INTEGER
    return a - b
  })
}

function StageIcon({ status }: { status: string }) {
  if (status === 'completed' || status === 'succeeded') return <CircleCheck size={15} className="icon--success" aria-hidden="true" />
  if (status === 'failed') return <CircleX size={15} className="icon--danger" aria-hidden="true" />
  if (status === 'running') return <LoaderCircle size={15} className="spin" aria-hidden="true" />
  return <CircleDashed size={15} className="muted" aria-hidden="true" />
}

export function TaskDetailPage({ runId, navigate }: { runId: string; navigate(path: string): void }) {
  const [task, setTask] = useState<TaskSummary>()
  const [activity, setActivity] = useState<Activity>()
  const [error, setError] = useState('')
  const [selected, setSelected] = useState<string>()
  const [tab, setTab] = useState<'output' | 'changes' | 'evidence' | 'details'>('output')
  const [loadedCode, setCode] = useState<TaskCode | null>()
  const [busy, setBusy] = useState(false)
  const [stopKey] = useState(() => createUUID())
  const [missing, setMissing] = useState(false)
  const [waiting, setWaiting] = useState(false)
  const [openedAt] = useState(() => Date.now())
  const [version, setVersion] = useState(0)
  const [streamBroken, setStreamBroken] = useState(false)
  const [logStream, setLogStream] = useState<string>()
  const [wait, setWait] = useState<TaskWait>()
  const done = activity ? terminal(activity.status) : false
  const refresh = useCallback(async () => {
    try {
      // Activity is authoritative; the summary only adds the readable title
      // and may be absent for development trials.
      const [summary, current] = await Promise.all([readTask(runId).catch(() => task), readActivity(runId)])
      if (summary) setTask(summary)
      setActivity(current)
      setVersion((value) => value + 1)
      setWait(terminal(current.status) ? undefined : await readQueue(runId).catch(() => undefined))
      if (summary && loadedCode !== null) {
        // Code context is fixed at submission: a task without one stays plain.
        setCode(await readTaskCode(runId).catch((failure) => failure instanceof ApiError && failure.status === 404 ? null : loadedCode))
      }
      setError('')
      setWaiting(false)
    } catch (failure) {
      // A just-submitted run is established by its consumer a moment later;
      // keep waiting briefly before reporting it as missing.
      if (failure instanceof ApiError && failure.status === 404 && !activity && Date.now() - openedAt < 30000) {
        setWaiting(true)
        return
      }
      setWaiting(false)
      setError(errorMessage(failure))
      if (failure instanceof ApiError && failure.status === 404) setMissing(true)
    }
  }, [runId, task, activity, openedAt, loadedCode])
  // The change stream drives refreshes; polling remains as a slow backup and
  // takes over when the stream is unavailable.
  usePolling(refresh, streamBroken ? 2000 : 10000, !done && !missing)
  const refreshRef = useRef(refresh)
  refreshRef.current = refresh
  useEffect(() => {
    if (done || missing) return
    return subscribeTask(runId, () => void refreshRef.current(), () => setStreamBroken(true))
  }, [runId, done, missing])

  const stages = useMemo(() => flatten(activity), [activity])
  const byNode = useMemo(() => new Map(stages.map((item) => [item.stage.node_id, item])), [stages])
  const current = stages.find((item) => item.key === selected) ?? stages.find((item) => item.stage.status === 'running') ?? stages.at(-1)
  // Activity and code are separate reads; until the activity itself shows a
  // finished run, the verdict stays pending so the page never contradicts itself.
  const code = loadedCode && activity && !terminal(activity.status) ? { ...loadedCode, verdict: { status: 'pending' as const } } : loadedCode
  const verification = code ? verdictLabel(code.verdict) : verificationLabel(activity?.delivery, activity?.status ?? '')
  const stageHead = (nodeId: string) => code?.stages.find((stage) => stage.node_id === nodeId)?.version.head_sha
  const stagePasses = (nodeId: string) => code?.stages.find((stage) => stage.node_id === nodeId)?.passes
  const trial = activity?.development_trial === true
  const backPath = trial ? activity.team_id ? `/teams/${encodeURIComponent(activity.team_id)}/trial` : '/teams' : '/runs'
  const backLabel = trial ? '返回团队试跑' : '返回运行列表'
  // A run someone else started is read here, never acted on.
  const readOnly = task?.mine === false

  const act = async (action: () => Promise<unknown>) => {
    setBusy(true)
    try {
      await action()
      await refresh()
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }

  return <section className="page page--wide">
    <header className="task-header">
      <button type="button" className="icon-button" aria-label={backLabel} disabled={!activity && !task} onClick={() => navigate(backPath)}><ArrowLeft size={16} /></button>
      <div className="task-header__title">
        <h1>{task?.title || task?.team_name || '试跑结果'}</h1>
        <p className="muted small">{task ? [task.team_name || '团队', `第 ${task.workflow_version} 版`, sourceLabel(task.source), task.actor, relativeTime(task.created_at)].filter(Boolean).join(' · ') : ''}{code ? <> · <CodeSummary code={code} /></> : null}</p>
      </div>
      {activity ? <div className="task-header__state">
        <Badge tone={executionTone(activity.status)}>{executionLabel(activity.status, activity.wait_kind)}</Badge>
        <Badge tone={verification.tone}>{verification.label}</Badge>
        {done && task ? <a className="button" href={`/v1/admin/tasks/${encodeURIComponent(runId)}/evidence`} download><Download size={13} />导出证据</a> : null}
        {!done && !readOnly ? <button type="button" className="button" disabled={busy || activity.status === 'cancel_requested'} onClick={() => void act(() => stopTask(runId, stopKey))}><Square size={13} />{activity.status === 'cancel_requested' ? '正在停止' : '停止'}</button> : null}
      </div> : null}
    </header>
    {activity?.stop_unconfirmed ? <p className="notice">已请求停止，节点尚未确认。</p> : null}
    {activity?.status === 'failed' && failureText(task?.failure_reason) ? <p className="alert" role="alert">{failureText(task?.failure_reason)}</p> : null}
    {error ? <InlineError message={error} onRetry={() => void refresh()} /> : null}
    {waiting ? <p className="muted" role="status"><span className="spinner" aria-hidden="true" />等待团队接手…</p> : null}
    {wait?.waiting ? <p className="notice" role="status">{waitText(wait)}</p> : null}
    {!activity ? null : <div className="task-layout">
      <aside className="task-chain" aria-label="交接链">
        <h2 className="section-title">交接链 <span className="muted small">{activity.completed_stages}/{activity.total_stages}</span></h2>
        <ol className="chain">{stages.map((item) => {
          const upstream = (item.stage.inputs ?? []).map((input) => input.node_id ? byNode.get(input.node_id) : undefined).filter((value): value is FlatStage => Boolean(value))
          return <li key={item.key}>
            <button type="button" className={`chain__step${item.key === current?.key ? ' is-selected' : ''}`} onClick={() => setSelected(item.key)} aria-current={item.key === current?.key ? 'step' : undefined}>
              <StageIcon status={item.stage.status} />
              <span className="chain__text">
                <strong>{item.stage.name || item.member.name}</strong>
                <span className="muted small">{item.member.name}{item.member.runtime?.engine ? ` · ${engineName(item.member.runtime.engine)}` : ''}{item.stage.duration_ms ? ` · ${duration(item.stage.duration_ms)}` : ''}</span>
                {upstream.length ? <span className="chain__handoff small">接收：{upstream.map((source) => source.stage.name || source.member.name).join('、')}{stageHead(upstream[0].stage.node_id) ? ` · 提交 ${shortSHA(stageHead(upstream[0].stage.node_id))}` : ''}</span> : null}
                {stageHead(item.stage.node_id) ? <span className="mono small muted">产出 {shortSHA(stageHead(item.stage.node_id))}</span> : null}
                {(stagePasses(item.stage.node_id) ?? 1) > 1 ? <span className="small muted">第 {stagePasses(item.stage.node_id)} 轮</span> : null}
              </span>
            </button>
          </li>
        })}</ol>
        {code ? <CodeVerdict code={code} executionLabel={executionLabel(activity.status, activity.wait_kind)} onOpenCommand={(nodeId, index) => {
          const target = stages.find((entry) => entry.stage.node_id === nodeId)
          if (target) setSelected(target.key)
          setLogStream(`command-${index + 1}`)
          setTab('output')
        }} /> : <Verdict activity={activity} />}
        {code ? <TaskThread runId={runId} status={activity.status} navigate={navigate} /> : null}
        {code && !readOnly ? <FollowUpComposer runId={runId} status={activity.status} navigate={navigate} /> : null}
      </aside>
      <div className="task-panel">
        <div className="tabs" role="tablist" aria-label="任务内容">
          {([['output', '输出'], ...(code ? [['changes', '变更']] as const : []), ['evidence', '证据'], ['details', '详情']] as const).map(([value, label]) => <button key={value} type="button" role="tab" aria-selected={tab === value} onClick={() => setTab(value)}>{label}</button>)}
        </div>
        {tab === 'output' ? current ? <StageOutput runId={runId} version={version} logStream={logStream} item={current} busy={busy} onRetry={readOnly ? undefined : () => void act(() => retryStage(runId, current.stage.node_id, createUUID()))} /> : <p className="muted">尚未开始执行。</p> : null}
        {tab === 'changes' && code ? <CodeChanges code={code} runId={runId} done={done} onChanged={() => void refresh()} /> : null}
        {tab === 'evidence' ? code ? <CodeEvidencePanel code={code} /> : <Evidence activity={activity} /> : null}
        {tab === 'details' ? <Details activity={activity} firstOutputAt={task?.first_output_at} /> : null}
      </div>
    </div>}
  </section>
}

function StageOutput({ runId, version, logStream, item, busy, onRetry }: { runId: string; version: number; logStream?: string; item: FlatStage; busy: boolean; onRetry?(): void }) {
  const { stage, member } = item
  const updates = stage.public_updates ?? []
  return <div className="stack">
    <div className="stage-head">
      <div><h2>{stage.name || member.name}</h2><p className="muted small">{member.name}{member.runtime?.engine ? ` · ${engineName(member.runtime.engine)}` : ''}{member.runtime?.model ? ` · ${member.runtime.model}` : ''} · {stageLabel(stage.status)}</p></div>
      {stage.status === 'failed' && stage.retryable && onRetry ? <button type="button" className="button" disabled={busy} onClick={onRetry}>重试此阶段</button> : null}
    </div>
    <LogViewer runId={runId} nodeId={stage.node_id} version={version} preferredStream={logStream} fallback={updates.length ? <div className="stream" aria-live="polite">
      {updates.map((update) => <div key={update.seq} className={`stream__line stream__line--${update.kind}`}>{update.kind === 'tool_call' ? '▸ ' : ''}{update.text}</div>)}
      {stage.public_updates_truncated ? <div className="muted small">较早的输出已省略。</div> : null}
    </div> : stage.status === 'running' ? <p className="muted">等待输出…</p> : null} />
    {stage.tools?.length ? <details className="disclosure"><summary>工具调用 {stage.tools.length}</summary>
      <ul className="tool-list">{stage.tools.map((tool) => <li key={tool.call_id}><span className="mono">{tool.name}</span> <span className="muted small">{stageLabel(tool.status)}</span>
        {tool.input ? <pre>{tool.input}</pre> : null}{tool.output ? <pre>{tool.output}</pre> : null}</li>)}</ul>
    </details> : null}
    {stage.outputs?.length ? <div className="stack">{stage.outputs.map((output, index) => <div key={`${output.path ?? output.kind}:${index}`} className="artifact">
      <ResultContent content={output.content} label={output.path || '结果'} detail={`${output.content_bytes} 字节${output.truncated ? ' · 已截断' : ''}`} />
    </div>)}</div> : null}
  </div>
}

function Verdict({ activity }: { activity: Activity }) {
  const verification = verificationLabel(activity.delivery, activity.status)
  const delivery = activity.delivery
  return <section className={`verdict verdict--${verification.tone}`} aria-label="结论">
    <h2 className="section-title">结论</h2>
    <dl className="verdict__rows">
      <div><dt>执行</dt><dd>{executionLabel(activity.status, activity.wait_kind)}</dd></div>
      <div><dt>验证</dt><dd>{verification.label}</dd></div>
    </dl>
    {terminal(activity.status) && (!delivery || !delivery.available) ? <p className="small">团队没有声明验证要求，执行完成不代表验证通过。</p> : null}
    {delivery?.checks?.length ? <ul className="check-list">{delivery.checks.map((check) => <li key={check.check_id}>
      {check.status === 'passed' ? <CircleCheck size={14} className="icon--success" aria-hidden="true" /> : check.status === 'failed' ? <CircleX size={14} className="icon--danger" aria-hidden="true" /> : <CircleDashed size={14} aria-hidden="true" />}
      <span>{checkTitle(check.check_id, check.title)}{check.status === 'failed' && check.actual !== undefined ? <span className="muted small"> 实际 {check.actual}，要求 {check.expected}</span> : checkReason(check.reason) ? <span className="muted small"> {checkReason(check.reason)}</span> : null}</span>
    </li>)}</ul> : null}
  </section>
}

function Evidence({ activity }: { activity: Activity }) {
  const delivery = activity.delivery
  if (!delivery || !delivery.available) return <p className="muted">这个任务没有可核对的验证证据。</p>
  return <div className="stack">
    <p>证据完整性：{delivery.evidence_completeness === 'complete' ? '完整' : '不完整'}</p>
    <table className="table table--compact"><thead><tr><th>检查</th><th>结果</th><th>实际</th><th>要求</th></tr></thead>
      <tbody>{(delivery.checks ?? []).map((check) => <tr key={check.check_id}><td>{checkTitle(check.check_id, check.title)}<div className="muted small">{checkReason(check.reason)}</div></td><td>{check.status === 'passed' ? '通过' : check.status === 'failed' ? '未通过' : '无法确认'}</td><td className="mono">{check.actual ?? '—'}</td><td className="mono">{check.expected ?? '—'}</td></tr>)}</tbody>
    </table>
  </div>
}

function Details({ activity, firstOutputAt }: { activity: Activity; firstOutputAt?: string | null }) {
  return <dl className="facts">
    <div><dt>开始</dt><dd>{relativeTime(activity.created_at)}</dd></div>
    {firstOutputAt ? <div><dt>首个输出</dt><dd>{Math.max(0, Math.round((Date.parse(firstOutputAt) - Date.parse(activity.created_at)) / 1000))} 秒</dd></div> : null}
    <div><dt>结束</dt><dd>{activity.terminal_at ? relativeTime(activity.terminal_at) : '—'}</dd></div>
    <div><dt>输入 token</dt><dd>{activity.tokens_in ?? '—'}</dd></div>
    <div><dt>输出 token</dt><dd>{activity.tokens_out ?? '—'}</dd></div>
    <div><dt>节点</dt><dd>{activity.runtimes?.length ? activity.runtimes.map((runtime) => `${runtime.name}${runtime.engine ? `（${engineName(runtime.engine)}）` : ''}`).join('、') : '—'}</dd></div>
  </dl>
}
