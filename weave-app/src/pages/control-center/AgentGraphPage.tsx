import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import {
  AlertTriangle,
  ArrowDown,
  ArrowRight,
  ArrowUp,
  Bot,
  GitBranch,
  Link2,
  ListOrdered,
  Pencil,
  Plus,
  RefreshCw,
  ScanSearch,
  Send,
  Trash2,
} from "lucide-react";
import { NavLink } from "react-router-dom";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type AgentChannel,
  type AgentLink,
  type AgentLinkType,
  type AgentRecord,
  type AgentTopologyResponse,
  type OrphanWorker,
  type PromptPreviewResponse,
} from "../../api";
import { useAuth } from "../../auth/AuthContext";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { ConfirmModal } from "../../ui/ConfirmModal";
import { Field } from "../../ui/Field";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";

class ContextValidationError extends Error {}

const linkTypeCopy: Record<AgentLinkType, string> = {
  peer: "对等协作 (peer)",
  manages: "管理与被管理 (manages)",
};

function agentLabel(agent: AgentRecord | undefined) {
  if (!agent) return "未知或已删除的智能体";
  return `${agent.display_name || agent.name} · ${agent.name}`;
}

function agentDisplayName(agent: AgentRecord | undefined) {
  return agent?.display_name || agent?.name || "未知或已删除的智能体";
}

function actionMessage(error: unknown, action: string) {
  const normalized = normalizeThrownError(error);
  if (normalized.code === "team_roster_write_required" || normalized.message.includes("team authorization must be changed through the team roster")) {
    return "该关系由团队管理，请前往「团队」页修改。";
  }
  if (normalized.status === 400) return `${action}未完成，请检查填写内容后重试。`;
  if (normalized.status === 404) return `${action}未完成，相关内容不存在或已被删除。`;
  if (normalized.status === 409) return `${action}发生冲突，请刷新后重试。`;
  return apiErrorMessage(normalized);
}

export function AgentGraphPage() {
  const [agents, setAgents] = useState<AgentRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [forbidden, setForbidden] = useState(false);

  const loadAgents = useCallback(async () => {
    setLoading(true); setError(null); setForbidden(false);
    try { setAgents((await api.listAgents()).filter((agent) => !agent.deleted)); }
    catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setForbidden(normalized.status === 403); setError(apiErrorMessage(normalized)); setAgents([]);
    } finally { setLoading(false); }
  }, []);

  useEffect(() => { void loadAgents(); }, [loadAgents]);

  return <section className="control-content agent-graph-page" aria-labelledby="agent-graph-heading">
    <div className="control-section-heading"><div><h2 id="agent-graph-heading">兼容关系（旧版）</h2><p>早期通用智能体关系，仅用于团队之外的存量场景；团队授权请在「团队」页管理。</p></div><Button variant="ghost" loading={loading} onClick={() => void loadAgents()}>{!loading && <RefreshCw size={16} aria-hidden="true" />}{loading ? "正在刷新" : "刷新"}</Button></div>
    {error && !forbidden && <ErrorNotice message={error} onRetry={() => void loadAgents()} />}
    {forbidden ? <div className="empty-surface control-forbidden"><Bot size={28} /><h2>无法查看智能体</h2><p>当前账号没有查看智能体的权限。</p></div> : loading && !agents.length ? <LoadingView label="正在读取智能体" /> : !agents.length ? <div className="empty-surface"><Bot size={28} /><h2>还没有智能体</h2><p>请联系管理员创建智能体后再配置关系、频道和运行信息。</p></div> : <LinksPanel agents={agents} />}
  </section>;
}

function LinksPanel({ agents }: { agents: AgentRecord[] }) {
  const { user } = useAuth();
  const canWrite = user?.role === "admin";
  const [links, setLinks] = useState<AgentLink[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [sourceID, setSourceID] = useState(agents[0]?.id || "");
  const [targetID, setTargetID] = useState(agents.find((agent) => agent.id !== agents[0]?.id)?.id || "");
  const [type, setType] = useState<AgentLinkType>("manages");
  const [instruction, setInstruction] = useState("");
  const [editingID, setEditingID] = useState<string | null>(null);
  const [editInstruction, setEditInstruction] = useState("");
  const [removingLink, setRemovingLink] = useState<AgentLink | null>(null);
  const [orphans, setOrphans] = useState<OrphanWorker[] | null>(null);
  const agentsByID = useMemo(() => new Map(agents.map((agent) => [agent.id, agent])), [agents]);
  const [orphanIDs, setOrphanIDs] = useState<Set<string> | null>(null);
  useEffect(() => {
    let cancelled = false;
    void Promise.all(agents.map(async (agent) => {
      try {
        const memberships = await api.getAgentTeamMemberships(agent.name);
        return { id: agent.id, orphan: memberships.lead_of.length === 0 && memberships.worker_of.length === 0 };
      } catch { return { id: agent.id, orphan: false }; }
    })).then((results) => { if (!cancelled) setOrphanIDs(new Set(results.filter((result) => result.orphan).map((result) => result.id))); });
    return () => { cancelled = true; };
  }, [agents]);
  const orphanAgents = useMemo(() => agents.filter((agent) => orphanIDs?.has(agent.id)), [agents, orphanIDs]);
  const targets = orphanAgents.filter((agent) => agent.id !== sourceID);

  const load = useCallback(async () => {
    setLoading(true); setError(null);
    try { setLinks(await api.listAgentLinks()); }
    catch (requestError) { setError(apiErrorMessage(requestError)); }
    finally { setLoading(false); }
  }, []);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => { if (orphanIDs && !orphanAgents.some((agent) => agent.id === sourceID)) setSourceID(orphanAgents[0]?.id || ""); }, [orphanIDs, orphanAgents, sourceID]);
  useEffect(() => { if (targetID === sourceID || !targets.some((agent) => agent.id === targetID)) setTargetID(targets[0]?.id || ""); }, [sourceID, targetID, targets]);

  async function create(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError(null);
    try {
      await api.createAgentLink({ source_agent_id: sourceID, target_agent_id: targetID, type, instruction });
      setInstruction(""); await load();
    } catch (requestError) { setError(actionMessage(requestError, "创建关系")); }
    finally { setBusy(false); }
  }

  async function saveInstruction(id: string) {
    setBusy(true); setError(null);
    try { await api.updateAgentLinkInstruction(id, editInstruction); setEditingID(null); await load(); }
    catch (requestError) { setError(actionMessage(requestError, "更新调度指引")); }
    finally { setBusy(false); }
  }

  async function remove() {
    if (!removingLink) return;
    setBusy(true); setError(null);
    try { await api.deleteAgentLink(removingLink.id); setRemovingLink(null); await load(); }
    catch (requestError) { setError(actionMessage(requestError, "删除关系")); }
    finally { setBusy(false); }
  }

  async function scanOrphans() {
    setBusy(true); setError(null);
    try { setOrphans((await api.scanOrphanWorkers()).orphans); }
    catch (requestError) { setError(actionMessage(requestError, "扫描孤立智能体")); }
    finally { setBusy(false); }
  }

  return <div className="agent-graph-stack">
    <div className="boundary-note"><AlertTriangle size={16} /><div><strong>团队关系</strong><p>团队成员关系由团队统一管理，请前往「团队」页修改。</p></div><NavLink className="ui-button ui-button--ghost" to="../teams">前往「团队」</NavLink></div>
    {error && <ErrorNotice message={error} onRetry={() => void load()} />}
    {canWrite && orphanIDs && orphanAgents.length >= 2 && <form className="link-create-form" onSubmit={create}>
      <Field label="来源智能体" help="仅列出团队之外的孤立智能体。">{(control) => <select {...control} value={sourceID} onChange={(event) => setSourceID(event.target.value)}>{orphanAgents.map((agent) => <option key={agent.id} value={agent.id}>{agentLabel(agent)}</option>)}</select>}</Field>
      <Field label="目标智能体" help="不能选择同一个智能体；同两个智能体之间的双向关系视为同一条。">{(control) => <select {...control} value={targetID} onChange={(event) => setTargetID(event.target.value)}>{targets.map((agent) => <option key={agent.id} value={agent.id}>{agentLabel(agent)}</option>)}</select>}</Field>
      <Field label="关系类型" help="管理关系从来源智能体指向目标智能体。">{(control) => <select {...control} value={type} onChange={(event) => setType(event.target.value as AgentLinkType)}><option value="manages">{linkTypeCopy.manages}</option></select>}</Field>
      <Field label="调度指引">{(control) => <input {...control} value={instruction} onChange={(event) => setInstruction(event.target.value)} placeholder="管理关系生效时，供来源智能体派发任务时参考" />}</Field>
      <Button variant="primary" type="submit" loading={busy} disabled={!sourceID || !targetID}>{!busy && <Plus size={16} aria-hidden="true" />}创建关系</Button>
    </form>}
    {canWrite && orphanIDs && orphanAgents.length < 2 && <p className="readonly-note">团队之外的孤立智能体不足两个，暂无可创建的兼容关系；团队成员关系请前往「团队」页管理。</p>}
    {!canWrite && <p className="readonly-note">当前账号可以查看智能体关系；创建、编辑、删除和孤立智能体扫描仅对管理员开放。</p>}
    <div className="link-list-heading"><div><h3>智能体关系</h3></div>{canWrite && <Button variant="ghost" loading={busy} onClick={() => void scanOrphans()}>{!busy && <ScanSearch size={16} aria-hidden="true" />}扫描孤立智能体</Button>}</div>
    {orphans && <section className="orphan-results"><strong>扫描结果 · {orphans.length}</strong>{orphans.length ? <ul>{orphans.map((orphan) => <li key={orphan.id}><span>{orphan.name}</span><code>{orphan.id}</code></li>)}</ul> : <p>没有发现孤立智能体。</p>}</section>}
    {loading ? <LoadingView label="正在读取智能体关系" /> : !links.length ? <div className="empty-surface graph-empty"><Link2 size={28} /><h2>还没有智能体关系</h2><p>{canWrite ? (orphanIDs && orphanAgents.length < 2 ? "团队之外的孤立智能体不足两个；团队成员关系请前往「团队」页管理。" : "请在上方选择两个孤立智能体并创建关系。") : "请联系管理员创建智能体关系。"}</p></div> : <div className="agent-link-list">{links.map((link) => {
      const editing = editingID === link.id;
      return <article key={link.id} className="agent-link-row"><div className="agent-link-path"><span><strong>{agentLabel(agentsByID.get(link.source_agent_id))}</strong><code>{link.source_agent_id}</code></span><span className="link-type"><ArrowRight size={16} /><em>{linkTypeCopy[link.type]}</em></span><span><strong>{agentLabel(agentsByID.get(link.target_agent_id))}</strong><code>{link.target_agent_id}</code></span></div>{editing ? <div className="link-edit-row"><Field label="调度指引">{(control) => <input {...control} autoFocus value={editInstruction} onChange={(event) => setEditInstruction(event.target.value)} />}</Field><Button variant="primary" loading={busy} onClick={() => void saveInstruction(link.id)}>保存</Button><Button loading={busy} onClick={() => setEditingID(null)}>取消</Button></div> : <p className="link-instruction">{link.instruction || "暂无调度指引"}</p>}{canWrite && !editing && <div className="row-actions"><button className="icon-button" type="button" aria-label="编辑调度指引" disabled={busy} onClick={() => { setEditingID(link.id); setEditInstruction(link.instruction); }}><Pencil size={16} /></button><button className="icon-button icon-button--danger" type="button" aria-label="删除智能体关系" disabled={busy} onClick={() => setRemovingLink(link)}><Trash2 size={16} /></button></div>}</article>;
    })}</div>}
    <ConfirmModal
      open={!!removingLink}
      title="删除兼容关系"
      description="删除后两者不再存在旧版调用关系，不可恢复。"
      busy={busy}
      onConfirm={() => void remove()}
      onClose={() => setRemovingLink(null)}
    ><p className="confirm-copy">删除「{removingLink ? agentDisplayName(agentsByID.get(removingLink.source_agent_id)) : ""}」与「{removingLink ? agentDisplayName(agentsByID.get(removingLink.target_agent_id)) : ""}」之间的关系？</p>{error && <ErrorNotice message={error} />}</ConfirmModal>
  </div>;
}


export function ChannelsPanel({ agents, agentName }: { agents: AgentRecord[]; agentName?: string }) {
  const [internalAgentName, setInternalAgentName] = useState(agents[0]?.name || "");
  const name = agentName ?? internalAgentName;
  const [channels, setChannels] = useState<AgentChannel[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [newName, setNewName] = useState("");
  const [editingID, setEditingID] = useState<string | null>(null);
  const [editingName, setEditingName] = useState("");
  const [removingChannel, setRemovingChannel] = useState<AgentChannel | null>(null);

  const load = useCallback(async (requestedName = name) => {
    if (!requestedName) return;
    setLoading(true); setError(null);
    try { setChannels(await api.listAgentChannels(requestedName)); }
    catch (requestError) { setError(apiErrorMessage(requestError)); setChannels([]); }
    finally { setLoading(false); }
  }, [name]);
  useEffect(() => { void load(name); }, [name, load]);

  async function create(event: FormEvent) {
    event.preventDefault(); setBusy(true); setError(null);
    try { await api.createAgentChannel(name, newName); setNewName(""); await load(); }
    catch (requestError) { setError(actionMessage(requestError, "创建频道")); }
    finally { setBusy(false); }
  }
  async function rename(id: string) {
    setBusy(true); setError(null);
    try { await api.renameAgentChannel(name, id, editingName); setEditingID(null); await load(); }
    catch (requestError) { setError(actionMessage(requestError, "重命名频道")); }
    finally { setBusy(false); }
  }
  async function remove() {
    if (!removingChannel) return;
    setBusy(true); setError(null);
    try { await api.deleteAgentChannel(name, removingChannel.id); setRemovingChannel(null); await load(); }
    catch (requestError) { setError(actionMessage(requestError, "删除频道")); }
    finally { setBusy(false); }
  }
  async function move(index: number, offset: -1 | 1) {
    const nextIndex = index + offset;
    if (nextIndex < 1 || nextIndex >= channels.length) return;
    const ordered = [...channels]; [ordered[index], ordered[nextIndex]] = [ordered[nextIndex], ordered[index]];
    setBusy(true); setError(null);
    try { await api.reorderAgentChannels(name, ordered.map((channel) => channel.id)); await load(); }
    catch (requestError) { setError(actionMessage(requestError, "保存频道顺序")); }
    finally { setBusy(false); }
  }

  return <div className="agent-graph-stack">{agentName === undefined && <div className="graph-agent-toolbar"><Field label="智能体">{(control) => <select {...control} value={internalAgentName} onChange={(event) => { setInternalAgentName(event.target.value); setEditingID(null); }}>{agents.map((agent) => <option key={agent.id} value={agent.name}>{agentLabel(agent)}</option>)}</select>}</Field></div>}
    <p className="contract-note">拥有智能体查看权限的账号都可以管理频道。默认频道始终排在第一位，且不能重命名或删除；调整顺序时会保存当前完整排序。</p>
    {error && <ErrorNotice message={error} onRetry={() => void load()} />}
    <form className="channel-create-form" onSubmit={create}><Field label="新频道名称">{(control) => <input {...control} value={newName} onChange={(event) => setNewName(event.target.value)} placeholder="例如：研究" />}</Field><Button variant="primary" type="submit" loading={busy} disabled={!newName.trim()}>{!busy && <Plus size={16} aria-hidden="true" />}创建</Button></form>
    {loading ? <LoadingView label="正在读取频道" /> : !channels.length ? <div className="empty-surface graph-empty"><ListOrdered size={28} /><h2>还没有频道</h2><p>请在上方创建频道；若默认频道缺失，请联系管理员检查配置。</p></div> : <ol className="channel-list">{channels.map((channel, index) => <li key={channel.id}><span className="channel-position">{channel.position}</span><div className="channel-copy">{editingID === channel.id ? <Field label="频道名称">{(control) => <input {...control} autoFocus value={editingName} onChange={(event) => setEditingName(event.target.value)} />}</Field> : <><strong>{channel.name}</strong><code>{channel.id}</code></>}</div>{channel.is_default ? <Badge tone="success">默认</Badge> : <Badge tone="neutral">自定义</Badge>}<div className="channel-actions">{editingID === channel.id ? <><Button variant="primary" loading={busy} onClick={() => void rename(channel.id)}>保存</Button><Button loading={busy} onClick={() => setEditingID(null)}>取消</Button></> : <><button className="icon-button" type="button" aria-label={`上移 ${channel.name}`} disabled={busy || index <= 1 || channel.is_default} onClick={() => void move(index, -1)}><ArrowUp size={16} /></button><button className="icon-button" type="button" aria-label={`下移 ${channel.name}`} disabled={busy || index === 0 || index >= channels.length - 1} onClick={() => void move(index, 1)}><ArrowDown size={16} /></button><button className="icon-button" type="button" aria-label={`重命名 ${channel.name}`} disabled={busy || channel.is_default} onClick={() => { setEditingID(channel.id); setEditingName(channel.name); }}><Pencil size={16} /></button><button className="icon-button icon-button--danger" type="button" aria-label={`删除 ${channel.name}`} disabled={busy || channel.is_default} onClick={() => setRemovingChannel(channel)}><Trash2 size={16} /></button></>}</div></li>)}</ol>}
    <ConfirmModal
      open={!!removingChannel}
      title="删除频道"
      description="删除后该频道不再接收消息，不可恢复。"
      busy={busy}
      onConfirm={() => void remove()}
      onClose={() => setRemovingChannel(null)}
    ><p className="confirm-copy">确定删除频道「{removingChannel?.name}」？</p>{error && <ErrorNotice message={error} />}</ConfirmModal>
  </div>;
}

function parseContext(text: string): Record<string, unknown> {
  const value: unknown = JSON.parse(text);
  if (!value || Array.isArray(value) || typeof value !== "object") throw new ContextValidationError("上下文必须是 JSON 对象，不能是数组、null 或单个值。");
  return value as Record<string, unknown>;
}

export function PromptPanel({ agents, agentName }: { agents: AgentRecord[]; agentName?: string }) {
  const [internalAgentName, setInternalAgentName] = useState(agents[0]?.name || "");
  const name = agentName ?? internalAgentName;
  const [message, setMessage] = useState("");
  const [profile, setProfile] = useState("");
  const [contextText, setContextText] = useState("{}");
  const [preview, setPreview] = useState<PromptPreviewResponse | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(event: FormEvent) {
    event.preventDefault(); setError(null); setBusy(true);
    try {
      const context = parseContext(contextText);
      setPreview(await api.previewAgentPrompt(name, { user_message: message, ...(profile ? { profile } : {}), ...(Object.keys(context).length ? { context } : {}) }));
    } catch (requestError) { setError(requestError instanceof SyntaxError ? "上下文不是有效的 JSON 对象。" : requestError instanceof ContextValidationError ? requestError.message : apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  return <div className="agent-graph-stack">{agentName === undefined && <div className="graph-agent-toolbar"><Field label="智能体">{(control) => <select {...control} value={internalAgentName} onChange={(event) => { setInternalAgentName(event.target.value); setPreview(null); setError(null); }}>{agents.map((agent) => <option key={agent.id} value={agent.name}>{agentLabel(agent)}</option>)}</select>}</Field></div>}
    <form className="prompt-preview-form" onSubmit={submit}>
      <div className="prompt-message"><Field label="用户消息">{(control) => <textarea {...control} rows={5} value={message} onChange={(event) => setMessage(event.target.value)} placeholder="用于匹配技能并组装提示词，不会发送给模型。" />}</Field></div>
      <Field label="用户画像（可选）" help="用于选择该智能体预先配置的用户画像。">{(control) => <input {...control} value={profile} onChange={(event) => setProfile(event.target.value)} placeholder="输入已配置的用户画像名称" />}</Field>
      <div className="prompt-context"><Field label="补充上下文（可选）" help="使用 JSON 对象提供本次预览需要的补充信息。">{(control) => <textarea {...control} className="code-input" rows={8} value={contextText} onChange={(event) => setContextText(event.target.value)} spellCheck={false} />}</Field></div>
      <div className="form-actions"><Button variant="primary" type="submit" loading={busy} disabled={!name}>{!busy && <Send size={16} aria-hidden="true" />}{busy ? "正在组装" : "生成预览"}</Button></div>
    </form>
    <p className="readonly-note">预览只会组装提示词、估算 Token 并显示匹配结果，不会调用模型或启动智能体。</p>
    {error && <ErrorNotice message={error} />}
    {preview && <section className="prompt-preview-result"><header><div><h3>最终提示词</h3><p>完整内容，未截断。</p></div><strong>{preview.tokens.toLocaleString("zh-CN")} Token（估算）</strong></header><dl><div><dt>已启用技能</dt><dd>{preview.active_skills.length ? preview.active_skills.join("、") : "无"}</dd></div><div><dt>已应用用户画像</dt><dd>{preview.profile_applied || "未应用"}</dd></div><div><dt>上下文字段</dt><dd>{preview.context_keys?.length ? preview.context_keys.join("、") : "无"}</dd></div></dl><pre>{preview.prompt || "（暂无提示词内容）"}</pre></section>}
  </div>;
}

export function TopologyPanel({ agents, agentName }: { agents: AgentRecord[]; agentName?: string }) {
  const [internalAgentName, setInternalAgentName] = useState(agents[0]?.name || "");
  const name = agentName ?? internalAgentName;
  const [topology, setTopology] = useState<AgentTopologyResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async (requestedName = name) => {
    if (!requestedName) return;
    setLoading(true); setError(null); setTopology(null);
    try { setTopology(await api.getAgentTopology(requestedName)); }
    catch (requestError) { setError(apiErrorMessage(requestError)); }
    finally { setLoading(false); }
  }, [name]);
  useEffect(() => { void load(name); }, [name, load]);

  return <div className="agent-graph-stack">{agentName === undefined && <div className="graph-agent-toolbar"><Field label="智能体">{(control) => <select {...control} value={internalAgentName} onChange={(event) => setInternalAgentName(event.target.value)}>{agents.map((agent) => <option key={agent.id} value={agent.name}>{agentLabel(agent)}</option>)}</select>}</Field></div>}
    <p className="contract-note">运行时拓扑根据当前模型服务、记忆、MCP 和智能体配置生成；若依赖不可用或配置有误，将显示具体错误。</p>
    {error && <ErrorNotice message={error} onRetry={() => void load()} />}
    {loading ? <LoadingView label="正在生成运行时拓扑" /> : topology && !topology.steps.length ? <div className="empty-surface graph-empty"><GitBranch size={28} /><h2>还没有运行步骤</h2><p>请检查该智能体的图配置，补充入口和运行步骤后刷新。当前入口：{topology.entry || "未设置"}</p></div> : topology && <section className="topology-result"><header><div><span>智能体</span><strong>{topology.agent}</strong></div><div><span>入口步骤</span><strong>{topology.entry || "（未设置）"}</strong></div><div><span>步骤数</span><strong>{topology.steps.length}</strong></div></header><ol className="topology-steps">{topology.steps.map((step, index) => <li key={`${step.name}-${index}`} className={step.name === topology.entry ? "is-entry" : undefined}><div className="topology-node"><span>{step.name === topology.entry ? "入口" : `步骤 ${index + 1}`}</span><strong>{step.name}</strong>{step.detail && <p>{step.detail}</p>}</div><ul className="topology-edges">{step.edges.length ? step.edges.map((edge, edgeIndex) => <li key={`${edge.to}-${edge.label || ""}-${edgeIndex}`}><ArrowRight size={14} /><span>{edge.label && <em>{edge.label}</em>}<strong>{edge.to || "终点"}</strong>{!edge.to && <small>未指定目标步骤</small>}</span></li>) : <li className="topology-no-edge">没有后续步骤</li>}</ul></li>)}</ol></section>}
  </div>;
}
