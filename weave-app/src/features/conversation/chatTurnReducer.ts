import type { PendingTurn } from "./ConversationMessage";

export function appendPendingTextSegment(current: PendingTurn, agent: string, content: string): PendingTurn {
  const last = current.segments[current.segments.length - 1];
  if (last?.type === "text" && last.agent === agent) {
    return {
      ...current,
      segments: [...current.segments.slice(0, -1), { ...last, content: last.content + content }],
    };
  }
  const now = new Date().toISOString();
  return {
    ...current,
    segments: [...current.segments, { id: `text:${agent}:${now}:${current.segments.length}`, type: "text", agent, content, created_at: now }],
  };
}

export function appendPendingStepSegment(current: PendingTurn, agent: string, label: string): PendingTurn {
  const now = new Date().toISOString();
  return {
    ...current,
    segments: [...current.segments, { id: `step:${agent}:${now}:${current.segments.length}`, type: "step", agent, label, created_at: now }],
  };
}

export function appendPendingToolSegment(current: PendingTurn, tool: PendingTurn["toolCalls"][number]): PendingTurn {
  const agent = tool.agent || current.request.agent;
  return {
    ...current,
    toolCalls: [...current.toolCalls, tool],
    segments: [...current.segments, { id: `tool:${tool.call_id || tool.name}:${current.segments.length}`, type: "tool", agent, tool, created_at: tool.started_at || new Date().toISOString() }],
  };
}

export function updatePendingToolSegment(current: PendingTurn, callId: string | undefined, name: string, update: Partial<PendingTurn["toolCalls"][number]>): PendingTurn {
  let updatedIndex = -1;
  const toolCalls = current.toolCalls.map((call, index) => {
    const matches = callId ? call.call_id === callId : call.name === name && call.status === "running";
    if (matches) updatedIndex = index;
    return matches ? { ...call, ...update } : call;
  });
  if (updatedIndex < 0) return current;
  let seenToolIndex = -1;
  const segments = current.segments.map((segment) => {
    if (segment.type !== "tool") return segment;
    seenToolIndex += 1;
    const matches = callId ? segment.tool.call_id === callId : seenToolIndex === updatedIndex;
    return matches ? { ...segment, tool: { ...segment.tool, ...update } } : segment;
  });
  return { ...current, toolCalls, segments };
}
