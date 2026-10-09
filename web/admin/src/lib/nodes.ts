import { api } from './api'

export interface EngineReadiness {
  engine: string
  accepting: boolean
  reason?: string
  binary_version?: string
  auth_mode?: string
  configured_model?: string
}

export interface RuntimeNode {
  id: string
  name: string
  online: boolean
  accepting: boolean
  enabled: boolean
  paused: boolean
  probe_pending: boolean
  health_status: string
  total_slots: number
  active_slots: number
  last_heartbeat_at?: string | null
  last_failure_reason?: string
  engine_readiness: EngineReadiness[]
  created_at: string
}

export interface CreatedNode {
  id: string
  name: string
  token: string
}

export const listNodes = () => api<{ runtimes: RuntimeNode[] }>('/v1/runtimes').then((body) => body.runtimes)
export const readNode = (id: string) => api<RuntimeNode>(`/v1/runtimes/${encodeURIComponent(id)}`)
export const createNode = (name: string) => api<CreatedNode>('/v1/runtimes', { method: 'POST', body: JSON.stringify({ name }) })
export const renameNode = (id: string, name: string) => api<void>(`/v1/runtimes/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify({ name }) })
export const rotateNodeToken = (id: string) => api<{ token: string }>(`/v1/runtimes/${encodeURIComponent(id)}/token`, { method: 'POST' })
export const setNodePaused = (id: string, paused: boolean) => api<void>(`/v1/runtimes/${encodeURIComponent(id)}/paused`, { method: 'PUT', body: JSON.stringify({ paused }) })
export const probeNode = (id: string) => api<void>(`/v1/runtimes/${encodeURIComponent(id)}/probe`, { method: 'POST' })
export const deleteNode = (id: string) => api<void>(`/v1/runtimes/${encodeURIComponent(id)}`, { method: 'DELETE' })

// Commands use the address the operator is looking at, so a copied command
// reaches this same Weave without editing.
export function installCommands(origin: string, token: string) {
  return {
    unix: `curl -fsSL "${origin}/install.sh" | sh -s -- --server "${origin}" --token '${token}'`,
    windows: `& ([scriptblock]::Create((irm "${origin}/install.ps1"))) -Server "${origin}" -Token "${token}"`,
    direct: `weave runtime --server "${origin}" --runtime-token '${token}'`,
  }
}
