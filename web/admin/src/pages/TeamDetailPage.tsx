import { createUUID } from '../lib/ids'
import { ArrowLeft } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Badge, InlineError } from '../components/ui'
import { MemberEditor } from '../components/members/MemberEditor'
import { FlowProfile, TeamProfile } from '../components/team/TeamProfile'
import { FeishuTeamAccessPanel } from '../components/feishu/FeishuTeamAccess'
import { errorMessage } from '../lib/api'
import { relativeTime } from '../lib/format'
import type { Graph } from '../lib/graph'
import { WorkflowEditor } from '../components/workflow/WorkflowEditor'
import { listNodes, type RuntimeNode } from '../lib/nodes'
import { executionLabel } from '../lib/tasks'
import { memberConfigIssues, nodeReadinessIssues, pinMembers, publishDevelopment, readDevelopment, saveDevelopment, startTrial, withStarterWorkflow, type DevelopmentDocument, type DevelopmentDraft, type DevelopmentMember } from '../lib/teams'

export function TeamDetailPage({ teamId, navigate }: { teamId: string; navigate(path: string): void }) {
  const [draft, setDraft] = useState<DevelopmentDraft>()
  const [document, setDocument] = useState<DevelopmentDocument>()
  const [nodes, setNodes] = useState<RuntimeNode[]>([])
  const [tab, setTab] = useState<'profile' | 'members' | 'workflow' | 'access' | 'trial'>('members')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const load = useCallback(async () => {
    try {
      const current = await readDevelopment(teamId)
      setDraft(current)
      setDocument(withStarterWorkflow(current.document))
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [teamId])
  useEffect(() => {
    void load()
    void listNodes().then(setNodes).catch(() => setNodes([]))
  }, [load])
  const nodeChoices = useMemo(() => nodes.filter((node) => node.enabled).map((node) => ({ id: node.id, engines: node.engine_readiness.map((engine) => engine.engine) })), [nodes])
  // A new team gets its CLI members pinned once nodes are known.
  useEffect(() => {
    if (!draft || !document || draft.document.workflows.length || !nodeChoices.length) return
    const pinned = pinMembers(document, nodeChoices)
    if (pinned !== document) setDocument(pinned)
  }, [draft, document, nodeChoices])

  const dirty = Boolean(draft && document && JSON.stringify(draft.document) !== JSON.stringify(document))
  const published = Boolean(draft && draft.published_revision > 0)
  const upToDate = Boolean(draft && draft.published_revision === draft.revision && !dirty)
  const accepting = useMemo(() => {
    const counts: Record<string, number> = {}
    for (const node of nodes) for (const engine of node.engine_readiness) if (engine.accepting) counts[engine.engine] = (counts[engine.engine] ?? 0) + 1
    return counts
  }, [nodes])
  const workflow = document?.workflows[0]
  const issues = useMemo(() => {
    if (!document) return []
    const found = [...nodeReadinessIssues(document, accepting), ...memberConfigIssues(document)]
    if (!workflow) found.push('还没有工作流程')
    return found
  }, [document, accepting, workflow])

  const run = async (action: () => Promise<DevelopmentDraft | void>) => {
    setBusy(true)
    setError('')
    try {
      const next = await action()
      if (next) {
        setDraft(next)
        setDocument(withStarterWorkflow(next.document))
      }
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const save = () => run(() => saveDevelopment(teamId, draft!.revision, document!))
  const editMember = (id: string, patch: Partial<DevelopmentMember['configuration']>) => setDocument((current) => {
    if (!current) return current
    const next = { ...current, members: current.members.map((member) => member.id === id ? { ...member, configuration: { ...member.configuration, ...patch } } : member) }
    // Changing the engine re-pins the member to a node that accepts it.
    return patch.engine !== undefined ? pinMembers(next, nodeChoices) : next
  })
  const editRelationship = (id: string, patch: Partial<DevelopmentMember['relationship']>) => setDocument((current) => current && { ...current, members: current.members.map((member) => member.id === id ? { ...member, relationship: { ...member.relationship, ...patch } } : member) })
  const editTeam = (patch: Partial<DevelopmentDocument>) => setDocument((current) => current && { ...current, ...patch })
  const editFlow = (patch: { name?: string; description?: string }) => setDocument((current) => current && { ...current, workflows: current.workflows.map((flow, index) => index === 0 ? { ...flow, ...patch } : flow) })
  const editGraph = (change: (graph: Graph) => Graph) => {
    if (!document?.workflows.length) return
    try {
      const next = change(document.workflows[0].graph_definition)
      setDocument({ ...document, workflows: document.workflows.map((flow, index) => index === 0 ? { ...flow, graph_definition: next } : flow) })
      setError('')
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : '流程修改失败')
    }
  }

  return <section className={`page${tab === 'workflow' ? ' page--wide' : ''}`}>
    <header className="task-header">
      <button type="button" className="icon-button" aria-label="返回团队列表" onClick={() => navigate('/teams')}><ArrowLeft size={16} /></button>
      <div className="task-header__title"><h1>{document?.name ?? '团队'}</h1><p className="muted small">{document?.objective}</p></div>
      {draft ? <div className="task-header__state">
        <Badge tone={upToDate ? 'success' : 'neutral'}>{!published ? '未发布' : upToDate ? '已发布' : '有未发布的修改'}</Badge>
        {dirty ? <button type="button" className="button button--primary" disabled={busy} onClick={() => void save()}>{busy ? '正在保存…' : '保存草稿'}</button> : null}
        {!dirty && !upToDate ? <button type="button" className="button button--primary" disabled={busy} onClick={() => void run(() => publishDevelopment(teamId, draft.revision))}>发布</button> : null}
      </div> : null}
    </header>
    {issues.length ? <ul className="readiness">{issues.map((issue) => <li key={issue}>{issue}</li>)}</ul> : null}
    {error ? <InlineError message={error} onRetry={draft ? undefined : () => void load()} /> : null}
    {!document || !draft ? null : <>
      <div className="tabs" role="tablist" aria-label="团队配置">
        {([['profile', '资料'], ['members', '成员'], ['workflow', '流程'], ['access', '接入'], ['trial', '试跑与发布']] as const).map(([value, label]) => <button key={value} type="button" role="tab" aria-selected={tab === value} onClick={() => setTab(value)}>{label}</button>)}
      </div>
      {tab === 'profile' ? <TeamProfile document={document} onChange={editTeam} /> : null}
      {tab === 'members' ? <MemberEditor document={document} nodes={nodes} accepting={accepting} onConfig={editMember} onRelationship={editRelationship} /> : null}
      {tab === 'workflow' ? workflow ? <><FlowProfile name={workflow.name} description={workflow.description} onChange={editFlow} /><WorkflowEditor key={workflow.id} graph={workflow.graph_definition} members={document.members} onChange={editGraph} /></> : <p className="muted">至少需要两名执行成员才能生成流程。</p> : null}
      {tab === 'access' ? <FeishuTeamAccessPanel teamId={teamId} workflows={document.workflows.map((flow) => ({ id: flow.id, name: flow.name }))} /> : null}
      {tab === 'trial' ? <Trial teamId={teamId} draft={draft} dirty={dirty} workflowId={workflow?.id} navigate={navigate} onStarted={() => void load()} /> : null}
    </>}
  </section>
}

function Trial({ teamId, draft, dirty, workflowId, navigate, onStarted }: { teamId: string; draft: DevelopmentDraft; dirty: boolean; workflowId?: string; navigate(path: string): void; onStarted(): void }) {
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [requestId, setRequestId] = useState(() => createUUID())
  const start = async () => {
    if (dirty) { setError('请先保存草稿'); return }
    if (!workflowId) { setError('还没有工作流程'); return }
    if (!input.trim()) { setError('请填写试跑任务'); return }
    setBusy(true)
    setError('')
    try {
      const result = await startTrial(teamId, draft.revision, workflowId, input.trim(), requestId)
      setRequestId(createUUID())
      onStarted()
      navigate(`/tasks/${encodeURIComponent(result.run_id)}`)
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const trials = [...draft.trials].sort((left, right) => Date.parse(right.created_at) - Date.parse(left.created_at))
  return <div className="stack">
    <p className="muted small">当前草稿的每个流程都试跑成功后才能发布。</p>
    <textarea className="input textarea" rows={3} value={input} placeholder="用于试跑的任务" onChange={(event) => { setInput(event.target.value); setError('') }} aria-label="试跑任务" />
    <div className="toolbar"><button type="button" className="button button--primary" disabled={busy} onClick={() => void start()}>{busy ? '正在提交…' : '试跑当前草稿'}</button></div>
    {error ? <InlineError message={error} /> : null}
    {trials.length ? <div className="task-list">{trials.map((trial) => <button key={trial.request_id} type="button" className="task-row" onClick={() => trial.run_id && navigate(`/tasks/${encodeURIComponent(trial.run_id)}`)}>
      <span className="task-row__main"><strong>草稿第 {trial.revision} 版试跑</strong><span className="muted small">{relativeTime(trial.created_at)}{trial.revision !== draft.revision ? ' · 不是当前草稿' : ''}</span></span>
      <Badge tone={trial.status === 'succeeded' ? 'success' : trial.status === 'failed' ? 'danger' : 'neutral'}>{executionLabel(trial.status)}</Badge>
    </button>)}</div> : null}
  </div>
}
