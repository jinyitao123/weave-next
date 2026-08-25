import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Bot, Pencil, Plus, RefreshCw, Trash2, UserRound } from "lucide-react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type AgentEngine,
  type AgentRecord,
  type AgentRole,
  type Runtime,
} from "../../api";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { engineLabel } from "../../workspace/labels";
import { isPlatformAsset } from "../../workspace/identity";
import { AgentCreateModal } from "./AgentDetailPage";

const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
function formatDate(value?: string) { return value ? dateTime.format(new Date(value)) : "—"; }
function isCLIEngine(engine: AgentEngine) { return engine === "opencode" || engine === "codex" || engine === "claude"; }
function editorError(error: unknown) {
  const normalized = normalizeThrownError(error);
  if (normalized.status === 403) return "当前账号没有编辑智能体的权限。";
  if (normalized.status === 409) return `配置冲突：${apiErrorMessage(normalized)}`;
  if (normalized.status === 422) return `所有者不是当前工作区成员：${apiErrorMessage(normalized)}`;
  return apiErrorMessage(normalized);
}

export function AgentsPage() {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const requestedAgentID = searchParams.get("agent") || "";
  const requestedCreateRole = searchParams.get("create");
  const createRequestHandled = useRef(false);
  const [agents, setAgents] = useState<AgentRecord[]>([]);
  const [runtimes, setRuntimes] = useState<Runtime[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [creatingRole, setCreatingRole] = useState<AgentRole | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<AgentRecord | null>(null);
  const [deleteText, setDeleteText] = useState("");
  const runtimesByID = useMemo(() => new Map(runtimes.map((runtime) => [runtime.id, runtime])), [runtimes]);
  // 平台内置资产（__team_architect 等）不进入名册——它们是基础设施，不是用户资产。
  const visibleAgents = useMemo(() => agents.filter((agent) => !isPlatformAsset(agent)), [agents]);

  const beginCreate = useCallback((requestedRole?: unknown) => {
    const role: AgentRole = requestedRole === "avatar" || requestedRole === "worker" ? requestedRole : "worker";
    setCreatingRole(role); setActionError(null);
  }, []);

  const refresh = useCallback(async (initial = false) => {
    if (initial) setLoading(true); else setRefreshing(true);
    setError(null); setForbidden(false);
    try {
      const records = (await api.listAgents()).filter((agent) => !agent.deleted);
      setAgents(records);
      if (initial && requestedAgentID) {
        const requested = records.find((agent) => agent.id === requestedAgentID || agent.name === requestedAgentID);
        navigate(`/control/agents/${encodeURIComponent(requested?.name || requestedAgentID)}`, { replace: true });
      }
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setForbidden(normalized.status === 403);
      setError(apiErrorMessage(normalized));
    } finally { setLoading(false); setRefreshing(false); }
  }, [navigate, requestedAgentID]);

  const refreshRuntimes = useCallback(async (signal?: AbortSignal) => {
    try { setRuntimes((await api.listRuntimes(signal)).runtimes); }
    catch { /* 运行环境名称仅用于列表展示，加载失败时回退为「已绑定的运行环境」。 */ }
  }, []);

  useEffect(() => { void refresh(true); }, [refresh]);
  useEffect(() => { const controller = new AbortController(); void refreshRuntimes(controller.signal); return () => controller.abort(); }, [refreshRuntimes]);

  useEffect(() => {
    if (requestedCreateRole !== "avatar" && requestedCreateRole !== "worker") {
      createRequestHandled.current = false;
      return;
    }
    if (loading || forbidden || createRequestHandled.current) return;
    createRequestHandled.current = true;
    beginCreate(requestedCreateRole);
    const nextParams = new URLSearchParams(searchParams);
    nextParams.delete("create");
    setSearchParams(nextParams, { replace: true });
  }, [beginCreate, forbidden, loading, requestedCreateRole, searchParams, setSearchParams]);

  function closeCreate() {
    setCreatingRole(null); setActionError(null);
  }

  async function deleteAgent() {
    if (!deleting || deleteText !== deleting.name) return;
    setBusy(true); setActionError(null);
    try { await api.deleteAgent(deleting.name); setDeleting(null); setDeleteText(""); await refresh(); }
    catch (requestError) { setActionError(editorError(requestError)); }
    finally { setBusy(false); }
  }

  return <section className="control-content" aria-labelledby="agents-heading">
    <div className="control-section-heading"><div><h2 id="agents-heading">智能体</h2><p>管理工作区内所有智能体的身份、提示词、工具与预算配置。</p></div><div className="heading-actions"><Button variant="ghost" loading={refreshing} disabled={loading} onClick={() => void refresh()}>{!refreshing && <RefreshCw size={16} aria-hidden="true" />}{refreshing ? "正在刷新" : "刷新"}</Button>{!forbidden && <Button variant="primary" onClick={beginCreate}><Plus size={16} />创建智能体</Button>}</div></div>
    {error && !forbidden && <ErrorNotice message={error} onRetry={() => void refresh()} />}
    {actionError && !deleting && <ErrorNotice message={actionError} onRetry={() => void refresh()} />}
    {forbidden ? <div className="empty-surface control-forbidden"><Bot size={28} /><h2>没有查看智能体的权限</h2><p>当前账号没有查看智能体的权限。</p></div> : loading && !agents.length ? <LoadingView label="正在加载智能体列表" /> : !agents.length ? <div className="empty-surface agents-empty"><Bot size={28} /><h2>还没有智能体</h2><p>当前工作区还没有智能体。创建后可在这里管理配置。</p><Button variant="primary" onClick={beginCreate}><Plus size={16} />创建第一个智能体</Button></div> : <>
      {!visibleAgents.length ? <div className="empty-surface agents-empty"><Bot size={28} /><h2>还没有智能体</h2><p>当前工作区还没有智能体。创建后可在这里管理配置。</p><Button variant="primary" onClick={beginCreate}><Plus size={16} />创建第一个智能体</Button></div> : <div className="control-table agents-table" role="table" aria-label="智能体列表">
      <div className="control-table__header" role="row"><span>智能体</span><span>角色</span><span>引擎 / 运行环境</span><span>模型</span><span>版本</span><span>更新时间</span><span /></div>
      {visibleAgents.map((agent) => <div className="control-table__row" role="row" key={agent.id || agent.name}>
        <div className="table-identity"><span className="agent-glyph">{agent.role === "avatar" ? <UserRound size={16} /> : <Bot size={16} />}</span><span><strong>{agent.display_name || agent.name}</strong>{agent.display_name && agent.display_name !== agent.name && <small>{agent.name}</small>}</span></div>
        <span>{agent.role === "avatar" ? "分身" : "员工"}</span><span title={agent.runtime_id || undefined}>{engineLabel(agent.engine)}{agent.runtime_id ? ` · ${runtimesByID.get(agent.runtime_id)?.name || "已绑定的运行环境"}` : isCLIEngine(agent.engine as AgentEngine) ? " · 平台本机" : ""}</span><span>{agent.model || "—"}</span><span>v{agent.version}</span><time dateTime={agent.updated_at}>{formatDate(agent.updated_at)}</time>
        <div className="row-actions"><Link className="icon-button" aria-label={`编辑 ${agent.display_name || agent.name}`} to={`/control/agents/${encodeURIComponent(agent.name)}`}><Pencil size={16} /></Link><button className="icon-button icon-button--danger" type="button" aria-label={`删除 ${agent.display_name || agent.name}`} disabled={busy} onClick={() => { setDeleting(agent); setDeleteText(""); setActionError(null); }}><Trash2 size={16} /></button></div>
      </div>)}
    </div>}</>}

    <AgentCreateModal role={creatingRole} onClose={closeCreate} onCreated={(name) => { void refresh(); navigate(`/control/agents/${encodeURIComponent(name)}`); }} />

    <Modal open={!!deleting} title="删除智能体" description="删除后不可恢复；若该智能体仍被团队引用，删除将被拒绝。" onClose={() => { if (!busy) { setDeleting(null); setDeleteText(""); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setDeleting(null)}>取消</Button><Button variant="danger" loading={busy} disabled={deleteText !== deleting?.name} onClick={() => void deleteAgent()}>{busy ? "正在删除…" : "确认删除"}</Button></>}>
      <div className="form-stack"><p className="confirm-copy">删除会影响使用 <strong>{deleting?.name}</strong> 的调用方。输入完整的智能体名称以启用确认按钮。</p><Field label="智能体名称">{(control) => <input {...control} autoFocus value={deleteText} onChange={(event) => setDeleteText(event.target.value)} autoComplete="off" />}</Field>{actionError && <ErrorNotice message={actionError} />}</div>
    </Modal>
  </section>;
}
