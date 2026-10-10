import type { DevelopmentDocument } from '../../lib/teams'
import { Info } from 'lucide-react'
import { useState } from 'react'
import { Dialog } from '../ui'
import './team.css'

// Team name and goal. Who may use the team is chosen on the Forge
// configuration page, not here.
export function TeamProfile({ document, onChange }: { document: DevelopmentDocument; onChange(patch: Partial<DevelopmentDocument>): void }) {
  return <section className="team-profile" aria-label="团队资料">
    <label className="field"><span>团队名称</span><input className="input" value={document.name} maxLength={80} onChange={(event) => onChange({ name: event.target.value })} /></label>
    <label className="field"><span>团队目标</span><textarea className="input textarea" rows={3} value={document.objective} onChange={(event) => onChange({ objective: event.target.value })} /></label>
  </section>
}

// The flow's own name and description, shown above its canvas.
export function FlowProfile({ name, description, onChange }: { name: string; description: string; onChange(patch: { name?: string; description?: string }): void }) {
  const [open, setOpen] = useState(false)
  return <>
    <div className="flow-profile">
      <strong>{name || '未命名流程'}</strong>
      <button type="button" className="icon-button" aria-label="流程基本信息" title="流程基本信息" onClick={() => setOpen(true)}><Info size={17} /></button>
    </div>
    {open ? <Dialog title="流程基本信息" onClose={() => setOpen(false)} footer={<button type="button" className="button" onClick={() => setOpen(false)}>完成</button>}>
      <div className="stack">
        <label className="field"><span>流程名称</span><input className="input" value={name} maxLength={80} onChange={(event) => onChange({ name: event.target.value })} /></label>
        <label className="field"><span>流程说明</span><textarea className="input textarea" rows={4} value={description} onChange={(event) => onChange({ description: event.target.value })} /></label>
      </div>
    </Dialog> : null}
  </>
}
