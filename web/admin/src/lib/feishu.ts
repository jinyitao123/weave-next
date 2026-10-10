import { api } from './api'

export interface FeishuApp {
  source: 'workspace' | 'deployment' | 'none'
  appId?: string
  tenantKey?: string
  secrets: { appSecret: boolean; verificationToken: boolean; encryptKey: boolean }
  callbackPath?: string
  revision: number
  updatedAt?: string
  updatedBy?: string
  lastCheck?: { at: string; ok: boolean; reason?: string }
  boundEmployees: number
}

// Secrets are omitted to keep the saved value and sent only when replaced.
export interface FeishuAppInput {
  expectedRevision: number
  appId: string
  tenantKey: string
  appSecret?: string
  verificationToken?: string
  encryptKey?: string
}

export interface FeishuNotify { result: boolean; revisionRequired: boolean; humanReview: boolean; failure: boolean; cancelled: boolean }

export interface FeishuTeamAccess {
  enabled: boolean
  workflowId: string | null
  notify: FeishuNotify
  revision: number
  updatedAt?: string
  updatedBy?: string
}

export const notifyKinds: Array<{ key: keyof FeishuNotify; label: string }> = [
  { key: 'result', label: '完成' },
  { key: 'revisionRequired', label: '需要补充' },
  { key: 'humanReview', label: '需要人工确认' },
  { key: 'failure', label: '失败' },
  { key: 'cancelled', label: '取消' },
]

export const checkReasons: Record<string, string> = {
  credentials_rejected: '飞书拒绝了应用凭据',
  unreachable: '无法连接飞书',
  not_configured: '本工作区还没有保存飞书应用',
}

export const readFeishuApp = () => api<FeishuApp>('/v1/integrations/feishu/app')
export const saveFeishuApp = (input: FeishuAppInput) => api<FeishuApp>('/v1/integrations/feishu/app', { method: 'PUT', body: JSON.stringify(input) })
export const deleteFeishuApp = (expectedRevision: number) => api<FeishuApp>('/v1/integrations/feishu/app', { method: 'DELETE', body: JSON.stringify({ expectedRevision }) })
export const checkFeishuApp = () => api<{ ok: boolean; reason?: string }>('/v1/integrations/feishu/app/check', { method: 'POST' })

const accessPath = (teamId: string) => `/v1/teams/${encodeURIComponent(teamId)}/feishu-access`
export const readTeamFeishuAccess = (teamId: string) => api<FeishuTeamAccess>(accessPath(teamId))
export const saveTeamFeishuAccess = (teamId: string, access: FeishuTeamAccess) =>
  api<FeishuTeamAccess>(accessPath(teamId), { method: 'PUT', body: JSON.stringify({ expectedRevision: access.revision, enabled: access.enabled, workflowId: access.workflowId, notify: access.notify }) })
