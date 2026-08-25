import type { ToolCallRecord } from "../../api";

export interface ToolFailureCounts {
  retryCount: number;
  unrecoveredCount: number;
}

function isCompleted(status: string): boolean {
  return status === "success" || status === "completed" || status === "resolved";
}

function isFailed(status: string): boolean {
  return status === "error" || status === "failed" || status === "timed_out" || status === "cancelled" || status === "cut";
}

function isExpectedProbeMiss(tool: ToolCallRecord): boolean {
  return isFailed(tool.status)
    && /(?:not[_ ]found|does not exist|no\s+\w+\s+found|未找到|不存在|结果不存在)/i.test(tool.result || "");
}

// Count failures in the inspected prefix while allowing later calls in the
// supplied timeline to prove that a same-name retry recovered.
export function toolFailureCounts(tools: ToolCallRecord[], inspectedCount = tools.length): ToolFailureCounts {
  let retryCount = 0;
  let unrecoveredCount = 0;
  tools.slice(0, inspectedCount).forEach((tool, index) => {
    if (!isFailed(tool.status) || isExpectedProbeMiss(tool)) return;
    const recovered = tools.slice(index + 1).some((later) => later.name === tool.name && isCompleted(later.status));
    if (recovered) retryCount += 1;
    else unrecoveredCount += 1;
  });
  return { retryCount, unrecoveredCount };
}
