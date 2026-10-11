import { api } from './api'

// MCP clients sign a person in through the console: the client sends their
// browser here, the person allows it by name, and the client gets a session of
// its own that can be taken back.

export interface ConnectRequest {
  client_id: string
  redirect_uri: string
  code_challenge: string
  state: string
}

export interface ConnectDescription {
  client_name: string
  /** Where the browser goes back to: this machine or a named site. */
  returns_to: string
  account_name: string
}

export function readConnectRequest(search: string): ConnectRequest | undefined {
  const query = new URLSearchParams(search)
  const request = { client_id: query.get('client_id') ?? '', redirect_uri: query.get('redirect_uri') ?? '', code_challenge: query.get('code_challenge') ?? '', state: query.get('state') ?? '' }
  return request.client_id && request.redirect_uri && request.code_challenge ? request : undefined
}

export const describeConnect = (request: ConnectRequest) =>
  api<ConnectDescription>(`/v1/mcp/oauth/consent?${new URLSearchParams({ client_id: request.client_id, redirect_uri: request.redirect_uri })}`)

/** Records the answer and returns the address the browser continues to. */
export const answerConnect = (request: ConnectRequest, allow: boolean) =>
  api<{ redirect_to: string }>('/v1/mcp/oauth/consent', { method: 'POST', body: JSON.stringify({ ...request, allow }) }).then((answer) => answer.redirect_to)

export interface McpSession {
  id: string
  client_name: string
  own: boolean
  user_name: string
  created_at: string
  last_used_at?: string
  expires_at: string
}

export const listMcpSessions = () => api<{ sessions: McpSession[]; entry: string }>('/v1/mcp/sessions')
export const revokeMcpSession = (id: string) => api<void>(`/v1/mcp/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' })
