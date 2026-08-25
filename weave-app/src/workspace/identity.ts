import type { AgentRecord, Project, Team } from "../api";

/** 平台内置「元团队」分身名，仅通过「新增团队」入口触达。 */
export const TEAM_ARCHITECT_AGENT_NAME = "__team_architect";

/** 平台内置元团队名；仅用于识别其系统会话，不向普通团队导航暴露。 */
export const META_TEAM_NAME = "__team-forge";

/** 平台内置资产（元团队等）：visibility='platform'，过渡期兼容 name 的 `__` 前缀。 */
export function isPlatformAsset(agent: { name: string; visibility?: string }): boolean {
  return agent.visibility === "platform" || agent.name.startsWith("__");
}

/** 系统兜底「未分类」项目，在团队树中显示为「快速会话」。 */
export function isUnclassifiedProject(project: Pick<Project, "name" | "system_kind">): boolean {
  return project.system_kind === "unclassified" || project.name.startsWith("未分类");
}

/** 元团队（团队搭建任务）所在的项目：元团队名下，或团队架构师分身的未分类项目。 */
export function isTeamBuildProject(
  project: Pick<Project, "team_id" | "avatar_id">,
  teams: Array<Pick<Team, "id" | "name">>,
  agents: Array<Pick<AgentRecord, "id" | "name">>,
): boolean {
  const metaTeam = teams.find((team) => team.name === META_TEAM_NAME);
  const architect = agents.find((agent) => agent.name === TEAM_ARCHITECT_AGENT_NAME);
  return (!!metaTeam && project.team_id === metaTeam.id) || (!!architect && project.avatar_id === architect.id);
}

/** 项目在主界面各处的统一显示名：团队搭建 > 快速会话 > 项目名。 */
export function projectDisplayName(
  project: Pick<Project, "name" | "system_kind" | "team_id" | "avatar_id">,
  teams: Array<Pick<Team, "id" | "name">>,
  agents: Array<Pick<AgentRecord, "id" | "name">>,
): string {
  if (isTeamBuildProject(project, teams, agents)) return "团队搭建";
  return isUnclassifiedProject(project) ? "快速会话" : project.name;
}

