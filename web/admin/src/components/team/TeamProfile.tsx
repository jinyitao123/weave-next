import type { DevelopmentDocument } from '../../lib/teams'
import { Info } from 'lucide-react'
import { useState } from 'react'
import { Dialog, Select } from '../ui'
import './team.css'

// Team name and goal. Who may use the team is chosen on the Forge
// configuration page, not here.
export function TeamProfile({ document, onChange }: { document: DevelopmentDocument; onChange(patch: Partial<DevelopmentDocument>): void }) {
  return <section className="team-profile" aria-label="团队资料">
    <label className="field"><span>团队名称</span><input className="input" value={document.name} maxLength={80} onChange={(event) => onChange({ name: event.target.value })} /></label>
    <label className="field"><span>团队目标</span><textarea className="input textarea" rows={3} value={document.objective} onChange={(event) => onChange({ objective: event.target.value })} /></label>
  </section>
}

// One row above the canvas: which flow is open (a team may have several), how
// many nodes it has, and its own name and description behind the info button.
// Choosing a flow only changes what is shown; nothing is saved or published.
export function FlowBar({ flows, flow, onSelect, onChange }: {
  flows: Array<{ id: string; name: string }>
  flow: { id: string; name: string; description: string; nodes: number }
  onSelect(id: string): void
  onChange(patch: { name?: string; description?: string }): void
}) {
  const [open, setOpen] = useState(false)
  return <>
    <div className="flow-bar">
      {flows.length > 1
        ? <div className="flow-bar__select"><Select label="当前流程" value={flow.id} options={flows.map((item) => ({ value: item.id, label: item.name || '未命名流程' }))} onChange={onSelect} /></div>
        : <strong className="flow-bar__name">{flow.name || '未命名流程'}</strong>}
      <span className="muted small">{flow.nodes} 个节点</span>
      <button type="button" className="icon-button" aria-label="流程基本信息" title="流程基本信息" onClick={() => setOpen(true)}><Info size={17} /></button>
      {flows.length > 1 ? <span className="muted small flow-bar__note">团队共 {flows.length} 条流程，保存和发布对全部流程一起生效</span> : null}
    </div>
    {open ? <Dialog title="流程基本信息" onClose={() => setOpen(false)} footer={<button type="button" className="button" onClick={() => setOpen(false)}>完成</button>}>
      <div className="stack">
        <label className="field"><span>流程名称</span><input className="input" value={flow.name} maxLength={80} onChange={(event) => onChange({ name: event.target.value })} /></label>
        <label className="field"><span>流程说明</span><textarea className="input textarea" rows={4} value={flow.description} onChange={(event) => onChange({ description: event.target.value })} /></label>
      </div>
    </Dialog> : null}
  </>
}
