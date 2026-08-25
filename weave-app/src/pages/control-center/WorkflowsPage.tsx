import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import {
  Archive,
  Bot,
  CheckCircle2,
  ChevronRight,
  CirclePlay,
  FileCode2,
  History,
  Link2,
  LockKeyhole,
  Plus,
  RefreshCw,
  Save,
  ShieldCheck,
  Trash2,
  TriangleAlert,
  Workflow as WorkflowIcon,
} from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type AgentRecord,
  type Team,
  type TeamRoster,
  type TeamWorkflowVersion,
  type WorkflowAdmissionStatusResponse,
  type WorkflowContract,
  type WorkflowDependenciesResponse,
  type WorkflowDetailResponse,
  type WorkflowEdge,
  type WorkflowEdgeRoute,
  type WorkflowGraphDefinition,
  type WorkflowInputBinding,
  type WorkflowJoinPolicy,
  type WorkflowManualRunResponse,
  type WorkflowNode,
  type WorkflowNodeType,
  type WorkflowPredicate,
  type WorkflowPredicateOperator,
  type WorkflowSummary,
  type WorkflowTransformOperation,
  type WorkflowTriggerConfig,
  type WorkflowTriggerType,
  type WorkflowValidationResponse,
  type WorkflowValueRef,
  type WorkflowValueSource,
  type WorkflowValueType,
  type WorkflowVersionResponse,
  type WorkflowWorkerKind,
} from "../../api";
import { useAuth } from "../../auth/AuthContext";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { Switch } from "../../ui/Switch";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { createClientUUID } from "../../platform/uuid";

const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
const triggerTypes: WorkflowTriggerType[] = ["conversation_explicit", "conversation_auto", "schedule", "api", "event"];
const nodeTypes: WorkflowNodeType[] = ["lead", "worker", "transform", "condition", "parallel", "join", "wait", "loop", "deliver", "handoff"];
const edgeRoutes: WorkflowEdgeRoute[] = ["success", "failure", "case", "default", "branch", "join", "timeout", "body", "exit", "back"];
const valueTypes: WorkflowValueType[] = ["text", "json", "boolean", "number"];
const valueSources: WorkflowValueSource[] = ["run_input", "node_output", "literal"];
const joinPolicies: WorkflowJoinPolicy[] = ["all_success", "quorum", "deadline", "fail_fast"];
const predicateOperators: WorkflowPredicateOperator[] = ["exists", "eq", "neq", "gt", "gte", "lt", "lte", "contains", "in"];
const nodeLabels: Record<WorkflowNodeType, string> = {
  lead: "Lead", worker: "Worker", transform: "转换", condition: "条件", parallel: "并行", join: "汇合",
  wait: "等待", loop: "循环", deliver: "交付", handoff: "移交",
};
const nodeTypeLabels: Record<WorkflowNodeType, string> = {
  lead: "负责人", worker: "执行者", transform: "转换", condition: "条件", parallel: "并行", join: "汇合",
  wait: "等待", loop: "循环", deliver: "交付", handoff: "移交",
};
const triggerLabels: Record<string, string> = {
  conversation_explicit: "会话显式触发", conversation_auto: "会话自动触发", schedule: "计划触发", api: "API 触发", event: "事件触发",
};
const workflowStatusLabels: Record<string, string> = { active: "启用", archived: "已归档" };
const teamStatusLabels: Record<string, string> = { active: "启用", archived: "已归档" };
const admissionLabels: Record<string, string> = { admitted: "允许启动", blocked: "已阻断", grandfathered: "沿用发布时授权（当前授权已收紧）", unknown: "未评估" };
const edgeRouteLabels: Record<WorkflowEdgeRoute, string> = {
  success: "成功", failure: "失败", case: "条件分支", default: "默认分支", branch: "并行分支", join: "汇合",
  timeout: "超时", body: "循环体", exit: "退出循环", back: "返回循环",
};
const valueTypeLabels: Record<WorkflowValueType, string> = { text: "文本", json: "JSON", boolean: "布尔值", number: "数字" };
const valueSourceLabels: Record<WorkflowValueSource, string> = { run_input: "运行输入", node_output: "节点输出", literal: "固定值" };
const joinPolicyLabels: Record<WorkflowJoinPolicy, string> = { all_success: "全部成功", quorum: "达到成功数量", deadline: "截止时间", fail_fast: "失败即停止" };
const predicateOperatorLabels: Record<WorkflowPredicateOperator, string> = { exists: "存在", eq: "等于", neq: "不等于", gt: "大于", gte: "大于或等于", lt: "小于", lte: "小于或等于", contains: "包含", in: "属于" };
const workerKindLabels: Record<WorkflowWorkerKind, string> = { consult: "协商", dispatch: "派发" };
const transformOperationLabels: Record<WorkflowTransformOperation, string> = { identity: "保持原值", object: "组成对象", array: "组成数组" };
const deliveryKindLabels: Record<string, string> = { job_record: "任务记录", callback_ref: "回调地址", target_ref: "交付目标" };
const iterationLabels: Record<string, string> = { current_iteration: "当前迭代", previous_iteration: "上一次迭代" };

function admissionReasonLabel(reason: string) {
  return reason === "authorization_tightened" ? "当前团队授权比发布时更严格" : reason;
}

type EditorNode = WorkflowNode & { clientKey: string };
type EditorEdge = WorkflowEdge & { clientKey: string };
interface EditorState {
  trigger: WorkflowTriggerConfig;
  entryNodeID: string;
  inputContract: WorkflowContract;
  outputContract: WorkflowContract;
  nodes: EditorNode[];
  edges: EditorEdge[];
}

interface WorkflowValidationFailure {
  code: string;
  message: string;
  recovery: string;
  status: number | null;
}

function formatDate(value?: string | null) { return value ? dateTime.format(new Date(value)) : "—"; }
function rowKey() { return createClientUUID(); }
function errorFor(error: unknown, action: string) {
  const normalized = normalizeThrownError(error);
  const prefix = normalized.status === 403 ? "没有权限" : normalized.status === 409 ? "发生冲突" : normalized.status === 422 ? "服务端拒绝该内容" : "请求失败";
  return `${action}${prefix}（${normalized.status || "网络"}）：${apiErrorMessage(normalized)}`;
}
function workflowValidationFailure(error: unknown): WorkflowValidationFailure {
  const normalized = normalizeThrownError(error);
  const code = normalized.code || "workflow_validation_request_failed";
  let recovery = "草稿没有被修改。请根据错误码检查工作流依赖和节点配置后重新校验。";
  if (code === "workflow_provider_revision_required") {
    recovery = "检查图中实际执行节点：Loom 节点需选择可解析的 Provider 模型；CLI 节点使用 Runtime 默认模型时应留空模型并绑定在线 Runtime。";
  } else if (code === "workflow_artifact_invalid" || code === "workflow_frozen_manifest_mismatch") {
    recovery = "刷新并重新读取草稿，确认页面与服务端版本一致；若仍失败，请检查冻结版本、Runtime 和依赖是否完整。";
  } else if ((normalized.status || 0) >= 500) {
    recovery = "这是服务端校验失败，草稿没有被修改。可按错误码检查依赖；无法修正时保留该错误码交给平台管理员诊断。";
  }
  return { code, message: apiErrorMessage(normalized), recovery, status: normalized.status };
}
function jsonObject(raw: string): Record<string, unknown> | undefined {
  if (!raw.trim()) return undefined;
  const parsed = JSON.parse(raw) as unknown;
  if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") throw new Error("JSON Schema 必须是 JSON 对象。");
  return parsed as Record<string, unknown>;
}
function parseManualRunInput(contract: WorkflowContract | null, raw: string, booleanValue: "true" | "false" | ""): { value?: unknown; error: string | null } {
  if (!contract) return { error: "尚未读取已发布版本的输入类型。" };
  switch (contract.type) {
    case "text":
      return raw.length ? { value: raw, error: null } : { error: "请输入文本；空文本不能启动此工作流。" };
    case "json":
      if (!raw.trim()) return { error: "请输入符合运行输入要求的 JSON 值。" };
      try { return { value: JSON.parse(raw) as unknown, error: null }; }
      catch { return { error: "JSON 格式无效，请检查引号、逗号和括号。" }; }
    case "boolean":
      return booleanValue ? { value: booleanValue === "true", error: null } : { error: "请选择“是”或“否”。" };
    case "number": {
      if (!raw.trim()) return { error: "请输入数字。" };
      const parsed = Number(raw);
      return Number.isFinite(parsed) ? { value: parsed, error: null } : { error: "请输入有限数字。" };
    }
  }
}

function readableJSON(value: unknown): string {
  if (typeof value === "string") return value;
  const serialized = JSON.stringify(value, null, 2);
  return serialized === undefined ? String(value) : serialized;
}
function versionRecord(value: WorkflowVersionResponse): TeamWorkflowVersion {
  return {
    workspace_id: "", workflow_id: value.workflow_id, version: value.version, status: value.status,
    trigger_config: value.trigger_config, graph_definition: value.graph_definition, created_by: "",
    created_at: value.created_at, updated_at: value.updated_at,
  };
}
function editorFromVersion(version: Pick<TeamWorkflowVersion, "trigger_config" | "graph_definition">): EditorState {
  const graph = version.graph_definition;
  return {
    trigger: structuredClone(version.trigger_config), entryNodeID: graph.entry_node_id,
    inputContract: structuredClone(graph.input_contract), outputContract: structuredClone(graph.output_contract),
    nodes: graph.nodes.map((node) => ({ ...structuredClone(node), clientKey: rowKey() })),
    edges: graph.edges.map((edge) => ({ ...structuredClone(edge), clientKey: rowKey() })),
  };
}
function workflowNodePayload(node: EditorNode): WorkflowNode {
  return {
    id: node.id,
    type: node.type,
    label: node.label,
    inputs: node.inputs,
    output: node.output,
    config: node.config,
  };
}
function workflowEdgePayload(edge: EditorEdge): WorkflowEdge {
  return {
    id: edge.id,
    from_node_id: edge.from_node_id,
    to_node_id: edge.to_node_id,
    route: edge.route,
    priority: edge.priority,
    predicate: edge.predicate,
  };
}
function graphFromEditor(editor: EditorState): WorkflowGraphDefinition {
  return {
    schema_version: 1,
    entry_node_id: editor.entryNodeID,
    input_contract: editor.inputContract,
    output_contract: editor.outputContract,
    nodes: editor.nodes.map(workflowNodePayload),
    edges: editor.edges.map(workflowEdgePayload),
  };
}
function emptyValueRef(source: WorkflowValueSource = "run_input"): WorkflowValueRef {
  if (source === "run_input") return { source, path: "" };
  if (source === "node_output") return { source, node_id: "", path: "" };
  return { source, value: "" };
}
function defaultNode(type: WorkflowNodeType, worker?: AgentRecord): EditorNode {
  const base = { id: `${type}-${rowKey().slice(0, 6)}`, type, label: nodeLabels[type], clientKey: rowKey() };
  switch (type) {
    case "lead": return { ...base, output: { type: "text" }, config: { instruction: "整理请求并明确团队任务。" } };
    case "worker": return { ...base, output: { type: "text" }, config: { agent_id: worker?.id || "", agent_version: worker?.version || 1, kind: "dispatch", result_requirement: "返回可核验的工作结果。" } };
    case "transform": return { ...base, output: { type: "text" }, config: { operation: "identity", value: emptyValueRef() } };
    case "condition": return { ...base, config: {} };
    case "parallel": return { ...base, config: { join_node_id: "" } };
    case "join": return { ...base, config: { policy: "all_success" } };
    case "wait": return { ...base, config: { resume_schema: { type: "object" } } };
    case "loop": return { ...base, config: { max_iterations: 3, latch_node_id: "", continue_predicate: { left: emptyValueRef(), operator: "exists" } } };
    case "deliver": return { ...base, config: { result: { source: "literal", value: "完成" } } };
    case "handoff": return { ...base, config: { agent_id: worker?.id || "", agent_version: worker?.version || 1, instruction: "接管并完成剩余工作。" } };
  }
}
function artistEditor(roster: TeamRoster | null, agents: AgentRecord[]): EditorState {
  const rosterWorkers = roster?.workers.filter((worker) => worker.enabled) || [];
  const workerAgents = rosterWorkers.map((worker) => agents.find((agent) => agent.id === worker.id && !agent.deleted && agent.role === "worker")).filter((agent): agent is AgentRecord => Boolean(agent));
  const leadBrief = defaultNode("lead");
  const deliver = defaultNode("deliver");
  leadBrief.id = "lead-brief";
  leadBrief.label = "Lead · 创意简报";
  leadBrief.inputs = { brief: { expected_type: "text", value: { source: "run_input", path: "" } } };
  leadBrief.config = { instruction: "将委托任务书转化为可执行的创意简报：明确目标、约束、目标受众和验收标准，并为两名 Worker 写出明确且不重叠的任务分工。" };
  deliver.id = "deliver";
  if (workerAgents.length < 2) {
    deliver.config = { result: { source: "node_output", node_id: leadBrief.id, path: "" } };
    return {
      trigger: { schema_version: 1, type: "conversation_explicit", config: {} }, entryNodeID: leadBrief.id,
      inputContract: { type: "text" }, outputContract: { type: "text" }, nodes: [leadBrief, deliver],
      edges: [{ id: "lead-brief-deliver", from_node_id: leadBrief.id, to_node_id: deliver.id, route: "success", clientKey: rowKey() }],
    };
  }
  const parallel = defaultNode("parallel"); parallel.id = "parallel";
  const join = defaultNode("join"); join.id = "join";
  parallel.config = { join_node_id: join.id };
  const workerIA = defaultNode("worker", workerAgents[0]);
  workerIA.id = "worker-ia";
  workerIA.label = "Worker · 信息架构与内容策略";
  workerIA.inputs = { brief: { expected_type: "text", value: { source: "node_output", node_id: leadBrief.id, path: "" } } };
  workerIA.config = { agent_id: workerAgents[0].id, agent_version: workerAgents[0].version, kind: "dispatch", result_requirement: "提交具体、可执行的信息架构与内容策略提案：包含页面层级、内容分组、关键路径、信息优先级、核心文案规则，以及逐条对应简报目标与验收标准的依据。" };
  const workerVisual = defaultNode("worker", workerAgents[1]);
  workerVisual.id = "worker-visual";
  workerVisual.label = "Worker · 视觉系统与交互";
  workerVisual.inputs = { brief: { expected_type: "text", value: { source: "node_output", node_id: leadBrief.id, path: "" } } };
  workerVisual.config = { agent_id: workerAgents[1].id, agent_version: workerAgents[1].version, kind: "dispatch", result_requirement: "提交具体、可执行的视觉系统与交互提案：包含版式、层级、色彩、字体、间距、组件状态、关键交互与响应式规则，并用明确数值或行为说明对应简报约束和验收标准。" };
  const leadFinalize = defaultNode("lead");
  leadFinalize.id = "lead-finalize";
  leadFinalize.label = "Lead · 方案整合与定稿";
  leadFinalize.inputs = {
    original_brief: { expected_type: "text", value: { source: "node_output", node_id: leadBrief.id, path: "" } },
    proposals: { expected_type: "json", value: { source: "node_output", node_id: join.id, path: "" } },
  };
  leadFinalize.config = { instruction: "对照 original_brief 比较两份 proposals，明确采纳与拒绝项并解决冲突；综合成最终可实施设计规格，完整保留验收约束，逐项陈述关键决策、实现要求、风险和待验证事项。" };
  deliver.config = { result: { source: "node_output", node_id: leadFinalize.id, path: "" } };
  return {
    trigger: { schema_version: 1, type: "conversation_explicit", config: {} }, entryNodeID: leadBrief.id,
    inputContract: { type: "text" }, outputContract: { type: "text" }, nodes: [leadBrief, parallel, workerIA, workerVisual, join, leadFinalize, deliver],
    edges: [
      { id: "lead-brief-parallel", from_node_id: leadBrief.id, to_node_id: parallel.id, route: "success", clientKey: rowKey() },
      { id: "parallel-worker-ia", from_node_id: parallel.id, to_node_id: workerIA.id, route: "branch", clientKey: rowKey() },
      { id: "parallel-worker-visual", from_node_id: parallel.id, to_node_id: workerVisual.id, route: "branch", clientKey: rowKey() },
      { id: "worker-ia-join", from_node_id: workerIA.id, to_node_id: join.id, route: "join", clientKey: rowKey() },
      { id: "worker-visual-join", from_node_id: workerVisual.id, to_node_id: join.id, route: "join", clientKey: rowKey() },
      { id: "join-lead-finalize", from_node_id: join.id, to_node_id: leadFinalize.id, route: "success", clientKey: rowKey() },
      { id: "lead-finalize-deliver", from_node_id: leadFinalize.id, to_node_id: deliver.id, route: "success", clientKey: rowKey() },
    ],
  };
}

export function WorkflowsPage({ fixedTeamID }: { fixedTeamID?: string } = {}) {
  const [searchParams, setSearchParams] = useSearchParams();
  const requestedTeamID = searchParams.get("team") || "";
  const requestedWorkflowID = searchParams.get("workflow") || "";
  const { user } = useAuth();
  const canWrite = user?.role === "admin" || user?.role === "owner";
  const [teams, setTeams] = useState<Team[]>([]);
  const [agents, setAgents] = useState<AgentRecord[]>([]);
  const [teamID, setTeamID] = useState("");
  const [roster, setRoster] = useState<TeamRoster | null>(null);
  const [workflows, setWorkflows] = useState<WorkflowSummary[]>([]);
  const [selectedID, setSelectedID] = useState("");
  const [detail, setDetail] = useState<WorkflowDetailResponse | null>(null);
  const [version, setVersion] = useState<TeamWorkflowVersion | null>(null);
  const [publishedGraph, setPublishedGraph] = useState<WorkflowGraphDefinition | null>(null);
  const [editor, setEditor] = useState<EditorState | null>(null);
  const [dirty, setDirty] = useState(false);
  const [validation, setValidation] = useState<WorkflowValidationResponse | null>(null);
  const [validationFailure, setValidationFailure] = useState<WorkflowValidationFailure | null>(null);
  const [dependencies, setDependencies] = useState<WorkflowDependenciesResponse | null>(null);
  const [admission, setAdmission] = useState<WorkflowAdmissionStatusResponse | null>(null);
  const [factsError, setFactsError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [listLoading, setListLoading] = useState(false);
  const [detailLoading, setDetailLoading] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [busy, setBusy] = useState<"save" | "validate" | "publish" | "draft" | "archive" | "run" | null>(null);
  const [includeArchived, setIncludeArchived] = useState(false);
  const [creating, setCreating] = useState(false);
  const [createName, setCreateName] = useState("");
  const [createDescription, setCreateDescription] = useState("");
  const [createEditor, setCreateEditor] = useState<EditorState | null>(null);
  const [publishing, setPublishing] = useState(false);
  const [archiving, setArchiving] = useState(false);
  const [runResult, setRunResult] = useState<WorkflowManualRunResponse | null>(null);
  const [confirmingRun, setConfirmingRun] = useState(false);
  const [runInput, setRunInput] = useState("");
  const [runBoolean, setRunBoolean] = useState<"true" | "false" | "">("");
  const [historyVersion, setHistoryVersion] = useState("");

  const workerCandidates = useMemo(() => {
    const ids = new Set(roster?.workers.filter((worker) => worker.enabled).map((worker) => worker.id) || []);
    return agents.filter((agent) => !agent.deleted && agent.role === "worker" && ids.has(agent.id));
  }, [agents, roster]);
  const selected = workflows.find((workflow) => workflow.id === selectedID) || detail?.workflow || null;
  const readOnly = version?.status !== "draft" || detail?.workflow.status === "archived" || !canWrite;
  const publishedInputContract = publishedGraph?.input_contract || null;
  const parsedRunInput = useMemo(() => parseManualRunInput(publishedInputContract, runInput, runBoolean), [publishedInputContract, runBoolean, runInput]);

  const readPublishedFacts = useCallback(async (workflowID: string, publishedVersion: number | null) => {
    setDependencies(null); setAdmission(null); setFactsError(null);
    if (!publishedVersion) return;
    const controller = new AbortController();
    const results = await Promise.allSettled([
      api.getWorkflowDependencies(workflowID, publishedVersion, controller.signal),
      api.getWorkflowAdmission(workflowID, publishedVersion, controller.signal),
    ]);
    if (results[0].status === "fulfilled") setDependencies(results[0].value); else setFactsError(errorFor(results[0].reason, "读取冻结依赖"));
    if (results[1].status === "fulfilled") setAdmission(results[1].value); else setFactsError(errorFor(results[1].reason, "读取启动权限"));
  }, []);

  const readWorkflow = useCallback(async (workflowID: string, preferredVersion?: number) => {
    setSelectedID(workflowID); setDetailLoading(true); setActionError(null); setValidation(null); setValidationFailure(null); setRunResult(null); setConfirmingRun(false); setRunInput(""); setRunBoolean("");
    try {
      const nextDetail = await api.getWorkflow(workflowID);
      setDetail(nextDetail);
      const target = preferredVersion || nextDetail.draft?.version || nextDetail.published?.version;
      if (target) {
        const nextVersion = await api.getWorkflowVersion(workflowID, target);
        const normalized = versionRecord(nextVersion);
        setVersion(normalized); setEditor(editorFromVersion(normalized)); setDirty(false); setHistoryVersion(String(target));
        if (nextDetail.published?.version === target) {
          setPublishedGraph(normalized.graph_definition);
        } else if (nextDetail.published?.version) {
          const publishedVersion = await api.getWorkflowVersion(workflowID, nextDetail.published.version);
          setPublishedGraph(publishedVersion.graph_definition);
        } else {
          setPublishedGraph(null);
        }
      } else { setVersion(null); setEditor(null); setPublishedGraph(null); }
      await readPublishedFacts(workflowID, nextDetail.workflow.published_version);
    } catch (requestError) {
      setActionError(errorFor(requestError, "读取工作流详情")); setDetail(null); setVersion(null); setEditor(null);
    } finally { setDetailLoading(false); }
  }, [readPublishedFacts]);

  const readTeamWorkflows = useCallback(async (nextTeamID: string, preferredID?: string, withArchived = includeArchived) => {
    if (!nextTeamID) { setWorkflows([]); setRoster(null); setSelectedID(""); setDetail(null); setConfirmingRun(false); setRunInput(""); setRunBoolean(""); setRunResult(null); return; }
    setListLoading(true); setError(null);
    try {
      const [response, nextRoster] = await Promise.all([api.listTeamWorkflows(nextTeamID, withArchived), api.getTeam(nextTeamID)]);
      setWorkflows(response.workflows); setRoster(nextRoster);
      const nextID = preferredID && response.workflows.some((item) => item.id === preferredID) ? preferredID : response.workflows[0]?.id;
      if (nextID) { await readWorkflow(nextID); const next = new URLSearchParams(searchParams); next.set("team", nextTeamID); next.set("workflow", nextID); setSearchParams(next, { replace: true }); } else { setSelectedID(""); setDetail(null); setVersion(null); setEditor(null); setPublishedGraph(null); }
    } catch (requestError) { setError(errorFor(requestError, "读取团队工作流")); }
    finally { setListLoading(false); }
  }, [includeArchived, readWorkflow, searchParams, setSearchParams]);

  const refreshRoot = useCallback(async () => {
    setLoading(true); setError(null); setForbidden(false);
    try {
      const [nextTeams, nextAgents] = await Promise.all([api.listTeams(), api.listAgents()]);
      setTeams(nextTeams); setAgents(nextAgents.filter((agent) => !agent.deleted));
      const nextTeamID = fixedTeamID || (requestedTeamID && nextTeams.some((team) => team.id === requestedTeamID) ? requestedTeamID : teamID && nextTeams.some((team) => team.id === teamID) ? teamID : nextTeams[0]?.id || "");
      setTeamID(nextTeamID);
      await readTeamWorkflows(nextTeamID, requestedWorkflowID || selectedID || undefined);
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError); setForbidden(normalized.status === 403); setError(errorFor(normalized, "读取工作流管理页"));
    } finally { setLoading(false); }
  }, [fixedTeamID, readTeamWorkflows, requestedTeamID, requestedWorkflowID, selectedID, teamID]);
  const initialRead = useRef(refreshRoot);

  useEffect(() => { void initialRead.current(); }, []);

  function replaceEditor(next: EditorState) {
    setEditor(next); setDirty(true); setValidation(null); setValidationFailure(null); setActionError(null);
  }
  function updateNode(clientKey: string, patch: Partial<WorkflowNode>) {
    if (!editor) return;
    const currentNode = editor.nodes.find((node) => node.clientKey === clientKey);
    const nextID = patch.id;
    const renamed = currentNode && nextID !== undefined && nextID !== currentNode.id;
    replaceEditor({
      ...editor,
      entryNodeID: renamed && editor.entryNodeID === currentNode.id ? nextID : editor.entryNodeID,
      nodes: editor.nodes.map((node) => node.clientKey === clientKey ? { ...node, ...patch } : node),
      edges: editor.edges.map((edge) => !renamed ? edge : {
        ...edge,
        from_node_id: edge.from_node_id === currentNode.id ? nextID : edge.from_node_id,
        to_node_id: edge.to_node_id === currentNode.id ? nextID : edge.to_node_id,
      }),
    });
  }
  function updateEdge(clientKey: string, patch: Partial<WorkflowEdge>) {
    if (!editor) return;
    replaceEditor({ ...editor, edges: editor.edges.map((edge) => edge.clientKey === clientKey ? { ...edge, ...patch } : edge) });
  }
  function beginCreate() { setCreateName(""); setCreateDescription(""); setCreateEditor(artistEditor(roster, agents)); setActionError(null); setCreating(true); }

  async function createWorkflow(event: FormEvent) {
    event.preventDefault(); if (!teamID || !createEditor || !createName.trim()) return;
    setBusy("save"); setActionError(null);
    try {
      const created = await api.createWorkflow(teamID, { name: createName.trim(), description: createDescription, trigger_config: createEditor.trigger, graph_definition: graphFromEditor(createEditor) });
      setCreating(false); await readTeamWorkflows(teamID, created.workflow.id);
    } catch (requestError) { setActionError(errorFor(requestError, "创建工作流")); }
    finally { setBusy(null); }
  }

  async function saveDraft() {
    if (!selectedID || !version || !editor || version.status !== "draft") return;
    setBusy("save"); setActionError(null);
    try {
      const saved = await api.updateWorkflowDraft(selectedID, version.version, { expected_updated_at: version.updated_at, trigger_config: editor.trigger, graph_definition: graphFromEditor(editor) });
      setVersion(saved); setEditor(editorFromVersion(saved)); setDirty(false); setValidation(null); setValidationFailure(null);
      const nextDetail = await api.getWorkflow(selectedID); setDetail(nextDetail);
    } catch (requestError) { setActionError(errorFor(requestError, "保存草稿")); }
    finally { setBusy(null); }
  }

  async function createDraft() {
    if (!selectedID) return; setBusy("draft"); setActionError(null);
    try { const created = await api.createWorkflowDraft(selectedID); await readWorkflow(selectedID, created.version); }
    catch (requestError) { setActionError(errorFor(requestError, "从已发布版本创建草稿")); }
    finally { setBusy(null); }
  }

  async function validateDraft() {
    if (!selectedID || !version || version.status !== "draft" || dirty) return;
    setBusy("validate"); setActionError(null); setValidationFailure(null);
    try { setValidation(await api.validateWorkflowVersion(selectedID, version.version)); }
    catch (requestError) { setValidationFailure(workflowValidationFailure(requestError)); setValidation(null); }
    finally { setBusy(null); }
  }

  async function publishDraft() {
    if (!selectedID || !version || !validation?.valid || dirty) return;
    setBusy("publish"); setActionError(null);
    try { await api.publishWorkflowVersion(selectedID, version.version); setPublishing(false); await readTeamWorkflows(teamID, selectedID); }
    catch (requestError) { setActionError(errorFor(requestError, "发布工作流")); setPublishing(false); }
    finally { setBusy(null); }
  }

  async function archiveSelected() {
    if (!selectedID) return; setBusy("archive"); setActionError(null);
    try { await api.archiveWorkflow(selectedID); setArchiving(false); await readTeamWorkflows(teamID); }
    catch (requestError) { setActionError(errorFor(requestError, "归档工作流")); setArchiving(false); }
    finally { setBusy(null); }
  }

  async function runWorkflow() {
    if (!selectedID || parsedRunInput.error) return;
    setBusy("run"); setActionError(null); setRunResult(null);
    try {
      setRunResult(await api.runLegacyWorkflow(selectedID, { input: parsedRunInput.value }));
      setConfirmingRun(false); setRunInput(""); setRunBoolean("");
    }
    catch (requestError) { setActionError(errorFor(requestError, "调试运行工作流")); }
    finally { setBusy(null); }
  }

  async function readHistoricalVersion(event: FormEvent) {
    event.preventDefault(); const target = Number(historyVersion);
    if (!selectedID || !Number.isInteger(target) || target < 1) return;
    setDetailLoading(true); setActionError(null);
    try { const record = versionRecord(await api.getWorkflowVersion(selectedID, target)); setVersion(record); setEditor(editorFromVersion(record)); setDirty(false); setValidation(null); setValidationFailure(null); }
    catch (requestError) { setActionError(errorFor(requestError, "读取指定版本")); }
    finally { setDetailLoading(false); }
  }

  if (loading && !teams.length) return <section className="control-content"><LoadingView label="正在读取团队、智能体与工作流" /></section>;
  if (forbidden) return <section className="control-content"><div className="empty-surface control-forbidden"><LockKeyhole size={28} /><h2>当前账号没有查看工作流的权限</h2><p>{error}</p></div></section>;

  return <section className="control-content workflow-control" aria-labelledby="workflows-heading">
    <div className="control-section-heading"><div><h2 id="workflows-heading">工作流</h2><p>为团队编排多智能体工作流：编辑草稿、校验发布、调试运行。</p></div><div className="heading-actions"><Button variant="ghost" loading={loading || listLoading} onClick={() => void refreshRoot()}>{!(loading || listLoading) && <RefreshCw size={16} aria-hidden="true" />}{loading || listLoading ? "正在刷新" : "刷新"}</Button>{canWrite && <Button variant="primary" disabled={!teamID} onClick={beginCreate}><Plus size={16} />创建工作流</Button>}</div></div>
    {error && <ErrorNotice message={error} onRetry={() => void refreshRoot()} />}
    <div className="workflow-team-toolbar">{!fixedTeamID && <Field label="团队">{(control) => <select {...control} value={teamID} onChange={(event) => { const next = new URLSearchParams(searchParams); next.set("team", event.target.value); next.delete("workflow"); setSearchParams(next); setTeamID(event.target.value); void readTeamWorkflows(event.target.value); }}><option value="">先选择团队</option>{teams.map((team) => <option key={team.id} value={team.id}>{team.name} · {teamStatusLabels[team.status] || team.status}</option>)}</select>}</Field>}<label className="check-field"><Switch checked={includeArchived} aria-label="显示已归档" onChange={(next) => { setIncludeArchived(next); void readTeamWorkflows(teamID, selectedID || undefined, next); }} />显示已归档</label><span>{workflows.length} 个工作流</span></div>
    {!teamID ? <div className="empty-surface workflow-empty"><WorkflowIcon size={28} /><h2>先选择团队</h2><p>工作流归属于团队；选择团队后即可查看工作流和可用智能体。</p></div> : listLoading && !workflows.length ? <LoadingView label="正在读取团队工作流" /> : !workflows.length ? <div className="empty-surface workflow-empty"><WorkflowIcon size={28} /><h2>这个团队还没有工作流</h2><p>{canWrite ? "创建后即可编辑第一个草稿版本。" : "当前账号仅可查看，请联系管理员创建工作流。"}</p>{canWrite && <Button variant="primary" onClick={beginCreate}><Plus size={16} />创建第一个工作流</Button>}</div> : <div className="workflow-browser">
      <aside className="workflow-master" aria-label="工作流列表">{workflows.map((workflow) => <button key={workflow.id} type="button" className={selectedID === workflow.id ? "workflow-row active" : "workflow-row"} onClick={() => { const next = new URLSearchParams(searchParams); next.set("team", teamID); next.set("workflow", workflow.id); setSearchParams(next); void readWorkflow(workflow.id); }}><span className="workflow-row__icon"><WorkflowIcon size={16} /></span><span><strong>{workflow.name}</strong><small>{workflow.trigger_summary?.type ? triggerLabels[workflow.trigger_summary.type] || "未知触发方式" : "触发方式未识别"} · {workflow.draft_version ? `草稿 v${workflow.draft_version}` : workflow.published_version ? `已发布 v${workflow.published_version}` : "无版本"}</small></span><Badge tone={workflow.status === "archived" ? "neutral" : "success"}>{workflowStatusLabels[workflow.status] || workflow.status}</Badge><ChevronRight size={16} /></button>)}</aside>
      <div className="workflow-detail">{detailLoading ? <LoadingView label="正在读取工作流版本" /> : detail && selected && version && editor ? <>
        <header className="workflow-detail__header"><div><Badge tone={selected.status === "archived" ? "neutral" : "success"}>{workflowStatusLabels[selected.status] || selected.status}</Badge><h3>{selected.name}</h3><p>{selected.description || "没有描述"}</p></div><div className="workflow-detail__actions">{canWrite && selected.status === "active" && !detail.draft && selected.published_version && <Button loading={!!busy} onClick={() => void createDraft()}><FileCode2 size={16} />{busy === "draft" ? "正在创建…" : "从发布版本建草稿"}</Button>}{canWrite && selected.status === "active" && selected.published_version && <Button variant="primary" loading={!!busy} disabled={admission?.status === "blocked" || !publishedGraph} onClick={() => { setRunInput(""); setRunBoolean(""); setRunResult(null); setActionError(null); setConfirmingRun(true); }}><CirclePlay size={16} />调试运行</Button>}{canWrite && selected.status === "active" && <Button variant="danger" loading={!!busy} onClick={() => setArchiving(true)}><Archive size={16} />归档</Button>}</div></header>
        {actionError && <ErrorNotice message={actionError} />}
        {runResult && <div className="workflow-run-result" role="status"><CheckCircle2 size={16} /><div><strong>调试运行已创建</strong><p>已按当前草稿创建一次调试运行。</p><div><Link className="text-link" to={`/activity?kind=job&id=${encodeURIComponent(runResult.task_id)}`}>查看任务进度</Link><Link className="text-link" to={`/activity?kind=run&id=${encodeURIComponent(runResult.run_id)}`}>查看运行详情</Link></div></div></div>}
        <nav className="workflow-version-strip" aria-label="版本"><button type="button" className={version.version === detail.draft?.version ? "active" : ""} disabled={!detail.draft} onClick={() => detail.draft && void readWorkflow(selectedID, detail.draft.version)}>草稿 {detail.draft ? `v${detail.draft.version}` : "—"}</button><button type="button" className={version.version === detail.published?.version ? "active" : ""} disabled={!detail.published} onClick={() => detail.published && void readWorkflow(selectedID, detail.published.version)}>已发布 {detail.published ? `v${detail.published.version}` : "—"}</button><form onSubmit={(event) => void readHistoricalVersion(event)}><label><span className="sr-only">指定版本号</span><input type="number" min="1" value={historyVersion} onChange={(event) => setHistoryVersion(event.target.value)} placeholder="版本号" /></label><Button variant="ghost" type="submit"><History size={14} />读取</Button></form></nav>
        <div className="workflow-version-boundary"><div><strong>v{version.version} · {version.status === "draft" ? "可变草稿" : "不可变已发布版本"}</strong><small>更新时间 {formatDate(version.updated_at)}</small></div>{readOnly && <Badge tone="neutral">只读</Badge>}{dirty && <Badge tone="warning">未保存</Badge>}</div>
        {!canWrite && <div className="boundary-banner"><LockKeyhole size={16} /><div><strong>仅可查看</strong><p>仅管理员可修改。</p></div></div>}
        {!workerCandidates.length && <div className="notice notice--warning"><TriangleAlert size={16} /><span>当前团队没有可用的执行者；添加执行者后才能为节点绑定智能体版本。</span></div>}
        <WorkflowEditor editor={editor} readOnly={readOnly} workers={workerCandidates} onChange={replaceEditor} onNodeChange={updateNode} onEdgeChange={updateEdge} />
        <div className="workflow-savebar"><div><strong>{dirty ? "页面内容尚未保存" : "页面与服务端草稿一致"}</strong><small>如果草稿已被他人修改，保存会被拒绝，不会覆盖对方的内容。</small></div>{version.status === "draft" && canWrite && selected.status === "active" && <Button variant="primary" loading={!!busy} disabled={!dirty} onClick={() => void saveDraft()}><Save size={16} />{busy === "save" ? "正在保存…" : "保存草稿"}</Button>}</div>
        <ValidationPanel validation={validation} failure={validationFailure} dirty={dirty} draft={version.status === "draft"} busy={busy} canValidate={!dirty && version.status === "draft"} onValidate={() => void validateDraft()} onPublish={() => setPublishing(true)} canPublish={canWrite && selected.status === "active" && validation?.valid === true && !dirty} />
        {selected.published_version ? <PublicationFacts version={selected.published_version} publishedGraph={publishedGraph} dependencies={dependencies} admission={admission} error={factsError} /> : <section className="workflow-facts"><header><div><h4>发布依赖</h4><p>尚未发布版本；发布后这里会显示依赖快照。</p></div></header></section>}
      </> : <div className="empty-surface"><WorkflowIcon size={28} /><h2>选择一个工作流</h2><p>选择左侧工作流查看草稿、已发布版本和运行状态。</p></div>}</div>
    </div>}

    <Modal open={confirmingRun} title="调试运行" description="本次运行不归属项目；输入需符合已发布版本的输入要求。" onClose={() => { if (busy !== "run") setConfirmingRun(false); }} footer={<><Button disabled={busy === "run"} onClick={() => setConfirmingRun(false)}>取消</Button><Button variant="primary" loading={busy === "run"} disabled={Boolean(parsedRunInput.error)} onClick={() => void runWorkflow()}>{busy === "run" ? "正在创建…" : "调试运行"}</Button></>}>
      <div className="workflow-run-confirm">
        <dl><div><dt>工作流</dt><dd>{selected?.name || "—"}</dd></div><div><dt>已发布版本</dt><dd>v{detail?.workflow.published_version || "—"}</dd></div><div><dt>输入类型</dt><dd>{publishedInputContract ? valueTypeLabels[publishedInputContract.type] : "未读取"}</dd></div>{publishedInputContract?.schema && <div><dt>输入结构</dt><dd><pre>{JSON.stringify(publishedInputContract.schema, null, 2)}</pre></dd></div>}</dl>
        {publishedInputContract?.type === "boolean" ? <fieldset className="workflow-boolean-input"><legend>运行输入（布尔值）</legend><label><input type="radio" name="workflow-run-boolean" value="true" checked={runBoolean === "true"} onChange={() => setRunBoolean("true")} />是</label><label><input type="radio" name="workflow-run-boolean" value="false" checked={runBoolean === "false"} onChange={() => setRunBoolean("false")} />否</label></fieldset> : <Field label={`运行输入（${publishedInputContract ? valueTypeLabels[publishedInputContract.type] : "未知类型"}）`}>{(control) => <textarea {...control} autoFocus rows={publishedInputContract?.type === "text" || publishedInputContract?.type === "json" ? 8 : 3} inputMode={publishedInputContract?.type === "number" ? "decimal" : undefined} value={runInput} placeholder={publishedInputContract?.type === "json" ? "输入任意合法 JSON 值，例如对象、数组、字符串、数字、布尔值或 null" : publishedInputContract?.type === "number" ? "输入有限数字" : "输入将发送给工作流的完整文本"} onChange={(event) => setRunInput(event.target.value)} />}</Field>}
        {parsedRunInput.error ? <p className="workflow-run-validation" role="alert">{parsedRunInput.error}</p> : <div className="workflow-run-preview"><strong>发送预览 · {publishedInputContract ? valueTypeLabels[publishedInputContract.type] : "未知类型"}</strong><pre>{readableJSON(parsedRunInput.value)}</pre></div>}
        <p className="contract-note">本次调试运行不归属项目，只会发送上方预览中的输入。</p>
      </div>
    </Modal>
    <Modal open={creating} size="wide" title="创建团队工作流" description="创建后将生成第一个可编辑的草稿版本。" onClose={() => { if (!busy) setCreating(false); }} footer={<><Button disabled={!!busy} onClick={() => setCreating(false)}>取消</Button><Button variant="primary" type="submit" form="workflow-create" loading={!!busy} disabled={!createName.trim() || !createEditor}>{busy === "save" ? "正在创建…" : "创建工作流"}</Button></>}>
      <form id="workflow-create" className="workflow-create-form" onSubmit={(event) => void createWorkflow(event)}><div className="agent-form-grid"><Field label="名称">{(control) => <input {...control} autoFocus required value={createName} onChange={(event) => setCreateName(event.target.value)} />}</Field><Field label="团队">{(control) => <input {...control} disabled value={teams.find((team) => team.id === teamID)?.name || teamID} />}</Field></div><Field label="描述">{(control) => <textarea {...control} rows={2} value={createDescription} onChange={(event) => setCreateDescription(event.target.value)} />}</Field>{actionError && <ErrorNotice message={actionError} />}{createEditor && <WorkflowEditor editor={createEditor} readOnly={false} workers={workerCandidates} onChange={setCreateEditor} onNodeChange={(key, patch) => setCreateEditor((current) => current ? { ...current, nodes: current.nodes.map((node) => node.clientKey === key ? { ...node, ...patch } : node) } : current)} onEdgeChange={(key, patch) => setCreateEditor((current) => current ? { ...current, edges: current.edges.map((edge) => edge.clientKey === key ? { ...edge, ...patch } : edge) } : current)} />}</form>
    </Modal>
    <Modal open={publishing} title="发布不可变版本" description="发布会冻结智能体版本、团队授权、能力、依赖和交付内容。" onClose={() => { if (!busy) setPublishing(false); }} footer={<><Button disabled={!!busy} onClick={() => setPublishing(false)}>取消</Button><Button variant="primary" loading={busy === "publish"} disabled={!validation?.valid || dirty} onClick={() => void publishDraft()}>{busy === "publish" ? "正在发布…" : `发布 v${version?.version || ""}`}</Button></>}><p className="confirm-copy">发布后该版本只读；后续修改需从已发布版本创建新草稿。</p></Modal>
    <Modal open={archiving} title="归档工作流" description="归档后不可恢复，但会保留全部版本和历史。" onClose={() => { if (!busy) setArchiving(false); }} footer={<><Button disabled={!!busy} onClick={() => setArchiving(false)}>取消</Button><Button variant="danger" loading={busy === "archive"} onClick={() => void archiveSelected()}>{busy === "archive" ? "正在归档…" : "确认归档"}</Button></>}><p className="confirm-copy">确定归档“{selected?.name}”吗？归档后不能再创建草稿、保存、发布或调试运行。</p></Modal>
  </section>;
}

function WorkflowEditor({ editor, readOnly, workers, onChange, onNodeChange, onEdgeChange }: { editor: EditorState; readOnly: boolean; workers: AgentRecord[]; onChange(next: EditorState): void; onNodeChange(key: string, patch: Partial<WorkflowNode>): void; onEdgeChange(key: string, patch: Partial<WorkflowEdge>): void }) {
  function change(next: EditorState) { onChange(next); }
  function addNode() { change({ ...editor, nodes: [...editor.nodes, defaultNode("lead", workers[0])] }); }
  function changeNodeType(node: EditorNode, type: WorkflowNodeType) { const replacement = defaultNode(type, workers[0]); onNodeChange(node.clientKey, { type, output: replacement.output, config: replacement.config }); }
  function removeNode(key: string) { const removed = editor.nodes.find((node) => node.clientKey === key); change({ ...editor, nodes: editor.nodes.filter((node) => node.clientKey !== key), edges: editor.edges.filter((edge) => edge.from_node_id !== removed?.id && edge.to_node_id !== removed?.id) }); }
  function addEdge() { const first = editor.nodes[0]?.id || ""; const second = editor.nodes[1]?.id || first; change({ ...editor, edges: [...editor.edges, { id: `edge-${rowKey().slice(0, 6)}`, from_node_id: first, to_node_id: second, route: "success", clientKey: rowKey() }] }); }
  return <div className="workflow-editor">
    <section className="workflow-editor__section"><header><div><h4>触发与交付</h4><p>选择工作流的启动方式；非会话触发还需指定交付方式。</p></div></header><TriggerEditor value={editor.trigger} readOnly={readOnly} onChange={(trigger) => change({ ...editor, trigger })} /></section>
    <section className="workflow-editor__section"><header><div><h4>输入与输出格式</h4><p>定义工作流接收和返回的数据类型；JSON 类型可进一步限制数据结构。</p></div></header><div className="workflow-contract-grid"><ContractEditor label="运行输入" value={editor.inputContract} readOnly={readOnly} onChange={(inputContract) => change({ ...editor, inputContract })} /><ContractEditor label="最终输出" value={editor.outputContract} readOnly={readOnly} onChange={(outputContract) => change({ ...editor, outputContract })} /></div></section>
    <section className="workflow-editor__section"><header><div><h4>节点</h4><p>为每一步选择职责、智能体和输出规则。</p></div>{!readOnly && <Button variant="ghost" onClick={addNode}><Plus size={14} />添加节点</Button>}</header><div className="workflow-entry"><Field label="入口节点" help="流程从这里开始执行。">{(control) => <select {...control} disabled={readOnly} value={editor.entryNodeID} onChange={(event) => change({ ...editor, entryNodeID: event.target.value })}><option value="">选择入口节点</option>{editor.nodes.map((node) => <option key={node.clientKey} value={node.id}>{node.id} · {nodeTypeLabels[node.type]}</option>)}</select>}</Field></div><div className="workflow-node-list">{editor.nodes.map((node, index) => <NodeEditor key={node.clientKey} node={node} index={index} nodes={editor.nodes} workers={workers} readOnly={readOnly} onType={(type) => changeNodeType(node, type)} onChange={(patch) => onNodeChange(node.clientKey, patch)} onRemove={() => removeNode(node.clientKey)} />)}</div></section>
    <section className="workflow-editor__section"><header><div><h4>连线</h4><p>连接节点并设置不同结果对应的流转路径。</p></div>{!readOnly && <Button variant="ghost" disabled={editor.nodes.length < 2} onClick={addEdge}><Plus size={14} />添加连线</Button>}</header><div className="workflow-edge-list">{editor.edges.map((edge, index) => <EdgeEditor key={edge.clientKey} edge={edge} index={index} nodes={editor.nodes} readOnly={readOnly} onChange={(patch) => onEdgeChange(edge.clientKey, patch)} onRemove={() => change({ ...editor, edges: editor.edges.filter((item) => item.clientKey !== edge.clientKey) })} />)}{!editor.edges.length && <p className="structured-list__empty">还没有连线。只有起点、终点和连线规则合法的图才能发布。</p>}</div></section>

  </div>;
}

function TriggerEditor({ value, readOnly, onChange }: { value: WorkflowTriggerConfig; readOnly: boolean; onChange(value: WorkflowTriggerConfig): void }) {
  const configField = triggerConfigField(value);
  const session = value.type === "conversation_explicit" || value.type === "conversation_auto";

  function changeConfig(next: string) {
    switch (value.type) {
      case "conversation_auto": onChange({ ...value, config: { catalog_key: next } }); break;
      case "schedule": onChange({ ...value, config: { schedule_id: next } }); break;
      case "api": onChange({ ...value, config: { endpoint_key: next } }); break;
      case "event": onChange({ ...value, config: { event_type: next } }); break;
    }
  }

  return <div className="workflow-trigger-grid"><Field label="触发类型">{(control) => <select {...control} disabled={readOnly} value={value.type} onChange={(event) => onChange(triggerForType(event.target.value as WorkflowTriggerType))}>{triggerTypes.map((type) => <option key={type} value={type}>{triggerLabels[type]}</option>)}</select>}</Field>{configField && <Field label={configField.label} help={configField.help}>{(control) => <input {...control} disabled={readOnly} value={configField.value} onChange={(event) => changeConfig(event.target.value)} />}</Field>}{!session && <><Field label="交付方式" help="运行结果如何送达：记入任务记录、回调通知或写入交付目标。">{(control) => <select {...control} disabled={readOnly} value={value.delivery?.kind || "job_record"} onChange={(event) => { const kind = event.target.value as NonNullable<WorkflowTriggerConfig["delivery"]>["kind"]; onChange({ ...value, delivery: kind === "job_record" ? { kind } : { kind, ref: "" } }); }}><option value="job_record">{deliveryKindLabels.job_record}</option><option value="callback_ref">{deliveryKindLabels.callback_ref}</option><option value="target_ref">{deliveryKindLabels.target_ref}</option></select>}</Field>{value.delivery?.kind !== "job_record" && <Field label="交付目标引用" help="填写回调地址或交付目标的引用标识。">{(control) => <input {...control} disabled={readOnly} value={value.delivery?.ref || ""} onChange={(event) => onChange({ ...value, delivery: { kind: value.delivery?.kind || "target_ref", ref: event.target.value } })} />}</Field>}</>}</div>;
}

function triggerForType(type: WorkflowTriggerType): WorkflowTriggerConfig {
  switch (type) {
    case "conversation_explicit": return { schema_version: 1, type, config: {} };
    case "conversation_auto": return { schema_version: 1, type, config: { catalog_key: "" } };
    case "schedule": return { schema_version: 1, type, config: { schedule_id: "" }, delivery: { kind: "job_record" } };
    case "api": return { schema_version: 1, type, config: { endpoint_key: "" }, delivery: { kind: "job_record" } };
    case "event": return { schema_version: 1, type, config: { event_type: "" }, delivery: { kind: "job_record" } };
  }
}

function triggerConfigField(value: WorkflowTriggerConfig): { label: string; field: string; help: string; value: string } | null {
  switch (value.type) {
    case "conversation_explicit": return null;
    case "conversation_auto": return { label: "目录键", field: "catalog_key", help: "该流程在自动选择目录中的标识。", value: "catalog_key" in value.config ? value.config.catalog_key : "" };
    case "schedule": return { label: "定时计划", field: "schedule_id", help: "触发该流程的定时计划标识。", value: "schedule_id" in value.config ? value.config.schedule_id : "" };
    case "api": return { label: "接入点标识", field: "endpoint_key", help: "外部系统调用该流程时使用的接入标识。", value: "endpoint_key" in value.config ? value.config.endpoint_key : "" };
    case "event": return { label: "事件类型", field: "event_type", help: "触发该流程的业务事件类型。", value: "event_type" in value.config ? value.config.event_type : "" };
  }
}

function ContractEditor({ label, value, readOnly, onChange }: { label: string; value: WorkflowContract; readOnly: boolean; onChange(value: WorkflowContract): void }) {
  const [schemaText, setSchemaText] = useState(value.schema ? JSON.stringify(value.schema, null, 2) : "");
  const [schemaError, setSchemaError] = useState("");
  useEffect(() => { setSchemaText(value.schema ? JSON.stringify(value.schema, null, 2) : ""); setSchemaError(""); }, [value.schema]);
  function commitSchema(text: string) { setSchemaText(text); try { onChange({ ...value, schema: jsonObject(text) }); setSchemaError(""); } catch (error) { setSchemaError(error instanceof Error ? error.message : "Schema 无效"); } }
  return <fieldset className="workflow-contract"><legend>{label}</legend><Field label="数据类型" help="该内容的数据形态；结构化内容可选择 JSON 并约束结构。">{(control) => <select {...control} disabled={readOnly} value={value.type} onChange={(event) => { const type = event.target.value as WorkflowValueType; onChange(type === "json" ? { type, schema: value.schema } : { type }); }}>{valueTypes.map((type) => <option key={type} value={type}>{valueTypeLabels[type]}</option>)}</select>}</Field>{value.type === "json" && <Field label="JSON 结构定义（可选）" help="使用 JSON Schema 对象约束输入或输出结构。" error={schemaError || undefined}>{(control) => <textarea {...control} className="code-input" rows={4} disabled={readOnly} value={schemaText} onChange={(event) => commitSchema(event.target.value)} />}</Field>}</fieldset>;
}

function NodeEditor({ node, index, nodes, workers, readOnly, onType, onChange, onRemove }: { node: EditorNode; index: number; nodes: EditorNode[]; workers: AgentRecord[]; readOnly: boolean; onType(type: WorkflowNodeType): void; onChange(patch: Partial<WorkflowNode>): void; onRemove(): void }) {
  const outputRequired = node.type === "lead" || node.type === "worker" || node.type === "transform";
  const legacyType = !nodeTypes.includes(node.type);
  return <fieldset className="workflow-node"><legend>节点 {index + 1} · {nodeTypeLabels[node.type]}</legend><div className="workflow-node__heading"><Field label="ID">{(control) => <input {...control} disabled={readOnly} value={node.id} onChange={(event) => onChange({ id: event.target.value })} />}</Field><Field label="节点类型">{(control) => <select {...control} disabled={readOnly} value={node.type} onChange={(event) => onType(event.target.value as WorkflowNodeType)}>{legacyType && <option value={node.type}>{nodeTypeLabels[node.type]}（旧类型）</option>}{nodeTypes.map((type) => <option key={type} value={type}>{nodeTypeLabels[type]}</option>)}</select>}</Field><Field label="显示名称（可选）" help="在流程结构中展示的名字；留空使用节点 ID。">{(control) => <input {...control} disabled={readOnly} value={node.label || ""} onChange={(event) => onChange({ label: event.target.value || undefined })} />}</Field>{!readOnly && <button className="icon-button icon-button--danger" type="button" aria-label={`删除节点 ${node.id}`} onClick={onRemove}><Trash2 size={16} /></button>}</div><NodeConfigEditor node={node} nodes={nodes} workers={workers} readOnly={readOnly} onChange={(next) => onChange({ config: next })} />{outputRequired && <ContractEditor label="节点输出格式" value={node.output || { type: "text" }} readOnly={readOnly} onChange={(output) => onChange({ output })} />}<InputsEditor value={node.inputs || {}} readOnly={readOnly} nodes={nodes} onChange={(inputs) => onChange({ inputs: Object.keys(inputs).length ? inputs : undefined })} /></fieldset>;
}

function NodeConfigEditor({ node, nodes, workers, readOnly, onChange }: { node: WorkflowNode; nodes: EditorNode[]; workers: AgentRecord[]; readOnly: boolean; onChange(value: WorkflowNode["config"]): void }) {
  const config = node.config as Record<string, unknown>;
  const worker = workers.find((agent) => agent.id === config.agent_id);
  if (node.type === "lead") return <Field label="任务指令" help="说明该节点要完成的工作。">{(control) => <textarea {...control} disabled={readOnly} rows={2} value={String(config.instruction || "")} onChange={(event) => onChange({ instruction: event.target.value })} />}</Field>;
  if (node.type === "worker") return <div className="agent-form-grid"><AgentBinding workers={workers} readOnly={readOnly} agentID={String(config.agent_id || "")} version={Number(config.agent_version || 1)} onChange={(agentID, agentVersion) => onChange({ ...config, agent_id: agentID, agent_version: agentVersion } as WorkflowNode["config"])} /><Field label="协作方式" help="同步咨询会等待结果返回；异步派工在后台执行后汇总。">{(control) => <select {...control} disabled={readOnly} value={String(config.kind || "consult")} onChange={(event) => onChange({ ...config, kind: event.target.value as WorkflowWorkerKind } as WorkflowNode["config"])}><option value="consult">{workerKindLabels.consult}</option><option value="dispatch">{workerKindLabels.dispatch}</option></select>}</Field><div className="workflow-grid-span"><Field label="结果要求" help="说明执行者必须返回的内容和验收要求。">{(control) => <textarea {...control} disabled={readOnly} rows={2} value={String(config.result_requirement || "")} onChange={(event) => onChange({ ...config, result_requirement: event.target.value } as WorkflowNode["config"])} />}</Field></div>{worker && <p className="candidate-note workflow-grid-span">已绑定{worker.display_name || worker.name}；发布后将固定使用其当前版本。</p>}</div>;
  if (node.type === "transform") return <TransformEditor config={config} readOnly={readOnly} nodes={nodes} onChange={onChange} />;
  if (node.type === "condition") return <p className="contract-note">条件节点的判断规则设置在“条件分支”连线上。</p>;
  if (node.type === "parallel") return <NodeSelect label="汇合节点" fieldName="join_node_id" help="选择所有并行分支最终汇合的节点。" value={String(config.join_node_id || "")} nodes={nodes.filter((item) => item.type === "join")} readOnly={readOnly} onChange={(join_node_id) => onChange({ join_node_id })} />;
  if (node.type === "join") return <div className="agent-form-grid"><Field label="汇合策略" help="并行分支如何收束：全部成功、达到数量或截止。">{(control) => <select {...control} disabled={readOnly} value={String(config.policy || "all_success")} onChange={(event) => { const policy = event.target.value as WorkflowJoinPolicy; onChange(policy === "quorum" ? { policy, success_count: 1 } : policy === "deadline" ? { policy, deadline_seconds: 60 } : { policy }); }}>{joinPolicies.map((policy) => <option key={policy} value={policy}>{joinPolicyLabels[policy]}</option>)}</select>}</Field>{config.policy === "quorum" && <NumberField label="所需成功数" fieldName="success_count" value={Number(config.success_count || 1)} readOnly={readOnly} onChange={(success_count) => onChange({ policy: "quorum", success_count })} />}{config.policy === "deadline" && <NumberField label="截止时间（秒）" fieldName="deadline_seconds" value={Number(config.deadline_seconds || 60)} readOnly={readOnly} onChange={(deadline_seconds) => onChange({ policy: "deadline", deadline_seconds })} />}</div>;
  if (node.type === "wait") return <SchemaTimeoutEditor schemaLabel="恢复输入结构" schemaField="resume_schema" schema={(config.resume_schema || {}) as Record<string, unknown>} timeout={config.timeout_seconds as number | undefined} readOnly={readOnly} onChange={(schema, timeout) => onChange({ resume_schema: schema, ...(timeout ? { timeout_seconds: timeout } : {}) })} />;
  if (node.type === "loop") return <div className="workflow-config-stack"><div className="agent-form-grid"><NumberField label="最大迭代次数" fieldName="max_iterations" value={Number(config.max_iterations || 1)} readOnly={readOnly} onChange={(max_iterations) => onChange({ ...config, max_iterations } as WorkflowNode["config"])} /><NodeSelect label="循环锁定节点" fieldName="latch_node_id" help="选择每轮完成后用于判断是否继续的节点。" value={String(config.latch_node_id || "")} nodes={nodes} readOnly={readOnly} onChange={(latch_node_id) => onChange({ ...config, latch_node_id } as WorkflowNode["config"])} /></div><PredicateEditor label="继续循环条件" fieldName="continue_predicate" value={config.continue_predicate as WorkflowPredicate} nodes={nodes} readOnly={readOnly} onChange={(continue_predicate) => onChange({ ...config, continue_predicate } as WorkflowNode["config"])} /></div>;
  if (node.type === "deliver") return <ValueRefEditor label="交付内容" fieldName="result" value={config.result as WorkflowValueRef} nodes={nodes} readOnly={readOnly} onChange={(result) => onChange({ result })} />;
  return <div className="workflow-config-stack"><AgentBinding workers={workers} readOnly={readOnly} agentID={String(config.agent_id || "")} version={Number(config.agent_version || 1)} onChange={(agent_id, agent_version) => onChange({ ...config, agent_id, agent_version } as WorkflowNode["config"])} /><Field label="移交指令" help="说明接手的成员要完成的工作。">{(control) => <textarea {...control} disabled={readOnly} rows={2} value={String(config.instruction || "")} onChange={(event) => onChange({ ...config, instruction: event.target.value } as WorkflowNode["config"])} />}</Field><OptionalTimeout value={config.timeout_seconds as number | undefined} readOnly={readOnly} onChange={(timeout_seconds) => onChange({ ...config, ...(timeout_seconds ? { timeout_seconds } : {}) } as WorkflowNode["config"])} /></div>;
}

function AgentBinding({ workers, readOnly, agentID, onChange }: { workers: AgentRecord[]; readOnly: boolean; agentID: string; version: number; onChange(id: string, version: number): void }) {
  return <><Field label="执行成员" help="从当前团队的成员中选择执行者。">{(control) => <select {...control} disabled={readOnly || !workers.length} value={agentID} onChange={(event) => { const agent = workers.find((item) => item.id === event.target.value); onChange(event.target.value, agent?.version || 1); }}><option value="">选择团队执行者</option>{workers.map((agent) => <option key={agent.id} value={agent.id}>{agent.display_name || agent.name}</option>)}{agentID && !workers.some((agent) => agent.id === agentID) && <option value={agentID}>{agentID}（当前候选中不存在）</option>}</select>}</Field></>;
}
function NodeSelect({ label, help, value, nodes, readOnly, onChange }: { label: string; fieldName?: string; help?: string; value: string; nodes: EditorNode[]; readOnly: boolean; onChange(value: string): void }) { return <Field label={label} help={help}>{(control) => <select {...control} disabled={readOnly} value={value} onChange={(event) => onChange(event.target.value)}><option value="">选择节点</option>{nodes.map((node) => <option key={node.clientKey} value={node.id}>{node.id} · {nodeTypeLabels[node.type]}</option>)}</select>}</Field>; }
function NumberField({ label, value, min = 1, readOnly, onChange }: { label: string; fieldName?: string; value: number; min?: number; readOnly: boolean; onChange(value: number): void }) { return <Field label={label}>{(control) => <input {...control} type="number" min={min} step="1" disabled={readOnly} value={value} onChange={(event) => onChange(Number(event.target.value))} />}</Field>; }
function OptionalTimeout({ value, readOnly, onChange }: { value?: number; readOnly: boolean; onChange(value?: number): void }) { return <Field label="超时时间（秒，可选）" help="超过该时长未完成视为超时。">{(control) => <input {...control} type="number" min="1" step="1" disabled={readOnly} value={value || ""} onChange={(event) => onChange(event.target.value ? Number(event.target.value) : undefined)} />}</Field>; }
function SchemaTimeoutEditor({ schemaLabel, schema, timeout, readOnly, onChange }: { schemaLabel: string; schemaField?: string; schema: Record<string, unknown>; timeout?: number; readOnly: boolean; onChange(schema: Record<string, unknown>, timeout?: number): void }) {
  const [text, setText] = useState(JSON.stringify(schema, null, 2)); const [error, setError] = useState("");
  useEffect(() => { setText(JSON.stringify(schema, null, 2)); setError(""); }, [schema]);
  return <div className="agent-form-grid"><Field label={schemaLabel} help="填写 JSON Schema 对象。" error={error || undefined}>{(control) => <textarea {...control} className="code-input" rows={5} disabled={readOnly} value={text} onChange={(event) => { setText(event.target.value); try { onChange(jsonObject(event.target.value) || {}, timeout); setError(""); } catch (parseError) { setError(parseError instanceof Error ? parseError.message : "JSON 结构无效"); } }} />}</Field><OptionalTimeout value={timeout} readOnly={readOnly} onChange={(value) => onChange(schema, value)} /></div>;
}

function InputsEditor({ value, readOnly, nodes, onChange }: { value: Record<string, WorkflowInputBinding>; readOnly: boolean; nodes: EditorNode[]; onChange(value: Record<string, WorkflowInputBinding>): void }) {
  const entries = Object.entries(value);
  return <details className="workflow-inputs"><summary>节点输入绑定 · {entries.length}</summary><div>{entries.map(([name, binding]) => <fieldset key={name}><legend>{name}</legend><div className="workflow-input-heading"><Field label="输入名称">{(control) => <input {...control} disabled={readOnly} value={name} onChange={(event) => { const next = { ...value }; delete next[name]; next[event.target.value] = binding; onChange(next); }} />}</Field><Field label="预期类型" help="该输入的数据形态。">{(control) => <select {...control} disabled={readOnly} value={binding.expected_type} onChange={(event) => onChange({ ...value, [name]: { ...binding, expected_type: event.target.value as WorkflowValueType } })}>{valueTypes.map((type) => <option key={type} value={type}>{valueTypeLabels[type]}</option>)}</select>}</Field>{!readOnly && <button className="icon-button icon-button--danger" type="button" aria-label={`删除输入 ${name}`} onClick={() => { const next = { ...value }; delete next[name]; onChange(next); }}><Trash2 size={14} /></button>}</div><ValueRefEditor label="输入值" fieldName="value" value={binding.value} nodes={nodes} readOnly={readOnly} onChange={(nextValue) => onChange({ ...value, [name]: { ...binding, value: nextValue } })} /></fieldset>)}{!readOnly && <Button variant="ghost" onClick={() => { let name = "input"; let index = 1; while (value[name]) { index += 1; name = `input_${index}`; } onChange({ ...value, [name]: { expected_type: "text", value: emptyValueRef() } }); }}><Plus size={14} />添加输入</Button>}</div></details>;
}

function ValueRefEditor({ label, value = emptyValueRef(), nodes, readOnly, onChange }: { label: string; fieldName?: string; value?: WorkflowValueRef; nodes: EditorNode[]; readOnly: boolean; onChange(value: WorkflowValueRef): void }) {
  const [literalText, setLiteralText] = useState(typeof value.value === "string" ? JSON.stringify(value.value) : JSON.stringify(value.value ?? ""));
  const [defaultText, setDefaultText] = useState(JSON.stringify(value.default?.value ?? ""));
  const [literalError, setLiteralError] = useState("");
  const [defaultError, setDefaultError] = useState("");
  useEffect(() => {
    setLiteralText(typeof value.value === "string" ? JSON.stringify(value.value) : JSON.stringify(value.value ?? ""));
    setDefaultText(JSON.stringify(value.default?.value ?? ""));
    setLiteralError(""); setDefaultError("");
  }, [value.value, value.default?.value]);
  return <fieldset className="value-ref"><legend>{label}</legend><div className="value-ref__grid">
    <Field label="来源" help="取值来自固定内容、用户输入或某个节点的输出。">{(control) => <select {...control} disabled={readOnly} value={value.source} onChange={(event) => onChange(emptyValueRef(event.target.value as WorkflowValueSource))}>{valueSources.map((source) => <option key={source} value={source}>{valueSourceLabels[source]}</option>)}</select>}</Field>
    {value.source === "node_output" && <NodeSelect label="来源节点" fieldName="node_id" value={value.node_id || ""} nodes={nodes} readOnly={readOnly} onChange={(node_id) => onChange({ ...value, node_id })} />}
    {value.source !== "literal" && <Field label="数据路径（可选）" help="使用 JSON Pointer 读取输入或节点输出中的指定位置；留空表示使用完整内容。">{(control) => <input {...control} disabled={readOnly} value={value.path || ""} onChange={(event) => onChange({ ...value, path: event.target.value })} />}</Field>}
    {value.source === "node_output" && <Field label="迭代范围（可选）" help="在循环节点内引用哪一轮的输出。">{(control) => <select {...control} disabled={readOnly} value={value.iteration || ""} onChange={(event) => onChange({ ...value, iteration: event.target.value ? event.target.value as WorkflowValueRef["iteration"] : undefined })}><option value="">当前图</option><option value="current_iteration">{iterationLabels.current_iteration}</option><option value="previous_iteration">{iterationLabels.previous_iteration}</option></select>}</Field>}
    {value.source === "node_output" && <label className="check-field workflow-grid-span"><Switch disabled={readOnly} checked={Boolean(value.default)} aria-label="节点输出缺失时使用默认值" onChange={(next) => onChange({ ...value, default: next ? { source: "literal", value: "" } : undefined })} />节点输出缺失时使用默认值</label>}
    {value.source === "node_output" && value.default && <div className="workflow-grid-span"><Field label="默认值" help="填写任意合法 JSON 值。" error={defaultError || undefined}>{(control) => <textarea {...control} className="code-input" rows={2} disabled={readOnly} value={defaultText} onChange={(event) => { setDefaultText(event.target.value); try { onChange({ ...value, default: { source: "literal", value: JSON.parse(event.target.value) as unknown } }); setDefaultError(""); } catch { setDefaultError("请输入有效的 JSON 值。"); } }} />}</Field></div>}
    {value.source === "literal" && <div className="workflow-grid-span"><Field label="固定值" help="填写任意合法 JSON 值。" error={literalError || undefined}>{(control) => <textarea {...control} className="code-input" rows={2} disabled={readOnly} value={literalText} onChange={(event) => { setLiteralText(event.target.value); try { onChange({ source: "literal", value: JSON.parse(event.target.value) as unknown }); setLiteralError(""); } catch { setLiteralError("请输入有效的 JSON 值。"); } }} />}</Field></div>}
  </div></fieldset>;
}

function TransformEditor({ config, readOnly, nodes, onChange }: { config: Record<string, unknown>; readOnly: boolean; nodes: EditorNode[]; onChange(value: WorkflowNode["config"]): void }) {
  const operation = (config.operation || "identity") as WorkflowTransformOperation;
  function changeOperation(next: WorkflowTransformOperation) { onChange(next === "identity" ? { operation: next, value: emptyValueRef() } : next === "object" ? { operation: next, fields: { field: emptyValueRef() } } : { operation: next, items: [emptyValueRef()] }); }
  return <div className="workflow-config-stack"><Field label="转换方式" help="原样传递、组装为对象或组装为数组。">{(control) => <select {...control} disabled={readOnly} value={operation} onChange={(event) => changeOperation(event.target.value as WorkflowTransformOperation)}><option value="identity">{transformOperationLabels.identity}</option><option value="object">{transformOperationLabels.object}</option><option value="array">{transformOperationLabels.array}</option></select>}</Field>{operation === "identity" && <ValueRefEditor label="原始值" fieldName="value" value={config.value as WorkflowValueRef} nodes={nodes} readOnly={readOnly} onChange={(value) => onChange({ operation, value })} />}{operation === "object" && <ValueRefMapEditor value={(config.fields || {}) as Record<string, WorkflowValueRef>} nodes={nodes} readOnly={readOnly} onChange={(fields) => onChange({ operation, fields })} />}{operation === "array" && <ValueRefListEditor value={(config.items || []) as WorkflowValueRef[]} nodes={nodes} readOnly={readOnly} onChange={(items) => onChange({ operation, items })} />}</div>;
}
function ValueRefMapEditor({ value, nodes, readOnly, onChange }: { value: Record<string, WorkflowValueRef>; nodes: EditorNode[]; readOnly: boolean; onChange(value: Record<string, WorkflowValueRef>): void }) { return <div className="workflow-config-stack">{Object.entries(value).map(([name, ref]) => <div className="transform-value" key={name}><Field label="字段名">{(control) => <input {...control} disabled={readOnly} value={name} onChange={(event) => { const next = { ...value }; delete next[name]; next[event.target.value] = ref; onChange(next); }} />}</Field><ValueRefEditor label="字段值" value={ref} nodes={nodes} readOnly={readOnly} onChange={(nextRef) => onChange({ ...value, [name]: nextRef })} />{!readOnly && <button className="icon-button icon-button--danger" type="button" aria-label={`删除字段 ${name}`} onClick={() => { const next = { ...value }; delete next[name]; onChange(next); }}><Trash2 size={14} /></button>}</div>)}{!readOnly && <Button variant="ghost" onClick={() => onChange({ ...value, [`field_${Object.keys(value).length + 1}`]: emptyValueRef() })}><Plus size={14} />添加字段</Button>}</div>; }
function ValueRefListEditor({ value, nodes, readOnly, onChange }: { value: WorkflowValueRef[]; nodes: EditorNode[]; readOnly: boolean; onChange(value: WorkflowValueRef[]): void }) { return <div className="workflow-config-stack">{value.map((ref, index) => <div className="transform-value" key={index}><ValueRefEditor label={`数组项 ${index + 1}`} value={ref} nodes={nodes} readOnly={readOnly} onChange={(next) => onChange(value.map((item, itemIndex) => itemIndex === index ? next : item))} />{!readOnly && <button className="icon-button icon-button--danger" type="button" aria-label={`删除数组项 ${index + 1}`} onClick={() => onChange(value.filter((_item, itemIndex) => itemIndex !== index))}><Trash2 size={14} /></button>}</div>)}{!readOnly && <Button variant="ghost" onClick={() => onChange([...value, emptyValueRef()])}><Plus size={14} />添加数组项</Button>}</div>; }

function PredicateEditor({ label, value = { left: emptyValueRef(), operator: "exists" }, nodes, readOnly, onChange }: { label: string; fieldName?: string; value?: WorkflowPredicate; nodes: EditorNode[]; readOnly: boolean; onChange(value: WorkflowPredicate): void }) { return <fieldset className="workflow-predicate"><legend>{label}</legend><ValueRefEditor label="左侧值" fieldName="left" value={value.left} nodes={nodes} readOnly={readOnly} onChange={(left) => onChange({ ...value, left })} /><Field label="比较方式">{(control) => <select {...control} disabled={readOnly} value={value.operator} onChange={(event) => { const operator = event.target.value as WorkflowPredicateOperator; onChange(operator === "exists" ? { left: value.left, operator } : { ...value, operator, right: value.right || { source: "literal", value: "" } }); }}>{predicateOperators.map((operator) => <option key={operator} value={operator}>{predicateOperatorLabels[operator]}</option>)}</select>}</Field>{value.operator !== "exists" && <ValueRefEditor label="右侧值" fieldName="right" value={value.right} nodes={nodes} readOnly={readOnly} onChange={(right) => onChange({ ...value, right })} />}</fieldset>; }

function EdgeEditor({ edge, index, nodes, readOnly, onChange, onRemove }: { edge: EditorEdge; index: number; nodes: EditorNode[]; readOnly: boolean; onChange(patch: Partial<WorkflowEdge>): void; onRemove(): void }) {
  const legacyRoute = !edgeRoutes.includes(edge.route);
  return <fieldset className="workflow-edge"><legend>连线 {index + 1}</legend><div className="workflow-edge__grid"><Field label="ID">{(control) => <input {...control} disabled={readOnly} value={edge.id} onChange={(event) => onChange({ id: event.target.value })} />}</Field><NodeSelect label="起点" fieldName="from_node_id" value={edge.from_node_id} nodes={nodes} readOnly={readOnly} onChange={(from_node_id) => onChange({ from_node_id })} /><NodeSelect label="终点" fieldName="to_node_id" value={edge.to_node_id} nodes={nodes} readOnly={readOnly} onChange={(to_node_id) => onChange({ to_node_id })} /><Field label="流转方式" help="顺序流转到下一节点，或按条件选择分支。">{(control) => <select {...control} disabled={readOnly} value={edge.route} onChange={(event) => { const route = event.target.value as WorkflowEdgeRoute; onChange({ route, ...(route === "case" ? {} : { priority: undefined, predicate: undefined }) }); }}>{legacyRoute && <option value={edge.route}>{edgeRouteLabels[edge.route]}（旧路线）</option>}{edgeRoutes.map((route) => <option key={route} value={route}>{edgeRouteLabels[route]}</option>)}</select>}</Field>{!readOnly && <button className="icon-button icon-button--danger" type="button" aria-label={`删除连线 ${edge.id}`} onClick={onRemove}><Trash2 size={16} /></button>}</div>{edge.route === "case" && <div className="workflow-case"><Field label="优先级（可选，非负）" help="多条分支同时命中时，数值小者优先。">{(control) => <input {...control} type="number" min="0" step="1" disabled={readOnly} value={edge.priority ?? ""} onChange={(event) => onChange({ priority: event.target.value ? Number(event.target.value) : undefined })} />}</Field><PredicateEditor label="分支条件" fieldName="predicate" value={edge.predicate} nodes={nodes} readOnly={readOnly} onChange={(predicate) => onChange({ predicate })} /></div>}</fieldset>;
}

function ValidationPanel({ validation, failure, dirty, draft, busy, canValidate, canPublish, onValidate, onPublish }: { validation: WorkflowValidationResponse | null; failure: WorkflowValidationFailure | null; dirty: boolean; draft: boolean; busy: string | null; canValidate: boolean; canPublish: boolean; onValidate(): void; onPublish(): void }) {
  return <section className="workflow-validation"><header><div><h4>校验与发布</h4><p>发布前需先通过服务端校验；所有问题消除后才能发布。</p></div><div>{draft && <Button loading={!!busy} disabled={!canValidate} onClick={onValidate}><ShieldCheck size={16} />{busy === "validate" ? "正在校验…" : "校验"}</Button>}<Button variant="primary" loading={!!busy} disabled={!canPublish} onClick={onPublish}>发布</Button></div></header>{dirty ? <div className="notice notice--warning"><TriangleAlert size={16} /><span>请先保存草稿，再校验已保存的版本。</span></div> : !draft ? <p className="contract-note">已发布版本不可再次校验或发布。</p> : failure ? <div className="workflow-issues" role="alert"><div className="notice notice--error"><TriangleAlert size={16} /><span>校验请求失败；草稿未被修改。</span></div><article><header><code>{failure.code}</code><span>{failure.status ? `HTTP ${failure.status}` : "网络错误"}</span></header><strong>{failure.message}</strong><p>{failure.recovery}</p></article></div> : !validation ? <p className="contract-note">当前保存版本尚未校验。</p> : validation.valid ? <div className="notice workflow-valid"><CheckCircle2 size={16} /><span>校验已通过，可以发布。</span></div> : <div className="workflow-issues"><div className="notice notice--error"><TriangleAlert size={16} /><span>发现 {validation.issues.length} 个问题；全部解决后才能发布。</span></div>{validation.issues.map((issue, index) => <article key={`${issue.phase}:${issue.path}:${issue.code}:${issue.occurrence}:${index}`}><header><code>{issue.code}</code><span>阶段 {issue.phase} · 序号 {issue.occurrence}</span></header><strong>{issue.path || "/"}</strong>{issue.node_id && <small>节点 ID · {issue.node_id}</small>}<p>{issue.message}</p></article>)}</div>}</section>;
}

function PublicationFacts({ version, publishedGraph, dependencies, admission, error }: { version: number; publishedGraph: WorkflowGraphDefinition | null; dependencies: WorkflowDependenciesResponse | null; admission: WorkflowAdmissionStatusResponse | null; error: string | null }) {
  const bindings = publishedGraph?.nodes.flatMap((node) => {
    const config = node.config as Record<string, unknown>;
    return node.type === "worker" || node.type === "handoff" ? [{ node: node.id, type: node.type, agent: String(config.agent_id || ""), version: Number(config.agent_version || 0), kind: node.type === "worker" ? String(config.kind || "") : "handoff" }] : [];
  }) || [];
  return <section className="workflow-facts"><header><div><h4>发布 v{version} · 依赖</h4><p>查看发布时固定的依赖和智能体版本。</p></div>{admission?.status === "blocked" && <Badge tone="danger">{admissionLabels.blocked}</Badge>}</header>{error && <ErrorNotice message={error} />}{admission?.status === "blocked" && <div className="workflow-admission"><dl><div><dt>启动状态</dt><dd>{admissionLabels.blocked}</dd></div><div><dt>更新时间</dt><dd>{formatDate(admission.updated_at)}</dd></div><div><dt>原因</dt><dd>{admission.reasons.length ? admission.reasons.map(admissionReasonLabel).join("、") : "无"}</dd></div></dl></div>}{dependencies && <><div className="workflow-fact-grid"><div><h5><Link2 size={14} />冻结依赖</h5>{dependencies.dependencies.length ? dependencies.dependencies.map((dependency, index) => <article key={`${dependency.kind}:${dependency.key}:${index}`}><strong>{dependency.kind} · {dependency.key}</strong><small>所有者 {dependency.owner}</small></article>) : <p>暂无冻结依赖。</p>}</div><div><h5><Bot size={14} />已发布版本的智能体绑定</h5>{publishedGraph === null ? <p>切换到已发布版本后显示执行者和移交节点的智能体绑定。</p> : bindings.length ? bindings.map((binding) => <article key={`${binding.node}:${binding.agent}:${binding.version}`}><strong>{binding.node} · {nodeTypeLabels[binding.type as WorkflowNodeType]}</strong><small>{binding.agent}（版本 {binding.version}） · {binding.kind === "handoff" ? "移交" : workerKindLabels[binding.kind as WorkflowWorkerKind] || binding.kind}</small></article>) : <p>该发布版本没有执行者或移交节点绑定。</p>}</div></div></>}</section>;
}
