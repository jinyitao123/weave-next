import { GitBranch, Plus, Trash2 } from 'lucide-react'
import { useState } from 'react'
import { Drawer, InlineError, Select, Switch } from '../ui'
import { maxVerifyRounds, serialSteps, verifyLoop, type Graph, type Step } from '../../lib/graph'
import type { DevelopmentMember } from '../../lib/teams'
import { addParallelBranch, assignExecutor, bindings, canInsertSerialStep, configureWorkflowResultProtocol, insertStep, isParallelBranchWorker, originalBinding, predecessors, removeStep, serializeParallel, setVerificationLoop, validateWorkflowResultProtocol, WORKBENCH_RESULT_PROTOCOL } from '../../lib/workflow-graph'
import { FlowCanvas, stepLabel } from './FlowCanvas'
import './workflow.css'

type Form = { placement: 'serial' | 'parallel'; after: string; member: string; name: string; requirement: string }

export function WorkflowEditor({ graph, members, onChange }: { graph: Graph; members: DevelopmentMember[]; onChange(change: (graph: Graph) => Graph): void }) {
  const [selected, setSelected] = useState(graph.entry_node_id)
  const [form, setForm] = useState<Form>()
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
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
  return <div className="flow-editor">
    {loopable ? <div className="flow-editor__loop">
      <Switch checked={Boolean(loop)} label="验证未通过时退回第一步" onChange={enabled => change(current => setVerificationLoop(current, enabled))} />
      {loop ? <div className="field"><span>最多轮数</span><Select label="最多轮数" value={String(loop.rounds)} options={Array.from({ length: maxVerifyRounds }, (_, index) => ({ value: String(index + 1), label: `${index + 1} 轮` }))} onChange={value => change(current => setVerificationLoop(current, true, Number(value)))} /></div> : null}
    </div> : null}
    <div className="flow-editor__workspace">
      <FlowCanvas graph={graph} members={members} selected={step?.id} onSelect={id => { setSelected(id); setError('') }} />
      {step ? <section className="flow-editor__inspector" aria-label={`${step.label || stepLabel(step.type)}配置`}>
        <h2>{step.label || stepLabel(step.type)}</h2>
        {!loop ? <div className="flow-editor__actions">
          {canInsertSerialStep(graph, step.id) ? <button type="button" className="button" onClick={() => open('serial')}><Plus size={14} />{step.type === 'parallel' ? '汇合后添加步骤' : '添加下一步'}</button> : null}
          {(step.type === 'parallel' || step.type === 'worker' && graph.edges.filter(edge => edge.to_node_id === step.id).length === 1 && graph.edges.filter(edge => edge.from_node_id === step.id).length === 1) ? <button type="button" className="button" onClick={() => open('parallel')}><GitBranch size={14} />添加并行分支</button> : null}
          {step.type === 'parallel' ? <button type="button" className="button" onClick={() => structure(current => serializeParallel(current, step.id))}>改为依次执行</button> : null}
          {['lead', 'worker'].includes(step.type) && step.id !== graph.entry_node_id ? <button type="button" className="button" onClick={() => setDeleting(true)}><Trash2 size={14} />删除步骤</button> : null}
        </div> : null}
        <StepInspector graph={graph} step={step} members={members} loop={Boolean(loop)} onChange={update} onGraph={change} />
      </section> : <p className="muted">流程中还没有步骤。</p>}
    </div>
    {error ? <InlineError message={error} /> : null}
    {form ? <Drawer title={form.placement === 'parallel' ? '添加并行分支' : '添加步骤'} onClose={() => setForm(undefined)} footer={<><button type="button" className="button" onClick={() => setForm(undefined)}>取消</button><button type="button" className="button button--primary" disabled={!form.member || !form.name.trim() || !form.requirement.trim()} onClick={add}>添加步骤</button></>}>
      <div className="stack"><p>{graph.nodes.find(node => node.id === form.after)?.label || '当前步骤'}之后</p>
        <div className="field"><span>执行成员</span><Select label="新步骤执行成员" value={form.member} options={available.map(member => ({ value: member.id, label: member.configuration.display_name }))} onChange={member => setForm({ ...form, member })} /></div>
        <label className="field"><span>步骤名称</span><input className="input" value={form.name} maxLength={80} onChange={event => setForm({ ...form, name: event.target.value })} /></label>
        <label className="field"><span>工作要求</span><textarea className="input textarea" rows={4} value={form.requirement} onChange={event => setForm({ ...form, requirement: event.target.value })} /></label>
        {error ? <InlineError message={error} /> : null}
      </div>
    </Drawer> : null}
    {deleting && step ? <Drawer title="删除步骤" onClose={() => setDeleting(false)} footer={<><button type="button" className="button" onClick={() => setDeleting(false)}>取消</button><button type="button" className="button" onClick={() => { if (structure(current => removeStep(current, step.id))) { setDeleting(false); setSelected(graph.entry_node_id) } }}>确定删除</button></>}><p>删除“{step.label || stepLabel(step.type)}”？</p>{error ? <InlineError message={error} /> : null}</Drawer> : null}
  </div>
}

function StepInspector({ graph, step, members, loop, onChange, onGraph }: { graph: Graph; step: Step; members: DevelopmentMember[]; loop: boolean; onChange(step: Step): void; onGraph(operation: (graph: Graph) => Graph): boolean }) {
  const lead = members.find(member => member.configuration.role === 'avatar' && member.relationship.enabled)
  const branch = isParallelBranchWorker(graph, step.id)
  const actors = members.filter(member => member.relationship.enabled && (!(branch || loop) || member.configuration.role === 'worker'))
  const actor = step.type === 'lead' ? lead : actors.find(member => member.id === step.config?.agent_id && member.configuration.role === 'worker')
  const inputs = bindings(step)
  const configureDelivery = (enabled: boolean, source?: string) => onGraph(current => configureWorkflowResultProtocol({ graph_definition: current }, enabled, source).graph_definition)
  const issue = validateWorkflowResultProtocol({ graph_definition: graph })
  if (!['lead', 'worker', 'join', 'parallel', 'deliver'].includes(step.type)) return <p className="muted">该步骤暂不提供配置编辑，原有节点与连接会保留。</p>
  return <div className="stack">
    <label className="field"><span>步骤名称</span><input className="input" value={step.label ?? ''} onChange={event => onChange({ ...step, label: event.target.value })} /></label>
    {['lead', 'worker'].includes(step.type) ? <>
      {!actor ? <InlineError message="原执行成员不可用，请重新选择已启用的成员。" /> : null}
      {step.type === 'lead' && step.id === graph.entry_node_id ? <p>由负责人{actor ? `“${actor.configuration.display_name}”` : ''}执行</p> : <div className="field"><span>执行成员</span><Select label="执行成员" value={actor?.id ?? ''} options={actors.map(member => ({ value: member.id, label: member.configuration.display_name }))} onChange={id => { const member = actors.find(item => item.id === id); if (member) onGraph(current => assignExecutor(current, step.id, member)) }} /></div>}
      <label className="field"><span>{step.type === 'lead' ? '处理指令' : '交付要求'}</span><textarea className="input textarea" rows={5} value={String(step.config?.[step.type === 'lead' ? 'instruction' : 'result_requirement'] ?? '')} onChange={event => onChange({ ...step, config: { ...step.config, [step.type === 'lead' ? 'instruction' : 'result_requirement']: event.target.value } })} /></label>
      {loop ? <p className="muted small">输入沿用验证回路的本轮结果与上一轮验证意见。</p> : <section className="flow-editor__sources"><h3>交给它哪些内容</h3>
        <button type="button" className="button" aria-pressed={Object.values(inputs).some(binding => binding.value.source === 'run_input')} onClick={() => { const enabled = Object.values(inputs).some(binding => binding.value.source === 'run_input'); const next = Object.fromEntries(Object.entries(inputs).filter(([, binding]) => binding.value.source !== 'run_input')); if (!enabled) next.original = originalBinding(); onChange({ ...step, inputs: next }) }}>本次任务输入</button>
        {predecessors(graph, step.id).map(prior => <button type="button" className="button" key={prior.id} aria-pressed={Object.values(inputs).some(binding => binding.value.node_id === prior.id)} onClick={() => { const enabled = Object.values(inputs).some(binding => binding.value.node_id === prior.id); const next = Object.fromEntries(Object.entries(inputs).filter(([, binding]) => binding.value.node_id !== prior.id)); if (!enabled) next[`result_${prior.id.replace(/[^a-zA-Z0-9_]/g, '_')}`] = { value: { source: 'node_output', node_id: prior.id, path: '' }, expected_type: prior.type === 'join' ? 'json' : 'text' }; onChange({ ...step, inputs: next }) }}>{prior.label || stepLabel(prior.type)}的结果</button>)}
      </section>}
    </> : null}
    {step.type === 'parallel' ? <p className="muted">分支成员并行执行，再进入汇合步骤。</p> : null}
    {step.type === 'join' ? <>
      <div className="field"><span>汇合条件</span><Select label="汇合条件" value={String(step.config?.policy ?? 'all_success')} options={[{ value: 'all_success', label: '全部成功后继续' }, { value: 'fail_fast', label: '任一失败即停止' }, { value: 'quorum', label: '指定数量成功后继续' }, { value: 'deadline', label: '等待到截止时间' }]} onChange={policy => { const config: Record<string, unknown> = { ...step.config, policy }; delete config.success_count; delete config.deadline_seconds; onChange({ ...step, config: { ...config, ...(policy === 'quorum' ? { success_count: 1 } : {}), ...(policy === 'deadline' ? { deadline_seconds: 300 } : {}) } }) }} /></div>
      {step.config?.policy === 'quorum' ? <label className="field"><span>所需成功分支数</span><input className="input" type="number" min={1} max={graph.edges.filter(edge => edge.to_node_id === step.id).length} value={Number(step.config.success_count ?? 1)} onChange={event => onChange({ ...step, config: { ...step.config, success_count: Number(event.target.value) } })} /></label> : null}
      {step.config?.policy === 'deadline' ? <label className="field"><span>最长等待时间（秒）</span><input className="input" type="number" min={1} value={Number(step.config.deadline_seconds ?? 300)} onChange={event => onChange({ ...step, config: { ...step.config, deadline_seconds: Number(event.target.value) } })} /></label> : null}
    </> : null}
    {step.type === 'deliver' ? loop ? <p>交付最后一轮的验证结论。</p> : <>
      <div className="field"><span>交付哪一步的结果</span><Select label="交付来源" value={String((step.config?.result as { node_id?: string })?.node_id ?? '')} options={predecessors(graph, step.id).map(prior => ({ value: prior.id, label: prior.label || stepLabel(prior.type) }))} onChange={id => configureDelivery(graph.result_protocol === WORKBENCH_RESULT_PROTOCOL, id)} /></div>
      <div className="field"><span>结果分类</span><Select label="结果分类" value={graph.result_protocol === WORKBENCH_RESULT_PROTOCOL ? WORKBENCH_RESULT_PROTOCOL : 'ordinary'} options={[{ value: 'ordinary', label: '普通结果' }, { value: WORKBENCH_RESULT_PROTOCOL, label: '可要求补充材料' }]} onChange={value => configureDelivery(value === WORKBENCH_RESULT_PROTOCOL)} /></div>
      {issue ? <InlineError message={issue} /> : null}
    </> : null}
  </div>
}
