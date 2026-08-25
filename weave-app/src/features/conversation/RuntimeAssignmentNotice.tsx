import { CheckCircle2, ChevronDown, Cpu } from "lucide-react";
import type { RuntimeAssignment } from "../../api";
import { engineLabel } from "../../workspace/labels";

const runtimeReasonLabels: Record<string, string> = {
  explicit_runtime_eligible: "已按本次任务指定节点分配",
  agent_default_eligible: "已自动分配到该分身的默认节点",
  most_recent_eligible: "自动选择最近在线且可用的节点",
  in_process_engine: "由平台内置引擎执行",
};

const runtimeUnavailableLabels: Record<string, string> = {
  runtime_disabled: "已停用",
  runtime_revoked: "已撤销",
  runtime_offline: "离线",
  engine_unavailable: "不支持当前引擎",
};

export function RuntimeAssignmentNotice({ assignment }: { assignment: RuntimeAssignment }) {
  const facts = assignment.capability_facts || [];
  const chosen = facts.find((fact) => fact.runtime_id === assignment.runtime_id);
  const engine = engineLabel(assignment.engine);
  const chosenAvailable = chosen ? chosen.eligible : assignment.reason_code === "in_process_engine";
  return <details className="runtime-assignment">
    <summary><span className="runtime-assignment__icon"><Cpu size={14} aria-hidden="true" /></span><span className="runtime-assignment__label">执行环境</span><strong>{engine}</strong><span className="runtime-assignment__node">· {chosen?.name || "平台内置"}</span><span className={`runtime-assignment__status${chosenAvailable ? " is-available" : ""}`}>{chosenAvailable && <CheckCircle2 size={14} aria-hidden="true" />}{chosenAvailable ? "可用" : "状态待确认"}</span><ChevronDown className="runtime-assignment__chevron" size={14} aria-hidden="true" /></summary>
    <div className="runtime-assignment__body"><p>{runtimeReasonLabels[assignment.reason_code] || "由平台按运行策略选择执行节点"}</p><dl><div><dt>分配方式</dt><dd>{assignment.mode === "explicit" ? "本次任务指定" : "自动选择"}</dd></div><div><dt>执行引擎</dt><dd>{engine}</dd></div>{chosen && <div><dt>节点负载</dt><dd>{chosen.active_slots}/{chosen.total_slots}</dd></div>}</dl>{facts.length > 0 && <div className="runtime-assignment__candidates"><span>本轮候选节点</span><ul>{facts.map((fact) => <li key={fact.runtime_id}><span>{fact.name}</span><small className={fact.eligible ? "is-available" : ""}>{fact.runtime_id === assignment.runtime_id ? "已分配" : fact.eligible ? "可用" : runtimeUnavailableLabels[fact.unavailable_reason || ""] || "不可用"}</small></li>)}</ul></div>}</div>
  </details>;
}
