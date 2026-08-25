import { useCallback, useEffect, useMemo, useRef, useState, type Dispatch, type FormEvent, type ReactNode, type SetStateAction } from "react";
import { ArrowLeft, Plus, Trash2, X } from "lucide-react";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type AgentEngine,
  type AgentGraphStep,
  type AgentRecord,
  type AgentRole,
  type AgentRunSummaryResponse,
  type AgentTeamMembershipsResponse,
  type AgentWriteInput,
  type Runtime,
  type User,
} from "../../api";
import { useAuth } from "../../auth/AuthContext";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { Switch } from "../../ui/Switch";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { Link, useParams } from "react-router-dom";
import { MemoryConfiguration, MemoriesPanel, ProfilePanel, SlotsPanel } from "./AgentMemoryPage";
import { ChannelsPanel, PromptPanel, TopologyPanel } from "./AgentGraphPage";

const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
const engines: Array<{ value: AgentEngine; label: string }> = [
  { value: "", label: "平台内置执行" },
  { value: "opencode", label: "OpenCode" },
  { value: "codex", label: "Codex" },
  { value: "claude", label: "Claude Code" },
];
const graphStepTypes: AgentGraphStep["type"][] = ["chat", "llm_call", "llm_check", "yield", "transform", "builtin", "worker"];
const tabs = [
  ["identity", "身份"],
  ["prompt", "提示与规格"],
  ["tools", "工具与委托"],
  ["memory", "记忆与守卫"],
  ["execution", "执行与预算"],
  ["advanced", "高级"],
] as const;
export type EditorTab = (typeof tabs)[number][0];
type DetailTab = "configuration" | "memory" | "graph";
type MemoryTab = "memories" | "slots" | "profile";
type GraphTab = "topology" | "prompt" | "channels";
type StringItem = { id: number; value: string };
type PairItem = { id: number; key: string; value: string };
type ProfileItem = { id: number; name: string; systemAddition: string; greeting: string };
type SkillItem = { id: number; name: string; description: string; body: string; alwaysActive: boolean; scripts: StringItem[]; references: StringItem[] };
type MCPItem = { id: number; serverId: string; url: string; filter: StringItem[]; writeTools: StringItem[]; headers: PairItem[] };
type SubAgentItem = { id: number; name: string; description: string; routeKey: string };
type MemorySlotItem = { id: number; key: string; label: string; description: string };
type GraphStepItem = { id: number; name: string; type: AgentGraphStep["type"]; display: string; config: PairItem[]; next: string; conditionEnabled: boolean; conditionKey: string; trueStep: string; falseStep: string };

export interface AgentFormState {
  name: string;
  displayName: string;
  role: AgentRole;
  ownerUserId: string;
  engine: AgentEngine;
  runtimeId: string;
  model: string;
  systemPrompt: string;
  identityCore: string;
  identityExtended: string;
  identityRaw: string;
  profiles: ProfileItem[];
  skills: SkillItem[];
  healthCheck: string;
  specSubAgents: SubAgentItem[];
  specGraphType: string;
  mcpServers: MCPItem[];
  permissionAllow: StringItem[];
  permissionDeny: StringItem[];
  permissionAsk: StringItem[];
  subAgents: SubAgentItem[];
  memoryEnabled: boolean;
  memoryTopK: string;
  autoRemember: boolean;
  memoryScope: "tenant" | "user" | "session";
  memorySlots: MemorySlotItem[];
  guardEnabled: boolean;
  maxInputLen: string;
  blockedTerms: StringItem[];
  compactionEnabled: boolean;
  compactionThreshold: string;
  maxCostUSD: string;
  maxTokens: string;
  maxOutputTokens: string;
  stepBudget: string;
  maxToolRepeats: string;
  fallbackModels: StringItem[];
  fallbackRetries: string;
  outputSchema: string;
  graphType: string;
  graphEntry: string;
  graphSteps: GraphStepItem[];
  tags: StringItem[];
}

let rowSequence = 0;
function nextID() { rowSequence += 1; return rowSequence; }
function stringItems(values: string[] = []): StringItem[] { return values.map((value) => ({ id: nextID(), value })); }
function pairs(values: Record<string, string> = {}): PairItem[] { return Object.entries(values).map(([key, value]) => ({ id: nextID(), key, value })); }
function numberText(value?: number) { return value ? String(value) : ""; }
function cleanStrings(items: StringItem[]) { return items.map(({ value }) => value.trim()).filter(Boolean); }
function formatDate(value?: string) { return value ? dateTime.format(new Date(value)) : "—"; }
function isCLIEngine(engine: AgentEngine) { return engine === "opencode" || engine === "codex" || engine === "claude"; }
function runtimeIsOpen(runtime: Runtime) { return runtime.enabled && !runtime.revoked_at && !runtime.deleted_at; }
function runtimeSupportsEngine(runtime: Runtime, engine: AgentEngine) { return isCLIEngine(engine) && runtime.engines.includes(engine); }
function runtimeOptionLabel(runtime: Runtime, engine: AgentEngine) {
  const state = !runtime.enabled ? "已停用" : runtime.revoked_at ? "已撤销" : runtime.deleted_at ? "已删除" : !runtimeSupportsEngine(runtime, engine) ? `不支持 ${engine || "当前引擎"}` : runtime.online ? "在线" : "离线";
  return `${runtime.name} · ${state} · ${runtime.engines.join("/") || "未上报引擎"}`;
}
function optionalNumber(value: string) { const parsed = Number(value); return value.trim() && Number.isFinite(parsed) ? parsed : 0; }
function parseConfigValue(value: string): unknown {
  const trimmed = value.trim();
  if (!trimmed) return "";
  try { return JSON.parse(trimmed) as unknown; } catch { return value; }
}

function emptyForm(): AgentFormState {
  return {
    name: "", displayName: "", role: "worker", ownerUserId: "", engine: "", runtimeId: "", model: "",
    systemPrompt: "", identityCore: "", identityExtended: "", identityRaw: "", profiles: [], skills: [], healthCheck: "", specSubAgents: [], specGraphType: "",
    mcpServers: [], permissionAllow: [], permissionDeny: [], permissionAsk: [], subAgents: [],
    memoryEnabled: true, memoryTopK: "", autoRemember: true, memoryScope: "tenant", memorySlots: [],
    guardEnabled: false, maxInputLen: "", blockedTerms: [], compactionEnabled: true, compactionThreshold: "6000",
    maxCostUSD: "", maxTokens: "", maxOutputTokens: "", stepBudget: "", maxToolRepeats: "", fallbackModels: [], fallbackRetries: "",
    outputSchema: "", graphType: "standard", graphEntry: "", graphSteps: [], tags: [],
  };
}

function formFromRecord(record: AgentRecord): AgentFormState {
  const graph = record.graph_definition;
  return {
    name: record.name,
    displayName: record.display_name || "",
    role: record.role || "worker",
    ownerUserId: record.owner_user_id || "",
    engine: (record.engine === "loom" ? "" : record.engine || "") as AgentEngine,
    runtimeId: record.runtime_id || "",
    model: record.model || "",
    systemPrompt: record.spec?.system_prompt || "",
    identityCore: record.spec?.identity?.core || "",
    identityExtended: record.spec?.identity?.extended || "",
    identityRaw: record.spec?.identity?.raw || "",
    profiles: Object.entries(record.spec?.profiles || {}).map(([name, value]) => ({ id: nextID(), name, systemAddition: value.system_addition || "", greeting: value.greeting || "" })),
    skills: (record.spec?.skills || []).map((skill) => ({ id: nextID(), name: skill.name, description: skill.description, body: skill.body || "", alwaysActive: Boolean(skill.always_active), scripts: stringItems(skill.scripts), references: stringItems(skill.references) })),
    healthCheck: record.spec?.health_check?.content || "",
    specSubAgents: (record.spec?.sub_agents || []).map((item) => ({ id: nextID(), name: item.name, description: item.description || "", routeKey: item.route_key || "" })),
    specGraphType: record.spec?.graph_type || "",
    mcpServers: (record.mcp_servers || []).map((server) => ({ id: nextID(), serverId: server.server_id || "", url: server.url || "", filter: stringItems(server.filter), writeTools: stringItems(server.write_tools), headers: pairs(server.headers) })),
    permissionAllow: stringItems(record.permissions?.allow), permissionDeny: stringItems(record.permissions?.deny), permissionAsk: stringItems(record.permissions?.ask),
    subAgents: (record.sub_agents || []).map((item) => ({ id: nextID(), name: item.name, description: item.description || "", routeKey: item.route_key || "" })),
    memoryEnabled: record.memory_config?.enabled !== false, memoryTopK: numberText(record.memory_config?.top_k), autoRemember: record.memory_config?.auto_remember !== false, memoryScope: record.memory_config?.scope || "tenant",
    memorySlots: (record.memory_slots || []).map((slot) => ({ id: nextID(), key: slot.key, label: slot.label, description: slot.description })),
    guardEnabled: Boolean(record.guard?.enabled), maxInputLen: numberText(record.guard?.max_input_len), blockedTerms: stringItems(record.guard?.blocked_terms),
    compactionEnabled: record.compaction?.enabled !== false, compactionThreshold: numberText(record.compaction?.token_threshold),
    maxCostUSD: numberText(record.max_cost_usd), maxTokens: numberText(record.max_tokens), maxOutputTokens: numberText(record.max_output_tokens), stepBudget: numberText(record.step_budget), maxToolRepeats: numberText(record.max_tool_repeats), fallbackModels: stringItems(record.fallback_models), fallbackRetries: numberText(record.fallback_retries),
    outputSchema: record.output_schema === undefined ? "" : JSON.stringify(record.output_schema, null, 2),
    graphType: record.graph_type || "standard",
    graphEntry: graph?.entry || "",
    graphSteps: (graph?.steps || []).map((step) => ({
      id: nextID(), name: step.name, type: step.type, display: step.display, config: Object.entries(step.config || {}).map(([key, value]) => ({ id: nextID(), key, value: typeof value === "string" ? value : JSON.stringify(value) })), next: step.next || "",
      conditionEnabled: Boolean(step.condition), conditionKey: step.condition?.key || "", trueStep: step.condition?.true || "", falseStep: step.condition?.false || "",
    })),
    tags: stringItems(record.tags),
  };
}

function payloadFromForm(form: AgentFormState, includeOwner: boolean, allowOwnerClear: boolean): AgentWriteInput {
  const profiles = Object.fromEntries(form.profiles.filter((item) => item.name.trim()).map((item) => [item.name.trim(), { system_addition: item.systemAddition, greeting: item.greeting }]));
  const skills = form.skills.filter((item) => item.name.trim()).map((item) => ({ name: item.name.trim(), description: item.description.trim(), body: item.body, always_active: item.alwaysActive, scripts: cleanStrings(item.scripts), references: cleanStrings(item.references) }));
  const toSubAgents = (items: SubAgentItem[]) => items.filter((item) => item.name.trim()).map((item) => ({ name: item.name.trim(), description: item.description.trim(), route_key: item.routeKey.trim() }));
  const payload: AgentWriteInput = {
    name: form.name.trim(),
    display_name: form.displayName.trim(),
    role: form.role,
    engine: form.engine,
    runtime_id: isCLIEngine(form.engine) ? form.runtimeId.trim() : "",
    model: form.model.trim(),
    spec: {
      system_prompt: form.systemPrompt,
      identity: { core: form.identityCore, extended: form.identityExtended, raw: form.identityRaw },
      profiles,
      skills,
      health_check: form.healthCheck ? { content: form.healthCheck } : null,
      sub_agents: toSubAgents(form.specSubAgents),
      graph_type: form.specGraphType.trim(),
    },
    permissions: { allow: cleanStrings(form.permissionAllow), deny: cleanStrings(form.permissionDeny), ask: cleanStrings(form.permissionAsk) },
    mcp_servers: form.mcpServers.map((server) => ({
      server_id: server.serverId.trim(), url: server.url.trim(), filter: cleanStrings(server.filter), write_tools: cleanStrings(server.writeTools),
      headers: Object.fromEntries(server.headers.filter((item) => item.key.trim()).map((item) => [item.key.trim(), item.value])),
    })).filter((server) => server.server_id || server.url),
    memory_config: { enabled: form.memoryEnabled, top_k: optionalNumber(form.memoryTopK), auto_remember: form.autoRemember, scope: form.memoryScope },
    memory_slots: form.memorySlots.filter((slot) => slot.key.trim()).map((slot) => ({ key: slot.key.trim(), label: slot.label.trim(), description: slot.description.trim() })),
    max_cost_usd: optionalNumber(form.maxCostUSD), max_tokens: optionalNumber(form.maxTokens), max_output_tokens: optionalNumber(form.maxOutputTokens), step_budget: optionalNumber(form.stepBudget), max_tool_repeats: optionalNumber(form.maxToolRepeats),
    fallback_models: cleanStrings(form.fallbackModels), fallback_retries: optionalNumber(form.fallbackRetries),
    guard: { enabled: form.guardEnabled, max_input_len: optionalNumber(form.maxInputLen), blocked_terms: cleanStrings(form.blockedTerms) },
    compaction: { enabled: form.compactionEnabled, token_threshold: optionalNumber(form.compactionThreshold) },
    sub_agents: form.role === "avatar" ? [] : toSubAgents(form.subAgents),
    graph_type: form.graphType,
    tags: cleanStrings(form.tags),
  };
  if (includeOwner && (form.ownerUserId.trim() || allowOwnerClear)) payload.owner_user_id = form.ownerUserId.trim() || null;
  if (form.outputSchema.trim()) payload.output_schema = JSON.parse(form.outputSchema) as unknown;
  if (form.graphType === "declarative" && form.role !== "avatar") {
    payload.graph_definition = {
      entry: form.graphEntry.trim(),
      steps: form.graphSteps.filter((step) => step.name.trim()).map((step) => ({
        name: step.name.trim(), type: step.type, display: step.display.trim(),
        config: Object.fromEntries(step.config.filter((item) => item.key.trim()).map((item) => [item.key.trim(), parseConfigValue(item.value)])),
        next: step.conditionEnabled ? null : step.next.trim() || null,
        condition: step.conditionEnabled ? { key: step.conditionKey.trim(), true: step.trueStep.trim() || null, false: step.falseStep.trim() || null } : null,
      })),
    };
  }
  return payload;
}

function editorError(error: unknown) {
  const normalized = normalizeThrownError(error);
  if (normalized.status === 403) return "当前账号没有编辑智能体的权限。";
  if (normalized.status === 409) return `配置冲突：${apiErrorMessage(normalized)}`;
  if (normalized.status === 422) return `所有者不是当前工作区成员：${apiErrorMessage(normalized)}`;
  return apiErrorMessage(normalized);
}

function validateAgentForm(form: AgentFormState): { message: string; tab?: EditorTab } | null {
  if (!/^[a-z0-9][a-z0-9_-]{0,63}$/.test(form.name)) return { message: "智能体名称必须为 1–64 位小写字母、数字、连字符或下划线，并以字母或数字开头。", tab: "identity" };
  if (form.runtimeId.trim() && !isCLIEngine(form.engine)) return { message: "仅 OpenCode、Codex 或 Claude 引擎可绑定运行环境。", tab: "execution" };
  if (isCLIEngine(form.engine) && form.mcpServers.some((server) => server.serverId.trim())) return { message: "CLI 引擎不能通过服务器 ID 引用 MCP 服务；请改用 URL 或移除该引用。", tab: "tools" };
  if (form.role === "avatar" && (form.subAgents.length || form.graphType === "declarative")) return { message: "分身不能配置授权子智能体或自定义执行图。" };
  if (form.graphType === "declarative" && (!form.graphEntry.trim() || !form.graphSteps.length)) return { message: "声明式执行图需要设置入口步骤，并至少添加一个步骤。", tab: "advanced" };
  return null;
}

interface AgentEditorFormProps {
  formID: string;
  form: AgentFormState;
  setForm: Dispatch<SetStateAction<AgentFormState>>;
  activeTab: EditorTab;
  setActiveTab: Dispatch<SetStateAction<EditorTab>>;
  actionError: string | null;
  editing: boolean;
  ownerWritable: boolean;
  editingRecord: AgentRecord | null;
  teamMemberships: AgentTeamMembershipsResponse | null;
  teamMembershipsLoading: boolean;
  teamMembershipsError: string | null;
  runSummary: AgentRunSummaryResponse | null;
  runSummaryLoading: boolean;
  runSummaryError: string | null;
  agents: AgentRecord[];
  runtimes: Runtime[];
  runtimesLoading: boolean;
  runtimesError: string | null;
  reloadRuntimes(): void;
  onSubmit(event: FormEvent): void;
}

export function AgentEditorForm({ formID, form, setForm, activeTab, setActiveTab, actionError, editing, ownerWritable, editingRecord, teamMemberships, teamMembershipsLoading, teamMembershipsError, runSummary, runSummaryLoading, runSummaryError, agents, runtimes, runtimesLoading, runtimesError, reloadRuntimes, onSubmit }: AgentEditorFormProps) {
  return <form id={formID} className="agent-editor" onSubmit={onSubmit}>
    {actionError && <ErrorNotice message={actionError} />}
    <div className="agent-editor__tabs" role="tablist" aria-label="智能体配置分区">{tabs.map(([id, label]) => <button key={id} role="tab" aria-selected={activeTab === id} className={activeTab === id ? "active" : ""} type="button" onClick={() => setActiveTab(id)}>{label}</button>)}</div>
    {activeTab === "identity" && <IdentitySection form={form} setForm={setForm} editing={editing} ownerWritable={ownerWritable} editingRecord={editingRecord} teamMemberships={teamMemberships} teamMembershipsLoading={teamMembershipsLoading} teamMembershipsError={teamMembershipsError} runSummary={runSummary} runSummaryLoading={runSummaryLoading} runSummaryError={runSummaryError} />}
    {activeTab === "prompt" && <PromptSection form={form} setForm={setForm} editing={editing} />}
    {activeTab === "tools" && <ToolsSection form={form} setForm={setForm} agents={agents} editingName={editingRecord?.name} />}
    {activeTab === "memory" && <MemorySection form={form} setForm={setForm} />}
    {activeTab === "execution" && <ExecutionSection form={form} setForm={setForm} runtimes={runtimes} runtimesLoading={runtimesLoading} runtimesError={runtimesError} reloadRuntimes={reloadRuntimes} />}
    {activeTab === "advanced" && <AdvancedSection form={form} setForm={setForm} editing={editing} />}
  </form>;
}

function slugifyAgentName(text: string): string {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, "-")
    .replace(/-{2,}/g, "-")
    .replace(/^[^a-z0-9]+/, "")
    .replace(/[-_]+$/, "")
    .slice(0, 64);
}

const AGENT_NAME_PATTERN = /^[a-z0-9][a-z0-9_-]{0,63}$/;

export function AgentCreateModal({ role, onClose, onCreated }: { role: AgentRole | null; onClose(): void; onCreated(name: string): void | Promise<void> }) {
  const [form, setForm] = useState<AgentFormState>(emptyForm);
  const [nameTouched, setNameTouched] = useState(false);
  const generatedNameRef = useRef("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  useEffect(() => {
    if (!role) return;
    setForm({ ...emptyForm(), role }); setNameTouched(false); generatedNameRef.current = ""; setActionError(null);
  }, [role]);

  function changeDisplayName(value: string) {
    setForm((current) => {
      if (nameTouched) return { ...current, displayName: value };
      let name = slugifyAgentName(value);
      if (!name) {
        if (!generatedNameRef.current) generatedNameRef.current = `agent-${Math.random().toString(36).slice(2, 6)}`;
        name = generatedNameRef.current;
      }
      return { ...current, displayName: value, name };
    });
  }

  const nameValid = AGENT_NAME_PATTERN.test(form.name);

  async function createAgent(event: FormEvent) {
    event.preventDefault();
    setActionError(null);
    if (!nameValid) { setActionError("内部名称需为 1–64 位小写字母、数字、连字符或下划线，并以字母或数字开头。"); return; }
    const validation = validateAgentForm(form);
    if (validation) { setActionError(validation.message); return; }
    setBusy(true);
    try {
      await api.createAgent(payloadFromForm(form, true, false));
      const createdName = form.name.trim();
      onClose(); await onCreated(createdName);
    } catch (requestError) { setActionError(editorError(requestError)); }
    finally { setBusy(false); }
  }

  return <Modal open={role !== null} title={form.role === "avatar" ? "创建分身" : "创建数字员工"} description="只需名称和一句话职责；技能、工具、预算与运行环境都可以在创建后继续配置。" onClose={() => { if (!busy) onClose(); }} footer={<><Button disabled={busy} onClick={onClose}>取消</Button><Button variant="primary" type="submit" form="agent-create-form" loading={busy} disabled={!nameValid || !form.identityCore.trim()}>{busy ? "正在创建…" : form.role === "avatar" ? "创建分身" : "创建数字员工"}</Button></>}>
    <form id="agent-create-form" className="form-stack" onSubmit={(event) => void createAgent(event)}>
      {actionError && <ErrorNotice message={actionError} />}
      <Field label="类型" help={form.role === "avatar" ? "分身面向人，可作为团队负责人。" : "数字员工面向任务，可被多个团队复用。"}>{<div className="agent-role-picker" role="radiogroup" aria-label="类型">
        <button type="button" role="radio" aria-checked={form.role === "worker"} className={form.role === "worker" ? "agent-role-option is-active" : "agent-role-option"} onClick={() => patchForm(setForm, { role: "worker" })}><strong>数字员工</strong><small>承担专业任务，可被多个团队调用</small></button>
        <button type="button" role="radio" aria-checked={form.role === "avatar"} className={form.role === "avatar" ? "agent-role-option is-active" : "agent-role-option"} onClick={() => patchForm(setForm, { role: "avatar" })}><strong>分身</strong><small>面向人对话，可负责一个团队</small></button>
      </div>}</Field>
      <Field label="显示名称">{(control) => <input {...control} autoFocus value={form.displayName} onChange={(event) => changeDisplayName(event.target.value)} placeholder={form.role === "avatar" ? "例如：运营助理" : "例如：库存专员"} />}</Field>
      <Field label="内部名称" help="小写字母、数字、连字符，用于 API 与流程绑定；创建后不可修改。" error={form.name && !nameValid ? "需以小写字母或数字开头，只含小写字母、数字、连字符或下划线。" : undefined}>{(control) => <input {...control} value={form.name} onChange={(event) => { setNameTouched(true); patchForm(setForm, { name: event.target.value }); }} placeholder="由显示名称自动生成，可修改" autoComplete="off" />}</Field>
      <Field label="它是做什么的" help="一句话职责，会作为该智能体的核心身份。">{(control) => <textarea {...control} rows={3} value={form.identityCore} onChange={(event) => patchForm(setForm, { identityCore: event.target.value })} placeholder={form.role === "avatar" ? "例如：帮助运营团队处理日常数据整理与周报" : "例如：负责库存盘点、缺货预警与补货建议"} />}</Field>
    </form>
  </Modal>;
}

export function AgentDetailPage() {
  const { name = "" } = useParams<{ name: string }>();
  const request = useRef<AbortController | null>(null);
  const { user } = useAuth();
  const [agent, setAgent] = useState<AgentRecord | null>(null);
  const [agents, setAgents] = useState<AgentRecord[]>([]);
  const [runtimes, setRuntimes] = useState<Runtime[]>([]);
  const [runtimesLoading, setRuntimesLoading] = useState(true);
  const [runtimesError, setRuntimesError] = useState<string | null>(null);
  const [form, setForm] = useState<AgentFormState>(emptyForm);
  const [activeTab, setActiveTab] = useState<EditorTab>("identity");
  const [detailTab, setDetailTab] = useState<DetailTab>("configuration");
  const [memoryTab, setMemoryTab] = useState<MemoryTab>("memories");
  const [graphTab, setGraphTab] = useState<GraphTab>("topology");
  const [detailLoading, setDetailLoading] = useState(true);
  const [detailReady, setDetailReady] = useState(false);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [teamMemberships, setTeamMemberships] = useState<AgentTeamMembershipsResponse | null>(null);
  const [teamMembershipsLoading, setTeamMembershipsLoading] = useState(false);
  const [teamMembershipsError, setTeamMembershipsError] = useState<string | null>(null);
  const [runSummary, setRunSummary] = useState<AgentRunSummaryResponse | null>(null);
  const [runSummaryLoading, setRunSummaryLoading] = useState(false);
  const [runSummaryError, setRunSummaryError] = useState<string | null>(null);
  const canChangeOwner = user?.source !== "apikey" && (user?.role === "admin" || user?.role === "owner");

  const clearEditorFacts = useCallback(() => {
    setTeamMemberships(null); setTeamMembershipsLoading(false); setTeamMembershipsError(null);
    setRunSummary(null); setRunSummaryLoading(false); setRunSummaryError(null);
  }, []);

  const refreshRuntimes = useCallback(async (signal?: AbortSignal) => {
    setRuntimesLoading(true); setRuntimesError(null);
    try { setRuntimes((await api.listRuntimes(signal)).runtimes); }
    catch (requestError) {
      if (!signal?.aborted) setRuntimesError(`运行环境列表加载失败：${apiErrorMessage(normalizeThrownError(requestError))}`);
    } finally { if (!signal?.aborted) setRuntimesLoading(false); }
  }, []);

  const loadAgent = useCallback(async () => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    clearEditorFacts();
    setAgent(null); setDetailLoading(true); setDetailReady(false); setActiveTab("identity"); setDetailTab("configuration"); setMemoryTab("memories"); setGraphTab("topology"); setActionError(null);
    setTeamMembershipsLoading(true); setRunSummaryLoading(true);

    void api.getAgentTeamMemberships(name, controller.signal)
      .then((response) => { if (request.current === controller) setTeamMemberships(response); })
      .catch((requestError: unknown) => { if (request.current === controller && !controller.signal.aborted) setTeamMembershipsError(`团队信息加载失败：${apiErrorMessage(normalizeThrownError(requestError))}`); })
      .finally(() => { if (request.current === controller) setTeamMembershipsLoading(false); });
    void api.getAgentRunSummary(name, controller.signal)
      .then((response) => { if (request.current === controller) setRunSummary(response); })
      .catch((requestError: unknown) => { if (request.current === controller && !controller.signal.aborted) setRunSummaryError(`运行信息加载失败：${apiErrorMessage(normalizeThrownError(requestError))}`); })
      .finally(() => { if (request.current === controller) setRunSummaryLoading(false); });
    void api.listAgents(controller.signal)
      .then((records) => { if (request.current === controller) setAgents(records.filter((record) => !record.deleted)); })
      .catch(() => { if (request.current === controller && !controller.signal.aborted) setAgents([]); });
    void refreshRuntimes(controller.signal);

    try {
      const detail = await api.getAgent(name, controller.signal);
      if (request.current !== controller) return;
      setAgent(detail); setForm(formFromRecord(detail)); setDetailReady(true);
    } catch (requestError) {
      if (request.current === controller && !controller.signal.aborted) setActionError(editorError(requestError));
    } finally {
      if (request.current === controller) setDetailLoading(false);
    }
  }, [clearEditorFacts, name, refreshRuntimes]);

  useEffect(() => {
    void loadAgent();
    return () => { request.current?.abort(); };
  }, [loadAgent]);

  function discardChanges() {
    if (!agent) return;
    setForm(formFromRecord(agent));
    setActionError(null);
  }

  async function saveAgent(event: FormEvent) {
    event.preventDefault();
    setActionError(null);
    const validation = validateAgentForm(form);
    if (validation) {
      setActionError(validation.message);
      if (validation.tab) setActiveTab(validation.tab);
      return;
    }
    setBusy(true);
    try {
      const payload = payloadFromForm(form, canChangeOwner, true);
      const saved = await api.updateAgent(agent?.name || form.name, payload);
      setAgent(saved); setForm(formFromRecord(saved));
    } catch (requestError) { setActionError(editorError(requestError)); }
    finally { setBusy(false); }
  }

  return <section className="control-content agent-detail" aria-labelledby="agent-detail-heading">
    <Link className="agent-detail__return" to="/control/agents"><ArrowLeft size={16} aria-hidden="true" />返回智能体名册</Link>
    <header className="agent-detail__header"><div><h2 id="agent-detail-heading">{agent?.display_name || agent?.name || name || "智能体"}</h2><p className="agent-detail__name">{agent?.name || name}</p><p className="agent-detail__description">编辑现有配置，保存后生效。</p></div></header>
    <div className="agent-detail__tabs" role="tablist" aria-label="智能体详情分区">
      <button className={detailTab === "configuration" ? "active" : ""} type="button" role="tab" aria-selected={detailTab === "configuration"} onClick={() => setDetailTab("configuration")}>配置</button>
      <button className={detailTab === "memory" ? "active" : ""} type="button" role="tab" aria-selected={detailTab === "memory"} onClick={() => setDetailTab("memory")}>记忆</button>
      <button className={detailTab === "graph" ? "active" : ""} type="button" role="tab" aria-selected={detailTab === "graph"} onClick={() => setDetailTab("graph")}>内部执行图</button>
    </div>
    {detailLoading ? <LoadingView label="正在加载智能体详情" /> : !detailReady || !agent ? <div className="agent-detail-error"><ErrorNotice message={actionError || "智能体详情未就绪。"} onRetry={() => void loadAgent()} /></div> : <>
      {detailTab === "configuration" ? <>
        <AgentEditorForm
          formID="agent-detail-form"
          form={form}
          setForm={setForm}
          activeTab={activeTab}
          setActiveTab={setActiveTab}
          actionError={actionError}
          editing
          ownerWritable={canChangeOwner}
          editingRecord={agent}
          teamMemberships={teamMemberships}
          teamMembershipsLoading={teamMembershipsLoading}
          teamMembershipsError={teamMembershipsError}
          runSummary={runSummary}
          runSummaryLoading={runSummaryLoading}
          runSummaryError={runSummaryError}
          agents={agents}
          runtimes={runtimes}
          runtimesLoading={runtimesLoading}
          runtimesError={runtimesError}
          reloadRuntimes={() => void refreshRuntimes()}
          onSubmit={(submitEvent) => void saveAgent(submitEvent)}
        />
        <div className="agent-detail__savebar"><Button disabled={busy} onClick={discardChanges}>放弃更改</Button><Button variant="primary" type="submit" form="agent-detail-form" loading={busy} disabled={!form.name.trim()}>{busy ? "正在保存…" : "保存更改"}</Button></div>
      </> : detailTab === "memory" ? <>
        <MemoryConfiguration agent={agent} />
        <div className="agent-detail__tabs" role="tablist" aria-label="Agent 记忆分区">
          <button className={memoryTab === "memories" ? "active" : ""} type="button" role="tab" aria-selected={memoryTab === "memories"} onClick={() => setMemoryTab("memories")}>记忆</button>
          <button className={memoryTab === "slots" ? "active" : ""} type="button" role="tab" aria-selected={memoryTab === "slots"} onClick={() => setMemoryTab("slots")}>记忆槽位</button>
          <button className={memoryTab === "profile" ? "active" : ""} type="button" role="tab" aria-selected={memoryTab === "profile"} onClick={() => setMemoryTab("profile")}>用户画像</button>
        </div>
        {memoryTab === "memories" ? <MemoriesPanel agentName={agent.name} configured={agent.memory_config?.enabled !== false} /> : memoryTab === "slots" ? <SlotsPanel agentName={agent.name} canWrite={user?.role === "admin"} /> : <ProfilePanel agentName={agent.name} canRead={user?.role === "admin" || user?.role === "owner"} />}
      </> : <>
        <div className="agent-detail__tabs" role="tablist" aria-label="内部执行图分区">
          <button className={graphTab === "topology" ? "active" : ""} type="button" role="tab" aria-selected={graphTab === "topology"} onClick={() => setGraphTab("topology")}>拓扑</button>
          <button className={graphTab === "prompt" ? "active" : ""} type="button" role="tab" aria-selected={graphTab === "prompt"} onClick={() => setGraphTab("prompt")}>提示词预览</button>
          <button className={graphTab === "channels" ? "active" : ""} type="button" role="tab" aria-selected={graphTab === "channels"} onClick={() => setGraphTab("channels")}>频道</button>
        </div>
        {graphTab === "topology" ? <TopologyPanel agents={[agent]} agentName={agent.name} /> : graphTab === "prompt" ? <PromptPanel agents={[agent]} agentName={agent.name} /> : <ChannelsPanel agents={[agent]} agentName={agent.name} />}
      </>}
    </>}
  </section>;
}

type SetForm = Dispatch<SetStateAction<AgentFormState>>;
function patchForm(setForm: SetForm, patch: Partial<AgentFormState>) { setForm((current) => ({ ...current, ...patch })); }

function Section({ title, description, children }: { title: string; description: string; children: ReactNode }) {
  return <section className="agent-form-section"><header><h3>{title}</h3><p>{description}</p></header><div className="agent-form-section__body">{children}</div></section>;
}

function IdentitySection({ form, setForm, editing, ownerWritable, editingRecord, teamMemberships, teamMembershipsLoading, teamMembershipsError, runSummary, runSummaryLoading, runSummaryError }: { form: AgentFormState; setForm: SetForm; editing: boolean; ownerWritable: boolean; editingRecord: AgentRecord | null; teamMemberships: AgentTeamMembershipsResponse | null; teamMembershipsLoading: boolean; teamMembershipsError: string | null; runSummary: AgentRunSummaryResponse | null; runSummaryLoading: boolean; runSummaryError: string | null }) {
  const [users, setUsers] = useState<User[]>([]);
  useEffect(() => {
    if (!ownerWritable) return;
    let cancelled = false;
    void api.listUsers().then((records) => { if (!cancelled) setUsers(records); }).catch(() => undefined);
    return () => { cancelled = true; };
  }, [ownerWritable]);
  const ownerName = users.find((item) => item.id === form.ownerUserId)?.display_name || users.find((item) => item.id === form.ownerUserId)?.username || "";
  return <>
    <Section title="基本信息" description="智能体名称创建后不可更改；编辑时清空显示名会保留原值。">
      <div className="agent-form-grid"><Field label="智能体名称" help="1–64 位小写字母、数字、连字符或下划线。">{(control) => <input {...control} autoFocus={!editing} value={form.name} disabled={editing} required maxLength={64} pattern="[a-z0-9][a-z0-9_-]*" onChange={(event) => patchForm(setForm, { name: event.target.value })} />}</Field><Field label="显示名">{(control) => <input {...control} value={form.displayName} onChange={(event) => patchForm(setForm, { displayName: event.target.value })} />}</Field></div>
      <div className="agent-form-grid"><Field label="角色" help={editing ? "角色由所属团队管理，创建后不能在此修改。" : "可创建员工或分身；分身不能配置授权子智能体或自定义执行图。"}>{(control) => <select {...control} value={form.role} disabled={editing} onChange={(event) => patchForm(setForm, { role: event.target.value as AgentRole })}><option value="worker">员工</option><option value="avatar">分身</option></select>}</Field><Field label="所有者" help={editing && !ownerWritable ? "当前账号没有更改所有者的权限。" : "所有者可转移给当前工作区的其他成员。"}>{(control) => ownerWritable
        ? <select {...control} value={form.ownerUserId} onChange={(event) => patchForm(setForm, { ownerUserId: event.target.value })}><option value="">由系统选择</option>{users.map((item) => <option key={item.id} value={item.id}>{item.display_name || item.username || item.id}</option>)}</select>
        : <input {...control} value={ownerName || "当前所有者"} disabled readOnly />}</Field></div>
    </Section>
    <Section title="系统信息" description="由系统维护的只读信息。">
      <dl className="agent-readonly-facts"><div><dt>智能体 ID</dt><dd>{editingRecord?.id || "创建后生成"}</dd></div><div><dt>工作区</dt><dd>{editingRecord?.workspace_id || "当前工作区"}</dd></div><div><dt>团队 ID</dt><dd>{editingRecord?.team_id || "未绑定"}</dd></div><div><dt>版本</dt><dd>{editingRecord ? `v${editingRecord.version}` : "创建后为 v1"}</dd></div><div><dt>创建时间</dt><dd>{formatDate(editingRecord?.created_at)}</dd></div><div><dt>更新时间</dt><dd>{formatDate(editingRecord?.updated_at)}</dd></div></dl>
    </Section>
    {editing && <Section title="团队与运行信息" description="查看所属团队和近期运行情况；团队成员关系请在团队页面修改。">
      <div className="agent-managed-facts">
        <section><h4>负责的团队</h4>{teamMembershipsLoading ? <p className="agent-fact-status">正在加载团队信息…</p> : teamMembershipsError ? <p className="agent-fact-error">{teamMembershipsError}</p> : teamMemberships && !teamMemberships.lead_of.length ? <p className="agent-fact-status">未负责任何团队</p> : <ul>{teamMemberships?.lead_of.map((membership) => <li key={membership.team_id}><Link to={`/control/teams?team=${encodeURIComponent(membership.team_id)}`}>{membership.team_name}</Link><span>{membership.team_status}</span><code>{membership.team_id}</code></li>)}</ul>}</section>
        <section><h4>加入的团队</h4>{teamMembershipsLoading ? <p className="agent-fact-status">正在加载团队信息…</p> : teamMembershipsError ? <p className="agent-fact-error">{teamMembershipsError}</p> : teamMemberships && !teamMemberships.worker_of.length ? <p className="agent-fact-status">未加入任何团队</p> : <ul>{teamMemberships?.worker_of.map((membership) => <li key={membership.team_id}><Link to={`/control/teams?team=${encodeURIComponent(membership.team_id)}`}>{membership.team_name}</Link><span>{membership.team_status} · {membership.enabled ? "已启用" : "未启用"}</span><code>{membership.team_id}</code><dl><div><dt>职责</dt><dd>{membership.duty || "—"}</dd></div><div><dt>支持模式</dt><dd>{membership.allowed_kinds.length ? membership.allowed_kinds.join("、") : "无"}</dd></div><div><dt>默认模式</dt><dd>{membership.default_kind}</dd></div></dl></li>)}</ul>}</section>
        <section className="agent-run-facts"><h4>运行摘要</h4>{runSummaryLoading ? <p className="agent-fact-status">正在加载运行信息…</p> : runSummaryError ? <p className="agent-fact-error">{runSummaryError}</p> : runSummary && <dl><div><dt>最近运行</dt><dd>{runSummary.latest_run ? <Link to={`/activity?kind=run&id=${encodeURIComponent(runSummary.latest_run.run_id)}`}>{runSummary.latest_run.run_id}</Link> : "暂无运行"}</dd></div><div><dt>分类</dt><dd>{runSummary.latest_run?.classification || "—"}</dd></div><div><dt>开始时间</dt><dd>{formatDate(runSummary.latest_run?.started_at)}</dd></div><div><dt>活跃运行数</dt><dd>{runSummary.active_run_count}</dd></div><div><dt>近期失败数</dt><dd>{runSummary.recent_failure_count}</dd></div><div><dt>统计窗口</dt><dd>{runSummary.window}</dd></div></dl>}</section>
      </div>
    </Section>}
  </>;
}

function PromptSection({ form, setForm, editing }: { form: AgentFormState; setForm: SetForm; editing: boolean }) {
  return <>
    <Section title="系统提示词与分层身份" description="系统提示词定义核心职责；分层身份可按注入优先级补充稳定信息和原始上下文。系统提示词与核心身份不能同时清空。">
      <Field label="系统提示词">{(control) => <textarea {...control} rows={7} value={form.systemPrompt} onChange={(event) => patchForm(setForm, { systemPrompt: event.target.value })} />}</Field>
      <Field label="核心身份" help="始终注入提示词。">{(control) => <textarea {...control} rows={5} value={form.identityCore} onChange={(event) => patchForm(setForm, { identityCore: event.target.value })} />}</Field>
      <Field label="扩展身份" help={editing ? "创建后只读。" : "预算允许时注入。"}>{(control) => <textarea {...control} rows={4} value={form.identityExtended} disabled={editing} onChange={(event) => patchForm(setForm, { identityExtended: event.target.value })} />}</Field>
      <Field label="原始身份内容" help={editing ? "创建后只读。" : "保存完整的原始身份内容。"}>{(control) => <textarea {...control} rows={4} value={form.identityRaw} disabled={editing} onChange={(event) => patchForm(setForm, { identityRaw: event.target.value })} />}</Field>
    </Section>
    <Section title="配置档案" description="为不同使用场景设置提示词补充和问候语。"><ProfileList items={form.profiles} disabled={false} onChange={(profiles) => patchForm(setForm, { profiles })} /></Section>
    <Section title="技能" description="为智能体配置可按需或始终注入的技能内容、脚本和参考资料。"><SkillList items={form.skills} onChange={(skills) => patchForm(setForm, { skills })} /></Section>
    <Section title="健康检查与扩展配置" description={editing ? "这些配置创建后只读。" : "配置健康检查、规格图类型和规格内子智能体。"}>
      <Field label="健康检查内容">{(control) => <textarea {...control} rows={4} value={form.healthCheck} disabled={editing} onChange={(event) => patchForm(setForm, { healthCheck: event.target.value })} />}</Field>
      <Field label="规格图类型" help="仅用于兼容既有规格；实际执行图类型在“高级”中设置。">{(control) => <input {...control} value={form.specGraphType} disabled={editing} onChange={(event) => patchForm(setForm, { specGraphType: event.target.value })} />}</Field>
      <SubAgentList title="规格内子智能体" items={form.specSubAgents} disabled={editing} options={[]} onChange={(specSubAgents) => patchForm(setForm, { specSubAgents })} />
    </Section>
  </>;
}

function ToolsSection({ form, setForm, agents, editingName }: { form: AgentFormState; setForm: SetForm; agents: AgentRecord[]; editingName?: string }) {
  const dependencies = agents.filter((agent) => agent.name !== editingName && !agent.deleted);
  return <>
    <Section title="MCP 服务" description="连接智能体可调用的 MCP 服务，并选择开放的工具和写入工具；CLI 引擎请使用 URL 配置。">
      <MCPList items={form.mcpServers} cli={isCLIEngine(form.engine)} onChange={(mcpServers) => patchForm(setForm, { mcpServers })} />
    </Section>
    <Section title="工具权限" description="允许工具为空时不限制可用工具；拒绝工具始终不可调用；需确认工具会先征求用户许可。">
      <StringList title="允许工具" items={form.permissionAllow} placeholder="query_inventory" onChange={(permissionAllow) => patchForm(setForm, { permissionAllow })} />
      <StringList title="拒绝工具" items={form.permissionDeny} placeholder="execute_purchase" onChange={(permissionDeny) => patchForm(setForm, { permissionDeny })} />
      <StringList title="需确认工具" items={form.permissionAsk} placeholder="execute_movement" onChange={(permissionAsk) => patchForm(setForm, { permissionAsk })} />
    </Section>
    <Section title="子智能体" description="授权后，智能体可以将任务委托给列表中的智能体；清空列表将移除全部委托。分身不能配置。">
      {form.role === "avatar" ? <p className="dependency-empty">分身不能配置子智能体。</p> : <SubAgentList title="授权子智能体" items={form.subAgents} options={dependencies} onChange={(subAgents) => patchForm(setForm, { subAgents })} />}
    </Section>
  </>;
}

function MemorySection({ form, setForm }: { form: AgentFormState; setForm: SetForm }) {
  return <>
    <Section title="记忆配置" description="设置记忆的召回范围、数量和自动提取方式。召回数量留空或为 0 时使用系统默认值。">
      <label className="switch-row"><span><strong>启用记忆</strong><small>按智能体检索相关记忆。</small></span><Switch checked={form.memoryEnabled} aria-label="启用记忆" onChange={(next) => patchForm(setForm, { memoryEnabled: next })} /></label>
      <div className="agent-form-grid"><Field label="召回数量">{(control) => <input {...control} type="number" min="0" value={form.memoryTopK} onChange={(event) => patchForm(setForm, { memoryTopK: event.target.value })} />}</Field><Field label="记忆范围">{(control) => <select {...control} value={form.memoryScope} onChange={(event) => patchForm(setForm, { memoryScope: event.target.value as AgentFormState["memoryScope"] })}><option value="tenant">当前租户</option><option value="user">当前用户</option><option value="session">当前会话</option></select>}</Field></div>
      <label className="switch-row"><span><strong>自动记忆</strong><small>对话结束后自动提取记忆。</small></span><Switch checked={form.autoRemember} aria-label="自动记忆" onChange={(next) => patchForm(setForm, { autoRemember: next })} /></label>
    </Section>
    <Section title="记忆槽位" description="为结构化记忆定义键、显示名称和说明。"><MemorySlotList items={form.memorySlots} onChange={(memorySlots) => patchForm(setForm, { memorySlots })} /></Section>
    <Section title="输入守卫" description="限制输入长度，并拦截包含指定词语的请求；最大长度留空或为 0 时不限制。关闭守卫需显式关闭下方开关。">
      <label className="switch-row"><span><strong>启用输入守卫</strong><small>关闭时仍保留配置值。</small></span><Switch checked={form.guardEnabled} aria-label="启用输入守卫" onChange={(next) => patchForm(setForm, { guardEnabled: next })} /></label>
      <Field label="最大输入长度">{(control) => <input {...control} type="number" min="0" value={form.maxInputLen} onChange={(event) => patchForm(setForm, { maxInputLen: event.target.value })} />}</Field>
      <StringList title="阻止词" items={form.blockedTerms} placeholder="受限词" onChange={(blockedTerms) => patchForm(setForm, { blockedTerms })} />
    </Section>
    <Section title="上下文压缩" description="对话内容达到阈值后自动压缩；阈值留空或为 0 时使用系统默认值。关闭压缩需显式关闭下方开关。">
      <label className="switch-row"><span><strong>启用压缩</strong><small>达到 Token 阈值后触发。</small></span><Switch checked={form.compactionEnabled} aria-label="启用压缩" onChange={(next) => patchForm(setForm, { compactionEnabled: next })} /></label>
      <Field label="触发阈值（Token）">{(control) => <input {...control} type="number" min="0" value={form.compactionThreshold} onChange={(event) => patchForm(setForm, { compactionThreshold: event.target.value })} />}</Field>
    </Section>
  </>;
}

function ExecutionSection({ form, setForm, runtimes, runtimesLoading, runtimesError, reloadRuntimes }: { form: AgentFormState; setForm: SetForm; runtimes: Runtime[]; runtimesLoading: boolean; runtimesError: string | null; reloadRuntimes(): void }) {
  const selectedRuntimeKnown = runtimes.some((runtime) => runtime.id === form.runtimeId);
  const compatibleRuntimeCount = runtimes.filter((runtime) => runtimeIsOpen(runtime) && runtimeSupportsEngine(runtime, form.engine)).length;
  function changeEngine(engine: AgentEngine) {
    const selectedRuntime = runtimes.find((runtime) => runtime.id === form.runtimeId);
    const keepRuntime = !form.runtimeId || Boolean(selectedRuntime && runtimeIsOpen(selectedRuntime) && runtimeSupportsEngine(selectedRuntime, engine));
    patchForm(setForm, { engine, runtimeId: isCLIEngine(engine) && keepRuntime ? form.runtimeId : "" });
  }
  return <>
    <Section title="引擎与运行环境" description="平台内置执行无需额外配置；OpenCode、Codex 与 Claude Code 可绑定当前工作区的远程运行环境。">
      {runtimesError && <ErrorNotice message={runtimesError} onRetry={reloadRuntimes} />}
      <div className="agent-form-grid"><Field label="引擎">{(control) => <select {...control} value={form.engine === "loom" ? "" : form.engine} onChange={(event) => changeEngine(event.target.value as AgentEngine)}>{engines.map((engine) => <option key={engine.value || "empty"} value={engine.value}>{engine.label}</option>)}</select>}</Field><Field label="运行环境" help={!isCLIEngine(form.engine) ? "先选择 OpenCode、Codex 或 Claude 引擎。" : runtimesLoading ? "正在加载当前工作区的运行环境…" : compatibleRuntimeCount ? `${compatibleRuntimeCount} 个运行环境支持当前引擎；离线运行环境仍可保存为默认绑定。` : "没有支持当前引擎的可绑定运行环境。"}>{(control) => <select {...control} value={form.runtimeId} disabled={!isCLIEngine(form.engine) || runtimesLoading} onChange={(event) => patchForm(setForm, { runtimeId: event.target.value })}><option value="">平台本机执行（不绑定远程运行环境）</option>{form.runtimeId && !selectedRuntimeKnown && <option value={form.runtimeId} disabled>{form.runtimeId} · 当前详情值不可用</option>}{runtimes.map((runtime) => <option key={runtime.id} value={runtime.id} disabled={!runtimeIsOpen(runtime) || !runtimeSupportsEngine(runtime, form.engine)}>{runtimeOptionLabel(runtime, form.engine)}</option>)}</select>}</Field></div>
      <Field label="模型" help={isCLIEngine(form.engine) ? "留空使用所选运行环境的已认证默认模型；填写后发布工作流会冻结该模型。" : "留空使用平台默认模型；编辑时留空保留原模型。"}>{(control) => <input {...control} value={form.model} onChange={(event) => patchForm(setForm, { model: event.target.value })} />}</Field>
    </Section>
    <Section title="预算与限制" description="成本、总 Token、步骤和重复工具调用的上限留空或为 0 时不限制；单次输出上限留空或为 0 时使用模型默认值，备用模型重试次数留空或为 0 时默认 2 次。">
      <div className="agent-form-grid agent-form-grid--three"><NumberField label="最大成本（美元）" value={form.maxCostUSD} step="0.01" onChange={(maxCostUSD) => patchForm(setForm, { maxCostUSD })} /><NumberField label="Token 总上限" value={form.maxTokens} onChange={(maxTokens) => patchForm(setForm, { maxTokens })} /><NumberField label="单次输出 Token 上限" value={form.maxOutputTokens} onChange={(maxOutputTokens) => patchForm(setForm, { maxOutputTokens })} /><NumberField label="步骤上限" value={form.stepBudget} onChange={(stepBudget) => patchForm(setForm, { stepBudget })} /><NumberField label="重复工具调用上限" value={form.maxToolRepeats} onChange={(maxToolRepeats) => patchForm(setForm, { maxToolRepeats })} /><NumberField label="备用模型重试次数" value={form.fallbackRetries} onChange={(fallbackRetries) => patchForm(setForm, { fallbackRetries })} /></div>
      <StringList title="备用模型" items={form.fallbackModels} placeholder="备用模型名" onChange={(fallbackModels) => patchForm(setForm, { fallbackModels })} />
    </Section>
  </>;
}

function AdvancedSection({ form, setForm, editing }: { form: AgentFormState; setForm: SetForm; editing: boolean }) {
  return <>
    <Section title="执行图" description="标准图适用于常规智能体；声明式图可逐步配置执行流程。分身不能使用自定义执行图。">
      <Field label="执行图类型">{(control) => <select {...control} value={form.graphType} disabled={form.role === "avatar"} onChange={(event) => patchForm(setForm, { graphType: event.target.value })}>{form.graphType !== "standard" && form.graphType !== "declarative" && <option value={form.graphType}>{form.graphType}（当前自定义类型）</option>}<option value="standard">标准</option><option value="declarative">声明式</option></select>}</Field>
      {form.graphType === "declarative" && form.role !== "avatar" && <GraphEditor entry={form.graphEntry} steps={form.graphSteps} onEntryChange={(graphEntry) => patchForm(setForm, { graphEntry })} onStepsChange={(graphSteps) => patchForm(setForm, { graphSteps })} />}
      {editing && form.graphType === "standard" && <p className="contract-note">标准图使用内置执行流程；已有声明式步骤不会参与运行。</p>}
    </Section>
    <Section title="输出结构" description="使用 JSON Schema 约束结构化输出；编辑时留空会保留现有配置。">
      <Field label="JSON Schema" help="仅填写 JSON Schema 内容。">{(control) => <textarea {...control} className="code-input" rows={8} value={form.outputSchema} placeholder={'{"type":"object","properties":{}}'} onChange={(event) => patchForm(setForm, { outputSchema: event.target.value })} />}</Field>
    </Section>
    <Section title="标签" description="用于对智能体分组和筛选；清空列表将移除所有标签。"><StringList title="标签" items={form.tags} placeholder="production" onChange={(tags) => patchForm(setForm, { tags })} /></Section>
  </>;
}

function NumberField({ label, value, step = "1", onChange }: { label: string; value: string; step?: string; onChange(value: string): void }) {
  return <Field label={label}>{(control) => <input {...control} type="number" min="0" step={step} value={value} onChange={(event) => onChange(event.target.value)} />}</Field>;
}

function StringList({ title, items, placeholder, disabled = false, onChange }: { title: string; items: StringItem[]; placeholder: string; disabled?: boolean; onChange(items: StringItem[]): void }) {
  return <div className="structured-list"><div className="structured-list__heading"><strong>{title}</strong><Button variant="ghost" disabled={disabled} onClick={() => onChange([...items, { id: nextID(), value: "" }])}><Plus size={14} />添加</Button></div>{!items.length ? <p className="structured-list__empty">尚未添加{title}，可点击“添加”开始配置。</p> : items.map((item) => <div className="structured-row" key={item.id}><input aria-label={title} placeholder={placeholder} value={item.value} disabled={disabled} onChange={(event) => onChange(items.map((current) => current.id === item.id ? { ...current, value: event.target.value } : current))} /><button className="icon-button" type="button" aria-label={`移除 ${title} 项`} disabled={disabled} onClick={() => onChange(items.filter((current) => current.id !== item.id))}><X size={16} /></button></div>)}</div>;
}

function PairList({ title, items, keyPlaceholder, valuePlaceholder, onChange }: { title: string; items: PairItem[]; keyPlaceholder: string; valuePlaceholder: string; onChange(items: PairItem[]): void }) {
  return <div className="structured-list"><div className="structured-list__heading"><strong>{title}</strong><Button variant="ghost" onClick={() => onChange([...items, { id: nextID(), key: "", value: "" }])}><Plus size={14} />添加</Button></div>{!items.length ? <p className="structured-list__empty">尚未添加{title}，可点击“添加”开始配置。</p> : items.map((item) => <div className="structured-row structured-row--pair" key={item.id}><input aria-label={`${title}键`} placeholder={keyPlaceholder} value={item.key} onChange={(event) => onChange(items.map((current) => current.id === item.id ? { ...current, key: event.target.value } : current))} /><input aria-label={`${title}值`} placeholder={valuePlaceholder} value={item.value} onChange={(event) => onChange(items.map((current) => current.id === item.id ? { ...current, value: event.target.value } : current))} /><button className="icon-button" type="button" aria-label={`移除 ${title} 项`} onClick={() => onChange(items.filter((current) => current.id !== item.id))}><X size={16} /></button></div>)}</div>;
}

function ProfileList({ items, disabled, onChange }: { items: ProfileItem[]; disabled: boolean; onChange(items: ProfileItem[]): void }) {
  return <div className="structured-list"><div className="structured-list__heading"><strong>{items.length} 个配置档案</strong><Button variant="ghost" disabled={disabled} onClick={() => onChange([...items, { id: nextID(), name: "", systemAddition: "", greeting: "" }])}><Plus size={14} />添加配置档案</Button></div>{!items.length ? <p className="structured-list__empty">尚未添加配置档案，可点击“添加配置档案”开始设置。</p> : items.map((item) => <fieldset className="structured-group" key={item.id}><div className="structured-group__header"><strong>{item.name || "新配置档案"}</strong><button className="icon-button" type="button" aria-label="移除配置档案" onClick={() => onChange(items.filter((current) => current.id !== item.id))}><Trash2 size={14} /></button></div><Field label="名称">{(control) => <input {...control} value={item.name} onChange={(event) => onChange(items.map((current) => current.id === item.id ? { ...current, name: event.target.value } : current))} />}</Field><Field label="提示词补充">{(control) => <textarea {...control} rows={3} value={item.systemAddition} onChange={(event) => onChange(items.map((current) => current.id === item.id ? { ...current, systemAddition: event.target.value } : current))} />}</Field><Field label="问候语">{(control) => <textarea {...control} rows={2} value={item.greeting} onChange={(event) => onChange(items.map((current) => current.id === item.id ? { ...current, greeting: event.target.value } : current))} />}</Field></fieldset>)}</div>;
}

function SkillList({ items, onChange }: { items: SkillItem[]; onChange(items: SkillItem[]): void }) {
  function update(id: number, patch: Partial<SkillItem>) { onChange(items.map((item) => item.id === id ? { ...item, ...patch } : item)); }
  return <div className="structured-list"><div className="structured-list__heading"><strong>{items.length} 个技能</strong><Button variant="ghost" onClick={() => onChange([...items, { id: nextID(), name: "", description: "", body: "", alwaysActive: false, scripts: [], references: [] }])}><Plus size={14} />添加技能</Button></div>{!items.length ? <p className="structured-list__empty">尚未添加技能，可点击“添加技能”开始设置。</p> : items.map((item) => <fieldset className="structured-group" key={item.id}><div className="structured-group__header"><strong>{item.name || "新技能"}</strong><button className="icon-button" type="button" aria-label="移除技能" onClick={() => onChange(items.filter((current) => current.id !== item.id))}><Trash2 size={14} /></button></div><div className="agent-form-grid"><Field label="名称">{(control) => <input {...control} value={item.name} onChange={(event) => update(item.id, { name: event.target.value })} />}</Field><Field label="说明">{(control) => <input {...control} value={item.description} onChange={(event) => update(item.id, { description: event.target.value })} />}</Field></div><Field label="内容">{(control) => <textarea {...control} rows={4} value={item.body} onChange={(event) => update(item.id, { body: event.target.value })} />}</Field><label className="switch-row"><span><strong>始终启用</strong><small>跳过关键词匹配，始终注入。</small></span><Switch checked={item.alwaysActive} aria-label="始终启用" onChange={(next) => update(item.id, { alwaysActive: next })} /></label><StringList title="脚本" items={item.scripts} placeholder="scripts/task.ts" onChange={(scripts) => update(item.id, { scripts })} /><StringList title="参考资料" items={item.references} placeholder="references/guide.md" onChange={(references) => update(item.id, { references })} /></fieldset>)}</div>;
}

function MCPList({ items, cli, onChange }: { items: MCPItem[]; cli: boolean; onChange(items: MCPItem[]): void }) {
  function update(id: number, patch: Partial<MCPItem>) { onChange(items.map((item) => item.id === id ? { ...item, ...patch } : item)); }
  return <div className="structured-list"><div className="structured-list__heading"><strong>{items.length} 个 MCP 服务</strong><Button variant="ghost" onClick={() => onChange([...items, { id: nextID(), serverId: "", url: "", filter: [], writeTools: [], headers: [] }])}><Plus size={14} />添加 MCP 服务</Button></div>{!items.length ? <p className="structured-list__empty">尚未配置 MCP 服务，可点击“添加 MCP 服务”开始设置。</p> : items.map((item) => <fieldset className="structured-group" key={item.id}><div className="structured-group__header"><strong>{item.serverId || item.url || "新 MCP 服务"}</strong><button className="icon-button" type="button" aria-label="移除 MCP 服务" onClick={() => onChange(items.filter((current) => current.id !== item.id))}><Trash2 size={14} /></button></div><div className="agent-form-grid"><Field label="服务器 ID" help={cli ? "CLI 引擎不能使用服务器 ID，请改用 URL。" : "填写工作区 MCP 服务的注册 ID，或直接使用 URL。"}>{(control) => <input {...control} value={item.serverId} disabled={cli} onChange={(event) => update(item.id, { serverId: event.target.value })} />}</Field><Field label="URL">{(control) => <input {...control} type="url" value={item.url} placeholder="https://example-mcp.test" onChange={(event) => update(item.id, { url: event.target.value })} />}</Field></div><StringList title="工具筛选" items={item.filter} placeholder="query_tool" onChange={(filter) => update(item.id, { filter })} /><StringList title="写入工具" items={item.writeTools} placeholder="write_tool" onChange={(writeTools) => update(item.id, { writeTools })} /><PairList title="请求头" items={item.headers} keyPlaceholder="请求头名称" valuePlaceholder="请求头值" onChange={(headers) => update(item.id, { headers })} /></fieldset>)}</div>;
}

function SubAgentList({ title, items, disabled = false, options, onChange }: { title: string; items: SubAgentItem[]; disabled?: boolean; options: AgentRecord[]; onChange(items: SubAgentItem[]): void }) {
  function update(id: number, patch: Partial<SubAgentItem>) { onChange(items.map((item) => item.id === id ? { ...item, ...patch } : item)); }
  return <div className="structured-list"><div className="structured-list__heading"><strong>{title}</strong><Button variant="ghost" disabled={disabled} onClick={() => onChange([...items, { id: nextID(), name: "", description: "", routeKey: "" }])}><Plus size={14} />添加</Button></div>{!options.length && !items.length ? <p className="dependency-empty">没有可选智能体；仍可手动添加并填写智能体名称。</p> : null}{items.map((item) => <fieldset className="structured-group structured-group--compact" key={item.id}><div className="structured-group__header"><strong>{item.name || "新引用"}</strong><button className="icon-button" type="button" disabled={disabled} aria-label="移除子智能体" onClick={() => onChange(items.filter((current) => current.id !== item.id))}><X size={14} /></button></div><div className="agent-form-grid"><Field label="智能体名称">{(control) => <><input {...control} list={`agent-options-${item.id}`} value={item.name} disabled={disabled} onChange={(event) => update(item.id, { name: event.target.value })} /><datalist id={`agent-options-${item.id}`}>{options.map((agent) => <option key={agent.name} value={agent.name}>{agent.display_name || agent.name}</option>)}</datalist></>}</Field><Field label="路由键">{(control) => <input {...control} value={item.routeKey} disabled={disabled} placeholder="留空时使用智能体名称" onChange={(event) => update(item.id, { routeKey: event.target.value })} />}</Field></div><Field label="委托说明">{(control) => <input {...control} value={item.description} disabled={disabled} onChange={(event) => update(item.id, { description: event.target.value })} />}</Field></fieldset>)}</div>;
}

function MemorySlotList({ items, onChange }: { items: MemorySlotItem[]; onChange(items: MemorySlotItem[]): void }) {
  function update(id: number, patch: Partial<MemorySlotItem>) { onChange(items.map((item) => item.id === id ? { ...item, ...patch } : item)); }
  return <div className="structured-list"><div className="structured-list__heading"><strong>{items.length} 个记忆槽位</strong><Button variant="ghost" onClick={() => onChange([...items, { id: nextID(), key: "", label: "", description: "" }])}><Plus size={14} />添加槽位</Button></div>{!items.length ? <p className="structured-list__empty">尚未添加记忆槽位，可点击“添加槽位”开始设置。</p> : items.map((item) => <div className="structured-row structured-row--slot" key={item.id}><input aria-label="槽位键" placeholder="键" value={item.key} onChange={(event) => update(item.id, { key: event.target.value })} /><input aria-label="槽位显示名称" placeholder="显示名称" value={item.label} onChange={(event) => update(item.id, { label: event.target.value })} /><input aria-label="槽位说明" placeholder="说明" value={item.description} onChange={(event) => update(item.id, { description: event.target.value })} /><button className="icon-button" type="button" aria-label="移除记忆槽位" onClick={() => onChange(items.filter((current) => current.id !== item.id))}><X size={16} /></button></div>)}</div>;
}

function GraphEditor({ entry, steps, onEntryChange, onStepsChange }: { entry: string; steps: GraphStepItem[]; onEntryChange(value: string): void; onStepsChange(steps: GraphStepItem[]): void }) {
  const names = useMemo(() => steps.map((step) => step.name).filter(Boolean), [steps]);
  function update(id: number, patch: Partial<GraphStepItem>) { onStepsChange(steps.map((step) => step.id === id ? { ...step, ...patch } : step)); }
  return <div className="graph-editor"><Field label="入口步骤">{(control) => <input {...control} list="graph-step-names" value={entry} onChange={(event) => onEntryChange(event.target.value)} />}</Field><datalist id="graph-step-names">{names.map((name) => <option key={name} value={name} />)}</datalist><div className="structured-list__heading"><strong>{steps.length} 个步骤</strong><Button variant="ghost" onClick={() => onStepsChange([...steps, { id: nextID(), name: "", type: "chat", display: "", config: [], next: "", conditionEnabled: false, conditionKey: "", trueStep: "", falseStep: "" }])}><Plus size={14} />添加步骤</Button></div>{steps.map((step) => <fieldset className="structured-group" key={step.id}><div className="structured-group__header"><strong>{step.name || "新步骤"}</strong><button className="icon-button" type="button" aria-label="移除执行图步骤" onClick={() => onStepsChange(steps.filter((current) => current.id !== step.id))}><Trash2 size={14} /></button></div><div className="agent-form-grid agent-form-grid--three"><Field label="名称">{(control) => <input {...control} value={step.name} onChange={(event) => update(step.id, { name: event.target.value })} />}</Field><Field label="类型">{(control) => <select {...control} value={step.type} onChange={(event) => update(step.id, { type: event.target.value as AgentGraphStep["type"] })}>{graphStepTypes.map((type) => <option key={type} value={type}>{type}</option>)}</select>}</Field><Field label="显示名称">{(control) => <input {...control} value={step.display} onChange={(event) => update(step.id, { display: event.target.value })} />}</Field></div><PairList title="配置" items={step.config} keyPlaceholder="配置键" valuePlaceholder="JSON 值或文本" onChange={(config) => update(step.id, { config })} /><label className="switch-row"><span><strong>条件路由</strong><small>后续步骤与条件路由不可同时设置。</small></span><Switch checked={step.conditionEnabled} aria-label="条件路由" onChange={(next) => update(step.id, { conditionEnabled: next })} /></label>{step.conditionEnabled ? <div className="agent-form-grid agent-form-grid--three"><Field label="状态键">{(control) => <input {...control} value={step.conditionKey} onChange={(event) => update(step.id, { conditionKey: event.target.value })} />}</Field><Field label="条件成立步骤">{(control) => <input {...control} list="graph-step-names" value={step.trueStep} onChange={(event) => update(step.id, { trueStep: event.target.value })} />}</Field><Field label="条件不成立步骤">{(control) => <input {...control} list="graph-step-names" value={step.falseStep} onChange={(event) => update(step.id, { falseStep: event.target.value })} />}</Field></div> : <Field label="后续步骤" help="留空表示结束。">{(control) => <input {...control} list="graph-step-names" value={step.next} onChange={(event) => update(step.id, { next: event.target.value })} />}</Field>}</fieldset>)}</div>;
}
