import { api } from './api'

export type TaskFilter = '' | 'active' | 'attention' | 'done'

export interface TaskSummary {
  run_id: string
  title: string
  team_id: string
  team_name: string
  workflow_id: string
  workflow_version: number
  status: string
  wait_kind?: string
  source_kind: string
  created_at: string
  updated_at: string
  terminal_at?: string | null
  failure_reason?: string
  first_output_at?: string | null
}

export interface TeamOption {
  id: string
  name: string
  display_name?: string
  status: string
  default_workflow_id?: string
}

export interface PublicUpdate { seq: number; kind: string; text: string; occurred_at: string; truncated?: boolean }
export interface StageTool { call_id: string; name: string; status: string; input?: string; output?: string; started_at?: string; completed_at?: string }
export interface StageInput { name: string; source: string; node_id?: string; summary?: string }
export interface StageOutput { kind: string; path?: string; content_type: string; content: string; content_bytes: number; truncated: boolean }

export interface MemberStage {
  node_id: string
  name: string
  status: string
  inputs: StageInput[]
  outputs?: StageOutput[]
  started_at?: string
  completed_at?: string
  duration_ms?: number
  tools: StageTool[]
  public_updates?: PublicUpdate[]
  public_updates_truncated?: boolean
  failure_class?: string
  retryable?: boolean
}

export interface ActivityMember {
  agent_id: string
  name: string
  role: string
  status: string
  runtime?: { engine?: string; model?: string; configured_model?: string; name?: string; auth_mode?: string }
  stages: MemberStage[]
}

export interface DeliveryCheck { check_id: string; title?: string; status: string; reason: string; actual?: string; expected?: string }

export interface Delivery {
  verification_status: 'pending' | 'passed' | 'failed' | 'unknown'
  reason?: string
  checks: DeliveryCheck[]
  available: boolean
  evidence_completeness: string
}

export interface Activity {
  run_id: string
  team_id?: string
  development_trial?: boolean
  status: string
  wait_kind?: string | null
  stop_unconfirmed?: boolean
  created_at: string
  terminal_at?: string | null
  tokens_in?: number
  tokens_out?: number
  members: ActivityMember[]
  runtimes: Array<{ name: string; engine?: string; status: string }>
  delivery?: Delivery
  completed_stages: number
  total_stages: number
}

export function listTasks(filter: TaskFilter, before?: string) {
  const query = new URLSearchParams()
  if (filter) query.set('filter', filter)
  if (before) query.set('before', before)
  return api<{ tasks: TaskSummary[]; next_before?: string }>(`/v1/admin/tasks${query.size ? `?${query}` : ''}`)
}

export const readTask = (runId: string) => api<TaskSummary>(`/v1/admin/tasks/${encodeURIComponent(runId)}`)
export const readActivity = (runId: string) => api<Activity>(`/v1/runs/${encodeURIComponent(runId)}/activity`)

export async function listTeams(): Promise<TeamOption[]> {
  const teams = await api<TeamOption[]>('/v1/teams?status=active&purpose=development')
  return teams.filter((team) => Boolean(team.default_workflow_id))
}

export function submitTask(teamId: string, task: string, clientRequestId: string) {
  return api<{ run_id: string }>(`/v1/teams/${encodeURIComponent(teamId)}/dispatch`, {
    method: 'POST',
    body: JSON.stringify({ task, client_request_id: clientRequestId }),
  })
}

export function stopTask(runId: string, idempotencyKey: string) {
  return api(`/v1/runs/${encodeURIComponent(runId)}/stop`, {
    method: 'POST',
    body: JSON.stringify({ reason: 'user_requested', idempotency_key: idempotencyKey }),
  })
}

export function retryStage(runId: string, nodeId: string, idempotencyKey: string) {
  return api(`/v1/runs/${encodeURIComponent(runId)}/stages/${encodeURIComponent(nodeId)}/retry`, {
    method: 'POST',
    body: JSON.stringify({ idempotency_key: idempotencyKey }),
  })
}

const executionCopy: Record<string, string> = {
  queued: '排队中', running: '执行中', parked: '等待中', cancel_requested: '正在停止',
  succeeded: '执行完成', failed: '执行失败', cancelled: '已停止', abandoned: '已中止',
}

export function executionLabel(status: string, waitKind?: string | null): string {
  if (status === 'parked') {
    return ({ human: '等待人工处理', correction: '等待更正', runtime: '等待节点', timer: '定时等待', fanout: '等待并行分支' } as Record<string, string>)[waitKind ?? ''] ?? '等待中'
  }
  return executionCopy[status] ?? '状态更新中'
}

export function executionTone(status: string): 'neutral' | 'accent' | 'success' | 'warning' | 'danger' {
  if (status === 'succeeded') return 'accent'
  if (status === 'failed' || status === 'abandoned') return 'danger'
  if (status === 'parked' || status === 'cancel_requested') return 'warning'
  if (status === 'running' || status === 'queued') return 'neutral'
  return 'neutral'
}

export const terminal = (status: string) => ['succeeded', 'failed', 'cancelled', 'abandoned'].includes(status)

export function verificationLabel(delivery: Delivery | undefined, status: string): { label: string; tone: 'neutral' | 'success' | 'danger' | 'warning' } {
  if (!terminal(status)) return { label: '待验证', tone: 'neutral' }
  switch (delivery?.verification_status) {
    case 'passed': return { label: '验证通过', tone: 'success' }
    case 'failed': return { label: '验证未通过', tone: 'danger' }
    case 'pending': return { label: '待验证', tone: 'neutral' }
    default: return { label: delivery?.available === false || !delivery ? '未声明验证' : '无法确认', tone: 'warning' }
  }
}

const stageCopy: Record<string, string> = {
  pending: '未开始', queued: '排队中', running: '执行中', completed: '完成', succeeded: '完成', failed: '失败',
  cancelled: '已停止', skipped: '跳过', waiting: '等待中', parked: '等待中',
}

export const stageLabel = (status: string) => stageCopy[status] ?? '进行中'

export function duration(ms?: number): string {
  if (!ms || ms < 0) return ''
  const seconds = Math.round(ms / 1000)
  if (seconds < 60) return `${seconds} 秒`
  const minutes = Math.floor(seconds / 60)
  return minutes < 60 ? `${minutes} 分 ${seconds % 60} 秒` : `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分`
}

const failureCopy: Record<string, string> = {
  git_missing: '执行节点没有安装 git',
  ref_not_found: '仓库里找不到所选分支',
  repository_fetch_failed: '执行节点无法拉取仓库，请检查节点的 git 登录和仓库地址',
  repository_checkout_failed: '执行节点无法检出代码',
  handoff_mismatch: '上一步交来的代码无法在本节点精确还原，已拒绝在其他提交上执行',
  change_too_large: '上一步的改动过大，无法交接',
  handoff_conflict: '多个上游步骤交来了不同的代码版本',
  engine_login_failed: '执行节点上的引擎登录失效，请在节点上重新登录',
}

export const failureText = (reason?: string) => (reason ? failureCopy[reason] ?? '' : '')

export interface LogLine { node_id: string; stream: string; seq: number; occurred_at: string; text: string }

export function readLogs(runId: string, options: { node?: string; stream?: string; after?: number; q?: string; limit?: number }) {
  const query = new URLSearchParams()
  if (options.node) query.set('node', options.node)
  if (options.stream) query.set('stream', options.stream)
  if (options.after) query.set('after', String(options.after))
  if (options.q) query.set('q', options.q)
  if (options.limit) query.set('limit', String(options.limit))
  return api<{ lines: LogLine[]; complete: boolean }>(`/v1/admin/tasks/${encodeURIComponent(runId)}/logs?${query}`)
}

export const streamLabel = (stream: string) => stream === 'agent' ? '成员输出' : stream === 'setup' ? '启动脚本' : stream.startsWith('command-') ? `验证命令 ${stream.slice(8)}` : '输出'

// Subscribes to a task's change stream; returns a stop function. onUpdate runs
// on every change and once more when the run ends.
export function subscribeTask(runId: string, onUpdate: () => void, onBroken: () => void): () => void {
  if (typeof EventSource === 'undefined') { onBroken(); return () => undefined }
  const source = new EventSource(`/v1/admin/tasks/${encodeURIComponent(runId)}/stream`)
  source.addEventListener('update', onUpdate)
  source.addEventListener('end', () => { onUpdate(); source.close() })
  source.onerror = () => { if (source.readyState === EventSource.CLOSED) onBroken() }
  return () => source.close()
}

export interface TaskWait { waiting: boolean; reason?: string; position?: number; node_name?: string; engine?: string }
export const readQueue = (runId: string) => api<TaskWait>(`/v1/admin/tasks/${encodeURIComponent(runId)}/queue`)
export const readMetrics = () => api<{ first_output_median_seconds: number | null; sample: number }>('/v1/admin/metrics')

export function waitText(wait: TaskWait): string {
  if (!wait.waiting) return ''
  if (wait.reason === 'team_pickup') return '等待团队接手'
  const node = wait.node_name ? `节点“${wait.node_name}”` : '节点'
  if (wait.reason === 'node_busy') return `${node}正忙，排在第 ${wait.position ?? 1} 位`
  const reasons: Record<string, string> = {
    runtime_offline: '离线', runtime_disabled: '已停用', runtime_revoked: '已吊销', runtime_quarantined: '已隔离', runtime_paused: '已暂停',
    engine_unavailable: '没有可用的引擎', provider_credentials_missing: '缺少模型凭据', codex_login_required: '需要登录 Codex',
  }
  return `等待${node}：${reasons[wait.reason ?? ''] ?? '暂不可接任务'}`
}
