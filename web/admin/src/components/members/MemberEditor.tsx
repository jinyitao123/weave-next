import { Plus, Trash2 } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { Badge, Select, Switch } from '../ui'
import { engineName } from '../../lib/format'
import type { RuntimeNode } from '../../lib/nodes'
import { handoffKinds, isCLIEngine, loadModelCatalog, memberProblems, type DevelopmentDocument, type DevelopmentMember, type MemberConfiguration, type MemberRelationship, type MemberSkill } from '../../lib/teams'
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

type Edit = { onConfig(id: string, patch: Partial<MemberConfiguration>): void; onRelationship(id: string, patch: Partial<MemberRelationship>): void }

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
      return <li key={item.id}><button type="button" className={`members__item${item.id === member.id ? ' is-selected' : ''}`} aria-pressed={item.id === member.id} onClick={() => setSelected(item.id)}>
        <span className="members__name">{item.configuration.display_name || '未命名成员'}</span>
        <span className="members__meta muted small">{item.configuration.role === 'avatar' ? '负责人' : item.relationship.enabled === false ? '不参与' : engineName(item.configuration.engine)}</span>
        {problems.length ? <span className="members__flag"><Badge tone="warning">待补充</Badge></span> : null}
      </button></li>
    })}</ul>
    <MemberDetail key={member.id} member={member} nodes={nodes} accepting={accepting} models={models} onConfig={(patch) => onConfig(member.id, patch)} onRelationship={(patch) => onRelationship(member.id, patch)} />
  </div>
}

function MemberDetail({ member, nodes, accepting, models, onConfig, onRelationship }: { member: DevelopmentMember; nodes: RuntimeNode[]; accepting: Record<string, number>; models: string[]; onConfig(patch: Partial<MemberConfiguration>): void; onRelationship(patch: Partial<MemberRelationship>): void }) {
  const config = member.configuration, relation = member.relationship
  const lead = config.role === 'avatar', cli = isCLIEngine(config.engine)
  const name = config.display_name || '未命名成员'
  const problems = memberProblems(member)
  const engineNodes = nodes.filter((node) => node.engine_readiness.some((engine) => engine.engine === config.engine)).map((node) => ({ value: node.id, label: node.name, detail: node.accepting ? '可接任务' : '暂不可接' }))
  const modelOptions = [...(lead ? [{ value: '', label: '不指定' }] : []), ...[...new Set([config.model, ...models].filter(Boolean))].map((model) => ({ value: model, label: model }))]
  const kinds = relation.allowed_kinds?.length ? relation.allowed_kinds : defaultKinds
  const defaultKind = relation.allowed_kinds?.length ? relation.default_kind ?? '' : 'dispatch'
  const toggleKind = (kind: string) => {
    const next = handoffKinds.map((item) => item.value).filter((value) => value === kind ? !kinds.includes(value) : kinds.includes(value))
    if (!next.length) return
    onRelationship({ allowed_kinds: next, default_kind: next.includes(defaultKind) ? defaultKind : next[0] })
  }
  const deny = config.permission_deny ?? []
  const skills = config.skills ?? []
  const editSkill = (index: number, patch: Partial<MemberSkill>) => onConfig({ skills: skills.map((skill, at) => at === index ? { ...skill, ...patch } : skill) })
  const setLimit = (key: typeof limits[number][0], raw: string) => {
    const value = Number(raw)
    onConfig({ [key]: raw.trim() === '' || !Number.isFinite(value) || value < 0 ? 0 : key === 'max_cost_usd' ? value : Math.floor(value) })
  }

  return <section className="members__detail" aria-label={`${name}配置`}>
    <header className="members__header"><h2>{name}</h2>{lead ? <Badge tone="accent">负责人</Badge> : null}</header>
    {problems.length ? <ul className="readiness">{problems.map((problem) => <li key={problem}>{problem}</li>)}</ul> : null}

    <section className="members__section" aria-label="职责与协作">
      <h3>职责与协作</h3>
      <label className="field"><span>名称</span><input className="input" value={config.display_name} maxLength={80} onChange={(event) => onConfig({ display_name: event.target.value })} /></label>
      <label className="field"><span>职责</span><textarea className="input textarea" rows={3} value={relation.duty ?? ''} onChange={(event) => onRelationship({ duty: event.target.value })} /></label>
      <label className="field"><span>工作方法</span><textarea className="input textarea" rows={6} value={config.system_prompt ?? ''} onChange={(event) => onConfig({ system_prompt: event.target.value })} /></label>
      {lead ? null : <>
        <label className="field"><span>何时参与</span><textarea className="input textarea" rows={2} value={relation.when_to_use ?? ''} onChange={(event) => onRelationship({ when_to_use: event.target.value })} /></label>
        <label className="field"><span>需要带上的上下文</span><textarea className="input textarea" rows={2} value={relation.context_instruction ?? ''} onChange={(event) => onRelationship({ context_instruction: event.target.value })} /></label>
        <label className="field"><span>结果要求</span><textarea className="input textarea" rows={3} value={relation.result_requirement ?? ''} onChange={(event) => onRelationship({ result_requirement: event.target.value })} /></label>
        <div className="members__grid">
          <div className="field"><span>交接方式</span><div className="members__choices" role="group" aria-label="交接方式">
            {handoffKinds.map((kind) => <button key={kind.value} type="button" className="button" aria-pressed={kinds.includes(kind.value)} onClick={() => toggleKind(kind.value)}>{kind.label}</button>)}
          </div></div>
          <div className="field"><span>默认交接方式</span><Select label="默认交接方式" value={defaultKind} options={handoffKinds.filter((kind) => kinds.includes(kind.value)).map((kind) => ({ value: kind.value, label: kind.label }))} onChange={(kind) => onRelationship({ allowed_kinds: kinds, default_kind: kind })} /></div>
        </div>
        <Switch checked={relation.enabled !== false} label="参与团队工作" onChange={(enabled) => onRelationship({ enabled })} />
      </>}
    </section>

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

    <section className="members__section" aria-label="工具权限">
      <h3>工具权限</h3>
      <Switch checked={deny.includes('*')} label="禁止使用工具" onChange={(on) => onConfig({ permission_deny: on ? [...deny.filter((item) => item !== '*'), '*'] : deny.filter((item) => item !== '*') })} />
    </section>

    {cli ? null : <>
      <section className="members__section" aria-label="技能">
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
      </section>

      <section className="members__section" aria-label="执行限制">
        <h3>执行限制</h3>
        <div className="members__grid">{limits.map(([key, label]) => <label key={key} className="field"><span>{label}</span>
          <input className="input" inputMode={key === 'max_cost_usd' ? 'decimal' : 'numeric'} value={config[key] ? String(config[key]) : ''} placeholder="不限制" onChange={(event) => setLimit(key, event.target.value)} /></label>)}</div>
      </section>

      <section className="members__section" aria-label="记忆">
        <h3>记忆</h3>
        <Switch checked={Boolean(config.memory_enabled)} label="启用记忆" onChange={(memory_enabled) => onConfig({ memory_enabled, memory_scope: config.memory_scope || 'tenant' })} />
        {config.memory_enabled ? <div className="field"><span>记忆范围</span><Select label="记忆范围" value={config.memory_scope || 'tenant'} options={memoryScopes} onChange={(memory_scope) => onConfig({ memory_scope })} /></div> : null}
      </section>
    </>}
  </section>
}
