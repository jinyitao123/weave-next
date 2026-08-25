import { useCallback, useEffect, useState, type FormEvent } from "react";
import { DatabaseZap, KeyRound, Pencil, Plus, RefreshCw, ServerCog, ShieldCheck, Trash2 } from "lucide-react";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type ApiError,
  type EmbedderConfig,
  type ProviderConfigInput,
  type ProviderHead,
  type ProviderMirrorResult,
  type SystemProviderSummary,
} from "../../api";
import { useAuth } from "../../auth/useAuth";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { Switch } from "../../ui/Switch";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";

const DEFAULT_EMBEDDER_MODEL = "text-embedding-3-small";
const DEFAULT_EMBEDDER_DIMENSION = 1536;
const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });

type ProviderEditor = "create" | "edit" | null;
interface ProviderForm { id: string; name: string; baseUrl: string; apiKey: string; models: string; jsonObjectMode: boolean }

function emptyProviderForm(): ProviderForm {
  return { id: "", name: "", baseUrl: "", apiKey: "", models: "", jsonObjectMode: false };
}
function providerForm(provider: ProviderHead): ProviderForm {
  return { id: provider.id, name: provider.name, baseUrl: provider.base_url, apiKey: provider.api_key || "", models: provider.models.join("\n"), jsonObjectMode: provider.json_object_mode };
}
function providerPayload(form: ProviderForm): ProviderConfigInput {
  return {
    id: form.id.trim(),
    name: form.name.trim(),
    base_url: form.baseUrl.trim(),
    api_key: form.apiKey,
    models: Array.from(new Set(form.models.split(/[\n,]/).map((value) => value.trim()).filter(Boolean))),
    json_object_mode: form.jsonObjectMode,
  };
}
function formatDate(value?: string | null) { return value ? dateTime.format(new Date(value)) : "—"; }
function providerStatus(provider: ProviderHead) {
  if (provider.deleted_at) return ["已删除", "danger"] as const;
  if (provider.revoked_at) return ["已撤销", "danger"] as const;
  if (!provider.enabled) return ["已关闭", "neutral"] as const;
  return ["可用", "success"] as const;
}
function providerSource(sourceKind: string) {
  if (sourceKind === "workspace") return "工作区配置";
  if (sourceKind === "system_mirror") return "系统供应商镜像";
  return "—";
}
function providerError(error: ApiError) {
  if (error.message === "credential encryption key not configured") return "凭据加密未配置：请联系管理员在服务端配置加密密钥。";
  if (error.status === 403) return "当前账号没有修改模型配置的权限。";
  return apiErrorMessage(error);
}
function validateHTTPURL(raw: string) {
  try {
    const parsed = new URL(raw);
    return /^https?:$/.test(parsed.protocol) && !parsed.username && !parsed.password && !parsed.search && !parsed.hash;
  } catch { return false; }
}

export function ProvidersPage() {
  const { user } = useAuth();
  const canWrite = user?.role === "admin";
  const [providers, setProviders] = useState<ProviderHead[]>([]);
  const [embedder, setEmbedder] = useState<EmbedderConfig | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [providerErrorState, setProviderErrorState] = useState<ApiError | null>(null);
  const [embedderError, setEmbedderError] = useState<ApiError | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [editor, setEditor] = useState<ProviderEditor>(null);
  const [editing, setEditing] = useState<ProviderHead | null>(null);
  const [form, setForm] = useState<ProviderForm>(emptyProviderForm);
  const [embedderOpen, setEmbedderOpen] = useState(false);
  const [embedderForm, setEmbedderForm] = useState<EmbedderConfig>({ url: "", api_key: "", model: DEFAULT_EMBEDDER_MODEL, dimension: DEFAULT_EMBEDDER_DIMENSION });
  const [busy, setBusy] = useState(false);
  const [deleting, setDeleting] = useState<ProviderHead | null>(null);
  const [deleteText, setDeleteText] = useState("");
  const [deleteEmbedderOpen, setDeleteEmbedderOpen] = useState(false);
  const [mirrorOpen, setMirrorOpen] = useState(false);
  const [mirrorID, setMirrorID] = useState("");
  const [mirrorReason, setMirrorReason] = useState("");
  const [systemProviders, setSystemProviders] = useState<SystemProviderSummary[] | null>(null);
  const [systemProvidersError, setSystemProvidersError] = useState<string | null>(null);

  const load = useCallback(async (initial = false) => {
    if (initial) setLoading(true); else setRefreshing(true);
    setProviderErrorState(null); setEmbedderError(null);
    const [providerResult, embedderResult] = await Promise.allSettled([api.listProviders(), api.getEmbedder()]);
    if (providerResult.status === "fulfilled") setProviders(providerResult.value);
    else setProviderErrorState(normalizeThrownError(providerResult.reason));
    if (embedderResult.status === "fulfilled") {
      setEmbedder(embedderResult.value);
      setEmbedderForm(embedderResult.value);
    } else setEmbedderError(normalizeThrownError(embedderResult.reason));
    setLoading(false); setRefreshing(false);
  }, []);

  useEffect(() => { void load(true); }, [load]);

  useEffect(() => {
    if (!mirrorOpen) return;
    const controller = new AbortController();
    setSystemProviders(null); setSystemProvidersError(null);
    api.listSystemProviders(controller.signal)
      .then(setSystemProviders)
      .catch((error: unknown) => {
        if (controller.signal.aborted) return;
        setSystemProvidersError(apiErrorMessage(normalizeThrownError(error)));
      });
    return () => controller.abort();
  }, [mirrorOpen]);

  function openMirror() { setMirrorID(""); setMirrorReason(""); setActionError(null); setMirrorOpen(true); }

  function beginCreate() { setEditing(null); setForm(emptyProviderForm()); setEditor("create"); setActionError(null); }
  function beginEdit(provider: ProviderHead) { setEditing(provider); setForm(providerForm(provider)); setEditor("edit"); setActionError(null); }

  async function saveProvider(event: FormEvent) {
    event.preventDefault(); setActionError(null); setNotice(null);
    const payload = providerPayload(form);
    if (!payload.id || !payload.name) { setActionError("ID 与名称不能为空。"); return; }
    if (payload.id.startsWith("system/") || payload.id !== form.id.trim()) { setActionError("ID 不能以 system/ 开头，该前缀为系统保留。"); return; }
    if (!validateHTTPURL(payload.base_url)) { setActionError("服务地址需为完整的 HTTP(S) 地址，不含用户名密码和额外参数。"); return; }
    if (!payload.models.length) { setActionError("至少填写一个模型 ID。"); return; }
    setBusy(true);
    try {
      if (editor === "create") await api.createProvider(payload);
      else await api.updateProvider(editing?.id || payload.id, payload);
      setEditor(null); setEditing(null); setNotice(editor === "create" ? "供应商已创建。" : "供应商已更新；未修改的 API Key 保持原值。") ;
      await load();
    } catch (error) { setActionError(providerError(normalizeThrownError(error))); }
    finally { setBusy(false); }
  }

  async function deleteProvider() {
    if (!deleting || deleteText !== deleting.id) return;
    setBusy(true); setActionError(null); setNotice(null);
    try {
      await api.deleteProvider(deleting.id);
      setDeleting(null); setDeleteText(""); setNotice("供应商已删除，不可恢复。"); await load();
    } catch (error) { setActionError(providerError(normalizeThrownError(error))); }
    finally { setBusy(false); }
  }

  async function mirrorProvider(event: FormEvent) {
    event.preventDefault(); setBusy(true); setActionError(null); setNotice(null);
    try {
      const result: ProviderMirrorResult = await api.mirrorSystemProvider(mirrorID.trim(), mirrorReason);
      const labels = { created: "已创建该工作区的供应商", functional_updated: "已更新功能配置", credential_rotated: "已轮换凭据", noop: "配置无变化" };
      setNotice(`镜像完成：${labels[result.outcome]}。`);
      setMirrorOpen(false); setMirrorID(""); setMirrorReason(""); await load();
    } catch (error) { setActionError(providerError(normalizeThrownError(error))); }
    finally { setBusy(false); }
  }

  async function saveEmbedder(event: FormEvent) {
    event.preventDefault(); setBusy(true); setActionError(null); setNotice(null);
    const payload: EmbedderConfig = {
      url: embedderForm.url.trim(), api_key: embedderForm.api_key || "",
      model: embedderForm.model.trim() || DEFAULT_EMBEDDER_MODEL,
      dimension: embedderForm.dimension > 0 ? embedderForm.dimension : DEFAULT_EMBEDDER_DIMENSION,
    };
    if (!payload.url) { setActionError("向量嵌入服务地址必填。"); setBusy(false); return; }
    try {
      await api.updateEmbedder(payload); setEmbedderOpen(false); setNotice("配置已保存并立即生效。"); await load();
    } catch (error) {
      const normalized = normalizeThrownError(error);
      setActionError(/does not match existing memory table dimension/i.test(normalized.message) ? "向量维度与已有记忆数据不一致。" : providerError(normalized));
    } finally { setBusy(false); }
  }

  async function deleteEmbedder() {
    setBusy(true); setActionError(null); setNotice(null);
    try {
      await api.deleteEmbedder(); setDeleteEmbedderOpen(false); setNotice("工作区向量嵌入配置已删除，将使用系统默认配置。"); await load();
    } catch (error) { setActionError(providerError(normalizeThrownError(error))); }
    finally { setBusy(false); }
  }

  return <section className="control-content" aria-labelledby="providers-heading">
    <div className="control-section-heading"><div><h2 id="providers-heading">模型供应商</h2><p>管理工作区的模型凭据与向量嵌入配置，支持从系统供应商镜像。</p></div><div className="heading-actions"><Button variant="ghost" loading={refreshing} disabled={loading} onClick={() => void load()}>{!refreshing && <RefreshCw size={16} aria-hidden="true" />}{refreshing ? "正在刷新" : "刷新"}</Button>{canWrite && <><Button onClick={openMirror}><DatabaseZap size={16} aria-hidden="true" />镜像系统供应商</Button><Button variant="primary" onClick={beginCreate}><Plus size={16} aria-hidden="true" />添加供应商</Button></>}</div></div>
    {!canWrite && <p className="readonly-note"><ShieldCheck size={14} />当前账号没有管理权限，仅可查看。</p>}
    {notice && <p className="success-note provider-notice" role="status">{notice}</p>}
    {actionError && !editor && !mirrorOpen && !embedderOpen && !deleting && !deleteEmbedderOpen && <ErrorNotice message={actionError} />}

    <section className="config-panel" aria-labelledby="provider-list-heading">
      <header><div><h3 id="provider-list-heading">模型供应商列表</h3><p>API Key 仅以掩码显示。</p></div><span>{providers.length} 项</span></header>
      {providerErrorState && <ErrorNotice message={providerError(providerErrorState)} onRetry={() => void load()} />}
      {loading && !providers.length ? <LoadingView label="正在加载供应商列表" /> : !providers.length && !providerErrorState ? <div className="empty-surface config-empty"><ServerCog size={28} /><h2>还没有配置模型供应商</h2><p>{canWrite ? "点击下方按钮添加，或输入系统供应商 ID 进行镜像。" : "当前工作区还没有配置模型供应商。"}</p>{canWrite && <Button variant="primary" onClick={beginCreate}><Plus size={16} aria-hidden="true" />添加第一个供应商</Button>}</div> : <div className="provider-grid">{providers.map((provider) => {
        const [label, tone] = providerStatus(provider);
        const closed = !provider.enabled || Boolean(provider.revoked_at || provider.deleted_at);
        return <article className="provider-card" key={provider.id}><header><span className="provider-glyph"><ServerCog size={16} /></span><div><h4>{provider.name}</h4></div><Badge tone={tone}>{label}</Badge></header><p className="provider-url">{provider.base_url}</p><div className="model-chips">{provider.models.map((model) => <code key={model}>{model}</code>)}</div><dl className="config-facts"><div><dt>来源</dt><dd>{providerSource(provider.source_kind)}</dd></div><div><dt>模型列表</dt><dd>{provider.models.length}</dd></div>{provider.revoked_at ? <div><dt>撤销时间</dt><dd>{formatDate(provider.revoked_at)}</dd></div> : null}{provider.deleted_at ? <div><dt>删除时间</dt><dd>{formatDate(provider.deleted_at)}</dd></div> : null}</dl>{canWrite && <footer><Button disabled={closed} title={closed ? "关闭状态不可恢复" : undefined} onClick={() => beginEdit(provider)}><Pencil size={14} aria-hidden="true" />编辑</Button><Button variant="ghost-danger" disabled={closed} onClick={() => { setDeleting(provider); setDeleteText(""); setActionError(null); }}><Trash2 size={14} aria-hidden="true" />删除</Button></footer>}</article>;
      })}</div>}
    </section>

    <section className="config-panel embedder-panel" aria-labelledby="embedder-heading"><header><div><h3 id="embedder-heading">向量嵌入（Embedder）</h3><p>展示当前工作区实际生效的向量嵌入配置。</p></div>{canWrite && <div className="heading-actions"><Button disabled={!embedder} onClick={() => { if (embedder) setEmbedderForm(embedder); setEmbedderOpen(true); setActionError(null); }}><Pencil size={14} aria-hidden="true" />配置</Button><Button variant="ghost-danger" onClick={() => { setDeleteEmbedderOpen(true); setActionError(null); }}><Trash2 size={14} aria-hidden="true" />删除工作区配置</Button></div>}</header>
      {embedderError ? <ErrorNotice message={providerError(embedderError)} onRetry={() => void load()} /> : loading && !embedder ? <LoadingView label="正在加载向量嵌入配置" /> : embedder && <div className="embedder-summary"><span className="provider-glyph"><KeyRound size={16} /></span><dl className="config-facts"><div><dt>服务地址</dt><dd><code>{embedder.url || "—"}</code></dd></div><div><dt>模型</dt><dd><code>{embedder.model || "—"}</code></dd></div><div><dt>向量维度</dt><dd>{embedder.dimension}</dd></div><div><dt>密钥</dt><dd>{embedder.api_key ? "已配置" : "未配置"}</dd></div></dl></div>}
    </section>

    <Modal open={!!editor} size="wide" title={editor === "create" ? "添加供应商" : `编辑 ${editing?.name || "供应商"}`} description="API Key 留空表示保持原密钥不变。" onClose={() => { if (!busy) { setEditor(null); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setEditor(null)}>取消</Button><Button variant="primary" type="submit" form="provider-form" loading={busy}>{busy ? "正在保存…" : "保存"}</Button></>}><form id="provider-form" className="form-stack" onSubmit={(event) => void saveProvider(event)}><div className="config-form-grid"><Field label="ID" help="创建后不可修改；system/ 为系统保留前缀。">{(control) => <input {...control} autoFocus value={form.id} disabled={editor === "edit"} onChange={(event) => setForm((current) => ({ ...current, id: event.target.value }))} required autoComplete="off" />}</Field><Field label="名称">{(control) => <input {...control} value={form.name} onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))} required />}</Field></div><Field label="服务地址">{(control) => <input {...control} type="url" value={form.baseUrl} onChange={(event) => setForm((current) => ({ ...current, baseUrl: event.target.value }))} required placeholder="https://api.example.test/v1" />}</Field><Field label="API Key" help="留空表示不修改原密钥，填写新值则替换。">{(control) => <input {...control} type="password" value={form.apiKey} onChange={(event) => setForm((current) => ({ ...current, apiKey: event.target.value }))} autoComplete="new-password" placeholder={editor === "edit" ? "留空表示不修改原密钥" : "可为空"} />}</Field><Field label="模型列表" help="每行一个或用逗号分隔，重复项会自动去除。">{(control) => <textarea {...control} rows={6} value={form.models} onChange={(event) => setForm((current) => ({ ...current, models: event.target.value }))} placeholder={"model-a\nmodel-b"} required />}</Field><label className="switch-row"><span><strong>JSON 对象模式</strong><small>启用后模型以 JSON 对象格式返回结果。</small></span><Switch checked={form.jsonObjectMode} aria-label="JSON 对象模式" onChange={(next) => setForm((current) => ({ ...current, jsonObjectMode: next }))} /></label>{actionError && editor && <ErrorNotice message={actionError} />}</form></Modal>

    <Modal open={mirrorOpen} title="镜像系统供应商" description="从部署中已配置的系统供应商选择镜像目标；已镜像的供应商再次镜像会更新功能配置或轮换凭据。" onClose={() => { if (!busy) { setMirrorOpen(false); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setMirrorOpen(false)}>取消</Button><Button variant="primary" type="submit" form="mirror-provider-form" loading={busy} disabled={!mirrorID.trim()}>{busy ? "正在镜像…" : "执行镜像"}</Button></>}><form id="mirror-provider-form" className="form-stack" onSubmit={(event) => void mirrorProvider(event)}>
      {systemProvidersError ? <ErrorNotice message={systemProvidersError} /> : systemProviders === null ? <LoadingView label="正在加载系统供应商" /> : systemProviders.length === 0 ? <p className="contract-note">部署中未配置任何系统供应商。</p> : (
        <Field label="系统供应商" help="仅展示 ID、服务地址与模型清单，密钥不会离开服务端。">{(control) => (
          <select {...control} autoFocus value={mirrorID} onChange={(event) => setMirrorID(event.target.value)} required>
            <option value="" disabled>选择要镜像的系统供应商</option>
            {systemProviders.map((provider) => (
              <option key={provider.id} value={provider.id}>
                {provider.name}（{provider.id}）{provider.mirrored ? ` · 已镜像为 ${provider.mirrored_as}` : ""}
              </option>
            ))}
          </select>
        )}</Field>
      )}
      {mirrorID && systemProviders && (() => {
        const selected = systemProviders.find((provider) => provider.id === mirrorID);
        return selected ? (
          <dl className="config-facts">
            <div><dt>服务地址</dt><dd><code>{selected.base_url}</code></dd></div>
            <div><dt>模型</dt><dd><code>{selected.models.join(", ") || "—"}</code></dd></div>
            {selected.mirrored && <div><dt>当前镜像</dt><dd><code>{selected.mirrored_as}</code>（第 {selected.mirror_revision ?? "—"} 版）</dd></div>}
          </dl>
        ) : null;
      })()}
      <Field label="镜像原因">{(control) => <textarea {...control} rows={4} value={mirrorReason} onChange={(event) => setMirrorReason(event.target.value)} placeholder="记录本次镜像原因" />}</Field>
      <p className="contract-note">镜像可能创建新配置、更新功能配置或轮换凭据。</p>{actionError && mirrorOpen && <ErrorNotice message={actionError} />}</form></Modal>

    <Modal open={embedderOpen} title="配置向量嵌入" description="地址必填；模型与向量维度留空时使用默认值。" onClose={() => { if (!busy) { setEmbedderOpen(false); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setEmbedderOpen(false)}>取消</Button><Button variant="primary" type="submit" form="embedder-form" loading={busy} disabled={!embedderForm.url.trim()}>{busy ? "正在应用…" : "保存并应用"}</Button></>}><form id="embedder-form" className="form-stack" onSubmit={(event) => void saveEmbedder(event)}><Field label="服务地址">{(control) => <input {...control} autoFocus type="url" value={embedderForm.url} onChange={(event) => setEmbedderForm((current) => ({ ...current, url: event.target.value }))} required />}</Field><Field label="API Key" help="留空表示不修改原密钥。">{(control) => <input {...control} type="password" value={embedderForm.api_key || ""} onChange={(event) => setEmbedderForm((current) => ({ ...current, api_key: event.target.value }))} autoComplete="new-password" />}</Field><div className="config-form-grid"><Field label="模型">{(control) => <input {...control} value={embedderForm.model} onChange={(event) => setEmbedderForm((current) => ({ ...current, model: event.target.value }))} placeholder={DEFAULT_EMBEDDER_MODEL} />}</Field><Field label="向量维度">{(control) => <input {...control} type="number" min={1} value={embedderForm.dimension} onChange={(event) => setEmbedderForm((current) => ({ ...current, dimension: Number(event.target.value) }))} />}</Field></div><p className="contract-note">向量维度需与已有记忆数据一致，否则无法保存。</p>{actionError && embedderOpen && <ErrorNotice message={actionError} />}</form></Modal>

    <Modal open={!!deleting} title="不可恢复地删除供应商" description="删除后该供应商立即停用，且不可恢复。" onClose={() => { if (!busy) { setDeleting(null); setDeleteText(""); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setDeleting(null)}>取消</Button><Button variant="danger" loading={busy} disabled={deleteText !== deleting?.id} onClick={() => void deleteProvider()}>{busy ? "正在删除…" : "删除供应商"}</Button></>}><div className="form-stack"><p className="confirm-copy">删除不可撤销。请输入完整 ID 确认。</p><Field label={`输入 ${deleting?.id}`}>{(control) => <input {...control} value={deleteText} onChange={(event) => setDeleteText(event.target.value)} autoComplete="off" />}</Field>{actionError && deleting && <ErrorNotice message={actionError} />}</div></Modal>

    <Modal open={deleteEmbedderOpen} title="删除工作区向量嵌入配置" description="仅删除当前工作区的覆盖配置，记忆功能本身不受影响。" onClose={() => { if (!busy) { setDeleteEmbedderOpen(false); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setDeleteEmbedderOpen(false)}>取消</Button><Button variant="danger" loading={busy} onClick={() => void deleteEmbedder()}>{busy ? "正在删除…" : "删除工作区配置"}</Button></>}><p className="confirm-copy">删除后回退到系统默认配置；若系统也未配置，向量记忆将不可用。</p>{actionError && deleteEmbedderOpen && <ErrorNotice message={actionError} />}</Modal>
  </section>;
}
