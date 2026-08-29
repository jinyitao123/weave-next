import type { ApiClient } from "./client";
import type {
  CreateTeamInput,
  CreateTeamFromTemplateInput,
  CreateTeamFromTemplateOutcome,
  CreateTeamResponse,
  Team,
  TeamDispatchRules,
  TeamRoster,
  TeamRosterResult,
  TeamTemplateSamplesResponse,
  TeamEvaluationInput,
  TeamEvaluationOutcome,
  UpdateTeamDispatchRulesInput,
  UpdateTeamRosterInput,
} from "./types";

/** 团队名册与并行派发规则。 */
export const teamsMethods = {
  listTeams(this: ApiClient, signal?: AbortSignal): Promise<Team[]> {
    return this.request<Team[]>("/v1/teams", { signal });
  },

  getTeam(this: ApiClient, id: string, signal?: AbortSignal): Promise<TeamRoster> {
    return this.request<TeamRoster>(`/v1/teams/${encodeURIComponent(id)}?include=summary`, { signal });
  },

  createTeam(this: ApiClient, input: CreateTeamInput): Promise<CreateTeamResponse> {
    return this.request<CreateTeamResponse>("/v1/teams", { method: "POST", body: input });
  },

  listTeamTemplateSamples(this: ApiClient, signal?: AbortSignal): Promise<TeamTemplateSamplesResponse> {
    return this.request<TeamTemplateSamplesResponse>("/v1/team-templates/samples", { signal });
  },

  createTeamFromTemplate(this: ApiClient, input: CreateTeamFromTemplateInput): Promise<CreateTeamFromTemplateOutcome> {
    return this.request<CreateTeamFromTemplateOutcome>("/v1/teams:from-template", { method: "POST", body: input });
  },

  evaluateTeam(this: ApiClient, id: string, input: TeamEvaluationInput): Promise<TeamEvaluationOutcome> {
    return this.request<TeamEvaluationOutcome>(`/v1/teams/${encodeURIComponent(id)}/evaluations`, { method: "POST", body: input });
  },

  updateTeamRoster(this: ApiClient, id: string, input: UpdateTeamRosterInput): Promise<TeamRosterResult> {
    return this.request<TeamRosterResult>(`/v1/teams/${encodeURIComponent(id)}/roster`, { method: "PUT", body: input });
  },

  getTeamDispatchRules(this: ApiClient, id: string, signal?: AbortSignal): Promise<TeamDispatchRules> {
    return this.request<TeamDispatchRules>(`/v1/teams/${encodeURIComponent(id)}/dispatch-rules`, { signal });
  },

  updateTeamDispatchRules(this: ApiClient, id: string, input: UpdateTeamDispatchRulesInput): Promise<TeamDispatchRules> {
    return this.request<TeamDispatchRules>(`/v1/teams/${encodeURIComponent(id)}/dispatch-rules`, { method: "PUT", body: input });
  },
};
