import { Plus, X } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { Badge, Drawer, EmptyState, InlineError, Select, Switch } from '../components/ui'
import type { AdminSession } from '../lib/api'
import {
  cleanModels, deleteProvider, endpointRefusal, hasKey, listProviders, listSystemProviders, mirrorSystemProvider, modelParametersText, parseModelParameters, probeProvider, protocolLabels, protocolOf, providerError, providerInput, resolveEndpoint, saveProvider, validBaseURL,
  type Endpoint, type EndpointRefusal, type KeyChange, type ProbeResult, type Provider, type ProviderInput, type ProviderProtocol, type SystemProvider,
} from '../lib/providers'
import { loadModelCatalog } from '../lib/teams'
import './models.css'

const canAdminister = (session: AdminSession) => ['admin', 'owner'].includes(session.role)
const canConfigure = (session: AdminSession) => ['developer', 'admin', 'owner'].includes(session.role)

const protocolOptions: Array<{ value: ProviderProtocol; label: string }> = [
  { value: 'openai', label: protocolLabels.openai },
  { value: 'systemone', label: protocolLabels.systemone },
]
const systemOneNote = '决策模型回答结构化问题，不调用工具；可以和其他模型成员一起参与团队流程。'

// The deployment page's "模型" section.
export function ModelsSection({ session }: { session: AdminSession }) {
  const editable = canConfigure(session)
  const [providers, setProviders] = useState<Provider[]>()
  const [system, setSystem] = useState<SystemProvider[]>([])
  const [catalog, setCatalog] = useState<string[]>()
  const [error, setError] = useState('')
  const [editing, setEditing] = useState<Provider | 'new'>()
  const [mirroring, setMirroring] = useState('')
  const refresh = useCallback(async () => {
    try {
      const all = await listProviders()
      // A mirrored system provider is shown with the built-in ones.
      setProviders(all.filter((provider) => !provider.deployment))
      setError('')
    } catch (failure) {
      setError(providerError(failure))
    }
    if (canAdminister(session)) void listSystemProviders().then(setSystem).catch(() => setSystem([]))
    void loadModelCatalog().then(setCatalog).catch(() => setCatalog(undefined))
  }, [session.role])
  useEffect(() => { void refresh() }, [refresh])
  const mirror = async (id: string) => {
    setMirroring(id)
    try {
      await mirrorSystemProvider(id)
      await refresh()
    } catch (failure) {
      setError(providerError(failure))
    } finally {
      setMirroring('')
    }
  }
  const selectable = new Set(catalog ?? [])
  const addButton = <button type="button" className="button button--primary" onClick={() => setEditing('new')}><Plus size={15} />添加 provider</button>

  return <div className="stack">
    <header className="page__header">
      <p className="muted">成员在团队的“成员”页从这里的模型中选择。决策模型成员也是普通成员，只是它的模型由 System One 协议的 provider 提供。</p>
      {editable ? addButton : null}
    </header>
    {error ? <InlineError message={error} onRetry={() => void refresh()} /> : null}

    <section className="stack" aria-label="本工作区 provider">
      <h2>本工作区</h2>
      {!providers ? null : providers.length === 0 ? <EmptyState title="还没有 provider" action={editable ? addButton : undefined}>
        添加一个 provider：填写地址和它提供的模型；密钥可以不填。
      </EmptyState> : <div className="task-list">{providers.map((provider) => <button key={provider.id} type="button" className="task-row" onClick={() => setEditing(provider)}>
        <span className="task-row__main">
          <span className="models__title"><strong>{provider.name || provider.id}</strong><Badge tone={protocolOf(provider) === 'systemone' ? 'claimed' : 'neutral'}>{protocolLabels[protocolOf(provider)]}</Badge></span>
          <span className="muted small mono">{provider.chat_url || provider.base_url}</span>
          <ModelChips models={provider.models} selectable={catalog ? selectable : undefined} />
        </span>
        <Badge tone={hasKey(provider) ? 'success' : 'neutral'}>{hasKey(provider) ? '密钥已设置' : '密钥未设置'}</Badge>
      </button>)}</div>}
    </section>

    {system.length ? <section className="stack" aria-label="内置 provider">
      <h2>内置</h2>
      <p className="muted small">随部署提供，不能在这里修改。镜像到本工作区后，成员可以选用它的模型。</p>
      <div className="task-list">{system.map((provider) => <div key={provider.id} className="task-row models__static">
        <span className="task-row__main">
          <span className="models__title"><strong>{provider.name || provider.id}</strong><Badge tone="accent">内置</Badge><Badge tone={protocolOf(provider) === 'systemone' ? 'claimed' : 'neutral'}>{protocolLabels[protocolOf(provider)]}</Badge></span>
          <span className="muted small mono">{provider.base_url}</span>
          <ModelChips models={provider.models} selectable={catalog ? selectable : undefined} />
        </span>
        {provider.mirrored ? <Badge tone="success">已镜像到本工作区</Badge>
          : editable ? <button type="button" className="button" disabled={Boolean(mirroring)} onClick={() => void mirror(provider.id)}>{mirroring === provider.id ? '正在镜像…' : '镜像到本工作区'}</button>
            : <Badge>未镜像</Badge>}
      </div>)}</div>
    </section> : null}

    <section className="stack" aria-label="成员可选模型">
      <h2>成员可选模型</h2>
      {catalog === undefined ? <p className="muted small">暂时读不到成员可选的模型。</p>
        : catalog.length ? <div className="chips">{catalog.map((model) => <Badge key={model}>{model}</Badge>)}</div>
          : <p className="muted small">还没有成员可选的模型。</p>}
    </section>

    {editing ? <ProviderDrawer provider={editing === 'new' ? undefined : editing} existing={providers ?? []} editable={editable} canDelete={canAdminister(session)}
      onClose={() => setEditing(undefined)} onSaved={() => { setEditing(undefined); void refresh() }} /> : null}
  </div>
}

function ModelChips({ models, selectable }: { models: string[]; selectable?: Set<string> }) {
  if (!models?.length) return <span className="muted small">没有模型</span>
  const missing = selectable ? models.filter((model) => !selectable.has(model)).length : 0
  return <span className="chips">
    {models.map((model) => <span key={model} className="badge mono">{model}</span>)}
    {missing ? <span className="muted small">{missing === models.length ? '成员暂不可选' : `其中 ${missing} 个成员暂不可选`}</span> : null}
  </span>
}

type KeyMode = KeyChange['kind']

function ProviderDrawer({ provider, existing, editable, canDelete, onClose, onSaved }: { provider?: Provider; existing: Provider[]; editable: boolean; canDelete:boolean; onClose(): void; onSaved(): void }) {
  const [form, setForm] = useState<ProviderInput>(() => providerInput(provider))
  const [keyMode, setKeyMode] = useState<KeyMode>(provider ? 'keep' : 'replace')
  const [key, setKey] = useState('')
  const [confirmClear, setConfirmClear] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [preview, setPreview] = useState<{ endpoint?: Endpoint; refusal?: EndpointRefusal }>({})
  const [probeModel, setProbeModel] = useState(provider?.models?.[0] ?? '')
  const [probing, setProbing] = useState(false)
  const [probe, setProbe] = useState<ProbeResult>()
  // Declarations travel only once edited; left alone, the server keeps them.
  const [parameters, setParameters] = useState(() => ({ text: modelParametersText(provider?.model_parameters), edited: false }))
  const stored = provider ? hasKey(provider) : false
  // The address is resolved by the server, the same way it is saved.
  useEffect(() => {
    if (!validBaseURL(form.base_url)) { setPreview({}); return }
    let current = true
    const timer = setTimeout(() => {
      resolveEndpoint(form.protocol, form.base_url, form.chat_path)
        .then((endpoint) => { if (current) setPreview(endpoint?.chat_url ? { endpoint } : {}) })
        .catch((failure) => { if (current) setPreview({ refusal: endpointRefusal(failure) ?? { message: providerError(failure) } }) })
    }, 250)
    return () => { current = false; clearTimeout(timer) }
  }, [form.protocol, form.base_url, form.chat_path])
  const adopt = (endpoint: Endpoint) => setForm((current) => ({ ...current, base_url: endpoint.base_url, chat_path: endpoint.chat_path }))
  const check = async () => {
    if (!provider) return
    setProbing(true)
    setProbe(undefined)
    setError('')
    try {
      setProbe(await probeProvider(provider.id, probeModel))
    } catch (failure) {
      setError(providerError(failure))
    } finally {
      setProbing(false)
    }
  }
  const openai = form.protocol === 'openai'
  const set = <K extends keyof ProviderInput>(field: K, value: ProviderInput[K]) => { setForm((current) => ({ ...current, [field]: value })); setError('') }
  const setModel = (index: number, value: string) => set('models', form.models.map((model, position) => position === index ? value : model))
  const chooseKey = (mode: KeyMode) => {
    setError('')
    if (mode === 'clear') { setConfirmClear(true); return }
    setConfirmClear(false)
    setKeyMode(mode)
    if (mode === 'keep') setKey('')
  }

  const submit = async () => {
    const name = form.name.trim()
    if (!name) { setError('请填写名称'); return }
    if (!provider && existing.some((item) => item.id === name || item.name === name)) { setError('已有同名的 provider，请换一个名称，或打开它进行编辑'); return }
    if (!validBaseURL(form.base_url)) { setError('地址须以 http:// 或 https:// 开头'); return }
    const { models, problem } = cleanModels(form.models)
    if (problem) { setError(problem); return }
    const declared = parameters.edited ? parseModelParameters(parameters.text) : {}
    if (declared.problem) { setError(declared.problem); return }
    let change: KeyChange
    if (keyMode === 'replace') {
      if (key.trim()) change = { kind: 'replace', value: key.trim() }
      else if (provider) { setError('请填写新密钥，或选择“保持不变”'); return }
      else change = { kind: 'keep' }
    } else change = { kind: keyMode }
    setBusy(true)
    try {
      await saveProvider({ ...form, name, base_url: form.base_url.trim(), models, model_parameters: declared.value }, change, provider?.id)
      onSaved()
    } catch (failure) {
      const refusal = endpointRefusal(failure)
      if (refusal) setPreview({ refusal })
      setError(refusal?.message ?? providerError(failure))
    } finally {
      setBusy(false)
    }
  }
  const remove = async () => {
    if (!provider) return
    setBusy(true)
    try {
      await deleteProvider(provider.id)
      onSaved()
    } catch (failure) {
      setError(providerError(failure))
      setBusy(false)
    }
  }

  return <Drawer title={provider ? provider.name || provider.id : '添加 provider'} onClose={onClose} footer={editable ? <div className="toolbar">
    <button type="button" className="button button--primary" disabled={busy || Boolean(preview.refusal)} onClick={() => void submit()}>{busy ? '正在保存…' : '保存'}</button>
    {provider && canDelete ? <button type="button" className="button button--danger" disabled={busy} onClick={() => setConfirmDelete(true)}>删除</button> : null}
  </div> : undefined}>
    <form className="stack" onSubmit={(event) => { event.preventDefault(); void submit() }}>
      <label className="field"><span>名称</span><input className="input" value={form.name} maxLength={80} disabled={!editable} onChange={(event) => set('name', event.target.value)} placeholder="deepseek" autoFocus /></label>
      <label className="field"><span>地址</span><input className="input mono" value={form.base_url} disabled={!editable} onChange={(event) => set('base_url', event.target.value)} placeholder="https://api.example.com/v1" spellCheck={false} /></label>
      <div className="field"><span>协议</span><Select label="协议" value={form.protocol} disabled={!editable} options={protocolOptions} onChange={(protocol) => set('protocol', protocol)} /></div>
      <label className="field"><span>接口路径</span><input className="input mono" value={form.chat_path} disabled={!editable} onChange={(event) => set('chat_path', event.target.value)}
        placeholder={form.protocol === 'systemone' ? '留空使用 /v1/systemone' : '留空使用 /v1/chat/completions'} spellCheck={false} /></label>
      <div className="models__preview" aria-live="polite" aria-label="实际请求地址">
        <span className="muted small">实际请求地址</span>
        {preview.endpoint ? <code className="mono">{preview.endpoint.chat_url}</code> : null}
        {preview.refusal ? <>
          <p className="small" role="alert">{preview.refusal.message}</p>
          {preview.refusal.suggestion ? <div className="toolbar">
            <span className="small">建议：地址 <code className="mono">{preview.refusal.suggestion.base_url}</code>，接口路径 <code className="mono">{preview.refusal.suggestion.chat_path}</code>，请求发往 <code className="mono">{preview.refusal.suggestion.chat_url}</code></span>
            {editable ? <button type="button" className="button" onClick={() => adopt(preview.refusal!.suggestion!)}>按建议填写</button> : null}
          </div> : null}
        </> : null}
        {!preview.endpoint && !preview.refusal ? <span className="muted small">{provider?.chat_url || '填写地址后显示'}</span> : null}
      </div>
      {form.protocol === 'systemone' ? <p className="muted small">{systemOneNote}</p> : null}

      <div className="field" role="group" aria-label="模型"><span>模型</span>
        <div className="models__list">{form.models.map((model, index) => <div key={index} className="models__item">
          <input className="input mono" aria-label={`模型 ${index + 1}`} value={model} disabled={!editable} onChange={(event) => setModel(index, event.target.value)} placeholder="deepseek-chat" spellCheck={false} />
          {editable ? <button type="button" className="icon-button" aria-label={`移除模型 ${index + 1}`} disabled={form.models.length <= 1} onClick={() => set('models', form.models.filter((_, position) => position !== index))}><X size={14} /></button> : null}
        </div>)}</div>
        {editable ? <div className="toolbar"><button type="button" className="button" onClick={() => set('models', [...form.models, ''])}><Plus size={14} />添加模型</button></div> : null}
      </div>

      {provider ? <div className="field"><span>密钥</span>
        <p className="small">{stored ? '已设置' : '未设置'}</p>
        {editable ? <div className="segmented" role="group" aria-label="密钥">
          <button type="button" aria-pressed={keyMode === 'keep'} onClick={() => chooseKey('keep')}>保持不变</button>
          <button type="button" aria-pressed={keyMode === 'replace'} onClick={() => chooseKey('replace')}>替换</button>
          <button type="button" aria-pressed={keyMode === 'clear'} disabled={!stored} onClick={() => chooseKey('clear')}>清空</button>
        </div> : null}
      </div> : null}
      {confirmClear ? <div className="confirm" role="alertdialog" aria-label="确认清空密钥">
        <p>清空后，调用这个 provider 时不再携带密钥。</p>
        <div className="toolbar"><button type="button" className="button" onClick={() => setConfirmClear(false)}>取消</button>
          <button type="button" className="button button--danger" onClick={() => { setConfirmClear(false); setKeyMode('clear'); setKey('') }}>清空密钥</button></div>
      </div> : null}
      {keyMode === 'clear' ? <p className="muted small">保存后清空密钥</p> : null}
      {keyMode === 'replace' && editable ? <label className="field"><span>{provider ? '新密钥' : '密钥'}</span>
        <input className="input mono" type="password" autoComplete="new-password" spellCheck={false} value={key} onChange={(event) => { setKey(event.target.value); setError('') }}
          placeholder={provider ? '填写替换后的密钥' : '可留空：不填则调用时不带密钥'} /></label> : null}

      {openai ? <details className="disclosure">
        <summary>高级选项</summary>
        <div className="stack models__advanced">
          <Switch checked={form.json_object_mode} disabled={!editable} label="结构化输出使用 JSON object 模式" onChange={(value) => set('json_object_mode', value)} />
          <div className="field"><span>输出上限字段</span><Select label="输出上限字段" value={form.output_limit_field || 'max_tokens'} disabled={!editable}
            options={[{ value: 'max_tokens', label: 'max_tokens（默认）' }, { value: 'max_completion_tokens', label: 'max_completion_tokens（推理模型和部分兼容服务要求）' }]}
            onChange={(field) => set('output_limit_field', field === 'max_tokens' ? '' : field as 'max_completion_tokens')} /></div>
          <label className="field"><span>模型参数声明（JSON）</span>
            <textarea className="input textarea" rows={6} value={parameters.text} disabled={!editable} spellCheck={false}
              placeholder={'{"模型名": {"reasoning_efforts": ["low", "high"], "thinking": ["enabled", "disabled"], "temperature": {"min": 0, "max": 1}, "top_p": {"min": 0, "max": 1}}}'}
              onChange={(event) => { setParameters({ text: event.target.value, edited: true }); setError('') }} /></label>
          <p className="muted small">按模型声明成员可设置的推理档位、思考开关和 temperature/top_p 范围；随供应方版本冻结进发布。未声明的模型只接受推理档位，成员设置的思考开关或采样参数会在发布时被拒绝。留空保存会清除全部声明。</p>
        </div>
      </details> : null}

      {provider ? <div className="field" role="group" aria-label="检查连接"><span>检查连接</span>
        <div className="toolbar">
          {provider.models.length > 1 ? <Select label="检查用的模型" value={probeModel} options={provider.models.map((model) => ({ value: model, label: model }))} onChange={setProbeModel} /> : null}
          <button type="button" className="button" disabled={probing} onClick={() => void check()}>{probing ? '正在检查…' : '检查连接'}</button>
        </div>
        {probe ? <p className="small" role="status">{probe.message}{probe.status ? `（HTTP ${probe.status}）` : ''}{probe.ok ? ` · ${probe.latency_ms} 毫秒` : ''}
          <span className="muted small"> · 已请求 {probe.chat_url} · 模型 {probe.model}</span></p> : null}
      </div> : null}

      {confirmDelete && provider ? <div className="confirm" role="alertdialog" aria-label="确认删除 provider">
        <p>使用该 provider 模型的成员将无模型可用，需改选。</p>
        {provider.models?.length ? <p className="muted small">涉及模型：{provider.models.join('、')}</p> : null}
        <div className="toolbar"><button type="button" className="button" onClick={() => setConfirmDelete(false)}>取消</button><button type="button" className="button button--danger" disabled={busy} onClick={() => void remove()}>删除</button></div>
      </div> : null}
      {error ? <InlineError message={error} /> : null}
    </form>
  </Drawer>
}
