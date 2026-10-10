import { CircleCheck, CircleDashed } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Badge, InlineError, Select, Switch } from '../ui'
import { errorMessage } from '../../lib/api'
import { relativeTime } from '../../lib/format'
import { createUUID } from '../../lib/ids'
import { executionLabel } from '../../lib/tasks'
import { capabilityName, startTrial, trialActions, workflowCapabilityIds, type BusinessCatalog, type DevelopmentDraft } from '../../lib/teams'
import { CatalogRefresh } from './CatalogRefresh'
import './team.css'

const flowName = (draft: DevelopmentDraft, id: string) => draft.document.workflows.find((flow) => flow.id === id)?.name || '未命名流程'

// Runs the saved draft once. Business actions are only simulated, and every
// developer of the workspace sees the same trial records.
export function TrialPanel({ teamId, draft, dirty, catalog, navigate, onStarted, onCatalog }: {
  teamId: string
  draft: DevelopmentDraft
  dirty: boolean
  catalog?: BusinessCatalog
  navigate(path: string): void
  onStarted(): void
  onCatalog?(catalog: BusinessCatalog): void
}) {
  const flows = draft.document.workflows
  const [flowId, setFlowId] = useState(flows[0]?.id ?? '')
  const flow = flows.find((item) => item.id === flowId) ?? flows[0]
  const [input, setInput] = useState('')
  const [skipped, setSkipped] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  // One request id per intended trial: a repeated click reuses it, a changed
  // task, flow or simulation choice is a new trial.
  const [requestId, setRequestId] = useState(() => createUUID())
  const change = (apply: () => void) => { apply(); setError(''); setRequestId(createUUID()) }

  const ids = useMemo(() => flow ? workflowCapabilityIds(draft.document, flow) : [], [draft.document, flow])
  const simulate = new Set(ids.filter((id) => !skipped.includes(id)))
  const { actions, missing } = trialActions(ids, catalog, simulate)
  const blocked = dirty ? '有未保存的修改。试跑用的是已保存的草稿，请先保存。'
    : !flow ? '还没有流程，先到“流程”页添加步骤。'
      : missing.length && !catalog?.available ? '还没有读到业务动作目录，这个流程用到的业务动作无法试跑。请先刷新目录。'
        : missing.length ? `业务动作目录里没有“${missing.map((id) => capabilityName(id, catalog)).join('”“')}”。请刷新目录，或在“成员”页移除它。`
          : ''

  const start = async () => {
    if (blocked || !flow) return
    if (!input.trim()) { setError('请填写试跑任务'); return }
    setBusy(true)
    setError('')
    try {
      const result = await startTrial(teamId, draft.revision, flow.id, input.trim(), requestId, actions)
      setRequestId(createUUID())
      onStarted()
      navigate(`/tasks/${encodeURIComponent(result.run_id)}`)
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const trials = [...draft.trials].sort((left, right) => Date.parse(right.created_at) - Date.parse(left.created_at))

  return <div className="release">
    <section className="release__section" aria-label="发起试跑">
      <h2>发起试跑</h2>
      <p className="muted small">试跑把已保存的草稿完整执行一遍，不影响线上版本，也不会访问业务系统。</p>
      {flows.length > 1 ? <div className="field"><span>流程</span><Select label="试跑的流程" value={flow?.id ?? ''} options={flows.map((item) => ({ value: item.id, label: item.name || '未命名流程' }))} onChange={(id) => change(() => setFlowId(id))} /></div> : null}
      {ids.length ? <div className="field"><span>业务动作（试跑中只模拟，不写入业务系统）</span>
        <ul className="release__actions">{ids.map((id) => <li key={id}>
          <span>{capabilityName(id, catalog)}</span>
          <Switch checked={simulate.has(id)} label="模拟执行" onChange={(on) => change(() => setSkipped(on ? skipped.filter((item) => item !== id) : [...skipped, id]))} />
        </li>)}</ul>
      </div> : null}
      <label className="field"><span>试跑任务</span>
        <textarea className="input textarea" rows={4} value={input} placeholder="贴一段真实的任务原话，例如员工平时会怎么提这件事" onChange={(event) => change(() => setInput(event.target.value))} /></label>
      {blocked ? <p className="notice" role="status">{blocked}</p> : null}
      {missing.length && !dirty && onCatalog ? <div className="toolbar"><CatalogRefresh catalog={catalog} onRefreshed={onCatalog} /></div> : null}
      {error ? <InlineError message={error} /> : null}
      <div className="toolbar"><button type="button" className="button button--primary" disabled={busy || Boolean(blocked)} onClick={() => void start()}>{busy ? '正在提交…' : '开始试跑'}</button></div>
    </section>

    <section className="release__section" aria-label="试跑记录">
      <h2>试跑记录</h2>
      {trials.length ? <div className="task-list">{trials.map((trial) => <button key={trial.request_id} type="button" className="task-row" disabled={!trial.run_id} onClick={() => trial.run_id && navigate(`/tasks/${encodeURIComponent(trial.run_id)}`)}>
        <span className="task-row__main">
          <strong>第 {trial.revision} 版草稿 · {flowName(draft, trial.workflow_id)}</strong>
          <span className="muted small">{trial.actor || '开发者'} · {relativeTime(trial.created_at)}{trial.revision !== draft.revision ? ' · 不是当前草稿' : ''}</span>
        </span>
        <Badge tone={trial.status === 'succeeded' ? 'success' : trial.status === 'failed' ? 'danger' : 'neutral'}>{executionLabel(trial.status)}</Badge>
      </button>)}</div> : <p className="muted">还没有试跑记录。</p>}
    </section>
  </div>
}

// What is live, what is waiting, and what still stands between the two.
export function PublishPanel({ draft, dirty, busy, catalog, blockers = [], onPublish, onOpenTrial, onOpenFlow }: {
  draft: DevelopmentDraft
  dirty: boolean
  busy: boolean
  catalog?: BusinessCatalog
  /** What the saved draft still lacks before any trial can count. */
  blockers?: string[]
  onPublish(): void
  onOpenTrial(): void
  onOpenFlow?(): void
}) {
  const published = draft.published_revision > 0
  const pending = dirty || draft.revision !== draft.published_revision
  const readiness = draft.publication_readiness
  const flows = draft.document.workflows
  const checks = flows.map((flow) => {
    const item = readiness?.workflows?.find((entry) => entry.workflow_id === flow.id)
    const missing = item?.missing_capability_ids ?? []
    return {
      id: flow.id, passed: !dirty && item?.passed === true,
      title: `试跑“${flow.name || '未命名流程'}”`,
      detail: !dirty && item?.passed ? '这一版草稿已经试跑成功。'
        : missing.length ? `试跑里还需要模拟执行：${missing.map((id) => capabilityName(id, catalog)).join('、')}。`
          : '这一版草稿还没有成功的试跑。',
    }
  })
  const ready = !dirty && pending && flows.length > 0 && !blockers.length && readiness?.ready === true

  return <div className="release">
    <section className="release__section" aria-label="版本">
      <h2>版本</h2>
      <dl className="release__facts">
        <div><dt>线上版本</dt><dd>{published ? `第 ${draft.published_revision} 版` : '还没有发布过'}</dd></div>
        <div><dt>草稿</dt><dd>{dirty ? `第 ${draft.revision} 版，另有未保存的修改` : pending ? `第 ${draft.revision} 版，尚未发布` : `第 ${draft.revision} 版，与线上一致`}</dd></div>
      </dl>
    </section>
    <section className="release__section" aria-label="发布">
      <h2>发布</h2>
      {!pending ? <p className="muted">没有待发布的修改。</p> : <>
        <ul className="release__checks">
          <li>{dirty ? <CircleDashed size={15} className="muted" aria-hidden="true" /> : <CircleCheck size={15} className="icon--success" aria-hidden="true" />}
            <span><strong>保存草稿</strong><span className="muted small">{dirty ? '有未保存的修改，先在页面右上角保存。' : '修改已保存。'}</span></span></li>
          {blockers.map((blocker) => <li key={blocker}><CircleDashed size={15} className="muted" aria-hidden="true" />
            <span><strong>选定必须办成的业务动作</strong><span className="muted small">{blocker}。到“流程”页点“交付结果”设置，保存后重新试跑。</span></span></li>)}
          {checks.map((check) => <li key={check.id}>{check.passed ? <CircleCheck size={15} className="icon--success" aria-hidden="true" /> : <CircleDashed size={15} className="muted" aria-hidden="true" />}
            <span><strong>{check.title}</strong><span className="muted small">{check.detail}</span></span></li>)}
          {flows.length === 0 ? <li><CircleDashed size={15} className="muted" aria-hidden="true" /><span><strong>添加流程</strong><span className="muted small">还没有流程。</span></span></li> : null}
        </ul>
        <div className="toolbar">
          <button type="button" className="button button--primary" disabled={busy || !ready} onClick={onPublish}>{busy ? '正在发布…' : `发布第 ${draft.revision} 版`}</button>
          {!ready && !dirty && blockers.length && onOpenFlow ? <button type="button" className="button" onClick={onOpenFlow}>去流程页</button> : null}
          {!ready && !dirty && !blockers.length && flows.length ? <button type="button" className="button" onClick={onOpenTrial}>去试跑</button> : null}
        </div>
      </>}
    </section>
  </div>
}
