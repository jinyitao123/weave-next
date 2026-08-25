import { useEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode } from "react";
import { Check, ChevronDown, GitFork, Pencil, Search, Terminal, Wrench, X } from "lucide-react";
import {
  api,
  apiErrorMessage,
  type AssistantExecutionMetadata,
  type ExecutionSegment,
  type TaskGroupDetail,
  type TeamRoster,
  type TerminalOutcome,
  type ToolCallRecord,
} from "../../api";
import { useWorkspace } from "../../workspace/useWorkspace";
import { executionStepLabel, memberFallbackLabel, UUID_PATTERN as uuidPattern } from "../../workspace/labels";
import { PixelAvatar } from "../../ui/PixelAvatar";
import { MarkdownText } from "./MarkdownText";
import { toolAction, type ToolAction } from "./executionToolAction";
import { toolFailureCounts } from "./executionToolFailures";
import { terminalTurnStateTitle } from "./terminalOutcome";

type ExecutionStatus = "waiting" | "running" | "completed" | "failed" | "blocked" | "yielded" | "stopped";

interface ExecutionNode {
  id: string;
  agent: string;
  input: string;
  output: string;
  error?: string;
  status: ExecutionStatus;
  startedAt?: string;
  completedAt?: string;
  tools: ToolCallRecord[];
}

interface ExecutionGroup {
  id: string;
  nodes: ExecutionNode[];
  parallel: boolean;
}

interface ExecutionProcessProps {
  execution: AssistantExecutionMetadata;
  live?: boolean;
  activity?: string;
  agentOutputs?: Record<string, string>;
  segments?: ExecutionSegment[];
  teamId?: string;
  titleOverride?: string;
  terminalOutcome?: TerminalOutcome;
}

interface DispatchExecutionProcessProps {
  groupId: string;
  terminal?: boolean;
  revision?: number;
  fallbackLegs?: Array<{ agent?: string; worker?: string; status?: string }>;
}

interface TraceCanvasProps {
  root?: ExecutionNode;
  groups: ExecutionGroup[];
  live: boolean;
  nameForAgent(agent: string): string;
}

interface IdentifierDirectory {
  nameForAgent(identifier: string): string;
  resolveIdentifier(identifier: string): string | undefined;
}

const terminalTaskStatuses = new Set(["completed", "failed", "cancelled", "superseded", "cut", "timed_out", "resolved"]);

function parseRecord(value?: string): Record<string, unknown> {
  if (!value) return {};
  try {
    const parsed: unknown = JSON.parse(value);
    return typeof parsed === "object" && parsed !== null && !Array.isArray(parsed) ? parsed as Record<string, unknown> : {};
  } catch {
    return {};
  }
}

function parseParallelResult(value?: string): Array<Record<string, unknown>> {
  if (!value) return [];
  try {
    const parsed: unknown = JSON.parse(value);
    return Array.isArray(parsed) ? parsed.filter((item): item is Record<string, unknown> => typeof item === "object" && item !== null && !Array.isArray(item)) : [];
  } catch {
    return [];
  }
}

function executionStatus(status: string, fallback: ExecutionStatus = "waiting"): ExecutionStatus {
  switch (status) {
    case "success":
    case "completed":
    case "resolved":
      return "completed";
    case "error":
    case "failed":
    case "timed_out":
    case "cancelled":
    case "cut":
      return "failed";
    case "running":
    case "active":
    case "resolving":
    case "queued":
    case "dispatched":
      return "running";
    case "yielded":
      return "yielded";
    case "stopped":
      return "stopped";
    case "blocked":
      return "blocked";
    default:
      return fallback;
  }
}

function statusLabel(status: ExecutionStatus): string {
  switch (status) {
    case "running": return "执行中";
    case "completed": return "已完成";
    case "failed": return "失败";
    case "blocked": return terminalTurnStateTitle("blocked");
    case "yielded": return "等待输入";
    case "stopped": return "已停止";
    default: return "等待中";
  }
}

function durationLabel(startedAt?: string, completedAt?: string, now = Date.now()): string {
  if (!startedAt) return "";
  const start = Date.parse(startedAt);
  const end = completedAt ? Date.parse(completedAt) : now;
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return "";
  const seconds = Math.max(0, Math.floor((end - start) / 1000));
  const minutes = Math.floor(seconds / 60);
  return minutes ? `${minutes}m ${seconds % 60}s` : `${seconds}s`;
}

function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [active]);
  return now;
}

function identifierLabel(identifier: string): string {
  return memberFallbackLabel(identifier);
}

function shortIdentifier(identifier: string): string {
  return uuidPattern.test(identifier) ? "一位成员" : identifier;
}

function AgentAvatar({ agent }: { agent: string }) {
  return <PixelAvatar seed={agent} size={28} className="execution-avatar" />;
}

function ActivitySignal() {
  return <span className="execution-activity-signal" aria-hidden="true"><i /><i /><i /></span>;
}

function IOBlock({ label, value, streaming = false, collapsible = false }: { label: string; value: string; streaming?: boolean; collapsible?: boolean }) {
  const displayValue = value || "未产生输出";
  const content = <pre className={streaming ? "execution-streaming-text" : undefined}>{displayValue}</pre>;
  if (collapsible) {
    return <details className="execution-io-block execution-io-block--collapsible"><summary><strong>{label}</strong><span className="execution-io-block__preview">{value.slice(0, 60)}{value.length > 60 ? "…" : ""}</span></summary>{content}</details>;
  }
  return <section className="execution-io-block"><strong>{label}</strong>{content}</section>;
}

function useAgentDirectory(rootIdentifier?: string, explicitTeamID?: string): IdentifierDirectory {
  const { agents, teams } = useWorkspace();
  const [roster, setRoster] = useState<TeamRoster | null>(null);
  const rootAgent = agents.find((agent) => agent.id === rootIdentifier || agent.name === rootIdentifier);
  const teamID = explicitTeamID || teams.find((team) => team.lead_avatar_id === rootAgent?.id)?.id;

  useEffect(() => {
    let active = true;
    setRoster(null);
    if (!teamID) return () => { active = false; };
    void api.getTeam(teamID).then((next) => {
      if (active) setRoster(next);
    }).catch(() => undefined);
    return () => { active = false; };
  }, [teamID]);

  const names = useMemo(() => {
    const next = new Map<string, string>();
    agents.forEach((agent) => {
      const label = agent.display_name || agent.name;
      next.set(agent.id, label);
      next.set(agent.name, label);
    });
    teams.forEach((team) => next.set(team.id, team.name));
    if (roster?.lead) {
      next.set(roster.lead.id, roster.lead.display_name || roster.lead.name);
      next.set(roster.lead.name, roster.lead.display_name || roster.lead.name);
    }
    roster?.workers.forEach((worker) => {
      next.set(worker.id, worker.display_name || worker.name);
      next.set(worker.name, worker.display_name || worker.name);
    });
    return next;
  }, [agents, roster, teams]);

  return useMemo(() => ({
    nameForAgent: (identifier: string) => names.get(identifier) || identifierLabel(identifier),
    resolveIdentifier: (identifier: string) => names.get(identifier),
  }), [names]);
}

function NodeCard({ node, label, phase, sequence, selected, onSelect }: {
  node: ExecutionNode;
  label: string;
  phase: string;
  sequence: number | string;
  selected: boolean;
  onSelect(): void;
}) {
  const active = node.status === "running";
  const now = useNow(active);
  const duration = durationLabel(node.startedAt, node.completedAt, now);
  return <button type="button" className={`execution-node-card execution-node-card--${node.status}${selected ? " is-selected" : ""}`} aria-pressed={selected} onClick={onSelect}>
    <AgentAvatar agent={node.agent} />
    <span className="execution-node-card__identity"><strong>{label}</strong><small>{phase}</small></span>
    <span className="execution-node-card__state">{active && <ActivitySignal />}<em>{statusLabel(node.status)}</em>{duration && <time>{duration}</time>}</span>
    <code>{String(sequence).padStart(2, "0")}</code>
  </button>;
}

function Connector({ status, label }: { status: ExecutionStatus; label: string }) {
  const state = status === "running" ? "is-active" : status === "completed" ? "is-complete" : "";
  return <div className={`execution-connector ${state}`} aria-hidden="true">
    <svg viewBox="0 0 100 24" preserveAspectRatio="none"><path className="execution-connector__base" d="M50 0V24" /><path className="execution-connector__flow" d="M50 0V24" /></svg>
    <span>{label}</span>
  </div>;
}

function groupStatus(group: ExecutionGroup): ExecutionStatus {
  if (group.nodes.some((node) => node.status === "running")) return "running";
  if (group.nodes.some((node) => node.status === "failed")) return "failed";
  if (group.nodes.length > 0 && group.nodes.every((node) => node.status === "completed")) return "completed";
  return group.nodes[0]?.status || "waiting";
}

function ParallelStage({ group, sequence, expanded, selectedKey, nameForAgent, onToggle, onSelect }: {
  group: ExecutionGroup;
  sequence: number;
  expanded: boolean;
  selectedKey: string;
  nameForAgent(agent: string): string;
  onToggle(): void;
  onSelect(node: ExecutionNode): void;
}) {
  const status = groupStatus(group);
  const failed = group.nodes.filter((node) => node.status === "failed").length;
  const running = group.nodes.filter((node) => node.status === "running").length;
  const completed = group.nodes.filter((node) => node.status === "completed").length;
  const branchPositions = group.nodes.map((_, index) => ((index + 0.5) / group.nodes.length) * 100);
  const branchPath = group.nodes.length === 1
    ? "M50 0V26"
    : `M50 0V12M${branchPositions[0]} 12H${branchPositions[branchPositions.length - 1]}${branchPositions.map((position) => `M${position} 12V26`).join("")}`;
  const branchWidth = Math.max(320, group.nodes.length * 168);
  if (expanded) return <div className={`execution-parallel-fanout execution-parallel-fanout--${status}`} aria-label={`${group.parallel ? "并行" : "委派"}协作成员`}>
    <div className="execution-parallel-fanout__inner" style={{ minWidth: `${branchWidth}px` }}>
      <svg className="execution-parallel-fanout__lines" viewBox="0 0 100 26" preserveAspectRatio="none" aria-hidden="true">
        <path className="execution-parallel-fanout__base" d={branchPath} />
        <path className="execution-parallel-fanout__flow" d={branchPath} />
      </svg>
      <button type="button" className="execution-parallel-junction" aria-label="收拢协作成员" title="收拢协作成员" onClick={onToggle}><GitFork size={14} aria-hidden="true" /></button>
      <div className="execution-parallel-branches">
        {group.nodes.map((node, index) => <div className="execution-parallel-branch" key={node.id} style={{ "--branch-offset": `${(group.nodes.length - 1 - index * 2) * 42}%`, "--branch-index": index } as CSSProperties}>
          <NodeCard node={node} label={nameForAgent(node.agent)} phase={statusLabel(node.status)} sequence={`${String(sequence).padStart(2, "0")}.${index + 1}`} selected={selectedKey === node.id} onSelect={() => onSelect(node)} />
        </div>)}
      </div>
    </div>
  </div>;
  return <div className={`execution-parallel-group execution-parallel-group--${status}`}>
    <button type="button" className="execution-parallel-summary" aria-expanded={expanded} onClick={onToggle}>
      <span className="execution-avatar-stack">{group.nodes.slice(0, 3).map((node) => <AgentAvatar key={node.id} agent={node.agent} />)}{group.nodes.length > 3 && <b>+{group.nodes.length - 3}</b>}</span>
      <span className="execution-parallel-summary__identity"><strong>{group.parallel ? "并行协作" : "委派协作"} · {group.nodes.length} 名成员</strong><small>{running ? `${running} 执行中` : `${completed} 已完成`}{failed ? ` · ${failed} 失败` : ""}</small></span>
      <span className="execution-parallel-summary__state">{status === "running" && <ActivitySignal />}<em>{statusLabel(status)}</em><ChevronDown className={expanded ? "is-open" : ""} size={14} aria-hidden="true" /></span>
      <code>{String(sequence).padStart(2, "0")}</code>
    </button>
  </div>;
}

function ToolRecord({ tool }: { tool: ToolCallRecord }) {
  const status = executionStatus(tool.status);
  return <details className="execution-tool-record">
    <summary><ToolStatusIcon status={status} /><code>{tool.name}</code><ChevronDown size={14} aria-hidden="true" /></summary>
    <div><IOBlock label="参数" value={tool.args || ""} /><IOBlock label="结果" value={tool.result || ""} streaming={status === "running"} /></div>
  </details>;
}

function ToolStatusIcon({ status }: { status: ExecutionStatus }) {
  if (status === "running") return <ActivitySignal />;
  if (status === "failed") return <X size={14} aria-hidden="true" className="execution-tool-icon execution-tool-icon--failed" />;
  return <Check size={14} aria-hidden="true" className="execution-tool-icon execution-tool-icon--complete" />;
}

function compactJSON(value: string | undefined, resolveIdentifier: (identifier: string) => string | undefined): string {
  if (!value) return "";
  const trimmed = value.trim();
  if (!trimmed) return "";
  try {
    const parsed: unknown = JSON.parse(trimmed);
    if (typeof parsed !== "object" || parsed === null) return compactValue(parsed, 40, resolveIdentifier);
    if (Array.isArray(parsed)) return "…";
    const record = parsed as Record<string, unknown>;
    const identityKey = ["team_name", "team_id", "agent_name", "agent_id", "agent", "worker_name", "worker_id", "worker"].find((key) => record[key] !== undefined && record[key] !== null);
    const identity = identityKey ? compactValue(record[identityKey], 40, resolveIdentifier) : "";
    const version = typeof record.version === "number" || typeof record.version === "string" ? compactValue(record.version, 40, resolveIdentifier) : "";
    if (identity === "…") return identity;
    if (identity && version) return `${identity} · v${version.replace(/^v/i, "")}`;
    const preferred = ["team_name", "team_id", "agent_name", "agent_id", "agent", "worker_name", "worker_id", "worker", "workflow_id", "run_id", "query", "message", "target", "version"];
    const keys = [...preferred, ...Object.keys(record)].filter((key, index, all) => all.indexOf(key) === index);
    const entries = keys.flatMap((key) => {
      const entry = record[key];
      if (entry === undefined || entry === null || typeof entry === "object") return [];
      const summary = compactValue(entry, 40, resolveIdentifier);
      return summary ? [{ key, summary }] : [];
    }).slice(0, 2);
    if (entries.length === 0) return "…";
    if (entries.length === 1 && identityKey === entries[0].key) return entries[0].summary;
    return entries.map(({ key, summary }) => `${key}: ${summary}`).join(", ");
  } catch {
    if (trimmed.startsWith("{") || trimmed.startsWith("[")) return "…";
    const oneLine = trimmed.replace(/\s+/g, " ");
    return compactValue(oneLine, 40, resolveIdentifier);
  }
}

function parseJSONRecord(value?: string): Record<string, unknown> | null {
  if (!value) return null;
  try {
    const parsed: unknown = JSON.parse(value);
    return typeof parsed === "object" && parsed !== null && !Array.isArray(parsed) ? parsed as Record<string, unknown> : null;
  } catch {
    return null;
  }
}

function sameJSONValue(left: unknown, right: unknown): boolean {
  return JSON.stringify(left) === JSON.stringify(right);
}

function adjacentRetryDiff(previousArgs?: string, currentArgs?: string): string {
  const previous = parseJSONRecord(previousArgs);
  const current = parseJSONRecord(currentArgs);
  if (!previous || !current) return "";
  const changed: string[] = [];
  for (const key of new Set([...Object.keys(previous), ...Object.keys(current)])) {
    if (sameJSONValue(previous[key], current[key])) continue;
    const previousChild = previous[key];
    const currentChild = current[key];
    if (previousChild && currentChild && typeof previousChild === "object" && typeof currentChild === "object" && !Array.isArray(previousChild) && !Array.isArray(currentChild)) {
      const left = previousChild as Record<string, unknown>;
      const right = currentChild as Record<string, unknown>;
      const childChanges = [...new Set([...Object.keys(left), ...Object.keys(right)])]
        .filter((childKey) => !sameJSONValue(left[childKey], right[childKey]));
      if (childChanges.length > 0 && childChanges.length <= 3) {
        changed.push(...childChanges.map((childKey) => `${key}.${childKey}`));
        continue;
      }
    }
    changed.push(key);
  }
  if (changed.length === 0) return "参数同上";
  return changed.length <= 3 ? `参数同上，差异：${changed.join("、")}` : "";
}

function compactValue(value: unknown, maxLength: number, resolveIdentifier: (identifier: string) => string | undefined): string {
  let text = "";
  if (typeof value === "string") {
    text = uuidPattern.test(value) ? resolveIdentifier(value) || "…" : value;
  } else if (typeof value === "number" || typeof value === "boolean") {
    text = String(value);
  } else if (value && typeof value === "object") {
    text = "…";
  }
  const oneLine = text.replace(/\s+/g, " ").trim();
  return oneLine.length > maxLength ? `${oneLine.slice(0, maxLength)}…` : oneLine;
}

function collectTraceNames(value: unknown, names: Map<string, string>): void {
  if (Array.isArray(value)) {
    value.forEach((item) => collectTraceNames(item, names));
    return;
  }
  if (!value || typeof value !== "object") return;
  const record = value as Record<string, unknown>;
  Object.entries(record).forEach(([key, identifier]) => {
    if (typeof identifier !== "string" || !uuidPattern.test(identifier)) return;
    const stem = key === "id" ? "" : key.replace(/_id$/, "");
    const candidates = stem
      ? [`${stem}_name`, `${stem}_display_name`, ...(["agent", "team", "worker"].includes(stem) ? ["display_name", "name"] : [])]
      : ["display_name", "name"];
    const label = candidates.map((candidate) => record[candidate]).find((candidate): candidate is string => typeof candidate === "string" && candidate.trim().length > 0);
    if (label) names.set(identifier, label);
  });
  Object.values(record).forEach((item) => collectTraceNames(item, names));
}

function traceIdentifierResolver(segments: ExecutionSegment[], resolveIdentifier: (identifier: string) => string | undefined): (identifier: string) => string | undefined {
  const names = new Map<string, string>();
  segments.forEach((segment) => {
    if (segment.type !== "tool" || !segment.tool.result) return;
    try {
      collectTraceNames(JSON.parse(segment.tool.result), names);
    } catch {
      return;
    }
  });
  return (identifier: string) => resolveIdentifier(identifier) || names.get(identifier);
}

function ThoughtTimelineItem({ segment, live, nameForAgent }: { segment: Extract<ExecutionSegment, { type: "text" }>; live: boolean; nameForAgent(agent: string): string }) {
  return <li className="execution-timeline__item execution-timeline__item--text">
    <div className={`execution-timeline__text${live ? " execution-timeline__text--streaming" : ""}`}>
      <small>{nameForAgent(segment.agent)}</small>
      <MarkdownText text={segment.content} />
    </div>
  </li>;
}

function stepName(label: string): string {
  return label.trim().replace(/^(?:正在|已)?执行\s*/, "");
}

function displayStepLabel(label: string): { label: string; mapped: boolean } {
  return executionStepLabel(stepName(label));
}

function fallbackSegments(root: ExecutionNode, tools: ToolCallRecord[]): ExecutionSegment[] {
  const next: ExecutionSegment[] = [];
  if (root.output.trim()) next.push({ id: "fallback-output", type: "text", agent: root.agent, content: root.output, created_at: root.completedAt || root.startedAt || new Date(0).toISOString() });
  tools.forEach((tool, index) => next.push({ id: `fallback-tool-${tool.call_id || index}`, type: "tool", agent: tool.agent || root.agent, tool, created_at: tool.started_at || root.startedAt || new Date(0).toISOString() }));
  return next;
}

interface ToolSegmentGroup {
  id: string;
  tools: Array<Extract<ExecutionSegment, { type: "tool" }>>;
}

type NarrativeSegment = Extract<ExecutionSegment, { type: "text" }> | ToolSegmentGroup;

function dominantToolAction(tools: ToolCallRecord[]): ToolAction {
  const counts: Record<ToolAction, number> = { read: 0, execute: 0, write: 0, generic: 0 };
  tools.forEach((tool) => { counts[toolAction(tool.name)] += 1; });
  return (Object.entries(counts) as Array<[ToolAction, number]>).sort((left, right) => right[1] - left[1])[0][0];
}

function ToolGroupIcon({ tools }: { tools: ToolCallRecord[] }) {
  const action = dominantToolAction(tools);
  if (action === "write") return <Pencil size={14} aria-hidden="true" />;
  if (action === "execute") return <Terminal size={14} aria-hidden="true" />;
  if (action === "read") return <Search size={14} aria-hidden="true" />;
  return <Wrench size={14} aria-hidden="true" />;
}

function toolGroupLabel(tools: ToolCallRecord[]): string {
  if (tools.every((tool) => /^tf_(?:list|get)_/i.test(tool.name))) return `已查看团队与配置 · ${tools.length} 个工具`;
  const actions = new Set(tools.map((tool) => toolAction(tool.name)));
  if (!actions.has("generic") && actions.size === 1) {
    if (actions.has("read")) return tools.length === 1 ? "已查看相关信息" : `已读取 ${tools.length} 项文件与数据`;
    if (actions.has("execute")) return `已运行 ${tools.length} 个命令`;
    if (actions.has("write")) return tools.length === 1 ? "已更新内容" : `已完成 ${tools.length} 项写入与更新`;
  }
  if (!actions.has("generic") && actions.size === 2 && actions.has("read") && actions.has("execute")) return `已读取信息、运行了命令 · ${tools.length} 个工具`;
  return `已调用 ${tools.length} 个工具`;
}

function narrativeSegments(segments: ExecutionSegment[]): NarrativeSegment[] {
  const entries: NarrativeSegment[] = [];
  let pending: Array<Extract<ExecutionSegment, { type: "tool" }>> = [];
  const flushTools = () => {
    if (pending.length === 0) return;
    entries.push({ id: pending[0].id || `tool-group-${entries.length}`, tools: pending });
    pending = [];
  };
  segments.forEach((segment) => {
    if (segment.type === "step") return;
    if (segment.type === "tool") {
      pending.push(segment);
      return;
    }
    flushTools();
    entries.push(segment);
  });
  flushTools();
  return entries;
}

function ToolGroup({ group, followingTools, resolveIdentifier }: { group: ToolSegmentGroup; followingTools: ToolCallRecord[]; resolveIdentifier(identifier: string): string | undefined }) {
  const tools = group.tools.map((segment) => segment.tool);
  const failures = toolFailureCounts(followingTools, tools.length);
  return <li className="execution-timeline__item execution-timeline__item--tool-group">
    <details className="execution-tool-group">
      <summary>
        <ToolGroupIcon tools={tools} />
        <span>{toolGroupLabel(tools)}</span>
        {failures.retryCount > 0 && <em className="execution-tool-group__retries">{failures.retryCount} 次重试</em>}
        {failures.unrecoveredCount > 0 && <em>{failures.unrecoveredCount} 项失败</em>}
        <ChevronDown size={14} aria-hidden="true" />
      </summary>
      <div className="execution-tool-group__items">
        {group.tools.map((segment, index) => {
          const tool = segment.tool;
          const status = executionStatus(tool.status);
          const previous = index > 0 ? group.tools[index - 1].tool : undefined;
          const retryDiff = previous?.name === tool.name ? adjacentRetryDiff(previous.args, tool.args) : "";
          const args = retryDiff || compactJSON(tool.args, resolveIdentifier);
          return <details className={`execution-timeline-tool execution-timeline-tool--${status}`} key={segment.id || `${tool.call_id || tool.name}:${index}`}>
            <summary>
              <code className="execution-timeline-tool__name">{tool.name}</code>
              {args && <span className="execution-timeline-tool__args">{args}</span>}
              {status === "failed" && <em>失败</em>}
              {status === "running" && <em>执行中</em>}
              <ChevronDown size={14} aria-hidden="true" />
            </summary>
            <div className="execution-timeline-tool__record"><IOBlock label="参数" value={tool.args || ""} /><IOBlock label="结果" value={tool.result || (status === "running" ? "执行中…" : "")} streaming={status === "running"} /></div>
          </details>;
        })}
      </div>
    </details>
  </li>;
}

function ExecutionTimeline({ segments, live, nameForAgent, resolveIdentifier }: { segments: ExecutionSegment[]; live: boolean; nameForAgent(agent: string): string; resolveIdentifier(identifier: string): string | undefined }) {
  if (segments.length === 0) return <p className="execution-trace__empty">{live ? "等待执行输出…" : "未产生可回放的执行记录"}</p>;
  const resolveTraceIdentifier = traceIdentifierResolver(segments, resolveIdentifier);
  const entries = narrativeSegments(segments);
  const hasText = entries.some((entry) => !("tools" in entry) && entry.type === "text" && entry.content.trim().length > 0);
  let lastTextIndex = -1;
  for (let index = entries.length - 1; index >= 0; index--) {
    if (!("tools" in entries[index])) {
      lastTextIndex = index;
      break;
    }
  }
  return <ol className="execution-timeline" aria-label={live ? "实时执行过程" : "执行过程"}>
    {!hasText && <li className="execution-timeline__item execution-timeline__item--text">
      <div className="execution-timeline__text">
        <small>过程说明</small>
        <p>{live ? "当前阶段尚未产生可展示的文本输出，正在通过工具读取、校验或提交数据。" : "本轮没有留下可回放的文本输出；下方显示的是已持久化的工具记录。"}</p>
      </div>
    </li>}
    {entries.map((segment, index) => {
      if ("tools" in segment) {
        const followingTools = entries.slice(index).flatMap((entry) => "tools" in entry ? entry.tools.map((item) => item.tool) : []);
        return <ToolGroup key={segment.id} group={segment} followingTools={followingTools} resolveIdentifier={resolveTraceIdentifier} />;
      }
      if (segment.type === "text") {
        return <ThoughtTimelineItem key={segment.id || `text-${index}`} segment={segment} live={live && index === lastTextIndex} nameForAgent={nameForAgent} />;
      }
      return null;
    })}
  </ol>;
}

function SelectionInspector({ node, label }: { node: ExecutionNode; label: string }) {
  const active = node.status === "running";
  const now = useNow(active);
  const duration = durationLabel(node.startedAt, node.completedAt, now);
  return <section className={`execution-selection execution-selection--${node.status}`} aria-label={`${label} 执行记录`}>
    <header><AgentAvatar agent={node.agent} /><span><strong>{label}</strong><small>{shortIdentifier(node.agent)}</small></span><em>{active && <ActivitySignal />}{statusLabel(node.status)}{duration && ` · ${duration}`}</em></header>
    <div className="execution-selection__io"><IOBlock label="输入" value={node.input} collapsible /><IOBlock label="输出" value={node.output} streaming={active} /></div>
    {node.error && <p className="execution-selection__error">{node.error}</p>}
    {node.tools.length > 0 && <div className="execution-selection__tools" aria-label="工具调用记录">{node.tools.map((tool, index) => <ToolRecord key={`${tool.call_id || tool.name}:${index}`} tool={tool} />)}</div>}
  </section>;
}

function TraceCanvas({ root, groups, live, nameForAgent }: TraceCanvasProps) {
  const firstChild = groups.flatMap((group) => group.nodes)[0];
  const initialKey = live && firstChild ? firstChild.id : root ? "root:start" : firstChild?.id || "";
  const [selectedKey, setSelectedKey] = useState(initialKey);
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(() => new Set());
  const autoSelected = useRef(Boolean(firstChild));
  const nodesByKey = useMemo(() => {
    const next = new Map<string, ExecutionNode>();
    if (root) {
      next.set("root:start", root);
      next.set("root:merge", root);
    }
    groups.forEach((group) => group.nodes.forEach((node) => next.set(node.id, node)));
    return next;
  }, [groups, root]);
  const runningChild = groups.flatMap((group) => group.nodes).find((node) => node.status === "running");
  const groupsSettled = groups.every((group) => {
    const status = groupStatus(group);
    return status === "completed" || status === "failed" || status === "stopped";
  });

  useEffect(() => {
    if (!live || autoSelected.current || !runningChild) return;
    autoSelected.current = true;
    setSelectedKey(runningChild.id);
  }, [live, runningChild]);

  const selectedNode = nodesByKey.get(selectedKey) || root || firstChild;
  const toggleGroup = (id: string) => setExpandedGroups((current) => {
    const next = new Set(current);
    if (next.has(id)) next.delete(id); else next.add(id);
    return next;
  });

  return <>
    {groups.length > 0 && <div className="execution-topology">
      {root && <div className="execution-topology__single"><NodeCard node={root} label={nameForAgent(root.agent)} phase="接收任务与规划" sequence={1} selected={selectedKey === "root:start"} onSelect={() => setSelectedKey("root:start")} /></div>}
      {groups.map((group, index) => <div key={group.id} className="execution-topology__stage">
        {root || index > 0 ? <Connector status={groupStatus(group)} label={group.parallel ? "并行分派" : "委派"} /> : null}
        <div className="execution-topology__cluster"><ParallelStage group={group} sequence={root ? index + 2 : index + 1} expanded={expandedGroups.has(group.id)} selectedKey={selectedKey} nameForAgent={nameForAgent} onToggle={() => toggleGroup(group.id)} onSelect={(node) => setSelectedKey(node.id)} /></div>
      </div>)}
      {root && groups.length > 0 && (groupsSettled
        ? <><Connector status={root.status} label="已汇合" /><div className="execution-topology__single"><NodeCard node={root} label={nameForAgent(root.agent)} phase={root.status === "running" ? "汇总与终审" : "结果已签发"} sequence={groups.length + 2} selected={selectedKey === "root:merge"} onSelect={() => setSelectedKey("root:merge")} /></div></>
        : <div className="execution-join-wait" aria-label="等待所有分支完成"><Connector status="running" label="等待汇合" /><span><i aria-hidden="true" />分支完成后由 Lead 汇总</span></div>)}
    </div>}
    {selectedNode && <SelectionInspector node={selectedNode} label={nameForAgent(selectedNode.agent)} />}
  </>;
}

function nodesFromExecution(execution: AssistantExecutionMetadata, agentOutputs: Record<string, string>): { root: ExecutionNode; groups: ExecutionGroup[] } {
  const calls = execution.tool_calls || [];
  const rootTools = calls.filter((call) => call.agent === execution.agent && call.name !== "delegate" && call.name !== "dispatch_parallel");
  const root: ExecutionNode = {
    id: "root",
    agent: execution.agent,
    input: execution.input,
    output: agentOutputs[execution.agent] ?? execution.output,
    status: executionStatus(execution.status),
    startedAt: execution.started_at,
    completedAt: execution.completed_at,
    tools: rootTools,
  };
  const groups: ExecutionGroup[] = [];
  calls.forEach((call, callIndex) => {
    if (call.name === "delegate") {
      const args = parseRecord(call.args);
      const agent = typeof args.agent === "string" ? args.agent : `delegate-${callIndex + 1}`;
      groups.push({ id: call.call_id || `delegate-${callIndex}`, parallel: false, nodes: [{
        id: call.call_id || `${agent}-${callIndex}`,
        agent,
        input: typeof args.message === "string" ? args.message : call.args || "",
        output: agentOutputs[agent] ?? call.result ?? "",
        status: executionStatus(call.status),
        startedAt: call.started_at,
        completedAt: call.completed_at,
        tools: calls.filter((tool) => tool.agent === agent && tool.name !== "delegate" && tool.name !== "dispatch_parallel"),
      }] });
      return;
    }
    if (call.name === "dispatch_parallel") {
      const args = parseRecord(call.args);
      const tasks = Array.isArray(args.tasks) ? args.tasks.filter((item): item is Record<string, unknown> => typeof item === "object" && item !== null && !Array.isArray(item)) : [];
      const results = parseParallelResult(call.result);
      const nodes = tasks.map((task, taskIndex): ExecutionNode => {
        const agent = typeof task.worker === "string" ? task.worker : typeof task.agent === "string" ? task.agent : `worker-${taskIndex + 1}`;
        const result = results.find((item) => item.worker === agent || item.agent === agent) || results[taskIndex] || {};
        const error = typeof result.error === "string" ? result.error : "";
        const output = typeof result.output === "string" ? result.output : call.status === "error" && !error ? call.result || "" : "";
        return {
          id: `${call.call_id || callIndex}:${agent}:${taskIndex}`,
          agent,
          input: typeof task.message === "string" ? task.message : "",
          output: agentOutputs[agent] ?? output,
          error,
          status: error ? "failed" : executionStatus(call.status),
          startedAt: call.started_at,
          completedAt: call.completed_at,
          tools: calls.filter((tool) => tool.agent === agent && tool.name !== "delegate" && tool.name !== "dispatch_parallel"),
        };
      });
      if (nodes.length > 0) groups.push({ id: call.call_id || `parallel-${callIndex}`, parallel: true, nodes });
    }
  });
  return { root, groups };
}

function toolCatalog(tools: ToolCallRecord[], showFailures: boolean): string {
  if (tools.length === 0) return "";
  if (!showFailures) return `${tools.length} 次工具调用`;
  const { unrecoveredCount } = toolFailureCounts(tools);
  return unrecoveredCount > 0 ? `${tools.length} 次工具调用 · ${unrecoveredCount} 次未恢复失败` : `${tools.length} 次工具调用`;
}

function actionLabel(value?: string): string {
  if (!value) return "";
  return displayStepLabel(value).label;
}

function processStatusPhrase(status: ExecutionStatus, active: boolean): string {
  if (active) return "运行中";
  switch (status) {
    case "completed": return "已完成";
    case "failed": return "失败";
    case "blocked": return "规划受阻";
    case "yielded": return "等待输入";
    case "stopped": return "已停止";
    default: return statusLabel(status);
  }
}

function ProcessFrame({ status, startedAt, completedAt, activity, participantLabel, currentAction, toolSummary, children, defaultOpen = false, titleOverride }: { status: ExecutionStatus; startedAt?: string; completedAt?: string; activity?: string; participantLabel: string; currentAction?: string; toolSummary?: string; children: ReactNode; defaultOpen?: boolean; titleOverride?: string }) {
  const active = status === "running";
  const now = useNow(active);
  const [open, setOpen] = useState(defaultOpen);
  const duration = durationLabel(startedAt, completedAt, now);
  const action = actionLabel(active ? (currentAction || activity || "") : "");
  const phrase = processStatusPhrase(status, active);
  const title = titleOverride || `本轮${phrase}`;
  const header = <>
    <strong>{title}</strong>
    {duration && <span>· {active ? "已运行" : "用时"} {duration}</span>}
    {action && <span>· {action}</span>}
    {toolSummary && <span className="execution-trace__tool-summary">· {toolSummary}</span>}
    <span className="execution-trace__summary-meta">· {participantLabel}</span>
  </>;
  if (active) {
    return <section className={`message-process message-process--trace message-process--${status}${active ? " message-process--live" : ""}`}>
      <div className="message-process__trace-head">{header}</div>
      {children && <div className="execution-trace execution-trace--topology">{children}</div>}
    </section>;
  }
  return <details className={`message-process message-process--trace message-process--${status}`} open={open} onToggle={(event) => setOpen(event.currentTarget.open)}>
    <summary>
      {header}
      <ChevronDown className="execution-trace__chevron" size={16} aria-hidden="true" />
    </summary>
    <div className="execution-trace execution-trace--topology">{children}</div>
  </details>;
}

export function ExecutionProcess({ execution, live = false, activity, agentOutputs = {}, segments = [], teamId, titleOverride, terminalOutcome }: ExecutionProcessProps) {
  const { nameForAgent, resolveIdentifier } = useAgentDirectory(execution.agent, teamId);
  const { root, groups } = useMemo(() => nodesFromExecution(execution, agentOutputs), [agentOutputs, execution]);
  const memberCount = new Set([root.agent, ...groups.flatMap((group) => group.nodes.map((node) => node.agent))]).size;
  const participantLabel = memberCount <= 1 ? `执行者：${nameForAgent(root.agent)}` : `${memberCount} 名参与者`;
  const allTools = execution.tool_calls || [];
  const runningTool = allTools.filter((tool) => executionStatus(tool.status) === "running").slice(-1)[0];
  const currentAction = live ? (runningTool ? `正在调用 ${runningTool.name}` : undefined) : undefined;
  const timeline = segments.length > 0 ? segments : fallbackSegments(root, allTools);
  const effectiveStatus = terminalOutcome?.turn_state === "blocked" ? "blocked" : root.status;
  const summary = toolCatalog(allTools, effectiveStatus === "failed" || effectiveStatus === "blocked");
  return <ProcessFrame status={effectiveStatus} startedAt={root.startedAt} completedAt={root.completedAt} activity={activity} participantLabel={participantLabel} currentAction={currentAction} toolSummary={summary} titleOverride={titleOverride}>
    <ExecutionTimeline segments={timeline} live={live} nameForAgent={nameForAgent} resolveIdentifier={resolveIdentifier} />
  </ProcessFrame>;
}

export function DispatchExecutionProcess({ groupId, terminal = false, revision = 0, fallbackLegs = [] }: DispatchExecutionProcessProps) {
  const { nameForAgent } = useAgentDirectory();
  const [detail, setDetail] = useState<TaskGroupDetail | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    let timer = 0;
    const load = async () => {
      try {
        const next = await api.getTaskGroup(groupId);
        if (!active) return;
        setDetail(next);
        setError("");
        if (!terminalTaskStatuses.has(next.status)) timer = window.setTimeout(load, 1500);
      } catch (reason) {
        if (!active) return;
        setError(apiErrorMessage(reason));
      }
    };
    void load();
    return () => { active = false; window.clearTimeout(timer); };
  }, [groupId, revision]);
  const nodes: ExecutionNode[] = detail ? detail.legs.map((leg) => ({
    id: leg.job_id,
    agent: leg.agent,
    input: leg.input,
    output: leg.output,
    error: leg.error,
    status: executionStatus(leg.status),
    startedAt: leg.started_at || leg.created_at,
    completedAt: leg.completed_at,
    tools: [],
  })) : fallbackLegs.map((leg, index) => ({
    id: `${leg.agent || leg.worker || "worker"}:${index}`,
    agent: leg.agent || leg.worker || `worker-${index + 1}`,
    input: "",
    output: "",
    status: executionStatus(leg.status || (terminal ? "completed" : "queued")),
    tools: [],
  }));
  const status = executionStatus(detail?.status || (terminal ? "completed" : "active"));
  return <ProcessFrame status={status} startedAt={detail?.created_at} completedAt={detail?.resolved_at} activity={status === "running" ? "正在并行分派" : statusLabel(status)} participantLabel={`${nodes.length} 名参与者`} defaultOpen={status === "running"}>
    {nodes.length > 0 ? <TraceCanvas groups={[{ id: groupId, nodes, parallel: true }]} live={status === "running"} nameForAgent={nameForAgent} /> : <p className="execution-trace__empty">等待服务端登记成员事实</p>}
    {error && <p className="execution-selection__error">无法读取任务组详情：{error}</p>}
  </ProcessFrame>;
}
