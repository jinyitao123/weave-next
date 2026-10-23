import type { DevelopmentDraft, RebaseChange, RebaseConflict } from '../../lib/teams'

const teamFields: Record<string, string> = { name: '团队名称', objective: '团队目标', audience: '可用人群' }

/** Names one part of a team the way the page shows it. */
export function rebasePartName(part: { kind: string; field?: string; name: string }): string {
  if (part.kind === 'team') return teamFields[part.field ?? ''] ?? '团队资料'
  return `${part.kind === 'member' ? '成员' : '流程'}“${part.name}”`
}

const actionLabels: Record<RebaseChange['action'], string> = { taken: '已采用线上', added: '线上新增', removed: '线上已删除', kept: '保留草稿' }

/** The live team changed after the draft was opened: say so and offer the merge. */
export function BaselineBanner({ draft, dirty, busy, conflicts, onRebase }: {
  draft: DevelopmentDraft
  dirty: boolean
  busy: boolean
  conflicts?: RebaseConflict[]
  onRebase(keepDraft: boolean): void
}) {
  if (!draft.baseline_changed) return null
  const removed = conflicts?.filter((conflict) => conflict.live_removed) ?? []
  return <div className="notice baseline" role="status">
    <p>线上团队在草稿打开后有变化（例如在别处改了团队资料、成员或流程）。合并之前，草稿不能试跑或发布。</p>
    {conflicts?.length ? <>
      <p>草稿和线上都改了这些地方：{conflicts.map(rebasePartName).join('、')}。</p>
      {removed.length ? <p>{removed.map(rebasePartName).join('、')}已在线上删除，请先在草稿中移除。</p> : null}
    </> : null}
    <div className="toolbar">
      {dirty ? <span>请先保存或放弃修改，再合并。</span> : <>
        <button type="button" className="button button--primary" disabled={busy} onClick={() => onRebase(false)}>读取线上变化并合并</button>
        {conflicts?.length && !removed.length ? <button type="button" className="button" disabled={busy} onClick={() => onRebase(true)}>保留草稿的修改</button> : null}
      </>}
    </div>
  </div>
}

/** What the last merge took in or kept. */
export function RebaseSummary({ changes }: { changes?: RebaseChange[] }) {
  if (!changes) return null
  if (!changes.length) return <p className="notice" role="status">线上没有新的变化。</p>
  return <p className="notice" role="status">已合并线上变化：{changes.map((change) => `${rebasePartName(change)}（${actionLabels[change.action]}）`).join('、')}。</p>
}
