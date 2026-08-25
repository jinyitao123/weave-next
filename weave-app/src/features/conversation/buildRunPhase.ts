export type BuildRunPhaseTone = "neutral" | "accent" | "success" | "warning" | "danger";

export interface BuildRunPhaseLabel {
  label: string;
  summary: string;
  tone: BuildRunPhaseTone;
  canSubmit: boolean;
}

export function buildRunPhaseLabel(status?: string): BuildRunPhaseLabel {
  switch (status) {
    case "planning":
      return { label: "待继续", summary: "规划完成，等待继续构建", tone: "warning", canSubmit: true };
    case "authorized":
    case "round_running":
    case "running":
    case "publishing":
      return { label: "构建中", summary: "正在构建", tone: "accent", canSubmit: false };
    case "passed":
      return { label: "已发布", summary: "团队已发布", tone: "success", canSubmit: false };
    case "blocked":
    case "failed":
      return { label: "构建受阻", summary: "构建受阻，请重新规划或放弃此次构建", tone: "danger", canSubmit: false };
    case "cancelled":
      return { label: "已取消", summary: "构建已取消", tone: "neutral", canSubmit: false };
    default:
      return status
        ? { label: "已处理", summary: "团队构建状态已更新", tone: "neutral", canSubmit: false }
        : { label: "待继续", summary: "规划完成，等待继续构建", tone: "warning", canSubmit: true };
  }
}
