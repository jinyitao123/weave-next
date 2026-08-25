import { useCallback, useEffect, useState, type FormEvent } from "react";
import { BookOpenText, ChevronRight, Download, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type ApiError,
  type LegacySkillImportResponse,
  type Skill,
  type SkillWriteInput,
} from "../../api";
import { useAuth } from "../../auth/useAuth";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { Switch } from "../../ui/Switch";
import { createClientUUID } from "../../platform/uuid";

const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
type EditorMode = "create" | "edit" | null;

const emptySkill: SkillWriteInput = { id: "", name: "", description: "", body: "", always_active: false, category: "" };

function skillInput(skill: Skill): SkillWriteInput {
  return {
    id: skill.id,
    name: skill.name,
    description: skill.description || "",
    body: skill.body,
    always_active: Boolean(skill.always_active),
    category: skill.category || "",
  };
}

function importErrorMessage(error: ApiError): string {
  const prefix = error.status === 400 ? "导入信息有误"
    : error.status === 404 ? "未找到可导入的历史版本技能"
      : error.status === 409 ? "导入内容发生冲突"
        : error.status === 503 ? "导入服务暂时不可用"
          : "导入失败";
  return `${prefix}：${apiErrorMessage(error)}`;
}

export function SkillsPage() {
  const { user } = useAuth();
  const [skills, setSkills] = useState<Skill[]>([]);
  const [selectedID, setSelectedID] = useState("");
  const [detail, setDetail] = useState<Skill | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [detailLoading, setDetailLoading] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [detailError, setDetailError] = useState<string | null>(null);
  const [editorMode, setEditorMode] = useState<EditorMode>(null);
  const [form, setForm] = useState<SkillWriteInput>(emptySkill);
  const [deleting, setDeleting] = useState<Skill | null>(null);
  const [importing, setImporting] = useState<Skill | null>(null);
  const [reason, setReason] = useState("");
  const [idempotencyKey, setIdempotencyKey] = useState("");
  const [expectedSourceHash, setExpectedSourceHash] = useState("");
  const [importResult, setImportResult] = useState<LegacySkillImportResponse | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const canImport = user?.role === "admin";
  const selected = detail?.id === selectedID ? detail : skills.find((skill) => skill.id === selectedID) || null;

  const loadDetail = useCallback(async (id: string) => {
    setDetailLoading(true); setDetailError(null);
    try { setDetail(await api.getSkill(id)); }
    catch (requestError) { setDetail(null); setDetailError(apiErrorMessage(requestError)); }
    finally { setDetailLoading(false); }
  }, []);

  const refresh = useCallback(async (initial = false) => {
    if (initial) setLoading(true); else setRefreshing(true);
    setError(null); setForbidden(false);
    try {
      const next = await api.listSkills();
      setSkills(next);
      const nextID = next[0]?.id || "";
      setSelectedID(nextID);
      if (nextID) await loadDetail(nextID); else setDetail(null);
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setForbidden(normalized.status === 403);
      setError(apiErrorMessage(normalized));
      setSkills([]); setDetail(null);
    } finally { setLoading(false); setRefreshing(false); }
  }, [loadDetail]);

  useEffect(() => { void refresh(true); }, [refresh]);

  function chooseSkill(id: string) {
    setSelectedID(id); setDetail(null); setActionError(null); void loadDetail(id);
  }

  function beginCreate() {
    setForm({ ...emptySkill }); setEditorMode("create"); setActionError(null);
  }

  async function beginEdit(skill: Skill) {
    setEditorMode("edit"); setActionError(null); setDetailLoading(true);
    try {
      const current = await api.getSkill(skill.id);
      setDetail(current); setForm(skillInput(current));
    } catch (requestError) { setEditorMode(null); setDetailError(apiErrorMessage(requestError)); }
    finally { setDetailLoading(false); }
  }

  async function saveSkill(event: FormEvent) {
    event.preventDefault(); setActionError(null);
    if (!form.id.trim()) { setActionError("请填写 ID。"); return; }
    if (editorMode === "create" && !form.body.trim()) { setActionError("请填写提示词正文。"); return; }
    setBusy(true);
    try {
      const saved = editorMode === "create"
        ? await api.createSkill({ ...form, id: form.id.trim() })
        : await api.updateSkill(form.id, form);
      setEditorMode(null); setSelectedID(saved.id); setDetail(saved);
      setSkills((current) => {
        const exists = current.some((skill) => skill.id === saved.id);
        return exists ? current.map((skill) => skill.id === saved.id ? saved : skill) : [...current, saved];
      });
    } catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function deleteSkill() {
    if (!deleting) return;
    setBusy(true); setActionError(null);
    try {
      await api.deleteSkill(deleting.id);
      const remaining = skills.filter((skill) => skill.id !== deleting.id);
      setSkills(remaining); setDeleting(null);
      const nextID = remaining[0]?.id || "";
      setSelectedID(nextID); setDetail(null);
      if (nextID) await loadDetail(nextID);
    } catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  function beginImport(skill: Skill) {
    setImporting(skill); setReason(""); setExpectedSourceHash("");
    setIdempotencyKey(createClientUUID()); setImportResult(null); setActionError(null);
  }

  async function importLegacy(event: FormEvent) {
    event.preventDefault();
    if (!importing) return;
    if (!reason.trim()) { setActionError("请填写导入原因。"); return; }
    if (expectedSourceHash.trim() && !/^[0-9a-f]{64}$/.test(expectedSourceHash.trim())) { setActionError("来源哈希必须是 64 位小写 SHA-256 值。"); return; }
    setBusy(true); setActionError(null); setImportResult(null);
    try {
      const result = await api.importLegacySkill(importing.id, {
        schema_version: 1,
        idempotency_key: idempotencyKey.trim(),
        reason: reason.trim(),
        ...(expectedSourceHash.trim() ? { expected_source_hash: expectedSourceHash.trim() } : {}),
      });
      setImportResult(result);
    } catch (requestError) { setActionError(importErrorMessage(normalizeThrownError(requestError))); }
    finally { setBusy(false); }
  }

  return <section className="control-content" aria-labelledby="skills-heading">
    <div className="control-section-heading"><div><h2 id="skills-heading">技能</h2><p>管理智能体可用的技能与提示模块；导入历史版本仅管理员可用。</p></div><div className="heading-actions"><Button variant="ghost" loading={refreshing} disabled={loading} onClick={() => void refresh()}>{!refreshing && <RefreshCw size={16} aria-hidden="true" />}{refreshing ? "正在刷新" : "刷新"}</Button>{!forbidden && <Button variant="primary" onClick={beginCreate}><Plus size={16} aria-hidden="true" />创建技能</Button>}</div></div>
    {error && !forbidden && <ErrorNotice message={error} onRetry={() => void refresh()} />}
    {forbidden ? <div className="empty-surface control-forbidden"><BookOpenText size={28} /><h2>没有技能权限</h2><p>{error || "当前账号没有管理技能的权限。"}</p></div> : loading && !skills.length ? <LoadingView label="正在加载技能" /> : !skills.length ? <div className="empty-surface skills-empty"><BookOpenText size={28} /><h2>还没有技能</h2><p>创建一个技能，把可复用的提示词模块注入智能体。</p><Button variant="primary" onClick={beginCreate}><Plus size={16} aria-hidden="true" />创建第一个技能</Button></div> : <div className="skills-browser">
      <div className="skills-master" role="list" aria-label="技能列表">{skills.map((skill) => <button key={skill.id} role="listitem" type="button" className={skill.id === selectedID ? "skill-row active" : "skill-row"} onClick={() => chooseSkill(skill.id)}><span className="skill-row__icon"><BookOpenText size={16} /></span><span><strong>{skill.name || skill.id}</strong><small>{skill.id}</small></span>{skill.always_active && <Badge tone="success">始终生效</Badge>}<ChevronRight size={16} /></button>)}</div>
      <div className="skills-detail" aria-live="polite">{detailLoading ? <LoadingView label="正在加载技能详情" /> : detailError ? <ErrorNotice message={detailError} onRetry={() => selectedID && void loadDetail(selectedID)} /> : selected ? <article className="skill-detail-card"><header><div><Badge tone={selected.always_active ? "success" : "neutral"}>{selected.always_active ? "始终生效" : "按需生效"}</Badge><h3>{selected.name || selected.id}</h3><p><code>{selected.id}</code> · {selected.category || "未分类"}</p></div><div className="skill-detail-actions"><Button variant="ghost" onClick={() => void beginEdit(selected)}><Pencil size={16} aria-hidden="true" />编辑</Button>{canImport && <Button variant="ghost" onClick={() => beginImport(selected)}><Download size={16} aria-hidden="true" />导入历史版本</Button>}<Button variant="ghost-danger" onClick={() => { setDeleting(selected); setActionError(null); }}><Trash2 size={16} aria-hidden="true" />删除</Button></div></header><p className="skill-description">{selected.description || "没有描述"}</p>{!canImport && <p className="readonly-note">只有管理员可以导入历史版本技能。</p>}<dl className="skill-facts"><div><dt>创建时间</dt><dd>{dateTime.format(new Date(selected.created_at))}</dd></div><div><dt>更新时间</dt><dd>{dateTime.format(new Date(selected.updated_at))}</dd></div></dl><section className="skill-body"><header><h4>提示词正文</h4><p>注入智能体系统提示的内容</p></header><pre>{selected.body}</pre></section></article> : <div className="empty-surface"><BookOpenText size={28} /><h2>选择技能</h2><p>从左侧列表查看详情。</p></div>}</div>
    </div>}

    <Modal open={!!editorMode} size="wide" title={editorMode === "create" ? "创建技能" : `编辑 ${form.name || form.id}`} description={editorMode === "create" ? "填写技能标识和提示词正文；标识创建后不可修改。" : "标识创建后不可修改。"} onClose={() => { if (!busy) { setEditorMode(null); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setEditorMode(null)}>取消</Button><Button variant="primary" type="submit" form="skill-form" loading={busy} disabled={!form.id.trim() || (editorMode === "create" && !form.body.trim())}>{busy ? "正在保存…" : editorMode === "create" ? "创建技能" : "保存更改"}</Button></>}><form id="skill-form" className="skill-form form-stack" onSubmit={(event) => void saveSkill(event)}>{actionError && <ErrorNotice message={actionError} />}<div className="skill-form-grid"><Field label="ID" help="创建后作为技能标识，且不可修改。">{(control) => <input {...control} autoFocus value={form.id} disabled={editorMode === "edit"} required onChange={(event) => setForm((current) => ({ ...current, id: event.target.value }))} />}</Field><Field label="名称">{(control) => <input {...control} value={form.name} onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))} />}</Field></div><div className="skill-form-grid"><Field label="分类">{(control) => <input {...control} value={form.category} onChange={(event) => setForm((current) => ({ ...current, category: event.target.value }))} />}</Field><label className="switch-row"><span><strong>始终生效</strong><small>开启后，提示词正文会始终注入智能体系统提示。</small></span><Switch checked={form.always_active} aria-label="始终生效" onChange={(next) => setForm((current) => ({ ...current, always_active: next }))} /></label></div><Field label="描述">{(control) => <textarea {...control} rows={3} value={form.description} onChange={(event) => setForm((current) => ({ ...current, description: event.target.value }))} />}</Field><Field label="提示词正文">{(control) => <textarea {...control} className="code-input skill-body-input" rows={18} required={editorMode === "create"} value={form.body} onChange={(event) => setForm((current) => ({ ...current, body: event.target.value }))} />}</Field></form></Modal>

    <Modal open={!!deleting} title="删除技能" description="删除后不可恢复，请确认不再需要该技能。" onClose={() => { if (!busy) { setDeleting(null); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setDeleting(null)}>取消</Button><Button variant="danger" loading={busy} onClick={() => void deleteSkill()}>{busy ? "正在删除…" : "确认删除"}</Button></>}><p className="confirm-copy">确定删除 <strong>{deleting?.name || deleting?.id}</strong>（<code>{deleting?.id}</code>）吗？</p>{actionError && <ErrorNotice message={actionError} />}</Modal>

    <Modal open={!!importing} size="wide" title={`导入历史版本技能 · ${importing?.id || ""}`} description="填写导入原因，导入该技能的历史版本。" onClose={() => { if (!busy) { setImporting(null); setImportResult(null); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setImporting(null)}>关闭</Button><Button variant="primary" type="submit" form="skill-import-form" loading={busy} disabled={!idempotencyKey.trim() || !reason.trim()}>{busy ? "正在导入…" : "导入历史版本"}</Button></>}><form id="skill-import-form" className="form-stack" onSubmit={(event) => void importLegacy(event)}>{actionError && <ErrorNotice message={actionError} />}<div className="skill-form-grid"></div><Field label="导入原因">{(control) => <textarea {...control} rows={3} required value={reason} onChange={(event) => setReason(event.target.value)} />}</Field><Field label="来源哈希（可选）" help="填写 64 位小写 SHA-256；若来源已发生变化，导入将失败。">{(control) => <input {...control} className="code-input" value={expectedSourceHash} onChange={(event) => setExpectedSourceHash(event.target.value)} />}</Field>{importResult && <section className="import-result" aria-live="polite"><header><strong>{importResult.changed ? "已创建新的不可变版本" : "内容未变化"}</strong><Badge tone="success">v{importResult.version}</Badge></header><details><summary>技术信息</summary><dl><div><dt>技能</dt><dd>{importResult.skill_id}</dd></div><div><dt>Schema 版本</dt><dd>{importResult.schema_version}</dd></div><div><dt>来源哈希</dt><dd><code>{importResult.source_hash}</code></dd></div><div><dt>内容哈希</dt><dd><code>{importResult.content_hash}</code></dd></div></dl></details></section>}</form></Modal>
  </section>;
}
