import { Plus, Trash2, X } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { Badge, Select, Switch } from '../ui'
import { engineName } from '../../lib/format'
import type { RuntimeNode } from '../../lib/nodes'
import { describeCapability, handoffKinds, isCLIEngine, loadModelCatalog, memberProblems, parseOutputSchema, toolLoopLimits, type DevelopmentDocument, type DevelopmentMember, type MemberConfiguration, type MemberRelationship, type MemberSkill } from '../../lib/teams'
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
const tabs = [{ id: 'duty', label: '职责' }, { id: 'ability', label: '能力' }, { id: 'run', label: '执行' }] as const
type TabId = typeof tabs[number]['id']

type Edit = { onConfig(id: string, patch: Partial<MemberConfiguration>): void; onRelationship(id: string, patch: Partial<MemberRelationship>): void }

const count = (value: unknown) => Array.isArray(value) ? value.filter((item) => typeof item === 'string' && item.trim()).length : 0

export function MemberEditor({ document, nodes, accepting, onConfig, onRelationship }: { document: DevelopmentDocument; nodes: RuntimeNode[]; accepting: Record<string, number> } & Edit) {
  const ordered = useMemo(() => [...document.members].sort((a, b) => Number(b.configuration.role === 'avatar') - Number(a.configuration.role === 'avatar')), [document.members])
  const [selected, setSelected] = useState(ordered[0]?.id)
  const [models, setModels] = useState<string[]>([])
  useEffect(() => { void loadModelCatalog().then(setModels).catch(() => setModels([])) }, [])
  const member = ordered.find((item) => item.id === selected) ?? ordered[0]
  if (!member) return <p className="muted">团队还没有成员。</p>
  return <div className="members">
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
    <MemberDetail key={member.id} member={member} nodes={nodes} accepting={accepting} models={models} onConfig={(patch) => onConfig(member.id, patch)} onRelationship={(patch) => onRelationship(member.id, patch)} />
  </div>
}

function MemberDetail({ member, nodes, accepting, models, onConfig, onRelationship }: { member: DevelopmentMember; nodes: RuntimeNode[]; accepting: Record<string, number>; models: string[]; onConfig(patch: Partial<MemberConfiguration>): void; onRelationship(patch: Partial<MemberRelationship>): void }) {
  const [tab, setTab] = useState<TabId>('duty')
  const config = member.configuration, relation = member.relationship
  const lead = config.role === 'avatar', cli = isCLIEngine(config.engine)
  const name = config.display_name || '未命名成员'
  const problems = memberProblems(member)
  return <section className="members__detail" aria-label={`${name}配置`}>
    <header className="members__header">
      <div className="members__title"><h2>{name}</h2>{lead ? <Badge tone="accent">负责人</Badge> : null}{!lead && relation.enabled === false ? <Badge>不参与</Badge> : null}</div>
      <p className="muted small">{engineName(config.engine)}{config.model ? ` · ${config.model}` : ''}</p>
    </header>
    {problems.length ? <ul className="readiness">{problems.map((problem) => <li key={problem}>{problem}</li>)}</ul> : null}
    <div className="tabs" role="tablist" aria-label={`${name}的配置`}>
      {tabs.map((item) => <button key={item.id} type="button" role="tab" aria-selected={tab === item.id} onClick={() => setTab(item.id)}>{item.label}</button>)}
    </div>
    {tab === 'duty' ? <DutyPanel member={member} onConfig={onConfig} onRelationship={onRelationship} /> : null}
    {tab === 'ability' ? <AbilityPanel member={member} cli={cli} onConfig={onConfig} /> : null}
    {tab === 'run' ? <RunPanel member={member} nodes={nodes} accepting={accepting} models={models} onConfig={onConfig} onRelationship={onRelationship} /> : null}
  </section>
}

function DutyPanel({ member, onConfig, onRelationship }: { member: DevelopmentMember; onConfig(patch: Partial<MemberConfiguration>): void; onRelationship(patch: Partial<MemberRelationship>): void }) {
  const config = member.configuration, relation = member.relationship, lead = config.role === 'avatar'
  const stored = config.output_schema ? JSON.stringify(config.output_schema, null, 2) : ''
  const [schema, setSchema] = useState(stored)
  const [schemaError, setSchemaError] = useState('')
  const editSchema = (text: string) => {
    setSchema(text)
    const parsed = parseOutputSchema(text)
    setSchemaError(parsed.error ?? '')
    if (!parsed.error) onConfig({ output_schema: parsed.value })
  }
  return <div className="members__panel" role="tabpanel" aria-label="职责">
    <section className="members__section" aria-label="职责与协作">
      <label className="field"><span>名称</span><input className="input" value={config.display_name} maxLength={80} onChange={(event) => onConfig({ display_name: event.target.value })} /></label>
      <label className="field"><span>职责</span><textarea className="input textarea" rows={3} value={relation.duty ?? ''} onChange={(event) => onRelationship({ duty: event.target.value })} /></label>
      <label className="field"><span>工作方法</span><textarea className="input textarea" rows={8} value={config.system_prompt ?? ''} onChange={(event) => onConfig({ system_prompt: event.target.value })} /></label>
    </section>
    {lead ? null : <section className="members__section" aria-label="被流程调用时">
      <h3>被流程调用时</h3>
      <label className="field"><span>何时参与</span><textarea className="input textarea" rows={2} value={relation.when_to_use ?? ''} onChange={(event) => onRelationship({ when_to_use: event.target.value })} /></label>
      <label className="field"><span>需要带上的上下文</span><textarea className="input textarea" rows={2} value={relation.context_instruction ?? ''} onChange={(event) => onRelationship({ context_instruction: event.target.value })} /></label>
      <label className="field"><span>结果要求</span><textarea className="input textarea" rows={3} value={relation.result_requirement ?? ''} onChange={(event) => onRelationship({ result_requirement: event.target.value })} /></label>
      <Switch checked={relation.enabled !== false} label="参与团队工作" onChange={(enabled) => onRelationship({ enabled })} />
    </section>}
    <section className="members__section" aria-label="输出结构">
      <h3>输出结构</h3>
      <label className="field"><span>JSON Schema（留空表示自由文本）</span>
        <textarea className="input textarea mono" rows={8} spellCheck={false} value={schema} aria-invalid={Boolean(schemaError)} onChange={(event) => editSchema(event.target.value)} /></label>
      {schemaError ? <p className="members__error" role="alert">{schemaError}</p> : null}
    </section>
  </div>
}

function AbilityPanel({ member, cli, onConfig }: { member: DevelopmentMember; cli: boolean; onConfig(patch: Partial<MemberConfiguration>): void }) {
  const config = member.configuration
  const actions = (config.business_capability_ids ?? []).filter((id) => typeof id === 'string' && id.trim())
  const bindings = config.business_capability_bindings ?? []
  const deny = config.permission_deny ?? []
  const skills = config.skills ?? []
  const external = [
    ['MCP 服务', config.mcp_server_ids], ['技能库技能', config.skill_names], ['允许的工具', config.permission_allow], ['需确认的工具', config.permission_ask],
  ] as const
  const attached = external.filter(([, value]) => count(value) > 0)
  const editSkill = (index: number, patch: Partial<MemberSkill>) => onConfig({ skills: skills.map((skill, at) => at === index ? { ...skill, ...patch } : skill) })
  const removeAction = (id: string) => onConfig({
    business_capability_ids: actions.filter((item) => item !== id),
    business_capability_bindings: bindings.filter((binding) => binding.capability_id !== id),
  })
  return <div className="members__panel" role="tabpanel" aria-label="能力">
    <section className="members__section" aria-label="业务动作">
      <h3>业务动作</h3>
      {actions.length ? <ul className="members__actions">{actions.map((id) => {
        const info = describeCapability(id)
        const mapped = bindings.find((binding) => binding.capability_id === id)?.parameters ?? []
        return <li key={id} className="members__action">
          <div className="members__action-main">
            <strong>{info.action}</strong>
            <span className="muted small mono">{info.object || id}</span>
            {mapped.length ? <span className="members__chips">{mapped.map((parameter) => <span key={parameter.name} className="members__chip"><code>{parameter.name}</code> ← <code>{parameter.source}</code></span>)}</span> : null}
          </div>
          <button type="button" className="icon-button" aria-label={`移除业务动作 ${info.action}`} onClick={() => removeAction(id)}><X size={14} /></button>
        </li>
      })}</ul> : <p className="muted">还没有分配业务动作，这个成员只能整理和分析，不能写入业务系统。</p>}
    </section>

    {cli ? null : <section className="members__section" aria-label="技能">
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
    </section>}

    <section className="members__section" aria-label="工具权限">
      <h3>工具权限</h3>
      <Switch checked={deny.includes('*')} label="禁止使用工具" onChange={(on) => onConfig({ permission_deny: on ? [...deny.filter((item) => item !== '*'), '*'] : deny.filter((item) => item !== '*') })} />
      {attached.length ? <div className="members__attached" role="status">
        <p>该成员带有试跑暂不支持的外部工具配置，发布前需要移除：</p>
        <ul>{attached.map(([label, value]) => <li key={label}>{label}：{(value as string[]).filter(Boolean).join('、')}</li>)}</ul>
        <button type="button" className="button" onClick={() => onConfig({ mcp_server_ids: [], skill_names: [], permission_allow: [], permission_ask: [] })}>移除这些配置</button>
      </div> : null}
    </section>
  </div>
}

function RunPanel({ member, nodes, accepting, models, onConfig, onRelationship }: { member: DevelopmentMember; nodes: RuntimeNode[]; accepting: Record<string, number>; models: string[]; onConfig(patch: Partial<MemberConfiguration>): void; onRelationship(patch: Partial<MemberRelationship>): void }) {
  const config = member.configuration, relation = member.relationship
  const lead = config.role === 'avatar', cli = isCLIEngine(config.engine)
  const name = config.display_name || '未命名成员'
  const engineNodes = nodes.filter((node) => node.engine_readiness.some((engine) => engine.engine === config.engine)).map((node) => ({ value: node.id, label: node.name, detail: node.accepting ? '可接任务' : '暂不可接' }))
  const modelOptions = [...(lead ? [{ value: '', label: '不指定' }] : []), ...[...new Set([config.model, ...models].filter(Boolean))].map((model) => ({ value: model, label: model }))]
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
  return <div className="members__panel" role="tabpanel" aria-label="执行">
    <section className="members__section" aria-label="引擎与模型">
      <h3>引擎与模型</h3>
      <div className="members__grid">
        <div className="field"><span>引擎</span><Select label={`${name}的引擎`} value={config.engine} disabled={lead} options={engines.map((engine) => ({ value: engine, label: engineName(engine), detail: isCLIEngine(engine) ? `${accepting[engine] ?? 0} 个节点可接` : undefined }))} onChange={(engine) => onConfig({ engine, runtime_id: '' })} /></div>
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
        <Switch checked={Boolean(config.memory_enabled)} label="启用记忆" onChange={(memory_enabled) => onConfig({ memory_enabled, memory_scope: config.memory_scope || 'tenant' })} />
        {config.memory_enabled ? <div className="field"><span>记忆范围</span><Select label="记忆范围" value={config.memory_scope || 'tenant'} options={memoryScopes} onChange={(memory_scope) => onConfig({ memory_scope })} /></div> : null}
      </section>
    </>}

    {lead ? null : <section className="members__section" aria-label="允许的交接方式">
      <h3>允许的交接方式</h3>
      <p className="muted small">流程里的步骤只能用这里勾选的方式调用该成员，发布时检查；依次执行的步骤是咨询，并行分支是派发。</p>
      <div className="members__grid">
        <div className="field"><span>允许</span><div className="members__choices" role="group" aria-label="交接方式">
          {handoffKinds.map((kind) => <button key={kind.value} type="button" className="button" aria-pressed={kinds.includes(kind.value)} onClick={() => toggleKind(kind.value)}>{kind.label}</button>)}
        </div></div>
        <div className="field"><span>新建步骤时默认</span><Select label="默认交接方式" value={defaultKind} options={handoffKinds.filter((kind) => kinds.includes(kind.value)).map((kind) => ({ value: kind.value, label: kind.label }))} onChange={(kind) => onRelationship({ allowed_kinds: kinds, default_kind: kind })} /></div>
      </div>
    </section>}
  </div>
}
