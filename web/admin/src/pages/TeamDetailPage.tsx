import { ArrowLeft } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Badge, Dialog, InlineError, type Tone } from '../components/ui'
import { MemberEditor } from '../components/members/MemberEditor'
import { PublishPanel, TrialPanel } from '../components/team/Release'
import { FlowProfile, TeamProfile } from '../components/team/TeamProfile'
import { FeishuTeamAccessPanel } from '../components/feishu/FeishuTeamAccess'
import { errorMessage } from '../lib/api'
import type { Graph } from '../lib/graph'
import { WorkflowEditor } from '../components/workflow/WorkflowEditor'
import { listNodes, type RuntimeNode } from '../lib/nodes'
import { useLeaveGuard } from '../lib/router'
import { allowRequiredHandoffKinds, loadBusinessCatalog, memberConfigIssues, nodeReadinessIssues, pinMembers, publishDevelopment, readDevelopment, saveDevelopment, withStarterWorkflow, type BusinessCatalog, type DevelopmentDocument, type DevelopmentDraft, type DevelopmentMember } from '../lib/teams'

const tabs = [['profile', '资料'], ['members', '成员'], ['workflow', '流程'], ['access', '接入'], ['trial', '试跑'], ['publish', '发布']] as const
type TabId = typeof tabs[number][0]

export function TeamDetailPage({ teamId, navigate }: { teamId: string; navigate(path: string, options?: { force?: boolean }): void }) {
  const [draft, setDraft] = useState<DevelopmentDraft>()
  const [document, setDocument] = useState<DevelopmentDocument>()
  const [nodes, setNodes] = useState<RuntimeNode[]>([])
  const [catalog, setCatalog] = useState<BusinessCatalog>()
  const [tab, setTab] = useState<TabId>('profile')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [leaving, setLeaving] = useState<string>()
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
    void loadBusinessCatalog().then(setCatalog).catch(() => setCatalog({ available: false, capabilities: [] }))
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
  // Leaving with unsaved edits asks first instead of dropping them silently.
  useLeaveGuard(dirty, setLeaving)
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
  const discard = () => { if (draft) { setDocument(withStarterWorkflow(draft.document)); setError('') } }
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
      // A member placed on a step is allowed the handoff kind that step needs.
      setDocument(allowRequiredHandoffKinds({ ...document, workflows: document.workflows.map((flow, index) => index === 0 ? { ...flow, graph_definition: next } : flow) }))
      setError('')
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : '流程修改失败')
    }
  }
  const state: { tone: Tone; label: string } = dirty ? { tone: 'warning', label: '有未保存的修改' }
    : !published ? { tone: 'neutral', label: '未发布' }
      : upToDate ? { tone: 'success', label: '已发布' } : { tone: 'accent', label: '有未发布的修改' }

  return <section className="page">
    <header className="task-header">
      <button type="button" className="icon-button" aria-label="返回团队列表" onClick={() => navigate('/teams')}><ArrowLeft size={16} /></button>
      <div className="task-header__title"><h1>{document?.name ?? '团队'}</h1><p className="muted small">{document?.objective}</p></div>
      {draft ? <div className="task-header__state">
        <Badge tone={state.tone}>{state.label}</Badge>
        {dirty ? <button type="button" className="button" disabled={busy} onClick={discard}>放弃修改</button> : null}
        {dirty ? <button type="button" className="button button--primary" disabled={busy} onClick={() => void save()}>{busy ? '正在保存…' : '保存草稿'}</button> : null}
      </div> : null}
    </header>
    {issues.length ? <ul className="readiness">{issues.map((issue) => <li key={issue}>{issue}</li>)}</ul> : null}
    {error ? <InlineError message={error} onRetry={draft ? undefined : () => void load()} /> : null}
    {!document || !draft ? null : <>
      <div className="tabs" role="tablist" aria-label="团队配置">
        {tabs.map(([value, label]) => <button key={value} type="button" role="tab" aria-selected={tab === value} onClick={() => setTab(value)}>{label}</button>)}
      </div>
      {tab === 'profile' ? <TeamProfile document={document} onChange={editTeam} /> : null}
      {tab === 'members' ? <MemberEditor document={document} nodes={nodes} accepting={accepting} catalog={catalog} onConfig={editMember} onRelationship={editRelationship} /> : null}
      {tab === 'workflow' ? workflow ? <><FlowProfile name={workflow.name} description={workflow.description} onChange={editFlow} /><WorkflowEditor key={workflow.id} graph={workflow.graph_definition} members={document.members} onChange={editGraph} /></> : <p className="muted">至少需要两名执行成员才能生成流程。</p> : null}
      {tab === 'access' ? <FeishuTeamAccessPanel teamId={teamId} workflows={document.workflows.map((flow) => ({ id: flow.id, name: flow.name }))} /> : null}
      {tab === 'trial' ? <TrialPanel teamId={teamId} draft={draft} dirty={dirty} catalog={catalog} navigate={navigate} onStarted={() => void load()} /> : null}
      {tab === 'publish' ? <PublishPanel draft={draft} dirty={dirty} busy={busy} catalog={catalog} onPublish={() => void run(() => publishDevelopment(teamId, draft.revision))} onOpenTrial={() => setTab('trial')} /> : null}
    </>}
    {leaving ? <Dialog title="有未保存的修改" onClose={() => setLeaving(undefined)} footer={<>
      <button type="button" className="button" onClick={() => setLeaving(undefined)}>留在此页</button>
      <button type="button" className="button button--danger" onClick={() => navigate(leaving, { force: true })}>放弃修改并离开</button>
    </>}><p>离开后，这些修改不会保留。</p></Dialog> : null}
  </section>
}
