import "../styles/runtimes.css";
import { useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Clipboard, LogOut, MonitorCog, Pencil, Play, Plus, RefreshCw, Server, Square, Trash2 } from "lucide-react";
import { api, apiErrorMessage, type Runtime, type RuntimeCreateResponse } from "../api";
import { useAuth } from "../auth/useAuth";
import { platform, type LocalRuntimeStatus } from "../platform";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Card } from "../ui/Card";
import { Field } from "../ui/Field";
import { ConfirmModal } from "../ui/ConfirmModal";
import { Modal } from "../ui/Modal";
import { ErrorNotice, LoadingView } from "../ui/StatusViews";
import { engineLabel } from "./runtime-labels";

const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });

const authModeLabels: Record<string, string> = {
  chatgpt: "Codex ChatGPT 订阅",
  oauth: "Claude 官方 OAuth",
  provider: "配置的 API Provider",
  unknown: "认证方式未知",
};

const endpointClassLabels: Record<string, string> = {
  openai_subscription: "OpenAI 官方订阅端点",
  anthropic_first_party: "Anthropic 官方端点",
  configured_provider: "工作区配置端点",
};

const fallbackLocalServerUrl = "http://127.0.0.1:8080";
const configuredLocalServerUrl = import.meta.env.VITE_WEAVE_API_URL?.trim();
const defaultLocalServerUrl = (configuredLocalServerUrl || fallbackLocalServerUrl).replace(/\/+$/, "") || fallbackLocalServerUrl;

function normalizedStatus(status: LocalRuntimeStatus): LocalRuntimeStatus {
  return {
    running: status.running,
    pid: status.pid,
    startedAt: status.startedAt,
    lastError: status.lastError?.replace(/rtk_[A-Za-z0-9._~-]+/g, "[已隐藏运行节点令牌]"),
  };
}

function normalizedServerUrl(value: string): { value?: string; error?: string } {
  const trimmed = value.trim();
  try {
    const parsed = new URL(trimmed);
    if ((parsed.protocol !== "http:" && parsed.protocol !== "https:") || !parsed.host) throw new Error("unsupported URL");
    return { value: trimmed.replace(/\/+$/, "") };
  } catch {
    return { error: "服务端地址必须以 http:// 或 https:// 开头" };
  }
}

function localStartedAt(value?: string): string {
  if (!value) return "—";
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? "时间不可用" : dateTime.format(parsed);
}

function shellSingleQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

function powershellSingleQuote(value: string): string {
  return `'${value.replace(/'/g, "''")}'`;
}

export function RuntimesPage() {
  const { logout, user } = useAuth();
  const desktopLocalRuntime = platform.kind === "desktop" && platform.capabilities.localRuntime;
  const [runtimes, setRuntimes] = useState<Runtime[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [createError, setCreateError] = useState<string | null>(null);
  const [created, setCreated] = useState<RuntimeCreateResponse | null>(null);
  const [copyNotice, setCopyNotice] = useState<string | null>(null);
  const [editing, setEditing] = useState<Runtime | null>(null);
  const [removing, setRemoving] = useState<Runtime | null>(null);
  const [confirmStopLocal, setConfirmStopLocal] = useState(false);
  const [renamed, setRenamed] = useState("");
	const [editedPoolId, setEditedPoolId] = useState("");
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [localStatus, setLocalStatus] = useState<LocalRuntimeStatus | null>(null);
  const [localInspecting, setLocalInspecting] = useState(desktopLocalRuntime);
  const [localAction, setLocalAction] = useState<"start" | "stop" | null>(null);
  const [localError, setLocalError] = useState<string | null>(null);
  const [localNotice, setLocalNotice] = useState<string | null>(null);
  const [tokenStartError, setTokenStartError] = useState<string | null>(null);
  const [serverUrl, setServerUrl] = useState(defaultLocalServerUrl);
  const [concurrency, setConcurrency] = useState("2");
  const [binaryPath, setBinaryPath] = useState("");
  const localStartPending = useRef(false);

  const refresh = useCallback(async () => {
    setLoading(true); setError(null);
    try { setRuntimes((await api.listRuntimes()).runtimes); }
    catch (requestError) { setError(apiErrorMessage(requestError)); }
    finally { setLoading(false); }
  }, []);

  const inspectLocal = useCallback(async () => {
    if (!desktopLocalRuntime) return;
    setLocalInspecting(true);
    try { setLocalStatus(normalizedStatus(await platform.inspectLocalRuntime())); setLocalError(null); }
    catch { setLocalError("无法读取本机运行节点进程状态。"); }
    finally { setLocalInspecting(false); }
  }, [desktopLocalRuntime]);

  useEffect(() => { void refresh(); }, [refresh]);
  useEffect(() => { void inspectLocal(); }, [inspectLocal]);

  async function create(event: FormEvent) {
    event.preventDefault();
    if (!name.trim()) return;
    setBusy(true); setCreateError(null);
    try {
      const response = await api.createRuntime(name.trim());
      setCreating(false); setName(""); setCreated(response); setCopyNotice(null); setTokenStartError(null);
      setServerUrl(defaultLocalServerUrl); setConcurrency("2"); setBinaryPath("");
      await refresh();
    } catch (requestError) { setCreateError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function rename(event: FormEvent) {
    event.preventDefault();
    if (!editing || !renamed.trim()) return;
    setBusy(true); setActionError(null);
		try { await api.configureRuntime(editing.id, { name: renamed.trim(), pool_id: editedPoolId.trim() }); setEditing(null); await refresh(); }
    catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function remove() {
    if (!removing) return;
    setBusy(true); setActionError(null);
    try { await api.deleteRuntime(removing.id); setRemoving(null); await refresh(); }
    catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  function acknowledgeCreatedToken() {
    setCreated(null);
    setCopyNotice(null);
    setTokenStartError(null);
  }

  async function startCreatedRuntime(event: FormEvent) {
    event.preventDefault();
    if (!desktopLocalRuntime || !created || localStartPending.current || localAction || localInspecting || !localStatus || localStatus.running) return;
    const normalizedUrl = normalizedServerUrl(serverUrl);
    if (!normalizedUrl.value) {
      setTokenStartError(normalizedUrl.error || "服务端地址无效");
      return;
    }
    const parsedConcurrency = Number(concurrency);
    if (!Number.isInteger(parsedConcurrency) || parsedConcurrency < 1) {
      setTokenStartError("并发数必须是大于或等于 1 的整数");
      return;
    }

    localStartPending.current = true;
    setLocalAction("start"); setTokenStartError(null); setLocalError(null); setLocalNotice(null);
    try {
      const status = await platform.startLocalRuntime({
        serverUrl: normalizedUrl.value,
        runtimeToken: created.token,
        concurrency: parsedConcurrency,
        ...(binaryPath.trim() ? { binaryPath: binaryPath.trim() } : {}),
      });
      setLocalStatus(normalizedStatus(status));
      setCreated(null); setCopyNotice(null); setTokenStartError(null);
      setLocalNotice("本机运行节点进程已启动；节点成功连接后，请点击「刷新」查看在线状态");
      await inspectLocal();
    } catch {
      setTokenStartError("启动本机运行节点失败。请检查服务端地址、并发数与运行程序路径后重试。");
      await inspectLocal();
    } finally { localStartPending.current = false; setLocalAction(null); }
  }

  async function stopLocalRuntime() {
    if (!desktopLocalRuntime || !localStatus?.running || localAction || localInspecting) return;
    setConfirmStopLocal(false);
    setLocalAction("stop"); setLocalError(null); setLocalNotice(null);
    try {
      setLocalStatus(normalizedStatus(await platform.stopLocalRuntime()));
      setLocalNotice("本机运行节点进程已停止；稍后点击「刷新」确认列表中的在线状态");
    } catch {
      setLocalError("停止本机运行节点失败，请刷新进程状态后重试。");
    } finally {
      await inspectLocal();
      setLocalAction(null);
    }
  }

  const localStateLabel = localInspecting && !localStatus ? "正在检查" : localStatus?.running ? "运行中" : localStatus ? "已停止" : "状态未知";
  const startDisabled = !!localAction || localInspecting || !localStatus || localStatus.running || !serverUrl.trim() || !concurrency.trim();
  const installServerUrl = normalizedServerUrl(serverUrl).value || defaultLocalServerUrl;
  const posixInstallCommand = created
    ? `WEAVE_RUNTIME_TOKEN=${shellSingleQuote(created.token)} sh -c 'curl -fsSL "$1/install.sh" | sh -s -- --server "$1" --token-env WEAVE_RUNTIME_TOKEN' sh ${shellSingleQuote(installServerUrl)}`
    : "";
  const powershellInstallCommand = created
    ? `iex "& { $(irm ${installServerUrl}/install.ps1) } -Server ${powershellSingleQuote(installServerUrl)} -Token ${powershellSingleQuote(created.token)}"`
    : "";

  const modalActions = (children: ReactNode) => <div className="runtime-modal-actions">{children}</div>;
  const copyText = async (value: string, success: string, failure: string) => {
    try {
      await navigator.clipboard.writeText(value);
      setCopyNotice(success);
    } catch {
      setCopyNotice(failure);
    }
  };

  return <div className="page operations-page runtime-page">
    <header className="page-header">
      <div><p className="eyebrow">Weave Runtime</p><h1>多运行时管理</h1><p>注册和管理在不同机器上执行任务的运行节点，节点连接后自动上报引擎能力与在线状态</p></div>
      <div className="runtime-page-actions">
        {user && <span title={user.username}>{user.username}</span>}
        <Button variant="ghost" onClick={() => void logout()}><LogOut size={16} aria-hidden="true" /> 退出</Button>
        <Button variant="primary" onClick={() => setCreating(true)}><Plus size={16} aria-hidden="true" /> 创建运行节点</Button>
      </div>
    </header>

    {desktopLocalRuntime && <div role="region" aria-labelledby="runtime-local-heading"><Card
      className="runtime-local-card"
      header={<div className="runtime-local-heading"><span className="runtime-local-icon"><MonitorCog size={20} aria-hidden="true" /></span><div><h2 id="runtime-local-heading">此电脑上的运行节点进程</h2><p>这里只显示此电脑上的进程状态，不代表列表中节点的在线状态</p></div><Badge tone={localStatus?.running ? "success" : "neutral"}>{localStateLabel}</Badge></div>}
      footer={<div className="runtime-local-actions"><Button variant="ghost" loading={localInspecting} disabled={!!localAction} onClick={() => { setLocalError(null); void inspectLocal(); }}>{!localInspecting && <RefreshCw size={16} aria-hidden="true" />}{localInspecting ? "正在检查" : "刷新本机状态"}</Button><Button variant="danger" loading={localAction === "stop"} disabled={!!localAction || localInspecting || !localStatus?.running} onClick={() => setConfirmStopLocal(true)}>{localAction !== "stop" && <Square size={14} aria-hidden="true" />}{localAction === "stop" ? "正在停止" : "停止本机进程"}</Button></div>}
    >
      <dl className="runtime-local-facts"><div><dt>本机进程</dt><dd>{localStatus?.running ? "运行中" : localStatus ? "已停止" : "未知"}</dd></div><div><dt>进程 ID</dt><dd>{localStatus?.pid ?? "—"}</dd></div><div><dt>启动时间</dt><dd>{localStartedAt(localStatus?.startedAt)}</dd></div></dl>
      {localStatus?.lastError && <div className="runtime-local-last-error"><strong>最近一次本机进程错误</strong><p>{localStatus.lastError}</p></div>}
      {localNotice && <p className="runtime-local-notice" role="status">{localNotice}</p>}
      {localError && <div className="runtime-local-error"><ErrorNotice message={localError} /></div>}
    </Card></div>}

    <div className="runtime-toolbar"><Button variant="ghost" size="small" onClick={() => void refresh()}><RefreshCw size={16} aria-hidden="true" /> 刷新</Button><span>{runtimes.length} 个运行节点</span></div>
    {error && <ErrorNotice message={error} onRetry={() => void refresh()} />}
    {actionError && <ErrorNotice message={actionError} />}

    {loading && !runtimes.length ? <LoadingView label="正在读取运行节点" /> : !runtimes.length ? <Card className="control-empty"><Server size={28} aria-hidden="true" /><h2>还没有运行节点</h2><p>点击「创建运行节点」注册第一个节点；令牌仅显示一次，请及时保存，节点连接后自动上报引擎</p></Card> : <div className="runtime-table-wrap">
      <table className="runtime-table">
        <caption className="sr-only">运行节点列表</caption>
        <thead><tr><th scope="col">名称</th><th scope="col">状态与容量</th><th scope="col">引擎与认证</th><th scope="col">最近连接</th><th scope="col">创建时间</th><th scope="col">操作</th></tr></thead>
        <tbody>{runtimes.map((runtime) => <tr key={runtime.id}>
			<th scope="row" data-label="名称"><strong>{runtime.name}</strong>{runtime.pool_id ? <small>已加入故障转移组</small> : null}</th>
			<td data-label="状态与容量"><div className="runtime-status"><Badge tone={runtime.health_status === "healthy" || runtime.health_status === "busy" ? "success" : runtime.health_status === "degraded" ? "warning" : "neutral"}>{runtime.health_status === "busy" ? "繁忙" : runtime.health_status === "degraded" ? "降级" : runtime.health_status === "quarantined" ? "已隔离" : runtime.online ? "健康" : "离线"}</Badge><small>{runtime.active_slots}/{runtime.total_slots} 槽位使用中</small>{runtime.consecutive_infra_failures > 0 && <small>连续执行异常：{runtime.consecutive_infra_failures} 次</small>}{runtime.last_failure_reason && <small>最近一次执行出现异常</small>}<small>{runtime.enabled ? "已启用" : "已停用"}</small>{runtime.revoked_at ? <small>已撤销：{dateTime.format(new Date(runtime.revoked_at))}</small> : null}{runtime.deleted_at ? <small>已删除：{dateTime.format(new Date(runtime.deleted_at))}</small> : null}</div></td>
          <td data-label="引擎与认证"><div className="runtime-engine-facts">{runtime.engines.length ? runtime.engines.map((engine) => {
            const capability = runtime.engine_capabilities?.[engine];
            return <details key={engine}><summary>{engineLabel(engine)} · {capability ? authModeLabels[capability.auth_mode] || "认证方式未知" : "旧节点未上报详情"}</summary>{capability && <dl><div><dt>程序</dt><dd><code>{capability.binary_path}</code></dd></div><div><dt>版本</dt><dd>{capability.binary_version}</dd></div><div><dt>连接</dt><dd>{endpointClassLabels[capability.endpoint_class] || "外部端点"}</dd></div><div><dt>协议</dt><dd>{capability.protocol_version}</dd></div></dl>}</details>;
          }) : "尚未上报"}</div></td>
                    <td data-label="最近连接"><time dateTime={runtime.last_heartbeat_at || undefined}>{runtime.last_heartbeat_at ? dateTime.format(new Date(runtime.last_heartbeat_at)) : "尚未连接"}</time></td>
          <td data-label="创建时间"><time dateTime={runtime.created_at}>{dateTime.format(new Date(runtime.created_at))}</time></td>
			<td data-label="操作"><div className="runtime-row-actions"><Button variant="ghost" size="small" aria-label={`配置 ${runtime.name}`} title="配置名称与迁移 Pool" disabled={busy} onClick={() => { setEditing(runtime); setRenamed(runtime.name); setEditedPoolId(runtime.pool_id || ""); setActionError(null); }}><Pencil size={16} aria-hidden="true" /></Button><Button variant="danger" size="small" aria-label={`删除 ${runtime.name}`} title="删除" disabled={busy} onClick={() => { setActionError(null); setRemoving(runtime); }}><Trash2 size={16} aria-hidden="true" /></Button></div></td>
        </tr>)}</tbody>
      </table>
    </div>}

    <Modal open={creating} title="创建运行节点" description="只需填写名称；系统生成的令牌仅在创建后显示一次，请及时保存" onClose={() => { if (!busy) { setCreating(false); setCreateError(null); } }} footer={modalActions(<><Button disabled={busy} onClick={() => setCreating(false)}>取消</Button><Button variant="primary" type="submit" form="runtime-create" loading={busy} disabled={!name.trim()}>{busy ? "正在创建" : "创建"}</Button></>)}>
      <form id="runtime-create" className="form-stack" onSubmit={(event) => void create(event)}><Field label="名称">{(control) => <input {...control} autoFocus value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：开发机运行节点" />}</Field>{createError && <ErrorNotice message={createError} />}</form>
    </Modal>

    <Modal open={!!editing} title="配置运行节点" description="加入同一故障转移组的同引擎节点，会在自动模式下承接故障迁移。" onClose={() => { if (!busy) setEditing(null); }} footer={modalActions(<><Button disabled={busy} onClick={() => setEditing(null)}>取消</Button><Button variant="primary" type="submit" form="runtime-rename" loading={busy} disabled={!renamed.trim()}>{busy ? "正在保存" : "保存"}</Button></>)}>
		<form id="runtime-rename" className="form-stack" onSubmit={(event) => void rename(event)}><Field label="名称">{(control) => <input {...control} autoFocus value={renamed} onChange={(event) => setRenamed(event.target.value)} />}</Field><Field label="故障转移组" help="加入同一组的同引擎节点可自动承接故障迁移；留空表示不参与。">{(control) => <input {...control} value={editedPoolId} maxLength={80} onChange={(event) => setEditedPoolId(event.target.value)} placeholder="留空：不加入 Pool" />}</Field>{actionError && <ErrorNotice message={actionError} />}</form>
    </Modal>

    <Modal open={!!created} title="保存运行节点令牌" description="令牌原文仅显示这一次；请立即复制并妥善保管，确认后再关闭" onClose={() => undefined} footer={modalActions(<><Button disabled={localAction === "start"} onClick={acknowledgeCreatedToken}>我已安全保存</Button>{desktopLocalRuntime && <Button variant="primary" type="submit" form="runtime-local-start" loading={localAction === "start"} disabled={startDisabled}>{localAction !== "start" && <Play size={16} aria-hidden="true" />}{localAction === "start" ? "正在启动" : "在此电脑启动"}</Button>}</>)}>
      <div className="runtime-token">
        <p>请立即复制并保存；此窗口关闭后无法再次读取。</p>
        <code>{created?.token}</code>
        <Button disabled={localAction === "start"} onClick={() => { if (created) void copyText(created.token, "令牌已复制到剪贴板", "复制失败，请手动选择上方令牌"); }}><Clipboard size={16} aria-hidden="true" /> 复制令牌</Button>
        <section className="runtime-install-commands" aria-label="运行节点安装命令">
          <div className="runtime-install-heading"><strong>页面注册命令</strong><p>优先使用这里的命令安装并注册 Runtime；脚本会自动下载二进制、写入仅本人可读的令牌文件，并注册后台进程。</p></div>
          <div className="runtime-command-row">
            <label htmlFor="runtime-install-posix">macOS / Linux · 一键安装</label>
            <input id="runtime-install-posix" readOnly value={posixInstallCommand} onFocus={(event) => event.currentTarget.select()} />
            <Button disabled={!created || localAction === "start"} onClick={() => void copyText(posixInstallCommand, "macOS / Linux 安装命令已复制", "复制失败，请手动选择命令")}><Clipboard size={16} aria-hidden="true" /> 复制</Button>
          </div>
          <div className="runtime-command-row">
            <label htmlFor="runtime-install-windows">Windows · PowerShell 一键安装</label>
            <input id="runtime-install-windows" readOnly value={powershellInstallCommand} onFocus={(event) => event.currentTarget.select()} />
            <Button disabled={!created || localAction === "start"} onClick={() => void copyText(powershellInstallCommand, "Windows 安装命令已复制", "复制失败，请手动选择命令")}><Clipboard size={16} aria-hidden="true" /> 复制</Button>
          </div>
          <p className="runtime-install-note">`--server` 使用当前填写的服务端地址；如果目标机器访问不到 localhost，请改成 Weave 主机的局域网 IP 或域名。高级用户也可以下载对应平台二进制后运行：weave runtime --server &lt;地址&gt; --runtime-token &lt;令牌&gt;。</p>
        </section>
        {copyNotice && <p role="status">{copyNotice}</p>}
        {desktopLocalRuntime && <form id="runtime-local-start" className="runtime-start-form" onSubmit={(event) => void startCreatedRuntime(event)}><div className="runtime-start-heading"><strong>在此电脑启动</strong><p>这是桌面便捷启动；业务验收以页面注册命令为准</p></div>{localStatus?.running && <div className="notice notice--warning"><span>此电脑已有运行节点进程在运行。请先关闭本窗口并停止该进程。</span></div>}<Field label="服务端地址">{(control) => <input {...control} type="url" required value={serverUrl} disabled={!!localAction || !!localStatus?.running} onChange={(event) => setServerUrl(event.target.value)} placeholder={fallbackLocalServerUrl} autoComplete="off" />}</Field><div className="runtime-start-grid"><Field label="并发数" help="此电脑同时执行的任务数上限">{(control) => <input {...control} type="number" required min={1} step={1} value={concurrency} disabled={!!localAction || !!localStatus?.running} onChange={(event) => setConcurrency(event.target.value)} />}</Field><Field label="运行程序路径（可选）">{(control) => <input {...control} value={binaryPath} disabled={!!localAction || !!localStatus?.running} onChange={(event) => setBinaryPath(event.target.value)} placeholder="留空则使用系统已安装的 weave" autoComplete="off" />}</Field></div>{tokenStartError && <ErrorNotice message={tokenStartError} />}</form>}
      </div>
    </Modal>

    <ConfirmModal
      open={!!removing}
      title="删除运行节点"
      description="删除后该节点的令牌立即失效，无法恢复。"
      busy={busy}
      onConfirm={() => void remove()}
      onClose={() => setRemoving(null)}
    ><p className="confirm-copy">确定删除运行节点「{removing?.name}」？</p>{actionError && <ErrorNotice message={actionError} />}</ConfirmModal>

    <ConfirmModal
      open={confirmStopLocal}
      title="停止本机运行节点"
      description="正在这台电脑上执行的任务会被中断。"
      confirmLabel="停止进程"
      busy={localAction === "stop"}
      onConfirm={() => void stopLocalRuntime()}
      onClose={() => setConfirmStopLocal(false)}
    ><p className="confirm-copy">确定停止此电脑上的运行节点进程？</p></ConfirmModal>
  </div>;
}
