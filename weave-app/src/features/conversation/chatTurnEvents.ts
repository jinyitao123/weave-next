import type { ChatStreamEvent, RuntimeAssignment } from "../../api";
import { executionStepLabel } from "../../workspace/labels";
import type { PendingTurn } from "./ConversationMessage";
import { appendPendingStepSegment, appendPendingTextSegment, appendPendingToolSegment, updatePendingToolSegment } from "./chatTurnReducer";

export interface ChatTurnEventResult {
  pending: PendingTurn;
  shouldRefreshConversations?: boolean;
}

export function applyChatTurnStreamEvent(
  current: PendingTurn,
  event: ChatStreamEvent,
  primaryAgent: string,
): ChatTurnEventResult {
  switch (event.type) {
    case "step_start":
      if (typeof event.data.step !== "string") return { pending: current };
      return applyStepStart(current, primaryAgent, event.data.step);
    case "content_block":
      return { pending: { ...current, activity: "正在整理结构化内容" } };
    case "done":
    case "yield":
      return applyRuntimeAssignment(current, event);
    case "chunk":
      return applyChunk(current, event, primaryAgent);
    case "tool_call":
      return applyToolCall(current, event, primaryAgent);
    case "tool_result":
      return applyToolResult(current, event);
    default:
      return { pending: current };
  }
}

function applyStepStart(current: PendingTurn, primaryAgent: string, step: string): ChatTurnEventResult {
  const stepLabel = executionStepLabel(step).label;
  return {
    pending: appendPendingStepSegment({ ...current, activity: stepLabel }, primaryAgent, stepLabel),
  };
}

function applyRuntimeAssignment(current: PendingTurn, event: ChatStreamEvent): ChatTurnEventResult {
  if (!event.data.runtime_assignment) return { pending: current };
  return {
    pending: { ...current, runtimeAssignment: event.data.runtime_assignment as RuntimeAssignment },
  };
}

function applyChunk(current: PendingTurn, event: ChatStreamEvent, primaryAgent: string): ChatTurnEventResult {
  if (typeof event.data.content !== "string") return { pending: current };
  const content = event.data.content;
  const streamAgent = typeof event.data.agent === "string" ? event.data.agent : primaryAgent;
  const withOutput = streamAgent === primaryAgent
    ? { ...current, streamedContent: current.streamedContent + content }
    : {
      ...current,
      agentOutputs: {
        ...current.agentOutputs,
        [streamAgent]: (current.agentOutputs[streamAgent] || "") + content,
      },
    };
  return {
    pending: appendPendingTextSegment(withOutput, streamAgent, content),
  };
}

function applyToolCall(current: PendingTurn, event: ChatStreamEvent, primaryAgent: string): ChatTurnEventResult {
  const name = typeof event.data.name === "string" ? event.data.name : "工具调用";
  const callID = typeof event.data.call_id === "string" ? event.data.call_id : undefined;
  const toolAgent = typeof event.data.agent === "string" ? event.data.agent : primaryAgent;
  const args = typeof event.data.args === "string" ? event.data.args : JSON.stringify(event.data.args ?? {});
  const startedAt = new Date().toISOString();
  return {
    pending: appendPendingToolSegment(current, {
      call_id: callID,
      agent: toolAgent,
      name,
      args,
      status: "running",
      started_at: startedAt,
    }),
  };
}

function applyToolResult(current: PendingTurn, event: ChatStreamEvent): ChatTurnEventResult {
  const name = typeof event.data.name === "string" ? event.data.name : "工具调用";
  const callID = typeof event.data.call_id === "string" ? event.data.call_id : undefined;
  const status = String(event.data.status || "success");
  return {
    pending: updatePendingToolSegment(current, callID, name, {
      status,
      result: String(event.data.content || ""),
      completed_at: new Date().toISOString(),
    }),
    shouldRefreshConversations: name === "tf_submit_brief" && status !== "error",
  };
}
