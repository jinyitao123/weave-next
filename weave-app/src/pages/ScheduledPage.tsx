import "../styles/scheduled.css";

import { useCallback, useEffect, useState, type FormEvent } from "react";
import { CalendarClock, Database, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import { api, apiErrorMessage, type AgentSchedule, type AgentScheduleInput, type ConnectorSchedule } from "../api";
import { useAuth } from "../auth/AuthContext";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Card } from "../ui/Card";
import { Field } from "../ui/Field";
import { ConfirmModal } from "../ui/ConfirmModal";
import { Modal } from "../ui/Modal";
import { ErrorNotice, LoadingView } from "../ui/StatusViews";
import { useWorkspace } from "../workspace/WorkspaceContext";
import {
  scheduleKindLabel,
  syncConflictLabel,
  syncFrequencyLabel,
  syncModeLabel,
  syncStrategyLabel,
} from "../workspace/labels";

type Tab = "agent" | "connector";
const emptyAgent: AgentScheduleInput = { agent: "", message: "", kind: "daily", time_of_day: "09:00", run_at: null, timezone: "Asia/Shanghai", enabled: true };
const emptyConnector: ConnectorSchedule = { source_url: "", display_name: "", sync_mode: "manual", frequency: "daily", time_of_day: "00:00", strategy: "full", conflict: "source", tables: [] };
const when = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });

export function ScheduledPage() {
  const { user } = useAuth();
  const workspace = useWorkspace();
  const [tab, setTab] = useState<Tab>("agent");
  const [agents, setAgents] = useState<AgentSchedule[]>([]);
  const [connectors, setConnectors] = useState<ConnectorSchedule[]>([]);
  const [loading, setLoading] = useState(true);
  const [agentError, setAgentError] = useState<string | null>(null);
  const [connectorError, setConnectorError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [agentEditor, setAgentEditor] = useState<AgentScheduleInput | null>(null);
  const [connectorEditor, setConnectorEditor] = useState<ConnectorSchedule | null>(null);
  const [tablesText, setTablesText] = useState("");
  const [removingAgent, setRemovingAgent] = useState<AgentSchedule | null>(null);
  const [removingConnector, setRemovingConnector] = useState<ConnectorSchedule | null>(null);
  const [busy, setBusy] = useState(false);
  const connectorWritable = user?.role === "admin" || user?.role === "owner";

  const refresh = useCallback(async () => {
    setLoading(true); setAgentError(null); setConnectorError(null);
    try {
      const [agentResult, connectorResult] = await Promise.allSettled([api.listAgentSchedules(), api.listConnectorSchedules()]);
      if (agentResult.status === "fulfilled") setAgents(agentResult.value);
      else { setAgents([]); setAgentError(apiErrorMessage(agentResult.reason)); }
      if (connectorResult.status === "fulfilled") setConnectors(connectorResult.value);
      else { setConnectors([]); setConnectorError(apiErrorMessage(connectorResult.reason)); }
    } finally { setLoading(false); }
  }, []);
  useEffect(() => { void refresh(); }, [refresh]);

  function editAgent(item?: AgentSchedule) {
    setActionError(null);
    setAgentEditor(item ? {
      id: item.id, agent: item.agent, message: item.message, kind: item.kind,
      time_of_day: item.time_of_day, run_at: item.run_at || null, timezone: item.timezone, enabled: item.enabled,
    } : { ...emptyAgent, agent: workspace.agents[0]?.name || "" });
  }

  async function editConnector(item?: ConnectorSchedule) {
    setActionError(null);
    if (!item) { setTablesText(""); setConnectorEditor({ ...emptyConnector, tables: [] }); return; }
    setBusy(true);
    try {
      const value = await api.getConnectorSchedule(item.source_url);
      setTablesText(value.tables.join("\n"));
      setConnectorEditor({ ...value, tables: [...value.tables] });
    } catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function saveAgent(event: FormEvent) {
    event.preventDefault(); if (!agentEditor) return;
    setBusy(true); setActionError(null);
    const payload: AgentScheduleInput = {
      ...agentEditor,
      time_of_day: agentEditor.kind === "daily" ? agentEditor.time_of_day : "",
      run_at: agentEditor.kind === "once" ? agentEditor.run_at : null,
      timezone: agentEditor.timezone.trim() || (agentEditor.kind === "once" ? "UTC" : ""),
    };
    try { await api.upsertAgentSchedule(payload); setAgentEditor(null); await refresh(); }
    catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function saveConnector(event: FormEvent) {
    event.preventDefault(); if (!connectorEditor || !connectorWritable) return;
    setBusy(true); setActionError(null);
    const payload: ConnectorSchedule = { source_url: connectorEditor.source_url.trim(), display_name: connectorEditor.display_name.trim(), sync_mode: connectorEditor.sync_mode, frequency: connectorEditor.frequency, time_of_day: connectorEditor.time_of_day, strategy: connectorEditor.strategy, conflict: connectorEditor.conflict, tables: tablesText.split("\n").map((value) => value.trim()).filter(Boolean) };
    try { await api.upsertConnectorSchedule(payload); setConnectorEditor(null); await refresh(); }
    catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function toggleAgent(item: AgentSchedule) {
    setBusy(true); setActionError(null);
    try {
      await api.upsertAgentSchedule({
        id: item.id, agent: item.agent, message: item.message, kind: item.kind,
        time_of_day: item.kind === "daily" ? item.time_of_day : "",
        run_at: item.kind === "once" ? item.run_at || null : null,
        timezone: item.timezone, enabled: !item.enabled,
      });
      await refresh();
    } catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function removeAgent() {
    if (!removingAgent) return;
    setBusy(true); setActionError(null);
    try { await api.deleteAgentSchedule(removingAgent.id); setRemovingAgent(null); await refresh(); }
    catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function removeConnector() {
    if (!removingConnector) return;
    setBusy(true); setActionError(null);
    try { await api.deleteConnectorSchedule(removingConnector.source_url); setRemovingConnector(null); await refresh(); }
    catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  return (
    <div className="page scheduled-page">
      <header className="shell-toolbar">
        <strong className="shell-toolbar__title">定时任务</strong>
        <div className="shell-toolbar__actions">
          <Button variant="ghost" size="small" onClick={() => void refresh()}><RefreshCw size={16} /> 重读</Button>
          {tab === "agent" ? <Button variant="primary" size="small" disabled={!workspace.agents.length} onClick={() => editAgent()}><Plus size={16} /> 创建</Button> : connectorWritable && <Button variant="primary" size="small" disabled={busy} onClick={() => void editConnector()}><Plus size={16} /> 创建</Button>}
        </div>
      </header>

      <div className="scheduled-tabs" role="tablist" aria-label="调度类型">
        <Button size="small" variant={tab === "agent" ? "secondary" : "ghost"} role="tab" aria-selected={tab === "agent"} onClick={() => setTab("agent")}><CalendarClock size={16} /> 分身定时</Button>
        <Button size="small" variant={tab === "connector" ? "secondary" : "ghost"} role="tab" aria-selected={tab === "connector"} onClick={() => setTab("connector")}><Database size={16} /> 数据源同步</Button>
      </div>

      {actionError && <ErrorNotice message={actionError} />}
      {loading ? <LoadingView label="正在读取定时任务" /> : tab === "agent" ? (
        <section className="scheduled-section">
          <header className="scheduled-section__header">
            <div><h2>分身定时任务</h2><p>到点后由对应分身自动执行你设定的消息。</p></div>
          </header>
          {agentError && <ErrorNotice message={agentError} onRetry={() => void refresh()} />}
          {!agentError && (!agents.length ? (
            <Card className="scheduled-empty">
              <CalendarClock size={28} />
              <h2>暂无定时任务</h2>
              <p>创建后到达设定时间会自动执行。</p>
            </Card>
          ) : (
            <div className="scheduled-list">
              {agents.map((item) => (
                <Card className="scheduled-item scheduled-item--agent" padding="compact" key={item.id}>
                  <div className="scheduled-identity">
                    <strong>{workspace.agents.find((agent) => agent.name === item.agent)?.display_name || item.agent}</strong>
                    <small>{item.message || "空消息"}</small>
                  </div>
                  <div className="scheduled-fact"><Badge tone="accent">{scheduleKindLabel(item.kind)}</Badge><span>{item.kind === "daily" ? `每天 ${item.time_of_day} · ${item.timezone}` : `${item.run_at ? when.format(new Date(item.run_at)) : "未设置执行时间"} · ${item.timezone}`}</span></div>
                  <span className="scheduled-last-run">{item.last_run_at ? `上次 ${when.format(new Date(item.last_run_at))}` : "尚未执行"}</span>
                  <label className="scheduled-switch"><input type="checkbox" checked={item.enabled} disabled={busy} onChange={() => void toggleAgent(item)} /><span>{item.enabled ? "启用" : "停用"}</span></label>
                  <div className="scheduled-actions">
                    <Button variant="ghost" size="small" aria-label="编辑" title="编辑" disabled={busy} onClick={() => editAgent(item)}><Pencil size={16} /></Button>
                    <Button variant="danger" size="small" aria-label="删除" title="删除" disabled={busy} onClick={() => { setActionError(null); setRemovingAgent(item); }}><Trash2 size={16} /></Button>
                  </div>
                </Card>
              ))}
            </div>
          ))}
        </section>
      ) : (
        <section className="scheduled-section">
          <header className="scheduled-section__header">
            <div><h2>数据源同步</h2><p>按计划把外部数据源的内容同步进来，供分身使用。</p></div>
          </header>
          {!connectorWritable && <div className="scheduled-readonly" role="status"><Badge tone="warning">只读</Badge><p>当前身份不是管理员或所有者，只能查看。</p></div>}
          {connectorError && <ErrorNotice message={connectorError} onRetry={() => void refresh()} />}
          {!connectorError && (!connectors.length ? (
            <Card className="scheduled-empty">
              <Database size={28} />
              <h2>{connectorWritable ? "暂无数据源同步" : "数据源配置不可读取"}</h2>
              <p>{connectorWritable ? "创建时填写数据源地址和要同步的表。" : "当前账号没有读取数据源配置的权限。"}</p>
            </Card>
          ) : (
            <div className="scheduled-list">
              {connectors.map((item) => (
                <Card className="scheduled-item scheduled-item--connector" padding="compact" key={item.source_url}>
                  <div className="scheduled-identity"><strong>{item.display_name || item.source_url}</strong><small>{item.source_url}</small></div>
                  <div className="scheduled-fact"><Badge tone="accent">{syncModeLabel(item.sync_mode)}</Badge><span>{syncFrequencyLabel(item.frequency)} · {item.time_of_day}</span></div>
                  <span>{syncStrategyLabel(item.strategy)} · {syncConflictLabel(item.conflict)} · {item.tables.length} 张表</span>
                  <span>{item.updated_at ? when.format(new Date(item.updated_at)) : "尚无更新时间"}</span>
                  <div className="scheduled-actions">
                    <Button variant="ghost" size="small" aria-label="编辑" title="编辑" disabled={!connectorWritable || busy} onClick={() => void editConnector(item)}><Pencil size={16} /></Button>
                    <Button variant="danger" size="small" aria-label="删除" title="删除" disabled={!connectorWritable || busy} onClick={() => { setActionError(null); setRemovingConnector(item); }}><Trash2 size={16} /></Button>
                  </div>
                </Card>
              ))}
            </div>
          ))}
        </section>
      )}

      <Modal open={!!agentEditor} title={agentEditor?.id ? "编辑定时任务" : "创建定时任务"} onClose={() => { if (!busy) setAgentEditor(null); }} footer={<><Button disabled={busy} onClick={() => setAgentEditor(null)}>取消</Button><Button variant="primary" type="submit" form="agent-schedule-form" disabled={busy}>保存</Button></>}>
        <form id="agent-schedule-form" className="scheduled-form" onSubmit={(event) => void saveAgent(event)}>
          {agentEditor && <>
            <Field label="分身">{(control) => <select {...control} value={agentEditor.agent} onChange={(event) => setAgentEditor({ ...agentEditor, agent: event.target.value })}>{workspace.agents.map((agent) => <option key={agent.name} value={agent.name}>{agent.display_name || agent.name}</option>)}</select>}</Field>
            <Field label="消息">{(control) => <textarea {...control} rows={3} value={agentEditor.message} onChange={(event) => setAgentEditor({ ...agentEditor, message: event.target.value })} />}</Field>
            <Field label="类型">{(control) => <select {...control} value={agentEditor.kind} onChange={(event) => setAgentEditor({ ...agentEditor, kind: event.target.value as AgentScheduleInput["kind"], time_of_day: event.target.value === "daily" ? agentEditor.time_of_day || "09:00" : "", run_at: event.target.value === "once" ? agentEditor.run_at : null })}><option value="daily">每天</option><option value="once">单次</option></select>}</Field>
            {agentEditor.kind === "daily" ? <Field label="每日时间">{(control) => <input {...control} type="time" required value={agentEditor.time_of_day} onChange={(event) => setAgentEditor({ ...agentEditor, time_of_day: event.target.value })} />}</Field> : <Field label="执行时间">{(control) => <input {...control} type="datetime-local" required value={agentEditor.run_at ? agentEditor.run_at.slice(0, 16) : ""} onChange={(event) => setAgentEditor({ ...agentEditor, run_at: event.target.value ? new Date(event.target.value).toISOString() : null })} />}</Field>}
            <Field label="时区" help="例如 Asia/Shanghai、UTC。">{(control) => <input {...control} required value={agentEditor.timezone} onChange={(event) => setAgentEditor({ ...agentEditor, timezone: event.target.value })} placeholder="Asia/Shanghai" />}</Field>
            <Field label="启用">{(control) => <input {...control} className="scheduled-checkbox" type="checkbox" checked={agentEditor.enabled} onChange={(event) => setAgentEditor({ ...agentEditor, enabled: event.target.checked })} />}</Field>
          </>}
        </form>
      </Modal>

      <Modal open={!!connectorEditor} title={connectorEditor?.updated_at ? "编辑数据源同步" : "创建数据源同步"} onClose={() => { if (!busy) setConnectorEditor(null); }} footer={<><Button disabled={busy} onClick={() => setConnectorEditor(null)}>取消</Button><Button variant="primary" type="submit" form="connector-schedule-form" disabled={busy}>保存</Button></>}>
        <form id="connector-schedule-form" className="scheduled-form" onSubmit={(event) => void saveConnector(event)}>
          {connectorEditor && <>
            <Field label="数据源地址">{(control) => <input {...control} required readOnly={!!connectorEditor.updated_at} value={connectorEditor.source_url} onChange={(event) => setConnectorEditor({ ...connectorEditor, source_url: event.target.value })} />}</Field>
            <Field label="显示名称">{(control) => <input {...control} value={connectorEditor.display_name} onChange={(event) => setConnectorEditor({ ...connectorEditor, display_name: event.target.value })} />}</Field>
            <div className="scheduled-form-grid">
              <Field label="同步方式">{(control) => <select {...control} value={connectorEditor.sync_mode} onChange={(event) => setConnectorEditor({ ...connectorEditor, sync_mode: event.target.value as ConnectorSchedule["sync_mode"] })}><option value="batch">定时批量</option><option value="event">事件触发</option><option value="manual">手动同步</option></select>}</Field>
              <Field label="频率">{(control) => <select {...control} value={connectorEditor.frequency} onChange={(event) => setConnectorEditor({ ...connectorEditor, frequency: event.target.value as ConnectorSchedule["frequency"] })}><option value="hourly">每小时</option><option value="daily">每天</option><option value="weekly">每周</option></select>}</Field>
              <Field label="时间">{(control) => <input {...control} type="time" value={connectorEditor.time_of_day} onChange={(event) => setConnectorEditor({ ...connectorEditor, time_of_day: event.target.value })} />}</Field>
              <Field label="同步范围">{(control) => <select {...control} value={connectorEditor.strategy} onChange={(event) => setConnectorEditor({ ...connectorEditor, strategy: event.target.value as ConnectorSchedule["strategy"] })}><option value="full">全量</option><option value="incremental">增量</option></select>}</Field>
              <Field label="冲突处理">{(control) => <select {...control} value={connectorEditor.conflict} onChange={(event) => setConnectorEditor({ ...connectorEditor, conflict: event.target.value as ConnectorSchedule["conflict"] })}><option value="source">以来源为准</option><option value="local">以本地为准</option><option value="mark">仅标记冲突</option></select>}</Field>
            </div>
            <Field label="同步的表（每行一个）">{(control) => <textarea {...control} rows={5} value={tablesText} onChange={(event) => setTablesText(event.target.value)} />}</Field>
          </>}
        </form>
      </Modal>
      <ConfirmModal
        open={!!removingAgent}
        title="删除定时任务"
        description="删除后该分身不会再按此计划自动执行。"
        busy={busy}
        onConfirm={() => void removeAgent()}
        onClose={() => setRemovingAgent(null)}
      ><p className="confirm-copy">删除「{removingAgent ? (workspace.agents.find((agent) => agent.name === removingAgent.agent)?.display_name || removingAgent.agent) : ""}」的{removingAgent ? scheduleKindLabel(removingAgent.kind) : ""}定时任务？</p>{actionError && <ErrorNotice message={actionError} />}</ConfirmModal>
      <ConfirmModal
        open={!!removingConnector}
        title="删除数据源同步"
        description="删除后不会再从该数据源同步内容，已同步的内容保留。"
        busy={busy}
        onConfirm={() => void removeConnector()}
        onClose={() => setRemovingConnector(null)}
      ><p className="confirm-copy">删除数据源「{removingConnector?.display_name || removingConnector?.source_url}」的同步配置？</p>{actionError && <ErrorNotice message={actionError} />}</ConfirmModal>
    </div>
  );
}
