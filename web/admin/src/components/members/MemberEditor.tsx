import { ChevronDown, ChevronRight, Plus, Trash2, X } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { Badge, Checkbox, Dialog, Select, Switch } from '../ui'
import { engineName } from '../../lib/format'
import type { RuntimeNode } from '../../lib/nodes'
import { fieldTypes, fieldsProblem, fieldsSchema, schemaFields, type OutputField } from '../../lib/output-format'
import { CatalogRefresh } from '../team/CatalogRefresh'
import { bindingSourceLabel, capabilityName, defaultCapabilityBinding, describeCapability, handoffKinds, isCLIEngine, loadModelCatalog, memberProblems, memberSteps, newMember, parseOutputSchema, requiredHandoffKinds, toolLoopLimits, type BusinessCatalog, type CatalogCapability, type DevelopmentDocument, type DevelopmentMember, type MemberConfiguration, type MemberRelationship, type MemberSkill } from '../../lib/teams'
import './members.css'

const engines = ['claude', 'codex', 'opencode', 'loom']
const memoryScopes = [{ value: 'tenant', label: '工作区共享' }, { value: 'user', label: '按员工区分' }, { value: 'session', label: '仅本次会话' }]
const limits = [
  ['max_tokens', '累计 token 上限'],
  ['max_output_tokens', '单次回复 token 上限'],
  ['step_budget', '最多执行步数'],
  ['max_cost_usd', '费用上限（美元）'],
  ['max_tool_repeats', '同一工具连续调用上限'],
] as const
// Trial preparation fills these in when a worker has none.
const defaultKinds = ['consult', 'dispatch']
// What a newcomer has to fill in is on the page; everything an engine expert
// tunes sits behind this one switch, which opens by itself when it holds
// something that still needs attention.
const basicProblems = ['还没有填写职责', '还没有填写工作方法']

type Edit = {
  onConfig(id: string, patch: Partial<MemberConfiguration>): void
  onRelationship(id: string, patch: Partial<MemberRelationship>): void
  onAdd?(member: DevelopmentMember): void
  onRemove?(id: string): void
  onCatalog?(catalog: BusinessCatalog): void
}

const count = (value: unknown) => Array.isArray(value) ? value.filter((item) => typeof item === 'string' && item.trim()).length : 0

export function MemberEditor({ document, nodes, accepting, catalog, onConfig, onRelationship, onAdd, onRemove, onCatalog }: { document: DevelopmentDocument; nodes: RuntimeNode[]; accepting: Record<string, number>; catalog?: BusinessCatalog } & Edit) {
  const ordered = useMemo(() => [...document.members].sort((a, b) => Number(b.configuration.role === 'avatar') - Number(a.configuration.role === 'avatar')), [document.members])
  const [selected, setSelected] = useState(ordered[0]?.id)
  const [models, setModels] = useState<string[]>([])
  const [advanced, setAdvanced] = useState(false)
  useEffect(() => { void loadModelCatalog().then(setModels).catch(() => setModels([])) }, [])
  const member = ordered.find((item) => item.id === selected) ?? ordered[0]
  if (!member) return <p className="muted">团队还没有成员。</p>
  const add = () => {
    const created = newMember(models[0] ?? '')
    onAdd?.(created)
    setSelected(created.id)
  }
  return <div className="members">
    <div className="members__side">
    <ul className="members__list" aria-label="成员列表">{ordered.map((item) => {
      const problems = memberProblems(item)
      const actions = count(item.configuration.business_capability_ids)
      const lead = item.configuration.role === 'avatar'
      return <li key={item.id}><button type="button" className={`members__item${item.id === member.id ? ' is-selected' : ''}`} aria-pressed={item.id === member.id} onClick={() => setSelected(item.id)}>
        <span className="members__name">{item.configuration.display_name || '未命名成员'}</span>
        <span className="members__meta muted small">{lead ? '负责人' : item.relationship.enabled === false ? '不参与' : engineName(item.configuration.engine)}{actions ? ` · ${actions} 个业务动作` : ''}</span>
        {item.relationship.duty?.trim() ? <span className="members__duty muted small">{item.relationship.duty}</span> : null}
        {problems.length ? <span className="members__flag"><Badge tone="warning">待补充</Badge></span> : null}
      </button></li>
    })}</ul>
    {onAdd ? <button type="button" className="button" onClick={add}><Plus size={14} />添加成员</button> : null}
    </div>
    <MemberDetail key={member.id} member={member} nodes={nodes} accepting={accepting} models={models} catalog={catalog} kindsInUse={requiredHandoffKinds(document, member.id)} steps={memberSteps(document, member.id)} advanced={advanced} onAdvanced={setAdvanced}
      onConfig={(patch) => onConfig(member.id, patch)} onRelationship={(patch) => onRelationship(member.id, patch)} onCatalog={onCatalog}
      onRemove={onRemove && member.configuration.role !== 'avatar' ? () => onRemove(member.id) : undefined} />
  </div>
}

type DetailProps = { member: DevelopmentMember; onConfig(patch: Partial<MemberConfiguration>): void; onRelationship(patch: Partial<MemberRelationship>): void }

function MemberDetail({ member, nodes, accepting, models, catalog, kindsInUse, steps, advanced, onAdvanced, onConfig, onRelationship, onCatalog, onRemove }: DetailProps & { nodes: RuntimeNode[]; accepting: Record<string, number>; models: string[]; catalog?: BusinessCatalog; kindsInUse: string[]; steps: string[]; advanced: boolean; onAdvanced(open: boolean): void; onCatalog?(catalog: BusinessCatalog): void; onRemove?(): void }) {
  const [removing, setRemoving] = useState(false)
  const config = member.configuration, relation = member.relationship
  const lead = config.role === 'avatar', cli = isCLIEngine(config.engine)
  const name = config.display_name || '未命名成员'
  const pending = memberProblems(member).some((problem) => !basicProblems.includes(problem))
  const open = advanced || pending
  return <section className="members__detail" aria-label={`${name}配置`}>
    <header className="members__header">
      <div className="members__title"><h2>{name}</h2>{lead ? <Badge tone="accent">负责人</Badge> : null}{!lead && relation.enabled === false ? <Badge>不参与</Badge> : null}</div>
      <p className="muted small">{engineName(config.engine)}{config.model ? ` · ${config.model}` : ''}</p>
      {onRemove ? <button type="button" className="button members__remove" onClick={() => setRemoving(true)}><Trash2 size={14} />删除成员</button> : null}
    </header>
    {removing && onRemove ? <div className="members__confirm" role="alertdialog" aria-label={`确认删除 ${name}`}>
      {steps.length ? <><span>流程里的“{steps.join('”“')}”还由这个成员负责，请先在流程里换人或删除这些步骤。</span>
        <button type="button" className="button" onClick={() => setRemoving(false)}>知道了</button></>
        : <><span>删除后，这个成员的配置不会保留。</span>
          <button type="button" className="button" onClick={() => setRemoving(false)}>取消</button>
          <button type="button" className="button button--danger" onClick={onRemove}>删除</button></>}
    </div> : null}
    <div className="members__panel">
      <section className="members__section" aria-label="基本信息">
        <label className="field"><span>名称</span><input className="input" value={config.display_name} maxLength={80} onChange={(event) => onConfig({ display_name: event.target.value })} /></label>
        <label className="field"><span>职责</span><textarea className="input textarea" rows={2} value={relation.duty ?? ''} placeholder="一句话说清这个成员负责什么，例如：整理线索材料，列出客户需求和待确认项" onChange={(event) => onRelationship({ duty: event.target.value })} /></label>
        <label className="field"><span>工作方法</span><textarea className="input textarea" rows={7} value={config.system_prompt ?? ''} placeholder={'写给这个成员的做事规矩，例如：\n只依据本次任务的原话和材料。\n把事实、估计和待确认事项分开写。\n没有给出的金额和日期不要推测。'} onChange={(event) => onConfig({ system_prompt: event.target.value })} /></label>
        {lead ? null : <label className="field"><span>结果要求</span><textarea className="input textarea" rows={2} value={relation.result_requirement ?? ''} placeholder="这个成员交出的结果要包含什么，例如：按出处列出事实、判断和待确认项" onChange={(event) => onRelationship({ result_requirement: event.target.value })} /></label>}
        {lead ? null : <Switch checked={relation.enabled !== false} label="参与团队工作" onChange={(enabled) => onRelationship({ enabled })} />}
      </section>
      <AbilityPanel part="actions" member={member} cli={cli} catalog={catalog} onConfig={onConfig} onCatalog={onCatalog} />
      <OutputFormat key={member.id} member={member} onConfig={onConfig} />
      <button type="button" className="members__advanced" aria-expanded={open} onClick={() => onAdvanced(!open)}>
        {open ? <ChevronDown size={15} aria-hidden="true" /> : <ChevronRight size={15} aria-hidden="true" />}高级设置
        {pending ? <Badge tone="warning">有待补充</Badge> : null}
      </button>
      {open ? <>
        {lead ? null : <section className="members__section" aria-label="被流程调用时">
          <h3>被流程调用时</h3>
          <label className="field"><span>何时参与</span><textarea className="input textarea" rows={2} value={relation.when_to_use ?? ''} onChange={(event) => onRelationship({ when_to_use: event.target.value })} /></label>
          <label className="field"><span>需要带上的上下文</span><textarea className="input textarea" rows={2} value={relation.context_instruction ?? ''} onChange={(event) => onRelationship({ context_instruction: event.target.value })} /></label>
        </section>}
        <AbilityPanel part="tools" member={member} cli={cli} catalog={catalog} onConfig={onConfig} onCatalog={onCatalog} />
        <RunPanel member={member} nodes={nodes} accepting={accepting} models={models} kindsInUse={kindsInUse} onConfig={onConfig} onRelationship={onRelationship} />
      </> : null}
    </div>
  </section>
}

// A fixed answer format as a list of named fields. The same format stays
// editable as JSON for the shapes the list cannot express.
function OutputFormat({ member, onConfig }: { member: DevelopmentMember; onConfig(patch: Partial<MemberConfiguration>): void }) {
  const stored = member.configuration.output_schema
  const [fixed, setFixed] = useState(stored != null)
  const [fields, setFields] = useState<OutputField[] | undefined>(() => stored == null ? [] : schemaFields(stored))
  const [text, setText] = useState(() => stored ? JSON.stringify(stored, null, 2) : '')
  const [raw, setRaw] = useState(false)
  const [error, setError] = useState('')
  const write = (next: OutputField[]) => {
    setFields(next)
    const problem = next.length ? fieldsProblem(next) : '至少添加一项'
    setError(problem ?? '')
    if (problem) return
    const schema = fieldsSchema(next)
    setText(JSON.stringify(schema, null, 2))
    onConfig({ output_schema: schema })
  }
  const toggle = (on: boolean) => {
    setFixed(on)
    setError('')
    if (!on) { setFields([]); setText(''); setRaw(false); onConfig({ output_schema: null }) } else if (!fields?.length) setFields([{ name: '', description: '', type: 'string', required: true }])
  }
  const editText = (value: string) => {
    setText(value)
    const parsed = parseOutputSchema(value)
    setError(parsed.error ?? '')
    if (parsed.error) return
    setFields(parsed.value ? schemaFields(parsed.value) : [])
    setFixed(parsed.value != null)
    onConfig({ output_schema: parsed.value })
  }
  const edit = (index: number, patch: Partial<OutputField>) => write((fields ?? []).map((field, at) => at === index ? { ...field, ...patch } : field))
  return <section className="members__section" aria-label="交付格式">
    <h3>交付格式</h3>
    <Switch checked={fixed} label="按固定格式交付" onChange={toggle} />
    {!fixed ? <p className="muted small">不固定格式时，这个成员用自己的话写结果。</p> : fields ? <>
      {fields.map((field, index) => <div key={index} className="members__field" role="group" aria-label={`第 ${index + 1} 项`}>
        <label className="field"><span>名称</span><input className="input" value={field.name} maxLength={60} placeholder="例如：是否跟进" onChange={(event) => edit(index, { name: event.target.value })} /></label>
        <div className="field"><span>类型</span><Select label={`第 ${index + 1} 项的类型`} value={field.type} options={fieldTypes} onChange={(type) => edit(index, { type })} /></div>
        <Checkbox checked={field.required} label="必填" onChange={(required) => edit(index, { required })} />
        <button type="button" className="icon-button" aria-label={`删除第 ${index + 1} 项`} onClick={() => write(fields.filter((_, at) => at !== index))}><X size={14} /></button>
        <label className="field members__field-note"><span>说明</span><input className="input" value={field.description} placeholder="可不填" onChange={(event) => edit(index, { description: event.target.value })} /></label>
      </div>)}
      <div className="toolbar"><button type="button" className="button" onClick={() => write([...fields, { name: '', description: '', type: 'string', required: true }])}><Plus size={14} />添加一项</button></div>
    </> : <p className="muted small">这个格式不是简单的几项内容，只能直接编辑 JSON。</p>}
    {fixed ? <div className="toolbar"><button type="button" className="link-button small" aria-expanded={raw || !fields} onClick={() => setRaw(!raw)}>直接编辑 JSON</button></div> : null}
    {fixed && (raw || !fields) ? <label className="field"><span>JSON Schema</span>
      <textarea className="input textarea mono" rows={8} spellCheck={false} value={text} aria-invalid={Boolean(error)} onChange={(event) => editText(event.target.value)} /></label> : null}
    {error ? <p className="members__error" role="alert">{error}</p> : null}
  </section>
}

function AbilityPanel({ part, member, cli, catalog, onConfig, onCatalog }: { part: 'actions' | 'tools'; member: DevelopmentMember; cli: boolean; catalog?: BusinessCatalog; onConfig(patch: Partial<MemberConfiguration>): void; onCatalog?(catalog: BusinessCatalog): void }) {
  const config = member.configuration
  const actions = (config.business_capability_ids ?? []).filter((id) => typeof id === 'string' && id.trim())
  const bindings = config.business_capability_bindings ?? []
  const deny = config.permission_deny ?? []
  const skills = config.skills ?? []
  const [removing, setRemoving] = useState<string>()
  const [picking, setPicking] = useState(false)
  const external = [
    ['MCP 服务', config.mcp_server_ids], ['技能库技能', config.skill_names], ['允许的工具', config.permission_allow], ['需确认的工具', config.permission_ask],
  ] as const
  const attached = external.filter(([, value]) => count(value) > 0)
  const editSkill = (index: number, patch: Partial<MemberSkill>) => onConfig({ skills: skills.map((skill, at) => at === index ? { ...skill, ...patch } : skill) })
  const removeAction = (id: string) => onConfig({
    business_capability_ids: actions.filter((item) => item !== id),
    business_capability_bindings: bindings.filter((binding) => binding.capability_id !== id),
  })
  const addAction = (capability: CatalogCapability) => {
    const binding = defaultCapabilityBinding(capability)
    onConfig({
      business_capability_ids: [...actions, capability.id],
      business_capability_bindings: binding ? [...bindings.filter((item) => item.capability_id !== capability.id), binding] : bindings,
    })
    setPicking(false)
  }
  // Only actions a team may carry out on an employee's behalf can be added.
  const addable = (catalog?.capabilities ?? []).filter((capability) => capability.status === 'available' && capability.executionMode !== 'employee_only' && !actions.includes(capability.id))
  return <>
    {part === 'actions' ? <section className="members__section" aria-label="业务动作">
      <h3>业务动作</h3>
      {actions.length ? <ul className="members__actions">{actions.map((id) => {
        const known = catalog?.capabilities.find((capability) => capability.id === id)
        const title = capabilityName(id, catalog)
        const mapped = bindings.find((binding) => binding.capability_id === id)?.parameters ?? []
        const parameterName = (name: string) => known?.params?.find((parameter) => parameter.name === name)?.label || name
        return <li key={id} className="members__action">
          <div className="members__action-main">
            <strong>{title}</strong>
            {known?.description ? <span className="muted small">{known.description}</span> : null}
            {mapped.length ? <span className="members__chips">{mapped.map((parameter) => <span key={parameter.name} className="members__chip">{parameterName(parameter.name)}：取自{bindingSourceLabel(parameter.source)}</span>)}</span> : null}
            {removing === id ? <div className="members__confirm" role="alertdialog" aria-label={`确认移除 ${title}`}>
              <span>移除后，这个成员不能再办理“{title}”。</span>
              <button type="button" className="button" onClick={() => setRemoving(undefined)}>取消</button>
              <button type="button" className="button button--danger" onClick={() => { removeAction(id); setRemoving(undefined) }}>移除</button>
            </div> : null}
          </div>
          {removing === id ? null : <button type="button" className="icon-button" aria-label={`移除业务动作 ${title}`} onClick={() => setRemoving(id)}><X size={14} /></button>}
        </li>
      })}</ul> : <p className="muted">还没有分配业务动作，这个成员只能整理和分析，不能写入业务系统。</p>}
      <div className="toolbar">
        <button type="button" className="button" disabled={!addable.length} onClick={() => setPicking(true)}><Plus size={14} />添加业务动作</button>
        {onCatalog ? <CatalogRefresh catalog={catalog} onRefreshed={onCatalog} /> : null}
        {!catalog?.available ? <span className="muted small">还没有读到业务动作目录。</span>
          : !addable.length ? <span className="muted small">目录里没有可再添加的业务动作。</span> : null}
      </div>
    </section> : null}

    {part === 'tools' && !cli ? <section className="members__section" aria-label="技能">
      <h3>技能</h3>
      {skills.map((skill, index) => <div key={index} className="members__skill" role="group" aria-label={`技能 ${index + 1}`}>
        <div className="members__grid">
          <label className="field"><span>技能名称</span><input className="input" value={skill.name ?? ''} maxLength={80} onChange={(event) => editSkill(index, { name: event.target.value })} /></label>
          <label className="field"><span>适用说明</span><input className="input" value={skill.description ?? ''} onChange={(event) => editSkill(index, { description: event.target.value })} /></label>
        </div>
        <label className="field"><span>技能内容</span><textarea className="input textarea" rows={5} value={skill.body ?? ''} onChange={(event) => editSkill(index, { body: event.target.value })} /></label>
        <div className="toolbar">
          <Switch checked={Boolean(skill.always_active)} label="始终启用" onChange={(always_active) => editSkill(index, { always_active })} />
          <button type="button" className="button" onClick={() => onConfig({ skills: skills.filter((_, at) => at !== index) })}><Trash2 size={14} />删除技能</button>
        </div>
      </div>)}
      <div className="toolbar"><button type="button" className="button" onClick={() => onConfig({ skills: [...skills, { name: '', description: '', body: '', always_active: false }] })}><Plus size={14} />添加技能</button></div>
    </section> : null}

    {part === 'tools' ? <section className="members__section" aria-label="工具权限">
      <h3>工具权限</h3>
      <Switch checked={deny.includes('*')} label="禁止使用工具" onChange={(on) => onConfig({ permission_deny: on ? [...deny.filter((item) => item !== '*'), '*'] : deny.filter((item) => item !== '*') })} />
      {attached.length ? <div className="members__attached" role="status">
        <p>该成员带有试跑暂不支持的外部工具配置，发布前需要移除：</p>
        <ul>{attached.map(([label, value]) => <li key={label}>{label}：{(value as string[]).filter(Boolean).join('、')}</li>)}</ul>
        <button type="button" className="button" onClick={() => onConfig({ mcp_server_ids: [], skill_names: [], permission_allow: [], permission_ask: [] })}>移除这些配置</button>
      </div> : null}
    </section> : null}

    {picking ? <Dialog title="添加业务动作" onClose={() => setPicking(false)}>
      <ul className="members__catalog">{addable.map((capability) => <li key={capability.id}>
        <button type="button" className="members__catalog-item" onClick={() => addAction(capability)}>
          <span className="members__action-main"><strong>{capability.name || describeCapability(capability.id).action}</strong>{capability.description ? <span className="muted small">{capability.description}</span> : null}</span>
          <Badge tone={capability.effect === 'write' ? 'warning' : 'neutral'}>{capability.effect === 'write' ? '会写入' : '只读取'}</Badge>
        </button>
      </li>)}</ul>
    </Dialog> : null}
  </>
}

function RunPanel({ member, nodes, accepting, models, kindsInUse, onConfig, onRelationship }: DetailProps & { nodes: RuntimeNode[]; accepting: Record<string, number>; models: string[]; kindsInUse: string[] }) {
  const config = member.configuration, relation = member.relationship
  const lead = config.role === 'avatar', cli = isCLIEngine(config.engine)
  const name = config.display_name || '未命名成员'
  const engineNodes = nodes.filter((node) => node.engine_readiness.some((engine) => engine.engine === config.engine)).map((node) => ({ value: node.id, label: node.name, detail: node.accepting ? '可接任务' : '暂不可接' }))
  const modelOptions = [...(lead ? [{ value: '', label: '不指定' }] : []), ...[...new Set([config.model, ...models].filter(Boolean))].map((model) => ({ value: model, label: model }))]
  // An engine that runs on nodes is offered only while a node can take its work.
  const engineOptions = engines.filter((engine) => engine === config.engine || !isCLIEngine(engine) || accepting[engine])
    .map((engine) => ({ value: engine, label: engineName(engine), detail: isCLIEngine(engine) ? `${accepting[engine] ?? 0} 个节点可接` : undefined }))
  const memoryScope = memoryScopes.some((scope) => scope.value === config.memory_scope) ? config.memory_scope! : 'tenant'
  const kinds = relation.allowed_kinds?.length ? relation.allowed_kinds : defaultKinds
  const defaultKind = relation.allowed_kinds?.length ? relation.default_kind ?? '' : 'dispatch'
  const toggleKind = (kind: string) => {
    const next: string[] = handoffKinds.map((item) => item.value).filter((value) => value === kind ? !kinds.includes(value) : kinds.includes(value))
    if (!next.length) return
    onRelationship({ allowed_kinds: next, default_kind: next.includes(defaultKind) ? defaultKind : next[0] })
  }
  const setLimit = (key: typeof limits[number][0], raw: string) => {
    const value = Number(raw)
    onConfig({ [key]: raw.trim() === '' || !Number.isFinite(value) || value < 0 ? 0 : key === 'max_cost_usd' ? value : Math.floor(value) })
  }
  const loopControl = config.tool_loop_control
  const setLoop = (patch: Partial<NonNullable<typeof loopControl>>) => onConfig({ tool_loop_control: { slice_rounds: 20, initial_total_rounds: 20, ...loopControl, ...patch } })
  const locked = handoffKinds.filter((kind) => kindsInUse.includes(kind.value) && kinds.includes(kind.value)).map((kind) => kind.label)
  return <>
    <section className="members__section" aria-label="引擎与模型">
      <h3>引擎与模型</h3>
      <div className="members__grid">
        <div className="field"><span>引擎</span><Select label={`${name}的引擎`} value={config.engine} disabled={lead} options={engineOptions} onChange={(engine) => onConfig({ engine, runtime_id: '' })} /></div>
        {cli ? <div className="field"><span>节点</span><Select label={`${name}的节点`} value={config.runtime_id ?? ''} placeholder={engineNodes.length ? '选择节点' : '没有提供该引擎的节点'} options={engineNodes} onChange={(runtime) => onConfig({ runtime_id: runtime })} /></div> : null}
        {cli
          ? <label className="field"><span>模型</span><input className="input" value={config.model ?? ''} placeholder="留空使用节点上的默认模型" onChange={(event) => onConfig({ model: event.target.value })} /></label>
          : <div className="field"><span>模型</span><Select label={`${name}的模型`} value={config.model ?? ''} placeholder={modelOptions.length ? '选择模型' : '还没有可用模型'} options={modelOptions} onChange={(model) => onConfig({ model })} /></div>}
      </div>
    </section>

    {cli ? null : <>
      <section className="members__section" aria-label="执行限制">
        <h3>执行限制</h3>
        <div className="members__grid">{limits.map(([key, label]) => <label key={key} className="field"><span>{label}</span>
          <input className="input" inputMode={key === 'max_cost_usd' ? 'decimal' : 'numeric'} value={config[key] ? String(config[key]) : ''} placeholder="不限制" onChange={(event) => setLimit(key, event.target.value)} /></label>)}</div>
      </section>

      <section className="members__section" aria-label="工具循环">
        <h3>工具循环</h3>
        <Switch checked={Boolean(loopControl)} label="自定义工具循环轮次" onChange={(on) => onConfig({ tool_loop_control: on ? { slice_rounds: 20, initial_total_rounds: 20 } : null })} />
        {loopControl ? <div className="members__grid">
          <label className="field"><span>每片最多轮次（1 到 {toolLoopLimits.sliceMax}）</span><input className="input" inputMode="numeric" value={String(loopControl.slice_rounds)} onChange={(event) => setLoop({ slice_rounds: Math.floor(Number(event.target.value)) || 0 })} /></label>
          <label className="field"><span>总轮次上限</span><input className="input" inputMode="numeric" value={String(loopControl.initial_total_rounds)} onChange={(event) => setLoop({ initial_total_rounds: Math.floor(Number(event.target.value)) || 0 })} /></label>
        </div> : <p className="muted small">未自定义时使用平台默认的工具循环轮次。</p>}
      </section>

      <section className="members__section" aria-label="记忆">
        <h3>记忆</h3>
        <Switch checked={Boolean(config.memory_enabled)} label="启用记忆" onChange={(memory_enabled) => onConfig({ memory_enabled, memory_scope: memoryScope })} />
        {config.memory_enabled ? <div className="field"><span>记忆范围</span><Select label="记忆范围" value={memoryScope} options={memoryScopes} onChange={(memory_scope) => onConfig({ memory_scope })} /></div> : null}
      </section>
    </>}

    {lead ? null : <section className="members__section" aria-label="允许的交接方式">
      <h3>允许的交接方式</h3>
      <p className="muted small">流程按步骤所在位置决定用哪种方式：依次执行的步骤是咨询，并行分支是派发。把成员放进流程时会自动允许所需的方式，一般不用改这里。</p>
      <div className="members__grid">
        <div className="field"><span>允许</span><div className="members__choices" role="group" aria-label="交接方式">
          {handoffKinds.map((kind) => <button key={kind.value} type="button" className="button" aria-pressed={kinds.includes(kind.value)} disabled={kindsInUse.includes(kind.value) && kinds.includes(kind.value)} onClick={() => toggleKind(kind.value)}>{kind.label}</button>)}
        </div></div>
        <div className="field"><span>新建步骤时默认</span><Select label="默认交接方式" value={defaultKind} options={handoffKinds.filter((kind) => kinds.includes(kind.value)).map((kind) => ({ value: kind.value, label: kind.label }))} onChange={(kind) => onRelationship({ allowed_kinds: kinds, default_kind: kind })} /></div>
      </div>
      {locked.length ? <p className="muted small">流程里已有步骤在用“{locked.join('”“')}”，不能取消；要取消请先在流程里调整。</p> : null}
    </section>}
  </>
}
