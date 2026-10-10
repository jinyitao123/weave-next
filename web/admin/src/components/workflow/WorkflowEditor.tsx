import { GitBranch, Plus, Trash2 } from 'lucide-react'
import { useRef, useState } from 'react'
import { Checkbox, Dialog, InlineError, Select, Switch } from '../ui'
import { maxVerifyRounds, serialSteps, verifyLoop, type Graph, type Step } from '../../lib/graph'
import { requiredActions, setRequiredActions } from '../../lib/business-completion'
import type { DevelopmentMember } from '../../lib/teams'
import { addParallelBranch, assignExecutor, canAddParallelBranch, canInsertSerialStep, feedsDelivery, configureWorkflowResultProtocol, hasNodeInput, insertStep, isParallelBranchWorker, predecessors, removeStep, serializeParallel, setVerificationLoop, stepFeeds, toggleNodeInput, toggleTaskInput, validateWorkflowResultProtocol, WORKBENCH_RESULT_PROTOCOL } from '../../lib/workflow-graph'
import { FlowCanvas, stepLabel } from './FlowCanvas'
import './workflow.css'

type Form = { placement: 'serial' | 'parallel'; after: string; member: string; name: string; requirement: string }

const nodeTitle = (node: Step) => node.label || stepLabel(node.type)

// Who produces a step's result, for the source list.
function actorName(node: Step, members: DevelopmentMember[]): string {
  if (node.type === 'join') return '汇合各分支的结果'
  const member = node.type === 'lead'
    ? members.find(item => item.configuration.role === 'avatar' && item.relationship.enabled)
    : members.find(item => item.id === node.config?.agent_id)
  return member?.configuration.display_name ?? stepLabel(node.type)
}

// Where a new step goes, in the words of the flow.
function placementText(graph: Graph, form: Form): string {
  const anchor = graph.nodes.find(node => node.id === form.after)
  const name = anchor ? nodeTitle(anchor) : '当前步骤'
  if (form.placement === 'serial') return anchor?.type === 'parallel' ? `加在“${name}”各分支汇合之后` : `加在“${name}”之后`
  return anchor?.type === 'parallel' || (anchor && isParallelBranchWorker(graph, anchor.id)) ? `在“${name}”所在的并行分工里再加一个分支` : `与“${name}”同时执行，两边完成后汇合`
}

function modeLabel(graph: Graph, step: Step, loop: boolean): string {
  if (step.type === 'lead') return '负责人处理'
  if (step.type === 'worker') return isParallelBranchWorker(graph, step.id) ? '并行分支，与其他分支同时执行' : loop ? '验证回路中的一步' : '依次执行'
  return stepLabel(step.type)
}

/** A business action the flow's members can carry out, named for the page. */
export interface FlowAction { id: string; name: string }

export function WorkflowEditor({ graph, members, actions = [], onChange }: { graph: Graph; members: DevelopmentMember[]; actions?: FlowAction[]; onChange(change: (graph: Graph) => Graph): void }) {
  const [selected, setSelected] = useState(graph.entry_node_id)
  const [form, setForm] = useState<Form>()
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
  const [hot, setHot] = useState<string>()
  const inspector = useRef<HTMLElement>(null)
  // Below the two-column breakpoint the inspector sits under the canvas, so a
  // selected step brings it into view instead of leaving the edit off screen.
  const revealInspector = () => {
    if (typeof window.matchMedia !== 'function' || !window.matchMedia('(max-width: 1040px)').matches) return
    requestAnimationFrame(() => inspector.current?.scrollIntoView?.({ block: 'nearest', behavior: 'smooth' }))
  }
  const step = graph.nodes.find(node => node.id === selected) ?? graph.nodes[0]
  const loop = verifyLoop(graph), serial = serialSteps(graph)
  const loopable = Boolean(loop) || (serial.serial && serial.steps.length === 2 && serial.steps.every(node => node.type === 'worker'))
  const change = (operation: (current: Graph) => Graph) => {
    try { const next = operation(graph); onChange(() => next); setError(''); return true }
    catch (failure) { setError(failure instanceof Error ? failure.message : '流程修改失败'); return false }
  }
  const update = (next: Step) => change(current => ({ ...current, nodes: current.nodes.map(node => node.id === next.id ? next : node) }))
  const deliverySource = () => String((graph.nodes.find(node => node.type === 'deliver')?.config?.result as { node_id?: string })?.node_id ?? '')
  const structure = (operation: (current: Graph) => Graph) => change(current => configureWorkflowResultProtocol({ graph_definition: operation(current) }, current.result_protocol === WORKBENCH_RESULT_PROTOCOL, undefined, deliverySource()).graph_definition)
  const available = members.filter(member => member.relationship.enabled && (form?.placement !== 'parallel' || member.configuration.role === 'worker'))
  const add = () => {
    if (!form) return
    const member = available.find(item => item.id === form.member)
    if (!member || !form.name.trim() || !form.requirement.trim()) { setError('请选择执行成员并填写步骤名称和工作要求'); return }
    let created = ''
    const accepted = structure(current => {
      const result = form.placement === 'parallel' ? addParallelBranch(current, form.after, member) : insertStep(current, form.after, member)
      created = result.selected
      return { ...result.graph, nodes: result.graph.nodes.map(node => node.id === created ? { ...node, label: form.name.trim(), config: { ...node.config, [node.type === 'lead' ? 'instruction' : 'result_requirement']: form.requirement.trim() } } : node) }
    })
    if (accepted) { setSelected(created); setForm(undefined) }
  }
  const open = (placement: Form['placement']) => { if (step) { setError(''); setForm({ placement, after: step.id, member: '', name: '', requirement: '' }) } }
  const select = (id: string) => { setSelected(id); setError(''); setHot(undefined); revealInspector() }
  return <div className="flow-editor">
    {loopable ? <div className="flow-editor__loop">
      <Switch checked={Boolean(loop)} label="验证未通过时退回第一步" onChange={enabled => change(current => setVerificationLoop(current, enabled))} />
      {loop ? <div className="field"><span>最多轮数</span><Select label="最多轮数" value={String(loop.rounds)} options={Array.from({ length: maxVerifyRounds }, (_, index) => ({ value: String(index + 1), label: `${index + 1} 轮` }))} onChange={value => change(current => setVerificationLoop(current, true, Number(value)))} /></div> : null}
    </div> : null}
    <div className="flow-editor__workspace">
      <FlowCanvas graph={graph} members={members} selected={step?.id} feeds={step ? stepFeeds(graph, step) : undefined} highlight={hot} onSelect={select} />
      {step ? <section ref={inspector} className="flow-editor__inspector" aria-label={`${nodeTitle(step)}配置`}>
        <header className="flow-editor__head">
          <div><h2>{nodeTitle(step)}</h2>{modeLabel(graph, step, Boolean(loop)) !== nodeTitle(step) ? <p className="muted small">{modeLabel(graph, step, Boolean(loop))}</p> : null}</div>
          {!loop ? <div className="flow-editor__actions">
            {canInsertSerialStep(graph, step.id) ? <button type="button" className="button" onClick={() => open('serial')}><Plus size={14} />{step.type === 'parallel' ? '汇合后添加步骤' : '添加下一步'}</button> : null}
            {canAddParallelBranch(graph, step.id) ? <button type="button" className="button" onClick={() => open('parallel')}><GitBranch size={14} />添加并行分支</button> : null}
            {step.type === 'parallel' ? <button type="button" className="button" onClick={() => structure(current => serializeParallel(current, step.id))}>改为依次执行</button> : null}
            {['lead', 'worker'].includes(step.type) && step.id !== graph.entry_node_id ? <button type="button" className="button" onClick={() => setDeleting(true)}><Trash2 size={14} />删除步骤</button> : null}
          </div> : null}
        </header>
        {!loop && step.type === 'worker' && feedsDelivery(graph, step.id) && !canAddParallelBranch(graph, step.id) ? <p className="muted small flow-editor__hint">交付前的最后一步不能并行。需要并行时，先在它后面添加一个汇总步骤。</p> : null}
        {loop && step.id === loop.loopId
          ? <div className="field"><span>最多轮数</span><Select label="验证回路最多轮数" value={String(loop.rounds)} options={Array.from({ length: maxVerifyRounds }, (_, index) => ({ value: String(index + 1), label: `${index + 1} 轮` }))} onChange={value => change(current => setVerificationLoop(current, true, Number(value)))} /></div>
          : <StepInspector graph={graph} step={step} members={members} actions={actions} loop={Boolean(loop)} onChange={update} onGraph={change} onHot={setHot} />}
      </section> : <p className="muted">流程中还没有步骤。</p>}
    </div>
    {error && !form && !deleting ? <InlineError message={error} /> : null}
    {form ? <Dialog title={form.placement === 'parallel' ? '添加并行分支' : '添加步骤'} onClose={() => { setForm(undefined); setError('') }} footer={<><button type="button" className="button" onClick={() => { setForm(undefined); setError('') }}>取消</button><button type="button" className="button button--primary" disabled={!form.member || !form.name.trim() || !form.requirement.trim()} onClick={add}>{form.placement === 'parallel' ? '添加分支' : '添加步骤'}</button></>}>
      <div className="stack"><p>{placementText(graph, form)}</p>
        <div className="field"><span>执行成员</span><Select label="新步骤执行成员" value={form.member} options={available.map(member => ({ value: member.id, label: member.configuration.display_name }))} onChange={member => setForm({ ...form, member })} /></div>
        <label className="field"><span>步骤名称</span><input className="input" value={form.name} maxLength={80} onChange={event => setForm({ ...form, name: event.target.value })} /></label>
        <label className="field"><span>工作要求</span><textarea className="input textarea" rows={4} value={form.requirement} onChange={event => setForm({ ...form, requirement: event.target.value })} /></label>
        {!form.member || !form.name.trim() || !form.requirement.trim() ? <p className="muted small">选择执行成员，并填写步骤名称和工作要求后才能添加。</p> : null}
        {error ? <InlineError message={error} /> : null}
      </div>
    </Dialog> : null}
    {deleting && step ? <Dialog title="删除步骤" onClose={() => { setDeleting(false); setError('') }} footer={<><button type="button" className="button" onClick={() => { setDeleting(false); setError('') }}>取消</button><button type="button" className="button button--danger" onClick={() => { if (structure(current => removeStep(current, step.id))) { setDeleting(false); setSelected(graph.entry_node_id) } }}>确定删除</button></>}><p>删除“{nodeTitle(step)}”？</p>{error ? <InlineError message={error} /> : null}</Dialog> : null}
  </div>
}

function SourceRow({ checked, title, detail, hotKey, onToggle, onHot }: { checked: boolean; title: string; detail: string; hotKey: string; onToggle(): void; onHot(key?: string): void }) {
  return <li><label className={`flow-source${checked ? ' is-on' : ''}`} onMouseEnter={() => onHot(hotKey)} onMouseLeave={() => onHot(undefined)} onFocus={() => onHot(hotKey)} onBlur={() => onHot(undefined)}>
    <input type="checkbox" checked={checked} onChange={onToggle} />
    <span className="flow-source__body"><strong>{title}</strong><small>{detail}</small></span>
  </label></li>
}

function StepInspector({ graph, step, members, actions, loop, onChange, onGraph, onHot }: { graph: Graph; step: Step; members: DevelopmentMember[]; actions: FlowAction[]; loop: boolean; onChange(step: Step): void; onGraph(operation: (graph: Graph) => Graph): boolean; onHot(key?: string): void }) {
  const required = requiredActions(graph) ?? []
  const toggleRequired = (id: string, on: boolean) => onGraph(current => setRequiredActions(current, on ? [...required.filter(item => item !== id), id] : required.filter(item => item !== id)))
  const lead = members.find(member => member.configuration.role === 'avatar' && member.relationship.enabled)
  const branch = isParallelBranchWorker(graph, step.id)
  const actors = members.filter(member => member.relationship.enabled && (!(branch || loop) || member.configuration.role === 'worker'))
  const actor = step.type === 'lead' ? lead : actors.find(member => member.id === step.config?.agent_id && member.configuration.role === 'worker')
  const feeds = stepFeeds(graph, step)
  const configureDelivery = (enabled: boolean, source?: string) => onGraph(current => configureWorkflowResultProtocol({ graph_definition: current }, enabled, source).graph_definition)
  const issue = validateWorkflowResultProtocol({ graph_definition: graph })
  if (!['lead', 'worker', 'join', 'parallel', 'deliver'].includes(step.type)) return <p className="muted">该步骤暂不提供配置编辑，原有节点与连接会保留。</p>
  const candidates = predecessors(graph, step.id)
  const received = [feeds.task ? '本次任务输入' : '', ...candidates.filter(prior => hasNodeInput(step, prior.id)).map(prior => `${nodeTitle(prior)}的结果`)].filter(Boolean)
  const name = <label className="field"><span>步骤名称</span><input className="input" value={step.label ?? ''} onChange={event => onChange({ ...step, label: event.target.value })} /></label>
  return <div className="flow-editor__panels">
    <section className="flow-editor__panel" aria-label="这一步做什么">
      <h3>这一步做什么</h3>
      {name}
      {['lead', 'worker'].includes(step.type) ? <>
        {!actor ? <InlineError message="原执行成员不可用，请重新选择已启用的成员。" /> : null}
        {step.type === 'lead' && step.id === graph.entry_node_id ? <p>由负责人{actor ? `“${actor.configuration.display_name}”` : ''}执行</p> : <div className="field"><span>执行成员</span><Select label="执行成员" value={actor?.id ?? ''} options={actors.map(member => ({ value: member.id, label: member.configuration.display_name }))} onChange={id => { const member = actors.find(item => item.id === id); if (member) onGraph(current => assignExecutor(current, step.id, member)) }} /></div>}
        <label className="field"><span>工作要求</span><textarea className="input textarea" rows={5} value={String(step.config?.[step.type === 'lead' ? 'instruction' : 'result_requirement'] ?? '')} onChange={event => onChange({ ...step, config: { ...step.config, [step.type === 'lead' ? 'instruction' : 'result_requirement']: event.target.value } })} /></label>
      </> : null}
      {step.type === 'parallel' ? <p className="muted">分支成员同时执行，全部进入后面的汇合步骤。</p> : null}
      {step.type === 'join' ? <>
        <div className="field"><span>汇合条件</span><Select label="汇合条件" value={String(step.config?.policy ?? 'all_success')} options={[{ value: 'all_success', label: '全部成功后继续' }, { value: 'fail_fast', label: '任一失败即停止' }, { value: 'quorum', label: '指定数量成功后继续' }, { value: 'deadline', label: '等待到截止时间' }]} onChange={policy => { const config: Record<string, unknown> = { ...step.config, policy }; delete config.success_count; delete config.deadline_seconds; onChange({ ...step, config: { ...config, ...(policy === 'quorum' ? { success_count: 1 } : {}), ...(policy === 'deadline' ? { deadline_seconds: 300 } : {}) } }) }} /></div>
        {step.config?.policy === 'quorum' ? <label className="field"><span>所需成功分支数</span><input className="input" type="number" min={1} max={graph.edges.filter(edge => edge.to_node_id === step.id).length} value={Number(step.config.success_count ?? 1)} onChange={event => onChange({ ...step, config: { ...step.config, success_count: Number(event.target.value) } })} /></label> : null}
        {step.config?.policy === 'deadline' ? <label className="field"><span>最长等待时间（秒）</span><input className="input" type="number" min={1} value={Number(step.config.deadline_seconds ?? 300)} onChange={event => onChange({ ...step, config: { ...step.config, deadline_seconds: Number(event.target.value) } })} /></label> : null}
      </> : null}
      {step.type === 'deliver' ? loop ? <p>交付最后一轮的验证结论。</p> : <>
        <div className="field"><span>交付哪一步的结果</span><Select label="交付来源" value={String((step.config?.result as { node_id?: string })?.node_id ?? '')} options={candidates.map(prior => ({ value: prior.id, label: nodeTitle(prior) }))} onChange={id => configureDelivery(graph.result_protocol === WORKBENCH_RESULT_PROTOCOL, id)} /></div>
        <div className="field"><span>材料不全时</span><Select label="材料不全时" disabled={actions.length > 0 && graph.result_protocol === WORKBENCH_RESULT_PROTOCOL} value={graph.result_protocol === WORKBENCH_RESULT_PROTOCOL ? WORKBENCH_RESULT_PROTOCOL : 'ordinary'} options={[{ value: 'ordinary', label: '照常交付结果' }, { value: WORKBENCH_RESULT_PROTOCOL, label: '可以退回，请发起人补充材料' }]} onChange={value => configureDelivery(value === WORKBENCH_RESULT_PROTOCOL)} /></div>
        {actions.length ? <div className="field" role="group" aria-label="必须办成的业务动作"><span>必须办成的业务动作</span>
          <div className="flow-editor__required">{actions.map(action => <Checkbox key={action.id} checked={required.includes(action.id)} label={action.name} disabled={required.length === 1 && required[0] === action.id} onChange={on => toggleRequired(action.id, on)} />)}</div>
          <p className="muted small">任务里授权办理这些动作时，要拿到业务系统的成功回执，这次任务才算办完。</p>
        </div> : null}
        {issue ? <InlineError message={issue} /> : null}
      </> : null}
    </section>

    {['lead', 'worker'].includes(step.type) ? <section className="flow-editor__panel" aria-label="这一步会收到">
      <h3>这一步会收到</h3>
      {loop ? <p className="muted">输入沿用验证回路的本轮结果与上一轮验证意见。</p> : <>
        <ul className="flow-sources">
          <SourceRow checked={feeds.task} title="本次任务输入" detail="发起任务时提交的原始内容" hotKey="task" onToggle={() => onChange(toggleTaskInput(step))} onHot={onHot} />
          {candidates.map(prior => <SourceRow key={prior.id} checked={hasNodeInput(step, prior.id)} title={`${nodeTitle(prior)}的结果`} detail={prior.type === 'join' ? '各分支结果的汇总' : `由${actorName(prior, members)}产出`} hotKey={prior.id} onToggle={() => onChange(toggleNodeInput(step, prior))} onHot={onHot} />)}
        </ul>
        {candidates.length === 0 ? <p className="muted small">这是第一步，前面没有其他步骤可选。</p> : null}
        {received.length ? <p className="flow-sources__summary" role="status">开始时会收到：{received.join('、')}</p>
          : <p className="flow-sources__summary is-empty" role="status">没有选择任何内容：这一步既收不到任务，也收不到前面步骤的结果。</p>}
      </>}
    </section> : null}

    {step.type === 'join' ? <section className="flow-editor__panel" aria-label="汇合的分支">
      <h3>汇合这些分支的结果</h3>
      <ul className="flow-sources">{feeds.nodes.map(id => {
        const node = graph.nodes.find(item => item.id === id)
        return node ? <li key={id}><div className="flow-source is-on" onMouseEnter={() => onHot(id)} onMouseLeave={() => onHot(undefined)}><span className="flow-source__body"><strong>{nodeTitle(node)}</strong><small>由{actorName(node, members)}产出</small></span></div></li> : null
      })}</ul>
    </section> : null}
  </div>
}
