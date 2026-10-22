import { useCallback, useEffect, useState } from 'react'
import { Badge, Dialog, EmptyState, InlineError, Select, Switch } from '../components/ui'
import { errorMessage } from '../lib/api'
import { endpointRefusal, listProviders, probeProvider, protocols, resolveEndpoint, saveProvider, type Endpoint, type EndpointRefusal, type ModelProvider, type ProbeResult, type Protocol, type ProviderInput } from '../lib/providers'
import '../components/feishu/feishu.css'
import './model-providers.css'

const protocolLabel = (protocol: string) => protocols.find((item) => item.value === protocol)?.label ?? '未设置协议'

// The organization's model providers. Team members pick their models from
// here; each provider's requests go to its address exactly as shown.
export function ModelProvidersPage() {
  const [providers, setProviders] = useState<ModelProvider[]>()
  const [error, setError] = useState('')
  const [editing, setEditing] = useState<ModelProvider | 'new'>()
  const load = useCallback(async () => {
    try {
      setProviders(await listProviders())
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [])
  useEffect(() => { void load() }, [load])
  return <section className="page">
    <header className="page__header"><h1>模型</h1><button type="button" className="button button--primary" onClick={() => setEditing('new')}>添加供应方</button></header>
    <p className="muted small">团队成员从这里的模型中选择。请求发往“服务地址＋接口路径”；团队发布时，这个地址随版本固定下来。</p>
    {error ? <InlineError message={error} onRetry={() => void load()} /> : null}
    {providers && !providers.length ? <EmptyState title="还没有模型供应方">添加一个供应方，团队成员才能选择模型。</EmptyState> : null}
    <div className="providers">{providers?.map((provider) => <ProviderCard key={provider.id} provider={provider} onEdit={() => setEditing(provider)} />)}</div>
    {editing ? <ProviderEditor provider={editing === 'new' ? undefined : editing} onClose={() => setEditing(undefined)} onSaved={() => { setEditing(undefined); void load() }} /> : null}
  </section>
}

function ProviderCard({ provider, onEdit }: { provider: ModelProvider; onEdit(): void }) {
  const [model, setModel] = useState(provider.models[0] ?? '')
  const [probing, setProbing] = useState(false)
  const [result, setResult] = useState<ProbeResult>()
  const [error, setError] = useState('')
  const probe = async () => {
    setProbing(true)
    setError('')
    setResult(undefined)
    try {
      setResult(await probeProvider(provider.id, model))
    } catch (failure) {
      setError(endpointRefusal(failure)?.message ?? errorMessage(failure))
    } finally {
      setProbing(false)
    }
  }
  return <section className="integration" aria-label={provider.name}>
    <header className="integration__header"><h2>{provider.name}</h2><Badge tone={provider.deployment ? 'accent' : 'neutral'}>{provider.deployment ? '随部署提供' : protocolLabel(provider.protocol)}</Badge></header>
    <dl className="integration__facts">
      <div><dt>请求地址</dt><dd className="mono provider__url">{provider.chat_url || '—'}</dd></div>
      <div><dt>模型</dt><dd>{provider.models.join('、')}</dd></div>
    </dl>
    {!provider.chat_url && !provider.deployment ? <p className="integration__failed" role="status">还没有记录接口路径，请编辑后重新保存。</p> : null}
    {result ? <p className={result.ok ? 'integration__ok' : 'integration__failed'} role="status">
      {result.message}{result.status ? `（HTTP ${result.status}）` : ''}{result.ok ? ` · ${result.latency_ms} 毫秒` : ''}
      <span className="muted small provider__probed">已请求 {result.chat_url} · 模型 {result.model}</span>
    </p> : null}
    {error ? <InlineError message={error} /> : null}
    <div className="toolbar">
      {provider.models.length > 1 ? <Select label={`${provider.name}检查用的模型`} value={model} options={provider.models.map((item) => ({ value: item, label: item }))} onChange={setModel} /> : null}
      <button type="button" className="button" disabled={probing || !provider.chat_url} onClick={() => void probe()}>{probing ? '正在检查…' : '检查连接'}</button>
      {provider.deployment ? null : <button type="button" className="button" onClick={onEdit}>编辑</button>}
    </div>
  </section>
}

const blank: ProviderInput = { name: '', protocol: 'openai-completions', base_url: '', chat_path: '', models: [], json_object_mode: false, thinking_default_mode: '', thinking_disable_with_tools: false, attempt_timeout_seconds: 0 }

function ProviderEditor({ provider, onClose, onSaved }: { provider?: ModelProvider; onClose(): void; onSaved(): void }) {
  const [id, setID] = useState(provider?.id ?? '')
  const [input, setInput] = useState<ProviderInput>(() => provider ? {
    name: provider.name, protocol: (provider.protocol || 'openai-completions') as Protocol, base_url: provider.base_url, chat_path: provider.chat_path,
    models: provider.models, json_object_mode: provider.json_object_mode, thinking_default_mode: provider.thinking_default_mode,
    thinking_disable_with_tools: provider.thinking_disable_with_tools, attempt_timeout_seconds: provider.attempt_timeout_seconds,
  } : blank)
  const [models, setModels] = useState((provider?.models ?? []).join('\n'))
  const [key, setKey] = useState('')
  const [preview, setPreview] = useState<{ endpoint?: Endpoint; refusal?: EndpointRefusal }>({})
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const change = (patch: Partial<ProviderInput>) => { setInput((current) => ({ ...current, ...patch })); setError('') }
  const completions = input.protocol === 'openai-completions'
  const defaultPath = protocols.find((item) => item.value === input.protocol)?.path ?? ''

  // The address is resolved by the server, the same way it is saved.
  useEffect(() => {
    if (!input.base_url.trim()) { setPreview({}); return }
    let current = true
    const timer = setTimeout(() => {
      resolveEndpoint(input.protocol, input.base_url, input.chat_path)
        .then((endpoint) => { if (current) setPreview({ endpoint }) })
        .catch((failure) => { if (current) setPreview({ refusal: endpointRefusal(failure) ?? { message: errorMessage(failure) } }) })
    }, 250)
    return () => { current = false; clearTimeout(timer) }
  }, [input.protocol, input.base_url, input.chat_path])

  const save = async () => {
    setBusy(true)
    setError('')
    try {
      const list = [...new Set(models.split(/[\n,，]/).map((item) => item.trim()).filter(Boolean))]
      await saveProvider(id.trim(), { ...input, models: list, ...(key.trim() ? { api_key: key.trim() } : {}),
        // Thinking switches belong to the Chat Completions dialect only.
        ...(completions ? {} : { json_object_mode: false, thinking_default_mode: '', thinking_disable_with_tools: false }) })
      onSaved()
    } catch (failure) {
      const refusal = endpointRefusal(failure)
      if (refusal) setPreview({ refusal })
      setError(refusal?.message ?? errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const adopt = (endpoint: Endpoint) => change({ base_url: endpoint.base_url, chat_path: endpoint.chat_path })

  return <Dialog title={provider ? `编辑“${provider.name}”` : '添加模型供应方'} onClose={onClose} footer={<>
    <button type="button" className="button" onClick={onClose}>取消</button>
    <button type="button" className="button button--primary" disabled={busy || Boolean(preview.refusal)} onClick={() => void save()}>{busy ? '正在保存…' : '保存'}</button>
  </>}>
    <form className="stack" onSubmit={(event) => { event.preventDefault(); void save() }}>
      <div className="integration__grid">
        <label className="field"><span>标识</span><input className="input mono" spellCheck={false} disabled={Boolean(provider)} value={id} placeholder="例如 kimi" onChange={(event) => setID(event.target.value)} /></label>
        <label className="field"><span>名称</span><input className="input" value={input.name} maxLength={80} onChange={(event) => change({ name: event.target.value })} /></label>
      </div>
      <div className="field"><span>接口协议</span><Select label="接口协议" value={input.protocol} options={protocols.map((item) => ({ value: item.value, label: item.label }))} onChange={(protocol) => change({ protocol })} /></div>
      <label className="field"><span>服务地址</span><input className="input mono" spellCheck={false} value={input.base_url} placeholder="https://api.example.com" onChange={(event) => change({ base_url: event.target.value })} /></label>
      <label className="field"><span>接口路径</span><input className="input mono" spellCheck={false} value={input.chat_path} placeholder={`留空使用 ${defaultPath}`} onChange={(event) => change({ chat_path: event.target.value })} /></label>
      <div className="provider__preview" aria-live="polite">
        <span className="muted small">实际请求地址</span>
        {preview.endpoint ? <code className="mono">{preview.endpoint.chat_url}</code> : null}
        {preview.refusal ? <>
          <p className="integration__failed">{preview.refusal.message}</p>
          {preview.refusal.suggestion ? <div className="provider__suggestion">
            <span>建议：服务地址 <code className="mono">{preview.refusal.suggestion.base_url}</code>，接口路径 <code className="mono">{preview.refusal.suggestion.chat_path}</code>，请求发往 <code className="mono">{preview.refusal.suggestion.chat_url}</code></span>
            <button type="button" className="button" onClick={() => adopt(preview.refusal!.suggestion!)}>按建议填写</button>
          </div> : null}
        </> : null}
        {!preview.endpoint && !preview.refusal ? <span className="muted">填写服务地址后显示</span> : null}
      </div>
      <label className="field"><span>模型（每行一个）</span><textarea className="input textarea mono" rows={3} spellCheck={false} value={models} onChange={(event) => { setModels(event.target.value); setError('') }} /></label>
      <label className="field"><span>密钥</span><input className="input mono" type="password" autoComplete="new-password" spellCheck={false} value={key} placeholder={provider ? '已保存，留空保持不变' : '必填'} onChange={(event) => { setKey(event.target.value); setError('') }} /></label>
      <details className="provider__advanced">
        <summary>高级设置</summary>
        <div className="stack">
          <label className="field"><span>单次请求超时（秒，0 为默认 900）</span><input className="input" type="number" min={0} max={86400} value={input.attempt_timeout_seconds} onChange={(event) => change({ attempt_timeout_seconds: Math.max(0, Number(event.target.value) || 0) })} /></label>
          {completions ? <>
            <Switch checked={input.json_object_mode} label="结构化输出只要求 JSON 对象（服务不支持 json_schema 时开启）" onChange={(json_object_mode) => change({ json_object_mode })} />
            <div className="field"><span>思考开关（DeepSeek 方言）</span><Select label="思考开关" value={input.thinking_default_mode || 'none'} options={[{ value: 'none', label: '不发送' }, { value: 'enabled', label: '默认开启' }, { value: 'disabled', label: '默认关闭' }]}
              onChange={(mode) => change({ thinking_default_mode: mode === 'none' ? '' : mode as 'enabled' | 'disabled', ...(mode === 'none' ? { thinking_disable_with_tools: false } : {}) })} /></div>
            {input.thinking_default_mode ? <Switch checked={input.thinking_disable_with_tools} label="带工具的请求关闭思考" onChange={(thinking_disable_with_tools) => change({ thinking_disable_with_tools })} /> : null}
          </> : null}
        </div>
      </details>
      {error ? <InlineError message={error} /> : null}
    </form>
  </Dialog>
}
