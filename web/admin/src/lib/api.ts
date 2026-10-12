// Same-origin API client. The session lives in an HttpOnly cookie that page
// scripts cannot read; every request carries the console header the server
// requires before it accepts a cookie-authenticated write.

export class ApiError extends Error {
  constructor(readonly status: number, readonly code: string) {
    super(code)
  }
}

export async function apiResponse(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers)
  headers.set('X-Weave-Admin', '1')
  headers.set('Accept', 'application/json')
  if (init.body !== undefined && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const response = await fetch(path, { ...init, headers, credentials: 'same-origin' })
  if (!response.ok) {
    const body = safeJSON(await response.text())
    const fields = typeof body === 'object' && body ? body as { error?: unknown; message?: unknown } : {}
    const code = fields.error !== undefined ? String(fields.error) : fields.message !== undefined ? String(fields.message) : `http_${response.status}`
    throw new ApiError(response.status, code)
  }
  return response
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await apiResponse(path, init)
  if (response.status === 204) return undefined as T
  return safeJSON(await response.text()) as T
}

function safeJSON(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return undefined
  }
}

export interface AdminConfig {
  sign_in_methods: Array<'forge' | 'api_key'>
  forge_origin?: string
  secure: boolean
  /** No key was set for this deployment, so the well-known default is in force. */
  default_key?: boolean
}

export interface AdminSession {
  name: string
  role: 'member' | 'developer' | 'admin' | string
  source: 'forge' | 'api_key' | 'session'
  expires_at?: string
  /** The session was opened with the default key. */
  default_key?: boolean
}

export const readConfig = () => api<AdminConfig>('/v1/admin/config')
export const readSession = () => api<AdminSession>('/v1/admin/session')
export const signOut = () => api<void>('/v1/admin/session', { method: 'DELETE' })
export const signInWithAPIKey = (apiKey: string) =>
  api<AdminSession>('/v1/admin/session', { method: 'POST', body: JSON.stringify({ api_key: apiKey }) })

/** Opens the console session for the Forge account that just approved it. */
export const signInWithForge = (forge_token: string) =>
  api<AdminSession>('/v1/admin/session', { method: 'POST', body: JSON.stringify({ forge_token }) })

const messages: Record<string, string> = {
  organization_connection_unavailable: '部署连接尚未配置，请联系部署管理员',
  organization_connection_invalid: '部署连接配置无效或不可读取，请联系部署管理员',
  forge_unreachable: '无法连接 Forge，请确认 Forge 允许本地址跨域登录',
  forge_approval_unavailable: '这个 Forge 还没有启用浏览器确认登录，请联系管理员',
  forge_client_refused: '这个 Forge 不接受 Weave 管理端登录，请联系管理员',
  forge_approval_denied: '已在 Forge 页面拒绝',
  forge_page_unavailable: 'Forge 页面没有回应，请重试',
  forge_approval_expired: '确认已过期，请重新开始',
  forge_unavailable: 'Forge 登录服务暂时不可用',
  forge_response_unrecognized: 'Forge 返回了无法识别的登录结果',
  'invalid api key': 'API Key 无效或已过期',
  'api keys not configured': '服务端未启用 API Key',
  'external identity verification failed': 'Forge 账号验证失败',
  'account binding failed': '当前账号无法进入 Weave',
  'native organization binding is unavailable': '当前账号没有可用的组织',
  'external identity is not configured': '服务端未配置外部身份',
  admin_request_rejected: '请求被拒绝，请刷新页面后重试',
  mcp_client_unknown: '接入请求无效，请回到客户端重新发起',
  mcp_session_not_found: '这个登录已经结束',
  task_not_found: '任务不存在，或你没有查看权限',
  run_not_found: '任务不存在，或你没有查看权限',
  team_not_found: '团队不存在',
  team_not_active: '团队未发布或已停用',
  no_default_workflow: '团队还没有发布的流程',
  stop_conflict: '任务状态已变化，请刷新',
  stage_not_retryable: '这个阶段现在不能重试',
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (messages[error.code]) return messages[error.code]
    // Server copy written for people (Chinese product messages) is shown as-is;
    // internal codes and raw errors are not.
    if (/[一-鿿]/.test(error.code) && error.code.length <= 120) return error.code
    return error.status >= 500 ? '服务暂时不可用' : '请求未完成'
  }
  return '请求未完成'
}
