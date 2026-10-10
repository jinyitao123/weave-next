import { Plus, X } from 'lucide-react'
import { useState } from 'react'
import { audienceLimit, normalizeAudience, type DevelopmentDocument } from '../../lib/teams'
import './team.css'

// Team name, goal and the permission sets allowed to use the team.
export function TeamProfile({ document, onChange }: { document: DevelopmentDocument; onChange(patch: Partial<DevelopmentDocument>): void }) {
  const [entry, setEntry] = useState('')
  const [error, setError] = useState('')
  const add = () => {
    const parsed = normalizeAudience([...document.audience, entry])
    if (parsed.error) { setError(parsed.error); return }
    setError('')
    setEntry('')
    onChange({ audience: parsed.value })
  }
  return <div className="team-profile">
    <section className="team-profile__section" aria-label="团队资料">
      <h2>团队资料</h2>
      <label className="field"><span>团队名称</span><input className="input" value={document.name} maxLength={80} onChange={(event) => onChange({ name: event.target.value })} /></label>
      <label className="field"><span>团队目标</span><textarea className="input textarea" rows={5} value={document.objective} onChange={(event) => onChange({ objective: event.target.value })} /></label>
    </section>
    <section className="team-profile__section" aria-label="可用人群">
      <h2>可用人群</h2>
      {document.audience.length ? <ul className="team-profile__tags" aria-label="已选权限集">{document.audience.map((name) => <li key={name}>
        <code>{name}</code>
        <button type="button" className="icon-button" aria-label={`移除权限集 ${name}`} onClick={() => onChange({ audience: document.audience.filter((item) => item !== name) })}><X size={13} /></button>
      </li>)}</ul> : <p className="muted">没有限定人群，组织内所有员工都可以使用这个团队。</p>}
      <div className="team-profile__add">
        <label className="field"><span>Forge 权限集名称</span>
          <input className="input mono" value={entry} spellCheck={false} placeholder="例如 sales_lead_owner" onChange={(event) => { setEntry(event.target.value); setError('') }}
            onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); add() } }} /></label>
        <button type="button" className="button" disabled={!entry.trim() || document.audience.length >= audienceLimit} onClick={add}><Plus size={14} />添加</button>
      </div>
      {error ? <p className="team-profile__error" role="alert">{error}</p> : null}
    </section>
  </div>
}

// The flow's own name and description, shown above its canvas.
export function FlowProfile({ name, description, onChange }: { name: string; description: string; onChange(patch: { name?: string; description?: string }): void }) {
  return <div className="flow-profile">
    <label className="field"><span>流程名称</span><input className="input" value={name} maxLength={80} onChange={(event) => onChange({ name: event.target.value })} /></label>
    <label className="field"><span>流程说明</span><input className="input" value={description} onChange={(event) => onChange({ description: event.target.value })} /></label>
  </div>
}
