import type { DevelopmentDocument } from '../../lib/teams'
import './team.css'

// Team name and goal. Who may use the team is chosen on the Forge
// configuration page, not here.
export function TeamProfile({ document, onChange }: { document: DevelopmentDocument; onChange(patch: Partial<DevelopmentDocument>): void }) {
  return <section className="team-profile" aria-label="团队资料">
    <label className="field"><span>团队名称</span><input className="input" value={document.name} maxLength={80} onChange={(event) => onChange({ name: event.target.value })} /></label>
    <label className="field"><span>团队目标</span><textarea className="input textarea" rows={5} value={document.objective} onChange={(event) => onChange({ objective: event.target.value })} /></label>
  </section>
}

// The flow's own name and description, shown above its canvas.
export function FlowProfile({ name, description, onChange }: { name: string; description: string; onChange(patch: { name?: string; description?: string }): void }) {
  return <div className="flow-profile">
    <label className="field"><span>流程名称</span><input className="input" value={name} maxLength={80} onChange={(event) => onChange({ name: event.target.value })} /></label>
    <label className="field"><span>流程说明</span><textarea className="input textarea" rows={2} value={description} onChange={(event) => onChange({ description: event.target.value })} /></label>
  </div>
}
