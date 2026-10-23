import { api, apiResponse, ApiError, errorMessage } from './api'

// Model providers: an address, the models it serves, the protocol it speaks
// and an optional key. The key is write-only; the list reports it masked.

export type ProviderProtocol = 'openai' | 'systemone'
export type ThinkingMode = '' | 'enabled' | 'disabled'
export type OutputLimitField = '' | 'max_tokens' | 'max_completion_tokens'

export interface Provider {
  id: string
  name: string
  base_url: string
  /** The recorded path, empty for a provider saved before paths were recorded. */
  chat_path?: string
  /** The address requests go to, as the server resolves it. */
  chat_url?: string
  protocol?: ProviderProtocol
  models: string[]
  /** A masked string when a key is stored; empty or absent when there is none. */
  api_key?: string
  has_api_key?: boolean
  json_object_mode?: boolean
  /** The field the output limit is sent as; empty means max_tokens. */
  output_limit_field?: OutputLimitField
  thinking_default_mode?: ThinkingMode
  thinking_disable_with_tools?: boolean
  attempt_timeout_seconds?: number
  source_kind?: string
  source_provider_id?: string
  deployment?: boolean
}

export interface SystemProvider {
  id: string
  name: string
  base_url: string
  protocol?: ProviderProtocol
  models: string[]
  mirrored: boolean
  mirrored_as?: string
}

export interface ProviderInput {
  name: string
  base_url: string
  /** Empty leaves the path to the server: a new provider records its protocol's default, an existing one keeps its own. */
  chat_path: string
  protocol: ProviderProtocol
  models: string[]
  json_object_mode: boolean
  output_limit_field: OutputLimitField
}

/** What happens to the stored key on save. A new provider only keeps or replaces. */
export type KeyChange = { kind: 'keep' } | { kind: 'replace'; value: string } | { kind: 'clear' }

export const protocolLabels: Record<ProviderProtocol, string> = { openai: 'OpenAI 兼容', systemone: 'System One 决策模型' }
export const protocolOf = (provider: { protocol?: string }): ProviderProtocol => provider.protocol === 'systemone' ? 'systemone' : 'openai'

export function hasKey(provider: Provider): boolean {
  if (typeof provider.has_api_key === 'boolean') return provider.has_api_key
  return typeof provider.api_key === 'string' && provider.api_key.trim() !== ''
}

// The one place that decides how a key change travels: leaving it out keeps
// the stored key, an empty string clears it.
export function keyPayload(change: KeyChange): { api_key?: string } {
  if (change.kind === 'replace') return { api_key: change.value }
  if (change.kind === 'clear') return { api_key: '' }
  return {}
}

export function providerBody(input: ProviderInput, key: KeyChange): Record<string, unknown> {
  const base = { name: input.name, base_url: input.base_url, protocol: input.protocol, models: input.models,
    ...(input.chat_path.trim() ? { chat_path: input.chat_path.trim() } : {}) }
  // The tuning switches only mean something to the OpenAI-compatible client.
  const tuning = input.protocol === 'openai' ? {
    json_object_mode: input.json_object_mode,
    output_limit_field: input.output_limit_field,
  } : {}
  return { ...base, ...tuning, ...keyPayload(key) }
}

export function providerInput(provider?: Provider): ProviderInput {
  return {
    name: provider?.name ?? '',
    base_url: provider?.base_url ?? '',
    chat_path: provider?.chat_path ?? '',
    protocol: provider ? protocolOf(provider) : 'openai',
    models: provider?.models?.length ? [...provider.models] : [''],
    json_object_mode: Boolean(provider?.json_object_mode),
    output_limit_field: provider?.output_limit_field ?? '',
  }
}

export function validBaseURL(value: string): boolean {
  try {
    const url = new URL(value.trim())
    return (url.protocol === 'http:' || url.protocol === 'https:') && Boolean(url.hostname)
  } catch {
    return false
  }
}

/** Trimmed, de-duplicated model names, or the problem with them. */
export function cleanModels(models: string[]): { models: string[]; problem?: string } {
  const seen = new Set<string>()
  for (const raw of models) {
    const model = raw.trim()
    if (!model) continue
    if (/\s/.test(model)) return { models: [], problem: `模型名称“${model}”不能含空白` }
    seen.add(model)
  }
  if (!seen.size) return { models: [], problem: '请至少填写一个模型' }
  return { models: [...seen] }
}

const list = <T>(body: unknown, key: string): T[] => Array.isArray(body) ? body as T[] : body && typeof body === 'object' && Array.isArray((body as Record<string, unknown>)[key]) ? (body as Record<string, T[]>)[key] : []

const sharedProviders = '/v1/development/model-providers'
export const listProviders = () => api<unknown>(sharedProviders).then((body) => list<Provider>(body, 'providers'))
export const listSystemProviders = () => api<unknown>('/v1/providers/system').then((body) => list<SystemProvider>(body, 'providers'))
export const saveProvider = (input: ProviderInput, key: KeyChange, id?: string) => {
  const providerID = id ?? (/^[a-z0-9][a-z0-9-]{0,62}$/.test(input.name) ? input.name : `provider-${crypto.randomUUID()}`)
  return api<unknown>(`${sharedProviders}/${encodeURIComponent(providerID)}`, { method: 'PUT', body: JSON.stringify(providerBody(input, key)) })
}
export async function deleteProvider(id: string): Promise<void> {
  const key = crypto.randomUUID()
  for (let attempt = 0; attempt < 5; attempt++) {
    const response = await apiResponse(`${sharedProviders}/${encodeURIComponent(id)}`, { method: 'DELETE', headers: { 'Idempotency-Key': key } })
    if (response.status === 204) return
    if (response.status !== 202) throw new ApiError(response.status, '请求未完成')
    await new Promise((resolve) => setTimeout(resolve, Math.min(5000, Math.max(100, Number(response.headers.get('Retry-After') ?? 1) * 1000))))
  }
  throw new ApiError(409, '删除尚未完成，请稍后重试')
}
export interface Endpoint { base_url: string; chat_path: string; chat_url: string }

/** A refused address: why, and the split that reaches what was most likely meant. */
export interface EndpointRefusal { message: string; suggestion?: Endpoint }

// The server resolves the address the same way it saves it; nothing is stored.
export const resolveEndpoint = (protocol: ProviderProtocol, baseURL: string, chatPath: string) =>
  api<Endpoint>(`${sharedProviders}/endpoint`, { method: 'POST', body: JSON.stringify({ protocol, base_url: baseURL, ...(chatPath.trim() ? { chat_path: chatPath.trim() } : {}) }) })

export interface ProbeResult { ok: boolean; chat_url: string; model: string; category: string; status: number; latency_ms: number; message: string }

export const probeProvider = (id: string, model: string) =>
  api<ProbeResult>(`${sharedProviders}/${encodeURIComponent(id)}/probe`, { method: 'POST', body: JSON.stringify({ model }) })

/** The refusal carried by a failed save or preview, when it was about the address. */
export function endpointRefusal(failure: unknown): EndpointRefusal | undefined {
  if (!(failure instanceof ApiError) || failure.status !== 422) return undefined
  const body = failure.body as { code?: string; error?: string; suggestion?: Endpoint } | undefined
  if (body?.code !== 'model_provider_endpoint_invalid') return undefined
  return { message: body.error ?? '地址无效', suggestion: body.suggestion }
}

export const mirrorSystemProvider = (id: string) => api<unknown>(`/v1/providers/system/${encodeURIComponent(id)}/mirror`, { method: 'POST', body: '{}' })

const messages: Record<string, string> = {
  'credential encryption key not configured': '服务端未配置凭据加密密钥，暂时不能保存模型',
  'insufficient permissions': '需要管理员权限',
  'no roles': '需要管理员权限',
  'user credential identity is required': '当前登录无法保存模型，请用管理员账号登录',
  workflow_provider_revision_required: '配置不完整或地址无效：地址须是完整的 http 或 https 地址，不带账号、查询参数和片段',
  workflow_credential_unavailable: '这个 provider 现在不能修改，请刷新后重试',
}

export function providerError(error: unknown): string {
  if (error instanceof ApiError && messages[error.code]) return messages[error.code]
  return errorMessage(error)
}
