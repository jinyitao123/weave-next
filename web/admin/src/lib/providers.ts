import { api, ApiError } from './api'

// Organization model providers. A provider's requests go to base_url followed
// by chat_path, exactly; the server resolves and freezes that address.

export type Protocol = 'openai-completions' | 'openai-responses' | 'anthropic-messages'

export const protocols: Array<{ value: Protocol; label: string; path: string }> = [
  { value: 'openai-completions', label: 'OpenAI Chat Completions 兼容', path: '/v1/chat/completions' },
  { value: 'openai-responses', label: 'OpenAI Responses', path: '/v1/responses' },
  { value: 'anthropic-messages', label: 'Anthropic Messages', path: '/v1/messages' },
]

export interface ModelProvider {
  id: string
  name: string
  protocol: Protocol | ''
  base_url: string
  chat_path: string
  /** Empty for a provider saved before chat paths were recorded. */
  chat_url: string
  models: string[]
  json_object_mode: boolean
  thinking_default_mode: '' | 'enabled' | 'disabled'
  thinking_disable_with_tools: boolean
  attempt_timeout_seconds: number
  /** Came with the deployment; not changed here. */
  deployment: boolean
}

export interface ProviderInput {
  name: string
  protocol: Protocol
  base_url: string
  chat_path: string
  models: string[]
  api_key?: string
  json_object_mode: boolean
  thinking_default_mode: '' | 'enabled' | 'disabled'
  thinking_disable_with_tools: boolean
  attempt_timeout_seconds: number
}

export interface Endpoint { base_url: string; chat_path: string; chat_url: string }

/** A refused address: why, and the split that reaches what was most likely meant. */
export interface EndpointRefusal { message: string; suggestion?: Endpoint }

export const listProviders = () => api<ModelProvider[]>('/v1/development/model-providers')

export const saveProvider = (id: string, input: ProviderInput) =>
  api<{ id: string; chat_url: string }>(`/v1/development/model-providers/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify(input) })

export const resolveEndpoint = (protocol: Protocol, baseURL: string, chatPath: string) =>
  api<Endpoint>('/v1/development/model-providers/endpoint', { method: 'POST', body: JSON.stringify({ protocol, base_url: baseURL, chat_path: chatPath }) })

export interface ProbeResult { ok: boolean; chat_url: string; model: string; category: string; status: number; latency_ms: number; message: string }

export const probeProvider = (id: string, model: string) =>
  api<ProbeResult>(`/v1/development/model-providers/${encodeURIComponent(id)}/probe`, { method: 'POST', body: JSON.stringify({ model }) })

/** The refusal carried by a failed save or preview, when it was about the address. */
export function endpointRefusal(failure: unknown): EndpointRefusal | undefined {
  if (!(failure instanceof ApiError) || failure.status !== 422) return undefined
  const body = failure.body as { code?: string; error?: string; suggestion?: Endpoint } | undefined
  if (body?.code !== 'model_provider_endpoint_invalid') return undefined
  return { message: body.error ?? '地址无效', suggestion: body.suggestion }
}
