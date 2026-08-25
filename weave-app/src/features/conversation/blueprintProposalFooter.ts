export type BlueprintFooterKind = "planning" | "building" | "passed" | "blocked" | "idle";

export interface BlueprintFooterPresentation {
  kind: BlueprintFooterKind;
  statusLabel: string;
  headline: string;
}

export function buildOperationFailureDescription(errorDetail?: string, errorCode?: string): string | undefined {
  const detail = errorDetail?.trim();
  if (detail) return detail;
  const code = errorCode?.trim();
  return code ? code.replace(/_+/g, " ") : undefined;
}

export function blueprintFooterPresentation(status?: string, currentOperation?: string): BlueprintFooterPresentation {
  switch (status) {
    case "planning":
    case undefined:
      return { kind: "planning", statusLabel: "待继续", headline: "规划完成，等待继续构建" };
    case "authorized":
    case "round_running":
    case "running":
    case "publishing":
      return {
        kind: "building",
        statusLabel: "构建中",
        headline: currentOperation ? `构建中 · 正在${currentOperation}` : "构建中",
      };
    case "passed":
      return { kind: "passed", statusLabel: "已发布", headline: "团队已发布" };
    case "blocked":
    case "failed":
      return { kind: "blocked", statusLabel: "构建受阻", headline: "构建受阻" };
    case "cancelled":
      return { kind: "blocked", statusLabel: "已取消", headline: "构建已取消" };
    default:
      return { kind: "idle", statusLabel: "已处理", headline: "团队构建状态已更新" };
  }
}
