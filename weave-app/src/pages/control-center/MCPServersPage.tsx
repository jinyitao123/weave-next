import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import {
  Braces,
  Cable,
  ChevronRight,
  CircleSlash2,
  Pencil,
  Plus,
  RefreshCw,
  SearchCheck,
  ServerCog,
  ShieldCheck,
  Trash2,
  Wrench,
  X,
} from "lucide-react";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type ApiError,
  type MCPProbeResult,
  type MCPServer,
  type MCPServerUpsertInput,
  type MCPTool,
  type MCPTransport,
} from "../../api";
import { useAuth } from "../../auth/AuthContext";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { Switch } from "../../ui/Switch";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";

const MASKED_SECRET = "••••••••";
const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });

type PairRow = { id: number; key: string; value: string; originalKey?: string; masked?: boolean };
type StringRow = { id: number; value: string };
type EditorMode = "create" | "edit";
type DetailTab = "overview" | "tools";

interface ServerForm {
  slug: string;
  displayName: string;
  transport: MCPTransport;
  url: string;
  command: string;
  args: StringRow[];
  headers: PairRow[];
  env: PairRow[];
  enabled: boolean;
}

let rowSequence = 0;
function nextRowID() { rowSequence += 1; return rowSequence; }
function formatDate(value?: string | null) { return value ? dateTime.format(new Date(value)) : "—"; }
function prettyJSON(value: unknown) {
  if (value === undefined || value === null) return "—";
  try { return JSON.stringify(value, null, 2); } catch { return String(value); }
}
function transportLabel(value: MCPTransport) { return value === "streamable_http" ? "远程服务（Streamable HTTP）" : "本地命令（stdio）"; }
function statusLabel(server: MCPServer) {
  if (server.deleted_at) return "已删除";
  if (server.revoked_at) return "已撤销";
  if (!server.enabled) return "已禁用";
  if (server.status === "online") return "在线";
  if (server.status === "offline") return "离线";
  return server.status === "unknown" ? "未测试" : "未知";
}
function statusTone(server: MCPServer): "success" | "neutral" | "danger" | "warning" {
  if (server.deleted_at || server.revoked_at || !server.enabled) return "neutral";
  if (server.status === "online") return "success";
  if (server.status === "offline") return "danger";
  return "warning";
}
function pairsFromRecord(values: Record<string, string>): PairRow[] {
  return Object.entries(values).map(([key, value]) => ({ id: nextRowID(), key, value, originalKey: key, masked: value === MASKED_SECRET }));
}
function emptyForm(): ServerForm {
  return { slug: "", displayName: "", transport: "streamable_http", url: "", command: "", args: [], headers: [], env: [], enabled: true };
}
function formFromServer(server: MCPServer): ServerForm {
  return {
    slug: server.slug,
    displayName: server.display_name,
    transport: server.transport,
    url: server.url || "",
    command: server.command || "",
    args: (server.args || []).map((value) => ({ id: nextRowID(), value })),
    headers: pairsFromRecord(server.headers || {}),
    env: pairsFromRecord(server.env || {}),
    enabled: server.enabled,
  };
}
function secretPayload(rows: PairRow[], original: Record<string, string>) {
  const values: Record<string, string> = {};
  const retainedOriginalKeys = new Set<string>();
  for (const row of rows) {
    const key = row.key.trim();
    if (!key) continue;
    const unchangedMasked = row.masked && row.originalKey === key && row.value === MASKED_SECRET;
    if (unchangedMasked) retainedOriginalKeys.add(key);
    else if (row.value && row.value !== MASKED_SECRET) values[key] = row.value;
  }
  const deleted = Object.keys(original).filter((key) => !retainedOriginalKeys.has(key) && !rows.some((row) => row.originalKey === key && row.key.trim() === key));
  return { values, deleted };
}
function payloadFromForm(form: ServerForm, editing: MCPServer | null): MCPServerUpsertInput {
  const headerPatch = secretPayload(form.headers, editing?.headers || {});
  const envPatch = secretPayload(form.env, editing?.env || {});
  const http = form.transport === "streamable_http";
  return {
    slug: form.slug.trim(),
    display_name: form.displayName.trim(),
    transport: form.transport,
    url: http ? form.url.trim() : "",
    command: http ? "" : form.command.trim(),
    args: http ? [] : form.args.map(({ value }) => value.trim()).filter(Boolean),
    headers: http ? headerPatch.values : {},
    deleted_headers: http ? headerPatch.deleted : Object.keys(editing?.headers || {}),
    env: http ? {} : envPatch.values,
    deleted_env: http ? Object.keys(editing?.env || {}) : envPatch.deleted,
    enabled: form.enabled,
  };
}
function mcpActionMessage(error: ApiError, operation: "save" | "delete" | "probe" | "tools") {
  if (error.status === 503) return "连接凭据的加密密钥不可用，请联系管理员恢复后重试。";
  if (error.status === 409) {
    if (operation === "delete" && /in use/i.test(error.message)) return "该服务器正被 Agent 使用，不能删除。请先移除所有 Agent 引用。";
    return "标识符已存在，或服务器状态发生变化，请刷新后重试。";
  }
  if (error.status === 422) return `提交内容不符合要求：${error.message}`;
  if (operation === "probe" && error.status === 502) return `连接测试失败：${error.message}`;
  if (error.status === 403) return "当前账号没有管理 MCP 服务器的权限。";
  if (operation === "tools" && error.code === "workflow_mcp_server_closed") return "该服务器已禁用或撤销，无法查看工具列表。";
  return apiErrorMessage(error);
}

export function MCPServersPage() {
  const { user } = useAuth();
  const [servers, setServers] = useState<MCPServer[]>([]);
  const [selectedID, setSelectedID] = useState("");
  const [detail, setDetail] = useState<MCPServer | null>(null);
  const [tools, setTools] = useState<MCPTool[]>([]);
  const [tab, setTab] = useState<DetailTab>("overview");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [detailLoading, setDetailLoading] = useState(false);
  const [toolsLoading, setToolsLoading] = useState(false);
  const [probing, setProbing] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [detailError, setDetailError] = useState<ApiError | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [probeNotice, setProbeNotice] = useState<string | null>(null);
  const [editorMode, setEditorMode] = useState<EditorMode | null>(null);
  const [editing, setEditing] = useState<MCPServer | null>(null);
  const [form, setForm] = useState<ServerForm>(emptyForm);
  const [busy, setBusy] = useState(false);
  const [deleting, setDeleting] = useState<MCPServer | null>(null);
  const [deleteText, setDeleteText] = useState("");

  // The current backend routes use RequireRole("admin") for every write operation.
  const canManage = user?.role === "admin";
  const selected = detail || servers.find((server) => server.id === selectedID) || null;
  const registryUnavailable = error?.status === 503;
  const selectedClosed = Boolean(selected && (!selected.enabled || selected.revoked_at || selected.deleted_at));

  const replaceServer = useCallback((server: MCPServer) => {
    setServers((current) => current.map((item) => item.id === server.id ? server : item));
    setDetail(server);
    setSelectedID(server.id);
  }, []);

  const loadList = useCallback(async (initial = false) => {
    if (initial) setLoading(true); else setRefreshing(true);
    setError(null);
    try {
      const next = await api.listMCPServers();
      setServers(next);
      setSelectedID((current) => current && next.some((server) => server.id === current) ? current : next[0]?.id || "");
      if (!next.length) { setDetail(null); setTools([]); }
    } catch (requestError) {
      setError(normalizeThrownError(requestError));
    } finally {
      setLoading(false); setRefreshing(false);
    }
  }, []);

  const loadDetail = useCallback(async (id: string) => {
    setDetailLoading(true); setDetailError(null); setActionError(null); setProbeNotice(null);
    try {
      const server = await api.getMCPServer(id);
      replaceServer(server);
    } catch (requestError) {
      setDetail(null);
      setDetailError(normalizeThrownError(requestError));
    } finally { setDetailLoading(false); }
  }, [replaceServer]);

  const loadTools = useCallback(async (id: string) => {
    setToolsLoading(true); setDetailError(null);
    try {
      const catalog = await api.getMCPServerTools(id);
      replaceServer(catalog.server);
      setTools(catalog.tools || []);
    } catch (requestError) {
      setTools([]);
      setDetailError(normalizeThrownError(requestError));
    } finally { setToolsLoading(false); }
  }, [replaceServer]);

  useEffect(() => { void loadList(true); }, [loadList]);
  useEffect(() => {
    if (!selectedID) return;
    setTools([]); setTab("overview");
    void loadDetail(selectedID);
  }, [selectedID, loadDetail]);

  function selectTab(next: DetailTab) {
    setTab(next); setActionError(null); setProbeNotice(null);
    if (next === "tools" && selectedID && !tools.length && !selectedClosed) void loadTools(selectedID);
  }

  function beginCreate() {
    setEditing(null); setForm(emptyForm()); setEditorMode("create"); setActionError(null);
  }

  async function beginEdit(server: MCPServer) {
    setBusy(false); setActionError(null); setDetailLoading(true);
    try {
      const current = await api.getMCPServer(server.id);
      replaceServer(current); setEditing(current); setForm(formFromServer(current)); setEditorMode("edit");
    } catch (requestError) { setActionError(mcpActionMessage(normalizeThrownError(requestError), "save")); }
    finally { setDetailLoading(false); }
  }

  function setTransport(transport: MCPTransport) {
    setForm((current) => ({ ...current, transport, url: transport === "streamable_http" ? current.url : "", command: transport === "stdio" ? current.command : "", args: transport === "stdio" ? current.args : [], headers: transport === "streamable_http" ? current.headers : [], env: transport === "stdio" ? current.env : [] }));
  }

  async function saveServer(event: FormEvent) {
    event.preventDefault(); setActionError(null);
    if (!/^[a-z0-9][a-z0-9_-]*$/.test(form.slug)) { setActionError("标识符只能包含小写字母、数字、连字符和下划线，并以字母或数字开头。"); return; }
    if (!form.displayName.trim()) { setActionError("显示名不能为空。"); return; }
    const activeSecrets = form.transport === "streamable_http" ? form.headers : form.env;
    if (activeSecrets.some((row) => !row.key.trim())) { setActionError(`${form.transport === "streamable_http" ? "请求头" : "环境变量"}的名称不能为空。`); return; }
    if (form.transport === "streamable_http") {
      try {
        const parsed = new URL(form.url);
        if (!/^https?:$/.test(parsed.protocol) || parsed.username || parsed.password || parsed.search || parsed.hash) throw new Error();
      } catch { setActionError("地址必须是完整的 http(s) 链接，不能包含用户名密码、参数或 # 片段。"); return; }
    }
    if (form.transport === "stdio" && !form.command.trim()) { setActionError("使用本地命令方式时必须填写启动命令。"); return; }
    setBusy(true);
    try {
      const payload = payloadFromForm(form, editing);
      const saved = editorMode === "create" ? await api.createMCPServer(payload) : await api.updateMCPServer(editing?.id || "", payload);
      setEditorMode(null); setEditing(null); replaceServer(saved); setTools([]); await loadList();
    } catch (requestError) { setActionError(mcpActionMessage(normalizeThrownError(requestError), "save")); }
    finally { setBusy(false); }
  }

  async function probeServer() {
    if (!selected) return;
    setProbing(true); setActionError(null); setProbeNotice(null);
    try {
      const result: MCPProbeResult = await api.probeMCPServer(selected.id);
      replaceServer(result.server); setTools(result.tools || []); setTab("tools");
      await loadList();
      replaceServer(result.server); setTools(result.tools || []); setTab("tools");
      setProbeNotice(`连接测试成功，发现 ${result.tools?.length || 0} 个工具。`);
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setActionError(mcpActionMessage(normalized, "probe"));
      try { replaceServer(await api.getMCPServer(selected.id)); } catch { /* Keep the observed probe error. */ }
    } finally { setProbing(false); }
  }

  async function deleteServer() {
    if (!deleting || deleteText !== deleting.slug) return;
    setBusy(true); setActionError(null);
    try {
      await api.deleteMCPServer(deleting.id);
      setDeleting(null); setDeleteText(""); setDetail(null); setTools([]); setSelectedID(""); await loadList();
    } catch (requestError) { setActionError(mcpActionMessage(normalizeThrownError(requestError), "delete")); }
    finally { setBusy(false); }
  }

  const editorOriginalSecrets = useMemo(() => editing ? (editing.transport === "streamable_http" ? Object.keys(editing.headers || {}).length : Object.keys(editing.env || {}).length) : 0, [editing]);

  return <section className="control-content" aria-labelledby="mcp-servers-heading">
    <div className="control-section-heading"><div><h2 id="mcp-servers-heading">MCP 服务器</h2><p>管理工作区的 MCP 服务器连接，可测试连接并查看可用工具；密钥仅显示掩码。</p></div><div className="heading-actions"><Button variant="ghost" loading={refreshing} disabled={loading} onClick={() => void loadList()}>{!refreshing && <RefreshCw size={16} aria-hidden="true" />}{refreshing ? "正在刷新" : "刷新"}</Button>{canManage && !registryUnavailable && <Button variant="primary" onClick={beginCreate}><Plus size={16} aria-hidden="true" />添加服务器</Button>}</div></div>
    {error && <ErrorNotice message={mcpActionMessage(error, "tools")} onRetry={() => void loadList()} />}
    {registryUnavailable ? <div className="empty-surface mcp-registry-unavailable"><CircleSlash2 size={28} /><h2>暂时无法读取服务器列表</h2><p>连接凭据的加密密钥不可用，请联系管理员恢复后重试。</p><Button onClick={() => void loadList()}>重试</Button></div> : loading && !servers.length ? <LoadingView label="正在加载 MCP 服务器" /> : !servers.length ? <div className="empty-surface mcp-empty"><Cable size={28} /><h2>还没有 MCP 服务器</h2><p>{canManage ? "添加服务器后执行连接测试，即可发现它提供的工具。" : "当前工作区尚未注册 MCP 服务器，注册后即可查看详情与工具。"}</p>{canManage && <Button variant="primary" onClick={beginCreate}><Plus size={16} aria-hidden="true" />添加第一个服务器</Button>}</div> : <div className="mcp-browser">
      <aside className="mcp-master" aria-label="MCP 服务器列表">
        {refreshing && <p className="refresh-note" role="status">正在刷新列表…</p>}
        {servers.map((server) => <button className={server.id === selectedID ? "mcp-row active" : "mcp-row"} type="button" key={server.id} onClick={() => setSelectedID(server.id)}>
          <span className="mcp-row__icon"><ServerCog size={16} /></span><span><strong>{server.display_name}</strong><small>{server.slug} · {transportLabel(server.transport)}</small><small>{server.tool_count} 个工具 · {server.agent_count} 个 Agent</small></span><Badge tone={statusTone(server)}>{statusLabel(server)}</Badge><ChevronRight size={16} />
        </button>)}
      </aside>
      <div className="mcp-detail">
        {detailLoading && !detail ? <LoadingView label="正在加载服务器详情" /> : detailError && !selected ? <ErrorNotice message={mcpActionMessage(detailError, "tools")} onRetry={() => selectedID && void loadDetail(selectedID)} /> : selected ? <article className="mcp-detail-card">
          <header><div><Badge tone={statusTone(selected)}>{statusLabel(selected)}</Badge><h3>{selected.display_name}</h3><p><code>{selected.slug}</code> · {transportLabel(selected.transport)}</p></div><div className="mcp-detail-actions">{canManage && <Button disabled={selectedClosed || detailLoading || busy} title={selectedClosed ? "已关闭的服务器不能编辑" : undefined} onClick={() => void beginEdit(selected)}><Pencil size={16} aria-hidden="true" />编辑</Button>}{canManage && <Button disabled={selectedClosed} loading={probing} onClick={() => void probeServer()}>{!probing && <SearchCheck size={16} aria-hidden="true" />}{probing ? "正在测试" : "测试连接"}</Button>}{canManage && <Button variant="ghost-danger" disabled={busy} onClick={() => { setDeleting(selected); setDeleteText(""); setActionError(null); }}><Trash2 size={16} aria-hidden="true" />删除</Button>}</div></header>
          {!canManage && <p className="readonly-note"><ShieldCheck size={14} />当前账号为只读，仅可查看服务器信息与工具列表。</p>}
          {actionError && !editorMode && !deleting && <ErrorNotice message={actionError} onRetry={() => void loadDetail(selected.id)} />}
          {probeNotice && <p className="success-note mcp-probe-notice" role="status">{probeNotice}</p>}
          {detailError && <ErrorNotice message={mcpActionMessage(detailError, tab === "tools" ? "tools" : "save")} onRetry={() => tab === "tools" ? void loadTools(selected.id) : void loadDetail(selected.id)} />}
          <nav className="mcp-detail-tabs" aria-label="服务器详情"><button className={tab === "overview" ? "active" : ""} type="button" onClick={() => selectTab("overview")}>概览</button><button className={tab === "tools" ? "active" : ""} type="button" onClick={() => selectTab("tools")}>工具 <span>{selected.tool_count}</span></button></nav>
          {tab === "overview" ? <ServerOverview server={selected} /> : <ToolsCatalog tools={tools} loading={toolsLoading} closed={selectedClosed} onRetry={() => void loadTools(selected.id)} />}
        </article> : <div className="empty-surface"><ServerCog size={28} /><h2>选择一个服务器</h2><p>在左侧选择一台服务器，查看详情与工具列表。</p></div>}
      </div>
    </div>}

    <Modal open={!!editorMode} size="wide" title={editorMode === "create" ? "添加 MCP 服务器" : `编辑 ${editing?.display_name || "MCP 服务器"}`} description="切换连接方式会清空另一种方式的专属配置。" onClose={() => { if (!busy) { setEditorMode(null); setEditing(null); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setEditorMode(null)}>取消</Button><Button variant="primary" type="submit" form="mcp-server-form" loading={busy} disabled={!form.slug.trim() || !form.displayName.trim()}>{busy ? "正在保存…" : editorMode === "create" ? "添加服务器" : "保存更改"}</Button></>}>
      <form id="mcp-server-form" className="mcp-editor form-stack" onSubmit={(event) => void saveServer(event)}>
        <div className="mcp-form-grid"><Field label="标识符" help="用于接口调用与引用；只能包含小写字母、数字、连字符和下划线。">{(control) => <input {...control} autoFocus value={form.slug} onChange={(event) => setForm((current) => ({ ...current, slug: event.target.value }))} required autoComplete="off" pattern="[a-z0-9][a-z0-9_-]*" />}</Field><Field label="显示名">{(control) => <input {...control} value={form.displayName} onChange={(event) => setForm((current) => ({ ...current, displayName: event.target.value }))} required />}</Field></div>
        <fieldset className="mcp-transport-picker"><legend>连接方式</legend><label><input type="radio" name="transport" value="streamable_http" checked={form.transport === "streamable_http"} onChange={() => setTransport("streamable_http")} />远程服务（Streamable HTTP）</label><label><input type="radio" name="transport" value="stdio" checked={form.transport === "stdio"} onChange={() => setTransport("stdio")} />本地命令（stdio）</label></fieldset>
        {form.transport === "streamable_http" ? <><Field label="URL" help="不能包含用户名密码、参数或 # 片段；此方式无需启动命令与环境变量。">{(control) => <input {...control} type="url" value={form.url} onChange={(event) => setForm((current) => ({ ...current, url: event.target.value }))} required placeholder="https://example-mcp.test/mcp" />}</Field><SecretEditor title="请求头" rows={form.headers} onChange={(headers) => setForm((current) => ({ ...current, headers }))} keyPlaceholder="Authorization" /></> : <><Field label="启动命令" help="此方式无需地址与请求头。">{(control) => <input {...control} value={form.command} onChange={(event) => setForm((current) => ({ ...current, command: event.target.value }))} required placeholder="mcp-server" />}</Field><StringEditor title="命令参数" rows={form.args} onChange={(args) => setForm((current) => ({ ...current, args }))} placeholder="--flag" /><SecretEditor title="环境变量" rows={form.env} onChange={(env) => setForm((current) => ({ ...current, env }))} keyPlaceholder="API_TOKEN" /></>}
        <label className="switch-row"><span><strong>启用</strong><small>关闭后无法测试连接，且关闭的服务器不能直接重新启用。</small></span><Switch checked={form.enabled} aria-label="启用" onChange={(next) => setForm((current) => ({ ...current, enabled: next }))} /></label>
        {editorMode === "edit" && editorOriginalSecrets > 0 && <p className="contract-note">已保存的 {editorOriginalSecrets} 个密钥以掩码显示；不改动即保留原值，删除某行即移除对应密钥。</p>}
        {actionError && editorMode && <ErrorNotice message={actionError} />}
      </form>
    </Modal>

    <Modal open={!!deleting} title="永久删除 MCP 服务器" description="删除后将禁用并撤销该服务器，清空连接测试记录与工具列表，且无法恢复。" onClose={() => { if (!busy) { setDeleting(null); setDeleteText(""); setActionError(null); } }} footer={<><Button disabled={busy} onClick={() => setDeleting(null)}>取消</Button><Button variant="danger" loading={busy} disabled={deleteText !== deleting?.slug} onClick={() => void deleteServer()}>{busy ? "正在删除…" : "永久删除"}</Button></>}>
      <div className="form-stack"><p className="confirm-copy">该操作不可逆。如有 Agent 正在使用该服务器，需先移除引用后才能删除。</p><Field label={`输入 ${deleting?.slug} 确认`}>{(control) => <input {...control} value={deleteText} onChange={(event) => setDeleteText(event.target.value)} autoComplete="off" />}</Field>{actionError && deleting && <ErrorNotice message={actionError} onRetry={() => void loadDetail(deleting.id)} />}</div>
    </Modal>
  </section>;
}

function ServerOverview({ server }: { server: MCPServer }) {
  const connection = server.transport === "streamable_http" ? server.url || "—" : [server.command, ...(server.args || [])].filter(Boolean).join(" ") || "—";
  const secretMap = server.transport === "streamable_http" ? server.headers : server.env;
  const facts: Array<[string, string | number | null | undefined, boolean?]> = [
    ["标识符", server.slug, true], ["连接方式", transportLabel(server.transport)],
    ["连接地址", connection, true], ["启用", server.enabled ? "是" : "否"], ["状态", statusLabel(server)],
    ["协议版本", server.protocol_version], ["工具数", server.tool_count], ["引用 Agent 数", server.agent_count], ["创建人", server.created_by],
    ["上次测试", formatDate(server.last_probed_at)], ["上次握手", formatDate(server.last_handshake_at)],
    ["创建时间", formatDate(server.created_at)],
  ];
  if (server.revoked_at) facts.push(["撤销时间", formatDate(server.revoked_at)]);
  if (server.deleted_at) facts.push(["删除时间", formatDate(server.deleted_at)]);
  return <div className="mcp-overview">
    {server.last_error && <div className="mcp-last-error"><strong>最近错误</strong><p>{server.last_error}</p></div>}
    <dl className="mcp-facts">{facts.map(([label, value, code]) => <div key={label}><dt>{label}</dt><dd>{code ? <code>{value ?? "—"}</code> : value ?? "—"}</dd></div>)}</dl>
    <details className="mcp-json-section"><summary><span>技术信息</span><Braces size={16} /></summary><p>最后一次成功握手返回的服务器原始信息。</p><pre>{prettyJSON(server.server_info ?? {})}</pre></details>
    <section className="mcp-secret-summary"><header><div><h4>{server.transport === "streamable_http" ? "请求头" : "环境变量"}</h4><p>出于安全仅显示键名与掩码。</p></div><span>{Object.keys(secretMap || {}).length} 项</span></header>{!Object.keys(secretMap || {}).length ? <p>未配置</p> : <dl>{Object.entries(secretMap || {}).map(([key, value]) => <div key={key}><dt><code>{key}</code></dt><dd>{value}</dd></div>)}</dl>}</section>
  </div>;
}

function ToolsCatalog({ tools, loading, closed, onRetry }: { tools: MCPTool[]; loading: boolean; closed: boolean; onRetry(): void }) {
  if (loading) return <LoadingView label="正在加载工具列表" />;
  if (closed) return <div className="empty-surface mcp-tools-empty"><CircleSlash2 size={28} /><h2>服务器已关闭</h2><p>已禁用或撤销的服务器无法查看工具列表。</p></div>;
  if (!tools.length) return <div className="empty-surface mcp-tools-empty"><Wrench size={28} /><h2>没有已发现的工具</h2><p>执行连接测试后，发现的工具会显示在这里。</p><Button onClick={onRetry}>刷新</Button></div>;
  return <div className="mcp-tools-catalog">{tools.map((tool) => <article className="mcp-tool-card" key={`${tool.server_id}:${tool.name}`}><header><div><h4>{tool.name}</h4><p>{tool.description || "暂无描述"}</p></div><Badge tone={tool.read_only_hint === true ? "success" : tool.read_only_hint === false ? "warning" : "neutral"}>{tool.read_only_hint === true ? "只读" : tool.read_only_hint === false ? "可能写入" : "未声明"}</Badge></header><dl><div><dt>发现时间</dt><dd><time dateTime={tool.discovered_at}>{formatDate(tool.discovered_at)}</time></dd></div></dl><details open><summary>输入参数</summary><pre>{prettyJSON(tool.input_schema)}</pre></details><details><summary>标注数据</summary><pre>{prettyJSON(tool.annotations ?? {})}</pre></details></article>)}</div>;
}

function StringEditor({ title, rows, onChange, placeholder }: { title: string; rows: StringRow[]; onChange(rows: StringRow[]): void; placeholder: string }) {
  return <div className="structured-list"><div className="structured-list__heading"><strong>{title}</strong><Button variant="ghost" onClick={() => onChange([...rows, { id: nextRowID(), value: "" }])}><Plus size={14} aria-hidden="true" />添加</Button></div>{!rows.length ? <p className="structured-list__empty">未配置{title}。</p> : rows.map((row) => <div className="structured-row" key={row.id}><input aria-label={`${title}值`} value={row.value} placeholder={placeholder} onChange={(event) => onChange(rows.map((item) => item.id === row.id ? { ...item, value: event.target.value } : item))} /><button className="icon-button icon-button--danger" type="button" aria-label={`删除${title}项`} onClick={() => onChange(rows.filter((item) => item.id !== row.id))}><X size={16} /></button></div>)}</div>;
}

function SecretEditor({ title, rows, onChange, keyPlaceholder }: { title: string; rows: PairRow[]; onChange(rows: PairRow[]): void; keyPlaceholder: string }) {
  return <div className="structured-list"><div className="structured-list__heading"><strong>{title}</strong><Button variant="ghost" onClick={() => onChange([...rows, { id: nextRowID(), key: "", value: "" }])}><Plus size={14} aria-hidden="true" />添加</Button></div>{!rows.length ? <p className="structured-list__empty">未配置{title}。</p> : rows.map((row) => <div className="structured-row structured-row--pair mcp-secret-row" key={row.id}><input aria-label={`${title}名称`} value={row.key} placeholder={keyPlaceholder} onChange={(event) => onChange(rows.map((item) => item.id === row.id ? { ...item, key: event.target.value } : item))} /><input aria-label={`${title}密钥`} type="password" value={row.value} placeholder="密钥" onFocus={() => { if (row.masked && row.value === MASKED_SECRET) onChange(rows.map((item) => item.id === row.id ? { ...item, value: "", masked: false } : item)); }} onChange={(event) => onChange(rows.map((item) => item.id === row.id ? { ...item, value: event.target.value, masked: false } : item))} /><button className="icon-button icon-button--danger" type="button" aria-label={`删除${row.key || title}密钥`} onClick={() => onChange(rows.filter((item) => item.id !== row.id))}><X size={16} /></button>{row.masked && <small>已保存；保持不动即保留</small>}</div>)}</div>;
}
