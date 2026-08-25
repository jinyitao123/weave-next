import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Brain, LockKeyhole, Plus, RefreshCw, Search, Trash2, UserRoundCog } from "lucide-react";
import { NavLink } from "react-router-dom";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type AgentMemory,
  type AgentMemoryProfile,
  type AgentMemorySearchResult,
  type AgentMemorySlot,
  type AgentRecord,
} from "../../api";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";

const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });

function disabledResponse(response: unknown): response is { enabled: false; memories: [] } {
  return !Array.isArray(response) && typeof response === "object" && response !== null && "enabled" in response && response.enabled === false;
}

function formatDate(value?: string) {
  if (!value || value.startsWith("0001-")) return "—";
  return dateTime.format(new Date(value));
}

function scopeLabel(scope?: string) {
  return scope === "user" ? "用户隔离" : scope === "session" ? "会话隔离" : "工作区 / 租户隔离";
}

export function MemoryConfiguration({ agent }: { agent: AgentRecord | null }) {
  const config = agent?.memory_config;
  const memoryEnabled = config?.enabled !== false;
  return <section className="memory-config-summary"><header><div><h3>当前记忆配置</h3><p>查看该智能体的记忆设置与运行状态。</p></div><NavLink className="ui-button ui-button--ghost" to="../agents">前往智能体</NavLink></header><dl><div><dt>启用状态</dt><dd><Badge tone={memoryEnabled ? "success" : "neutral"}>{memoryEnabled ? "已启用" : "未启用"}</Badge></dd></div><div><dt>召回条数 Top K</dt><dd>{config?.top_k || "服务端默认"}</dd></div><div><dt>自动记忆</dt><dd>{config?.auto_remember !== false ? "开启" : "关闭"}</dd></div><div><dt>隔离范围</dt><dd>{scopeLabel(config?.scope)}</dd></div></dl>{config?.enabled === false && <p className="contract-note">此处为只读状态；修改记忆配置请前往「智能体」页。</p>}</section>;
}

export function MemoriesPanel({ agentName, configured }: { agentName: string; configured: boolean }) {
  const [memories, setMemories] = useState<Array<AgentMemory | AgentMemorySearchResult>>([]);
  const [enabled, setEnabled] = useState(true);
  const [loading, setLoading] = useState(true);
  const [searching, setSearching] = useState(false);
  const [query, setQuery] = useState("");
  const [topK, setTopK] = useState("5");
  const [creating, setCreating] = useState(false);
  const [content, setContent] = useState("");
  const [metadataText, setMetadataText] = useState("{}");
  const [deleting, setDeleting] = useState<AgentMemory | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const load = useCallback(async () => {
    if (!agentName) return;
    setLoading(true); setError(null); setSearching(false);
    try {
      const response = await api.listAgentMemories(agentName);
      if (disabledResponse(response)) { setEnabled(false); setMemories([]); }
      else { setEnabled(true); setMemories(response); }
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      if (normalized.status === 501) { setEnabled(false); setMemories([]); }
      else setError(apiErrorMessage(normalized));
    } finally { setLoading(false); }
  }, [agentName]);

  useEffect(() => { void load(); }, [load]);

  async function searchMemories(event: FormEvent) {
    event.preventDefault();
    if (!query.trim()) return;
    setBusy(true); setError(null);
    try {
      const response = await api.searchAgentMemories(agentName, { query: query.trim(), top_k: Math.max(1, Number(topK) || 5) });
      if (disabledResponse(response)) { setEnabled(false); setMemories([]); setSearching(false); }
      else { setEnabled(true); setMemories(response); setSearching(true); }
    } catch (requestError) { setError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function createMemory(event: FormEvent) {
    event.preventDefault(); setActionError(null);
    let metadata: Record<string, unknown> | undefined;
    try {
      const parsed = JSON.parse(metadataText) as unknown;
      if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) throw new Error();
      metadata = parsed as Record<string, unknown>;
    } catch { setActionError("附加信息必须是合法的 JSON 对象。"); return; }
    setBusy(true);
    try {
      await api.createAgentMemory(agentName, { content: content.trim(), metadata });
      setCreating(false); setContent(""); setMetadataText("{}"); await load();
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      if (normalized.status === 501) { setEnabled(false); setCreating(false); setError("记忆服务未配置嵌入模型，暂不可用，请联系平台管理员。"); }
      else setActionError(apiErrorMessage(normalized));
    } finally { setBusy(false); }
  }

  async function deleteMemory() {
    if (!deleting) return;
    setBusy(true); setActionError(null);
    try { await api.deleteAgentMemory(agentName, deleting.id); setDeleting(null); await load(); }
    catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      if (normalized.status === 501) { setEnabled(false); setDeleting(null); setError("记忆服务未配置嵌入模型，暂不可用，请联系平台管理员。"); }
      else setActionError(apiErrorMessage(normalized));
    } finally { setBusy(false); }
  }

  return <section className="memory-operation"><header><div><h3>向量记忆</h3><p>最多显示 100 条记忆；语义搜索结果会显示相关度。</p></div><div className="heading-actions"><Button variant="ghost" loading={loading} onClick={() => void load()}>{!loading && <RefreshCw size={16} aria-hidden="true" />}刷新</Button><Button variant="primary" disabled={!enabled} onClick={() => { setCreating(true); setActionError(null); }}><Plus size={16} aria-hidden="true" />添加记忆</Button></div></header>
    {!configured && <p className="contract-note">该智能体的记忆功能未启用；此处展示运行时实际状态，修改配置请前往「智能体」页。</p>}
    {error && <ErrorNotice message={error} onRetry={() => void load()} />}{actionError && !creating && !deleting && <ErrorNotice message={actionError} />}
    <form className="memory-search-form" onSubmit={(event) => void searchMemories(event)}><label className="field"><span>语义搜索</span><div className="inline-input"><input value={query} placeholder="输入语义查询" onChange={(event) => setQuery(event.target.value)} /><span>返回条数</span><input className="top-k" type="number" min="1" value={topK} aria-label="返回条数" onChange={(event) => setTopK(event.target.value)} /><Button variant="ghost" type="submit" disabled={busy || !enabled || !query.trim()}><Search size={16} aria-hidden="true" />搜索</Button></div></label>{searching && <Button variant="ghost" onClick={() => void load()}>退出搜索</Button>}</form>
    {loading ? <LoadingView label="正在读取智能体记忆" /> : !enabled ? <div className="empty-surface memory-disabled"><Brain size={28} /><h2>记忆未配置</h2><p>记忆服务未配置嵌入模型，暂不可用，请联系平台管理员。</p></div> : !memories.length ? <div className="empty-surface memory-empty"><Brain size={28} /><h2>{searching ? "没有语义匹配" : "还没有记忆"}</h2><p>{searching ? "请调整关键词后重新搜索。" : "点击「添加记忆」创建第一条长期记忆。"}</p></div> : <div className="agent-memory-list">{memories.map((memory) => <article key={memory.id}><header><strong>{memory.content}</strong>{"score" in memory && <Badge tone="success">相关度 {memory.score.toFixed(2)}</Badge>}</header><dl><div><dt>创建</dt><dd>{formatDate(memory.created_at)}</dd></div><div><dt>最近访问</dt><dd>{formatDate(memory.accessed_at)}</dd></div><div><dt>访问次数</dt><dd>{memory.access_count}</dd></div></dl><details><summary>附加数据</summary><pre>{JSON.stringify(memory.metadata || {}, null, 2)}</pre></details><footer><Button variant="ghost-danger" onClick={() => { setDeleting(memory); setActionError(null); }}><Trash2 size={14} aria-hidden="true" />删除</Button></footer></article>)}</div>}
    <Modal open={creating} size="wide" title="添加记忆" description="保存后立即生效。" onClose={() => { if (!busy) { setCreating(false); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setCreating(false)}>取消</Button><Button variant="primary" type="submit" form="agent-memory-create" loading={busy} disabled={!content.trim()}>{busy ? "正在保存…" : "保存并刷新"}</Button></>}><form id="agent-memory-create" className="form-stack" onSubmit={(event) => void createMemory(event)}>{actionError && <ErrorNotice message={actionError} />}<Field label="记忆内容">{(control) => <textarea {...control} autoFocus rows={7} required value={content} onChange={(event) => setContent(event.target.value)} />}</Field><Field label="附加信息（JSON，可选）" help="必须是合法的 JSON 对象；可提交空对象。">{(control) => <textarea {...control} className="code-input" rows={10} value={metadataText} onChange={(event) => setMetadataText(event.target.value)} />}</Field></form></Modal>
    <Modal open={!!deleting} title="删除记忆" description="删除后不可恢复。" onClose={() => { if (!busy) { setDeleting(null); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setDeleting(null)}>取消</Button><Button variant="danger" loading={busy} onClick={() => void deleteMemory()}>{busy ? "正在删除…" : "确认删除"}</Button></>}><p className="confirm-copy">确定删除记忆“{deleting?.content.slice(0, 80)}{(deleting?.content.length || 0) > 80 ? "…" : ""}”吗？</p>{actionError && <ErrorNotice message={actionError} />}</Modal>
  </section>;
}

export function SlotsPanel({ agentName, canWrite }: { agentName: string; canWrite: boolean }) {
  const [slots, setSlots] = useState<AgentMemorySlot[]>([]);
  const [draft, setDraft] = useState<AgentMemorySlot[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true); setError(null); setNotice(null);
    try { const next = await api.getAgentMemorySlots(agentName); setSlots(next); setDraft(next); }
    catch (requestError) { setError(apiErrorMessage(requestError)); setSlots([]); setDraft([]); }
    finally { setLoading(false); }
  }, [agentName]);
  useEffect(() => { void load(); }, [load]);

  function update(index: number, patch: Partial<AgentMemorySlot>) { setDraft((current) => current.map((slot, slotIndex) => slotIndex === index ? { ...slot, ...patch } : slot)); }
  async function save(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError(null); setNotice(null);
    try {
      const saved = await api.updateAgentMemorySlots(agentName, draft.map((slot) => ({ key: slot.key.trim(), label: slot.label.trim(), description: slot.description.trim() })));
      setSlots(saved); setDraft(saved); setNotice("已保存");
    } catch (requestError) { setError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }
  const dirty = JSON.stringify(slots) !== JSON.stringify(draft);
  return <section className="memory-operation"><header><div><h3>记忆槽位</h3><p>以键值形式保存智能体的长期结构化信息；保存为整体覆盖。</p></div><Button variant="ghost" loading={loading} onClick={() => void load()}>{!loading && <RefreshCw size={16} aria-hidden="true" />}刷新</Button></header>{!canWrite && <div className="boundary-banner"><LockKeyhole size={16} /><div><strong>只读</strong><p>当前角色只读，仅管理员可编辑。</p></div></div>}{error && <ErrorNotice message={error} onRetry={() => void load()} />}{notice && <p className="success-note" role="status">{notice}</p>}{loading ? <LoadingView label="正在读取记忆槽位" /> : <form className="slots-editor" onSubmit={(event) => void save(event)}><div className="structured-list__heading"><strong>{draft.length} 个记忆槽位</strong><span>键 / 名称 / 描述</span>{canWrite && <Button variant="ghost" onClick={() => setDraft((current) => [...current, { key: "", label: "", description: "" }])}><Plus size={14} aria-hidden="true" />添加槽位</Button>}</div>{!draft.length ? <div className="empty-surface slots-empty"><Brain size={28} /><h2>还没有记忆槽位</h2><p>请根据需要添加长期结构化信息。</p></div> : draft.map((slot, index) => <div className="structured-row structured-row--slot" key={`${index}-${slot.key}`}><input aria-label={`记忆槽位 ${index + 1} 的键`} placeholder="键" value={slot.key} disabled={!canWrite} onChange={(event) => update(index, { key: event.target.value })} /><input aria-label={`记忆槽位 ${index + 1} 的名称`} placeholder="名称" value={slot.label} disabled={!canWrite} onChange={(event) => update(index, { label: event.target.value })} /><input aria-label={`记忆槽位 ${index + 1} 的描述`} placeholder="描述" value={slot.description} disabled={!canWrite} onChange={(event) => update(index, { description: event.target.value })} />{canWrite && <button className="icon-button icon-button--danger" type="button" aria-label={`删除记忆槽位 ${slot.key || index + 1}`} onClick={() => setDraft((current) => current.filter((_, slotIndex) => slotIndex !== index))}><Trash2 size={14} /></button>}</div>)}{canWrite && <div className="form-actions"><Button disabled={!dirty || busy} onClick={() => setDraft(slots)}>放弃更改</Button><Button variant="primary" type="submit" loading={busy} disabled={!dirty}>{busy ? "正在保存…" : "保存记忆槽位"}</Button></div>}</form>}</section>;
}

export function ProfilePanel({ agentName, canRead }: { agentName: string; canRead: boolean }) {
  const [userID, setUserID] = useState("");
  const [profile, setProfile] = useState<AgentMemoryProfile | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [userOptions, setUserOptions] = useState<Array<{ id: string; username?: string; display_name?: string }>>([]);

  useEffect(() => {
    if (!canRead) return;
    let cancelled = false;
    void api.listUsers().then((records) => { if (!cancelled) setUserOptions(records); }).catch(() => undefined);
    return () => { cancelled = true; };
  }, [canRead]);

  async function readProfile(event?: FormEvent) {
    event?.preventDefault();
    if (!userID.trim() || !canRead) return;
    setLoading(true); setError(null); setProfile(null);
    try { setProfile(await api.getAgentMemoryProfile(agentName, userID.trim())); }
    catch (requestError) { setError(apiErrorMessage(requestError)); }
    finally { setLoading(false); }
  }

  return <section className="memory-operation"><header><div><h3>用户画像</h3><p>按用户查看该智能体记录的个人画像（只读）。</p></div></header>{!canRead ? <div className="empty-surface control-forbidden"><UserRoundCog size={28} /><h2>当前账号没有查看用户画像的权限</h2><p>仅管理员和所有者可查看用户画像。</p></div> : <><form className="profile-query" onSubmit={(event) => void readProfile(event)}><label className="field"><span>用户</span><div className="inline-input"><select value={userID} onChange={(event) => { setUserID(event.target.value); setProfile(null); setError(null); }}><option value="">选择用户</option>{userOptions.map((user) => <option key={user.id} value={user.id}>{user.display_name || user.username || user.id}</option>)}</select><Button variant="primary" type="submit" disabled={loading || !userID.trim()}><Search size={16} aria-hidden="true" />查看画像</Button></div><small>查看该用户在本智能体记录下的个人画像。</small></label></form>{error && <ErrorNotice message={error} onRetry={() => void readProfile()} />}{loading ? <LoadingView label="正在读取用户画像" /> : profile ? <article className="profile-result"><header><div><Badge tone="neutral">只读</Badge><h3>{profile.user_id}</h3><p>更新 {formatDate(profile.updated_at)}</p></div></header><details className="memory-technical"><summary>技术信息</summary><dl><div><dt>工作区</dt><dd><code>{profile.workspace_id}</code></dd></div><div><dt>智能体</dt><dd><code>{profile.agent_id}</code></dd></div><div><dt>用户</dt><dd><code>{profile.user_id}</code></dd></div><div><dt>版本</dt><dd>{profile.version}</dd></div></dl></details><section><header><h4>记忆槽位</h4><p>{Object.keys(profile.slots).length} 个已存字段</p></header>{Object.keys(profile.slots).length ? <dl className="profile-slots">{Object.entries(profile.slots).map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{value}</dd></div>)}</dl> : <div className="empty-surface profile-empty"><Brain size={28} /><h2>暂无用户画像</h2><p>该用户尚未形成个人画像。</p></div>}</section></article> : <div className="empty-surface profile-empty"><UserRoundCog size={28} /><h2>选择用户</h2><p>从上方选择用户后查看该智能体记录的个人画像。</p></div>}</>}</section>;
}
