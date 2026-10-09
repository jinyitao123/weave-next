import { api } from './api'

export interface Environment {
  id: string
  name: string
  repository_url: string
  default_branch: string
  setup_script: string
  verify_commands: string[]
  push_branches: boolean
  default_team_id: string
  git_username: string
  // git_credential reports a saved server-side token; the token itself is
  // write-only and never returned.
  git_credential: boolean
  updated_at: string
}

// git_token: absent keeps the saved token, '' clears it.
export type EnvironmentInput = Omit<Environment, 'id' | 'updated_at' | 'git_credential'> & { git_token?: string }

export const listEnvironments = () => api<{ environments: Environment[] }>('/v1/environments').then((body) => body.environments)
export function environmentInput(environment: Environment): EnvironmentInput {
  const { id: _id, updated_at: _updated, git_credential: _credential, ...input } = environment
  return input
}

export const saveEnvironment = (input: EnvironmentInput, id?: string) =>
  api<Environment>(id ? `/v1/environments/${encodeURIComponent(id)}` : '/v1/environments', { method: id ? 'PUT' : 'POST', body: JSON.stringify(input) })
export const archiveEnvironment = (id: string) => api<void>(`/v1/environments/${encodeURIComponent(id)}`, { method: 'DELETE' })

export function submitCodeTask(input: { teamId: string; environmentId?: string; ref?: string; task: string; clientRequestId: string }) {
  return api<{ run_id: string }>('/v1/admin/tasks', {
    method: 'POST',
    body: JSON.stringify({ team_id: input.teamId, environment_id: input.environmentId ?? '', ref: input.ref ?? '', task: input.task, client_request_id: input.clientRequestId }),
  })
}

export interface CodeFile { path: string; added: number; deleted: number; binary?: boolean }
export interface CodeCommand { command: string; exit_code: number; timed_out?: boolean; started_at: string; duration_ms: number; output_sha256: string; output_bytes: number; output_tail: string }
export interface CodeEvidence { commit: string; node_id: string; reused_from?: string; setup?: CodeCommand; commands: CodeCommand[] }
export interface CodeVersion { base_sha: string; parent_sha: string; head_sha: string; node_id: string; changed: boolean; files: CodeFile[]; patch: string; push?: { branch: string; status: string; error?: string } }
export interface CodeStage { node_id: string; completed_at: string; version: CodeVersion; evidence?: CodeEvidence; passes?: number }
export interface CodeVerdict { status: 'passed' | 'failed' | 'unknown' | 'not_declared' | 'pending'; reason?: string; commit?: string }

export interface TaskCode {
  repository: string
  ref: string
  push_branch?: string
  verify_commands: string[]
  stages: CodeStage[]
  final?: CodeStage
  patch?: string
  verdict: CodeVerdict
  pull_request?: PullRequest
}

export interface PullRequest { url: string; number: number }

export const readTaskCode = (runId: string) => api<TaskCode>(`/v1/admin/tasks/${encodeURIComponent(runId)}/code`)
export const createPullRequest = (runId: string) => api<PullRequest>(`/v1/admin/tasks/${encodeURIComponent(runId)}/pull-request`, { method: 'POST' })

export const shortSHA = (sha?: string) => (sha ? sha.slice(0, 7) : '')

const verdictCopy: Record<CodeVerdict['status'], { label: string; tone: 'neutral' | 'success' | 'danger' | 'warning' }> = {
  passed: { label: '验证通过', tone: 'success' },
  failed: { label: '验证未通过', tone: 'danger' },
  unknown: { label: '无法确认', tone: 'warning' },
  not_declared: { label: '未声明验证', tone: 'warning' },
  pending: { label: '待验证', tone: 'neutral' },
}

export const verdictLabel = (verdict: CodeVerdict) => verdictCopy[verdict.status] ?? verdictCopy.unknown

const reasonCopy: Record<string, string> = {
  no_code_version: '没有产生代码版本',
  no_verify_commands: '环境没有声明验证命令，执行完成不代表验证通过',
  no_evidence: '缺少节点执行的验证记录',
  evidence_commit_mismatch: '被验证的提交不是交付的提交',
  evidence_tree_mismatch: '验证期间代码版本变化，无法确认交付提交通过',
  evidence_incomplete: '验证记录不完整',
  evidence_commands_mismatch: '执行的命令与声明不一致',
  command_failed: '有验证命令未通过',
}

export const verdictReason = (reason?: string) => (reason ? reasonCopy[reason] ?? '' : '')

export interface DiffFile { path: string; binary: boolean; lines: Array<{ kind: 'add' | 'del' | 'ctx' | 'hunk'; text: string }> }

// Parses the unified diff the runtime node recorded for the delivered commit.
export function parsePatch(patch: string): DiffFile[] {
  const files: DiffFile[] = []
  let current: DiffFile | undefined
  let inHunk = false
  for (const line of patch.split('\n')) {
    if (line.startsWith('diff --git ')) {
      const match = / b\/(.+)$/.exec(line)
      current = { path: match?.[1] ?? line.slice(11), binary: false, lines: [] }
      files.push(current)
      inHunk = false
      continue
    }
    if (!current) continue
    if (line.startsWith('GIT binary patch') || line.startsWith('Binary files')) { current.binary = true; continue }
    if (line.startsWith('@@')) { inHunk = true; current.lines.push({ kind: 'hunk', text: line }); continue }
    if (!inHunk || current.binary) continue
    if (line.startsWith('+')) current.lines.push({ kind: 'add', text: line.slice(1) })
    else if (line.startsWith('-')) current.lines.push({ kind: 'del', text: line.slice(1) })
    else if (line.startsWith(' ')) current.lines.push({ kind: 'ctx', text: line.slice(1) })
  }
  return files
}

export const submitFollowUp = (runId: string, task: string, clientRequestId: string) =>
  api<{ run_id: string }>(`/v1/admin/tasks/${encodeURIComponent(runId)}/follow-ups`, { method: 'POST', body: JSON.stringify({ task, client_request_id: clientRequestId }) })

export const readThread = (runId: string) =>
  api<{ runs: Array<{ run_id: string; title: string; status: string; wait_kind?: string; created_at: string }> }>(`/v1/admin/tasks/${encodeURIComponent(runId)}/thread`).then((body) => body.runs)
