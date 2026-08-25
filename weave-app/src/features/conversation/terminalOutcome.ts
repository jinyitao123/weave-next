import type { TerminalOutcome } from "../../api";

export type TerminalOutcomeAction = "replan" | "abandon";

export interface TerminalOutcomePresentation {
  title: string;
  description: string;
  modeDescription?: string;
  actions: TerminalOutcomeAction[];
}

const reasonDescriptions: Record<NonNullable<TerminalOutcome["reason_code"]>, string> = {
  blueprint_plan_budget_exhausted: "蓝图连续未通过平台校验，规划预算已耗尽，本次构建已暂停。",
  blueprint_planning_no_revision: "本轮规划没有产出可批准的团队蓝图，本次构建已暂停。",
  blueprint_planning_transition_failed: "团队蓝图规划未能写入终态，本次构建已暂停，请检查平台运行状态。",
  blueprint_validation_failed: "团队蓝图未通过平台校验，本次构建已暂停。",
  candidate_run_failed: "试运行（业务验收）执行失败，本次构建已暂停。",
  infra_failure: "平台基础设施异常导致本轮无法继续，本次构建已暂停。",
};

const stateCopy: Record<TerminalOutcome["turn_state"], Pick<TerminalOutcomePresentation, "title" | "description">> = {
  plan_ready: { title: "方案就绪", description: "团队方案已生成，等待你的批准。" },
  needs_clarification: { title: "等待澄清", description: "请补充目标或关键约束后继续。" },
  blocked: { title: "规划受阻", description: "本次构建已暂停。可重新规划或放弃此次构建。" },
  completed: { title: "本轮已完成", description: "本轮处理已完成。" },
};

export function terminalTurnStateTitle(turnState: TerminalOutcome["turn_state"]): string {
  return stateCopy[turnState].title;
}

export function terminalBlockedFollowupNote(outcome: TerminalOutcome | undefined, hasLegacyReport: boolean): string | undefined {
  return outcome?.turn_state === "blocked" && hasLegacyReport ? "该方案后续已受阻" : undefined;
}

export function terminalBuildStatusDescription(
  outcome: TerminalOutcome | undefined,
  buildRunStatus: string | undefined,
  hasCandidateEvaluation: boolean,
): string | undefined {
  if (buildRunStatus === "blocked" || buildRunStatus === "failed") {
    return hasCandidateEvaluation
      ? reasonDescriptions.candidate_run_failed
      : stateCopy.blocked.description;
  }
  return outcome ? terminalOutcomePresentation(outcome).description : undefined;
}

export function terminalOutcomePresentation(outcome: TerminalOutcome): TerminalOutcomePresentation {
  const copy = stateCopy[outcome.turn_state];
  const description = outcome.turn_state === "blocked" && outcome.reason_code
    ? reasonDescriptions[outcome.reason_code]
    : copy.description;
  const actionableStatus = outcome.build_run_status === "blocked"
    || outcome.build_run_status === "failed"
    || outcome.build_run_status === "planning";
  return {
    ...copy,
    description,
    modeDescription: outcome.mode_note?.mode === "optimize"
      ? `检测到同名团队 ${outcome.mode_note.team_name}，本次为优化`
      : undefined,
    actions: outcome.turn_state === "blocked" && actionableStatus ? ["abandon", "replan"] : [],
  };
}
