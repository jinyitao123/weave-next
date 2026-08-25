import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Ban, ChevronRight, FileClock, KeyRound, Pencil, Plus, RefreshCw, RotateCw, Send, ShieldAlert, Trash2, X } from "lucide-react";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type ApiError,
  type DeliveryTarget,
  type DeliveryTargetConfigInput,
  type DeliveryTargetDetailResponse,
  type DeliveryTargetKind,
  type DeliveryTargetMutation,
  type DeliveryTargetRevision,
} from "../../api";
import { useAuth } from "../../auth/AuthContext";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";

const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
type HeaderRow = { id: number; name: string; value: string };
type EditorMode = "create" | "edit" | "rotate" | null;
type ClosureAction = "disable" | "revoke" | "delete" | null;
interface TargetForm { id: string; kind: DeliveryTargetKind; url: string; timeoutSeconds: number; headers: HeaderRow[] }
let headerSequence = 0;

function emptyForm(): TargetForm { return { id: "", kind: "callback", url: "", timeoutSeconds: 30, headers: [] }; }
function formatDate(value?: string | null) { return value ? dateTime.format(new Date(value)) : "—"; }
function headerRows(names: string[]): HeaderRow[] { return names.map((name) => ({ id: ++headerSequence, name, value: "" })); }
function targetStatus(target: DeliveryTarget) {
  if (target.deleted_at) return ["已删除", "danger"] as const;
  if (target.revoked_at) return ["已撤销", "danger"] as const;
  if (!target.enabled) return ["已禁用", "neutral"] as const;
  return ["可用", "success"] as const;
}
function deliveryError(error: ApiError) {
  if (error.status === 400) return `提交的内容不符合交付目标的要求：${apiErrorMessage(error)}`;
  if (error.status === 404) return "目标或该版本不存在";
  if (error.status === 409) return `交付目标已关闭，不能继续修改：${apiErrorMessage(error)}`;
  if (error.status === 503) return "交付服务或凭据暂时不可用，请稍后重试";
  return apiErrorMessage(error);
}
function validateHeaders(rows: HeaderRow[], exactNames?: string[]) {
  const payload: Record<string, string> = {};
  for (const row of rows) {
    const name = row.name.trim();
    if (!name || !row.value) return { error: "每个请求头都必须填写名称和完整密钥", headers: payload };
    const normalized = name.toLowerCase();
    if (Object.prototype.hasOwnProperty.call(payload, normalized)) return { error: "请求头名称不能重复（不区分大小写）", headers: payload };
    payload[normalized] = row.value;
  }
  if (exactNames) {
    const submitted = Object.keys(payload).sort();
    const expected = [...exactNames].sort();
    if (submitted.length !== expected.length || submitted.some((name, index) => name !== expected[index])) return { error: `请求头名称必须与当前保持一致：${expected.join(", ") || "无"}`, headers: payload };
  }
  return { error: "", headers: payload };
}

export function DeliveryTargetsPage() {
  const { user } = useAuth();
  const isAdmin = user?.role === "admin";
  const [targets, setTargets] = useState<DeliveryTarget[]>([]);
  const [selectedID, setSelectedID] = useState("");
  const [detail, setDetail] = useState<DeliveryTargetDetailResponse | null>(null);
  const [displayRevision, setDisplayRevision] = useState<DeliveryTargetRevision | null>(null);
  const [revisionInput, setRevisionInput] = useState("");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [detailLoading, setDetailLoading] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [detailError, setDetailError] = useState<ApiError | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [editor, setEditor] = useState<EditorMode>(null);
  const [form, setForm] = useState<TargetForm>(emptyForm);
  const [busy, setBusy] = useState(false);
  const [closureAction, setClosureAction] = useState<ClosureAction>(null);
  const [confirmText, setConfirmText] = useState("");

  const loadList = useCallback(async (initial = false) => {
    if (!isAdmin) { setLoading(false); return; }
    if (initial) setLoading(true); else setRefreshing(true);
    setError(null);
    try {
      const response = await api.listDeliveryTargets();
      setTargets(response.targets || []);
      setSelectedID((current) => current && response.targets.some((target) => target.id === current) ? current : response.targets[0]?.id || "");
      if (!response.targets.length) { setDetail(null); setDisplayRevision(null); }
    } catch (requestError) { setError(normalizeThrownError(requestError)); }
    finally { setLoading(false); setRefreshing(false); }
  }, [isAdmin]);

  const loadDetail = useCallback(async (id: string) => {
    setDetailLoading(true); setDetailError(null); setActionError(null);
    try {
      const response = await api.getDeliveryTarget(id);
      setDetail(response); setDisplayRevision(response.revision); setRevisionInput(String(response.revision.revision));
      setTargets((current) => current.map((target) => target.id === response.target.id ? response.target : target));
    } catch (requestError) { setDetail(null); setDisplayRevision(null); setDetailError(normalizeThrownError(requestError)); }
    finally { setDetailLoading(false); }
  }, []);

  useEffect(() => { void loadList(true); }, [loadList]);
  useEffect(() => { if (selectedID && isAdmin) void loadDetail(selectedID); }, [selectedID, isAdmin, loadDetail]);

  if (!isAdmin) return <section className="control-content"><div className="empty-surface control-forbidden"><ShieldAlert size={28} /><h2>当前账号没有管理交付目标的权限</h2><p>如需访问请联系管理员</p></div></section>;

  function beginCreate() { setForm(emptyForm()); setEditor("create"); setActionError(null); }
  function beginEdit(revision: DeliveryTargetRevision) {
    setForm({ id: revision.target_id, kind: revision.kind, url: revision.url, timeoutSeconds: revision.timeout_seconds, headers: headerRows(revision.header_names) });
    setEditor("edit"); setActionError(null);
  }
  function beginRotate(revision: DeliveryTargetRevision) {
    setForm({ id: revision.target_id, kind: revision.kind, url: revision.url, timeoutSeconds: revision.timeout_seconds, headers: headerRows(revision.header_names) });
    setEditor("rotate"); setActionError(null);
  }
  function recordMutation(result: DeliveryTargetMutation, operation: string) {
    setSelectedID(result.target.id);
    setTargets((current) => current.some((target) => target.id === result.target.id)
      ? current.map((target) => target.id === result.target.id ? result.target : target)
      : [...current, result.target]);
    setDetail({ target: result.target, revision: result.revision }); setDisplayRevision(result.revision); setRevisionInput(String(result.revision.revision));
    setNotice(result.advanced ? `${operation}，已生成新版本 ${result.revision.revision}` : "内容无变化，未生成新版本");
  }

  async function saveTarget(event: FormEvent) {
    event.preventDefault(); setActionError(null); setNotice(null);
    if (editor === "create" && !/^[A-Za-z0-9._~-]+$/.test(form.id)) { setActionError("ID 不能为空，且只能包含字母、数字、点、下划线、波浪号或连字符。"); return; }
    let parsed: URL;
    try { parsed = new URL(form.url); } catch { setActionError("必须是完整的 http(s) 链接"); return; }
    if (!/^https?:$/.test(parsed.protocol) || parsed.username || parsed.password || parsed.search || parsed.hash || parsed.host.endsWith(":")) { setActionError("必须是完整的 http(s) 链接，且不能包含用户名密码、查询参数或 # 锚点"); return; }
    if (form.timeoutSeconds < 1 || form.timeoutSeconds > 300) { setActionError("超时时间必须在 1–300 秒之间"); return; }
    const validated = validateHeaders(form.headers);
    if (validated.error) { setActionError(validated.error); return; }
    const config: DeliveryTargetConfigInput = { kind: form.kind, url: form.url.trim(), timeout_seconds: form.timeoutSeconds, headers: validated.headers };
    setBusy(true);
    try {
      const result = editor === "create" ? await api.createDeliveryTarget({ id: form.id.trim(), ...config }) : await api.updateDeliveryTarget(selectedID, config);
      setEditor(null); recordMutation(result, "保存成功"); await loadList();
    } catch (requestError) { setActionError(deliveryError(normalizeThrownError(requestError))); }
    finally { setBusy(false); }
  }

  async function rotateHeaders(event: FormEvent) {
    event.preventDefault(); if (!detail) return;
    setActionError(null); setNotice(null);
    const validated = validateHeaders(form.headers, detail.revision.header_names);
    if (validated.error) { setActionError(validated.error); return; }
    setBusy(true);
    try {
      const result = await api.rotateDeliveryTargetHeaders(detail.target.id, validated.headers);
      setEditor(null); recordMutation(result, "保存成功"); await loadList();
    } catch (requestError) { setActionError(deliveryError(normalizeThrownError(requestError))); }
    finally { setBusy(false); }
  }

  async function readRevision(event: FormEvent) {
    event.preventDefault(); if (!detail) return;
    const revision = Number(revisionInput);
    if (!Number.isSafeInteger(revision) || revision < 1) { setActionError("版本号必须是正整数"); return; }
    setDetailLoading(true); setActionError(null);
    try { setDisplayRevision((await api.getDeliveryTargetRevision(detail.target.id, revision)).revision); }
    catch (requestError) { setActionError(deliveryError(normalizeThrownError(requestError))); }
    finally { setDetailLoading(false); }
  }

  async function closeTarget() {
    if (!closureAction || !detail || confirmText !== detail.target.id) return;
    setBusy(true); setActionError(null); setNotice(null);
    try {
      if (closureAction === "disable") await api.disableDeliveryTarget(detail.target.id);
      else if (closureAction === "revoke") await api.revokeDeliveryTarget(detail.target.id);
      else await api.deleteDeliveryTarget(detail.target.id);
      const label: Record<Exclude<ClosureAction, null>, string> = { disable: "禁用", revoke: "撤销", delete: "删除" };
      setNotice(`${label[closureAction]}已完成，该目标不可恢复，且不再出现在列表中`);
      setClosureAction(null); setConfirmText(""); setSelectedID(""); setDetail(null); setDisplayRevision(null); await loadList();
    } catch (requestError) { setActionError(deliveryError(normalizeThrownError(requestError))); }
    finally { setBusy(false); }
  }

  const selectedTarget = detail?.target.id === selectedID ? detail.target : targets.find((target) => target.id === selectedID) || null;
  return <section className="control-content" aria-labelledby="delivery-targets-heading">
    <div className="control-section-heading"><div><h2 id="delivery-targets-heading">交付目标</h2><p>管理接收交付结果的外部地址；密钥一经保存不可查看，修改时需重新输入</p></div><div className="heading-actions"><Button variant="ghost" loading={refreshing} disabled={loading} onClick={() => void loadList()}>{!refreshing && <RefreshCw size={16} aria-hidden="true" />}{refreshing ? "正在刷新" : "刷新"}</Button><Button variant="primary" onClick={beginCreate}><Plus size={16} aria-hidden="true" />添加交付目标</Button></div></div>
    {notice && <p className="success-note provider-notice" role="status">{notice}</p>}
    {error && <ErrorNotice message={deliveryError(error)} onRetry={() => void loadList()} />}
    {loading && !targets.length ? <LoadingView label="正在加载交付目标" /> : !targets.length && !error ? <div className="empty-surface config-empty"><Send size={28} /><h2>还没有交付目标</h2><p>添加一个交付目标，即可把交付结果推送到你的系统</p><Button variant="primary" onClick={beginCreate}><Plus size={16} aria-hidden="true" />添加第一个交付目标</Button></div> : <div className="delivery-browser"><aside className="delivery-master" aria-label="交付目标列表">{refreshing && <p className="refresh-note" role="status">正在刷新…</p>}{targets.map((target) => { const [label, tone] = targetStatus(target); return <button className={target.id === selectedID ? "delivery-row active" : "delivery-row"} type="button" key={target.id} onClick={() => setSelectedID(target.id)}><span className="provider-glyph"><Send size={16} /></span><span><strong>{target.id}</strong><small>版本 {target.latest_revision} · {formatDate(target.updated_at)}</small></span><Badge tone={tone}>{label}</Badge><ChevronRight size={16} /></button>; })}</aside><div className="delivery-detail">{detailLoading && !selectedTarget ? <LoadingView label="正在加载详情" /> : detailError && !selectedTarget ? <ErrorNotice message={deliveryError(detailError)} onRetry={() => selectedID && void loadDetail(selectedID)} /> : detail && displayRevision ? <article className="delivery-detail-card"><header><div><Badge tone={targetStatus(detail.target)[1]}>{targetStatus(detail.target)[0]}</Badge><h3>{detail.target.id}</h3><p>最新版本 {detail.target.latest_revision} · 当前查看版本 {displayRevision.revision}</p></div><div className="delivery-actions"><Button onClick={() => beginEdit(detail.revision)}><Pencil size={14} aria-hidden="true" />编辑</Button><Button onClick={() => beginRotate(detail.revision)}><RotateCw size={14} aria-hidden="true" />更新请求头密钥</Button><Button onClick={() => { setClosureAction("disable"); setConfirmText(""); }}><Ban size={14} aria-hidden="true" />禁用</Button><Button variant="ghost-danger" onClick={() => { setClosureAction("revoke"); setConfirmText(""); }}><ShieldAlert size={14} aria-hidden="true" />撤销</Button><Button variant="ghost-danger" onClick={() => { setClosureAction("delete"); setConfirmText(""); }}><Trash2 size={14} aria-hidden="true" />删除</Button></div></header>{actionError && !editor && !closureAction && <ErrorNotice message={actionError} onRetry={() => void loadDetail(detail.target.id)} />}<form className="revision-reader" onSubmit={(event) => void readRevision(event)}><Field label="查看历史版本">{(control) => <input {...control} type="number" min={1} max={Number.MAX_SAFE_INTEGER} value={revisionInput} onChange={(event) => setRevisionInput(event.target.value)} />}</Field><Button type="submit" disabled={detailLoading}><FileClock size={14} aria-hidden="true" />查看</Button><Button variant="ghost" onClick={() => { setDisplayRevision(detail.revision); setRevisionInput(String(detail.revision.revision)); }}>回到最新版本</Button></form><RevisionView revision={displayRevision} target={detail.target} /></article> : <div className="empty-surface"><Send size={28} /><h2>选择一个交付目标</h2><p>查看其配置与历史版本</p></div>}</div></div>}

    <Modal open={editor === "create" || editor === "edit"} size="wide" title={editor === "create" ? "添加交付目标" : "编辑交付目标"} description={editor === "edit" ? "保存后配置整体生效；密钥不可回显，需为每个请求头重新输入完整密钥" : "填写名称、类型、接收地址与超时时间即可创建"} onClose={() => { if (!busy) { setEditor(null); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setEditor(null)}>取消</Button><Button variant="primary" type="submit" form="delivery-target-form" loading={busy}>{busy ? "正在保存…" : editor === "create" ? "创建" : "保存"}</Button></>}><form id="delivery-target-form" className="form-stack" onSubmit={(event) => void saveTarget(event)}>{editor === "create" && <Field label="ID">{(control) => <input {...control} autoFocus value={form.id} onChange={(event) => setForm((current) => ({ ...current, id: event.target.value }))} required autoComplete="off" />}</Field>}<div className="config-form-grid"><Field label="类型">{(control) => <select {...control} value={form.kind} onChange={(event) => setForm((current) => ({ ...current, kind: event.target.value as DeliveryTargetKind }))}><option value="callback">回调通知</option><option value="target">交付目标</option></select>}</Field><Field label="超时时间（秒）">{(control) => <input {...control} type="number" min={1} max={300} value={form.timeoutSeconds} onChange={(event) => setForm((current) => ({ ...current, timeoutSeconds: Number(event.target.value) }))} required />}</Field></div><Field label="URL" help="系统将以 JSON POST 请求推送到该地址；地址不能包含用户名密码、查询参数或锚点">{(control) => <input {...control} type="url" value={form.url} onChange={(event) => setForm((current) => ({ ...current, url: event.target.value }))} required placeholder="https://delivery.example.test/hook" />}</Field><HeaderEditor rows={form.headers} onChange={(headers) => setForm((current) => ({ ...current, headers }))} />{editor === "edit" && <p className="contract-note">密钥不可回显，保留的请求头也需重新输入；内容无变化时不会生成新版本；只想换密钥请使用「更新请求头密钥」</p>}{actionError && editor && <ErrorNotice message={actionError} />}</form></Modal>

    <Modal open={editor === "rotate"} title="更新请求头密钥" description="仅更新密钥，不会改变其他配置；请求头名称需与当前保持一致" onClose={() => { if (!busy) { setEditor(null); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setEditor(null)}>取消</Button><Button variant="primary" type="submit" form="rotate-target-headers" loading={busy}>{busy ? "正在更新…" : "确认更新"}</Button></>}><form id="rotate-target-headers" className="form-stack" onSubmit={(event) => void rotateHeaders(event)}><HeaderEditor rows={form.headers} onChange={(headers) => setForm((current) => ({ ...current, headers }))} lockNames />{!form.headers.length && <p className="contract-note">当前版本没有配置请求头，直接提交不会产生任何变化</p>}{actionError && editor === "rotate" && <ErrorNotice message={actionError} />}</form></Modal>

    <Modal open={!!closureAction} title={closureAction === "disable" ? "不可逆禁用交付目标" : closureAction === "revoke" ? "不可逆撤销交付目标" : "不可逆删除交付目标"} description={closureAction === "disable" ? "禁用后停止推送，配置保留" : closureAction === "revoke" ? "撤销后停止推送并标记为已撤销，配置保留" : "删除后不可访问，历史版本仅存档保留"} onClose={() => { if (!busy) { setClosureAction(null); setConfirmText(""); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setClosureAction(null)}>取消</Button><Button variant="danger" loading={busy} disabled={confirmText !== detail?.target.id} onClick={() => void closeTarget()}>{busy ? "正在执行…" : closureAction === "disable" ? "永久禁用" : closureAction === "revoke" ? "永久撤销" : "删除并关闭"}</Button></>}><div className="form-stack"><p className="confirm-copy">以上操作均不可撤销；操作后该目标不再出现在列表中</p><Field label={`输入 ${detail?.target.id} 确认`}>{(control) => <input {...control} value={confirmText} onChange={(event) => setConfirmText(event.target.value)} autoComplete="off" />}</Field>{actionError && closureAction && <ErrorNotice message={actionError} />}</div></Modal>
  </section>;
}

function HeaderEditor({ rows, onChange, lockNames }: { rows: HeaderRow[]; onChange(rows: HeaderRow[]): void; lockNames?: boolean }) {
  return <div className="structured-list"><div className="structured-list__heading"><strong>请求头（密钥）</strong>{!lockNames && <Button variant="ghost" onClick={() => onChange([...rows, { id: ++headerSequence, name: "", value: "" }])}><Plus size={14} aria-hidden="true" />添加请求头</Button>}</div>{!rows.length ? <p className="structured-list__empty">尚未配置请求头</p> : rows.map((row) => <div className="structured-row structured-row--pair" key={row.id}><input aria-label="请求头名称" value={row.name} disabled={lockNames} placeholder="authorization" onChange={(event) => onChange(rows.map((item) => item.id === row.id ? { ...item, name: event.target.value } : item))} /><input aria-label={`${row.name || "请求头"}的密钥`} type="password" value={row.value} placeholder="输入完整密钥" autoComplete="new-password" onChange={(event) => onChange(rows.map((item) => item.id === row.id ? { ...item, value: event.target.value } : item))} />{!lockNames && <button className="icon-button icon-button--danger" type="button" aria-label={`删除 ${row.name || "请求头"}`} onClick={() => onChange(rows.filter((item) => item.id !== row.id))}><X size={16} /></button>}</div>)}</div>;
}

function RevisionView({ revision, target }: { revision: DeliveryTargetRevision; target: DeliveryTarget }) {
  return <div className="delivery-revision"><header><div><h4>版本 {revision.revision}</h4><p>该版本配置不可修改，不包含密钥</p></div>{revision.revision === target.latest_revision ? <Badge tone="success">最新</Badge> : <Badge tone="neutral">历史</Badge>}</header><dl className="config-facts"><div><dt>目标 ID</dt><dd><code>{revision.target_id}</code></dd></div><div><dt>类型</dt><dd>{revision.kind}</dd></div><div><dt>传输方式</dt><dd>{revision.transport}</dd></div><div><dt>请求方法</dt><dd>{revision.method}</dd></div>{revision.content_type && revision.content_type !== "application/json" ? <div><dt>内容类型</dt><dd>{revision.content_type}</dd></div> : null}<div><dt>超时时间</dt><dd>{revision.timeout_seconds} 秒</dd></div><div><dt>创建时间</dt><dd>{formatDate(revision.created_at)}</dd></div><div><dt>URL</dt><dd><code>{revision.url}</code></dd></div></dl><section className="header-name-facts"><header><div><h5>请求头名称</h5><p>出于安全考虑仅显示名称，密钥不可查看</p></div><KeyRound size={16} /></header>{!revision.header_names.length ? <p>无</p> : <div className="model-chips">{revision.header_names.map((name) => <code key={name}>{name}</code>)}</div>}</section></div>;
}
