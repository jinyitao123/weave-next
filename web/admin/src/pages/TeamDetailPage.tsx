import { ArrowLeft, Plus, Trash2 } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Badge, InlineError, Select, Switch } from '../components/ui'
import { errorMessage } from '../lib/api'
import { engineName, relativeTime } from '../lib/format'
import { insertStepAfter, maxVerifyRounds, removeStep, serialSteps, updateStep, verifyLoop, withoutVerifyLoop, withVerifyLoop, type Graph } from '../lib/graph'
import { listNodes, type RuntimeNode } from '../lib/nodes'
import { executionLabel } from '../lib/tasks'
import { isCLIEngine, memberIssues, pinMembers, publishDevelopment, readDevelopment, saveDevelopment, startTrial, withStarterWorkflow, type DevelopmentDocument, type DevelopmentDraft, type DevelopmentMember } from '../lib/teams'

const engines = ['claude', 'codex', 'opencode', 'loom']

export function TeamDetailPage({ teamId, navigate }: { teamId: string; navigate(path: string): void }) {
  const [draft, setDraft] = useState<DevelopmentDraft>()
  const [document, setDocument] = useState<DevelopmentDocument>()
  const [nodes, setNodes] = useState<RuntimeNode[]>([])
  const [tab, setTab] = useState<'members' | 'workflow' | 'trial'>('members')
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
    const found: string[] = []
    for (const member of document.members.filter((item) => item.configuration.role === 'worker' && item.relationship.enabled !== false)) {
      if (!accepting[member.configuration.engine]) found.push(`“${member.configuration.display_name}”使用的 ${engineName(member.configuration.engine)} 当前没有可接任务的节点`)
    }
    found.push(...memberIssues(document))
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

  return <section className="page">
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
        {([['members', '成员'], ['workflow', '流程'], ['trial', '试跑与发布']] as const).map(([value, label]) => <button key={value} type="button" role="tab" aria-selected={tab === value} onClick={() => setTab(value)}>{label}</button>)}
      </div>
      {tab === 'members' ? <Members document={document} nodes={nodes} accepting={accepting} onEdit={editMember} /> : null}
      {tab === 'workflow' ? workflow ? <Workflow graph={workflow.graph_definition} members={document.members} onChange={editGraph} /> : <p className="muted">至少需要两名执行成员才能生成流程。</p> : null}
      {tab === 'trial' ? <Trial teamId={teamId} draft={draft} dirty={dirty} workflowId={workflow?.id} navigate={navigate} onStarted={() => void load()} /> : null}
    </>}
  </section>
}

function Members({ document, nodes, accepting, onEdit }: { document: DevelopmentDocument; nodes: RuntimeNode[]; accepting: Record<string, number>; onEdit(id: string, patch: Partial<DevelopmentMember['configuration']>): void }) {
  const workers = document.members.filter((member) => member.configuration.role === 'worker')
  return <div className="member-grid">{workers.map((member) => {
    const config = member.configuration
    const engineNodes = nodes.filter((node) => node.engine_readiness.some((engine) => engine.engine === config.engine)).map((node) => ({ value: node.id, label: node.name, detail: node.accepting ? '可接任务' : '暂不可接' }))
    const nodeOptions = isCLIEngine(config.engine) ? engineNodes : [{ value: '', label: '不需要节点' }]
    return <section key={member.id} className="member-card" aria-label={config.display_name}>
      <label className="field"><span>名称</span><input className="input" value={config.display_name} maxLength={80} onChange={(event) => onEdit(member.id, { display_name: event.target.value })} /></label>
      <div className="member-card__row">
        <div className="field"><span>引擎</span><Select label={`${config.display_name}的引擎`} value={config.engine} options={engines.map((engine) => ({ value: engine, label: engineName(engine), detail: `${accepting[engine] ?? 0} 个节点可接` }))} onChange={(engine) => onEdit(member.id, { engine, runtime_id: '' })} /></div>
        <div className="field"><span>节点</span><Select label={`${config.display_name}的节点`} value={config.runtime_id ?? ''} placeholder={nodeOptions.length ? '选择节点' : '没有提供该引擎的节点'} options={nodeOptions} onChange={(runtime) => onEdit(member.id, { runtime_id: runtime })} /></div>
      </div>
      <label className="field"><span>模型</span><input className="input" value={config.model ?? ''} placeholder="留空使用引擎默认模型" onChange={(event) => onEdit(member.id, { model: event.target.value })} /></label>
      <label className="field"><span>职责</span><textarea className="input textarea" rows={4} value={config.system_prompt ?? ''} onChange={(event) => onEdit(member.id, { system_prompt: event.target.value })} /></label>
      <p className="muted small">{accepting[config.engine] ? `${accepting[config.engine]} 个节点可接 ${engineName(config.engine)}` : `没有节点可接 ${engineName(config.engine)}`}</p>
    </section>
  })}</div>
}

function Workflow({ graph, members, onChange }: { graph: Graph; members: DevelopmentMember[]; onChange(change: (graph: Graph) => Graph): void }) {
  const loop = verifyLoop(graph)
  const serialShape = serialSteps(graph)
  const steps = loop ? loop.steps : serialShape.steps
  const workers = members.filter((member) => member.configuration.role === 'worker')
  const memberOptions = workers.map((member) => ({ value: member.id, label: `${member.configuration.display_name}（${engineName(member.configuration.engine)}）` }))
  if (!loop && !serialShape.serial) return <p className="notice">这个流程包含并行或条件分支，请在桌面团队工作区调整。</p>
  const loopable = Boolean(loop) || (steps.length === 2 && steps.every((step) => step.type === 'worker'))
  return <div className="stack">
  {loopable ? <div className="member-card__row">
    <Switch checked={Boolean(loop)} label="验证未通过时退回第一步" onChange={(on) => onChange((current) => on ? withVerifyLoop(current, 3) : withoutVerifyLoop(current))} />
    {loop ? <div className="field"><span>最多轮数</span><Select label="最多轮数" value={String(loop.rounds)} options={Array.from({ length: maxVerifyRounds }, (_, index) => ({ value: String(index + 1), label: `${index + 1} 轮` }))}
      onChange={(value) => onChange((current) => withVerifyLoop(current, Number(value)))} /></div> : null}
  </div> : null}
  <ol className="step-list">{steps.map((step, index) => {
    const agentId = String(step.config?.agent_id ?? '')
    return <li key={step.id} className="step-card">
      <div className="step-card__index" aria-hidden="true">{index + 1}</div>
      <div className="stack">
        <div className="member-card__row">
          <label className="field"><span>步骤名称</span><input className="input" value={step.label ?? ''} onChange={(event) => onChange((current) => updateStep(current, step.id, { label: event.target.value }))} /></label>
          <div className="field"><span>执行成员</span><Select label={`第 ${index + 1} 步的执行成员`} value={agentId} options={memberOptions} onChange={(value) => onChange((current) => updateStep(current, step.id, { agentId: value }))} /></div>
        </div>
        <label className="field"><span>结果要求</span><textarea className="input textarea" rows={2} value={String(step.config?.result_requirement ?? '')} onChange={(event) => onChange((current) => updateStep(current, step.id, { requirement: event.target.value }))} /></label>
        {index > 0 ? <p className="chain__handoff small">接收：{steps[index - 1].label || '上一步'}的结果</p> : null}
        {loop && index === 0 ? <p className="chain__handoff small">重做时接收：{steps[1].label || '下一步'}的验证意见</p> : null}
        {loop ? null : <div className="toolbar">
          {workers[0] ? <button type="button" className="button" onClick={() => onChange((current) => insertStepAfter(current, step.id, { id: workers[0].id, displayName: workers[0].configuration.display_name }))}><Plus size={14} />在此后添加步骤</button> : null}
          {index > 0 ? <button type="button" className="button" onClick={() => onChange((current) => removeStep(current, step.id))}><Trash2 size={14} />删除</button> : null}
        </div>}
      </div>
    </li>
  })}<li className="step-card step-card--end"><div className="step-card__index" aria-hidden="true">✓</div><span>{loop ? '交付最后一轮的验证结论' : '交付最后一步的结果'}</span></li></ol>
  </div>
}

function Trial({ teamId, draft, dirty, workflowId, navigate, onStarted }: { teamId: string; draft: DevelopmentDraft; dirty: boolean; workflowId?: string; navigate(path: string): void; onStarted(): void }) {
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [requestId, setRequestId] = useState(() => crypto.randomUUID())
  const start = async () => {
    if (dirty) { setError('请先保存草稿'); return }
    if (!workflowId) { setError('还没有工作流程'); return }
    if (!input.trim()) { setError('请填写试跑任务'); return }
    setBusy(true)
    setError('')
    try {
      const result = await startTrial(teamId, draft.revision, workflowId, input.trim(), requestId)
      setRequestId(crypto.randomUUID())
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
