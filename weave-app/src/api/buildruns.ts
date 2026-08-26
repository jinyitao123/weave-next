import type { ApiClient } from "./client";
import type { BlueprintRevisionToken, BuildRunProgress, SubmitBuildRunResponse, TeamBuildRunListResponse, TeamEvaluationRoundReportResponse, TeamRunListResponse, UsageReport } from "./types";

/** 团队构建运行聚合与用量（team-build read aggregation）。 */
export const buildRunsMethods = {
  listTeamBuildRuns(this: ApiClient, options: { limit?: number; offset?: number } = {}, signal?: AbortSignal): Promise<TeamBuildRunListResponse> {
    const query = new URLSearchParams({
      limit: String(options.limit ?? 200),
      offset: String(options.offset ?? 0),
    });
    return this.request<TeamBuildRunListResponse>(`/v1/internal/team-build-runs?${query.toString()}`, { signal });
  },

  getTeamBuildRunProgress(this: ApiClient, buildRunId: string, signal?: AbortSignal): Promise<BuildRunProgress> {
    return this.request<BuildRunProgress>(`/v1/internal/team-build-runs/${encodeURIComponent(buildRunId)}/progress`, { signal });
  },

  getTeamBuildRunRoundReport(this: ApiClient, buildRunId: string, roundNo: number, signal?: AbortSignal): Promise<TeamEvaluationRoundReportResponse> {
    return this.request<TeamEvaluationRoundReportResponse>(`/v1/internal/team-build-runs/${encodeURIComponent(buildRunId)}/rounds/${roundNo}/report`, { signal });
  },

  submitTeamBuildRun(this: ApiClient, buildRunId: string, input: { authority?: string; revision_token?: BlueprintRevisionToken }): Promise<SubmitBuildRunResponse> {
    return this.request<SubmitBuildRunResponse>(`/v1/internal/team-build-runs/${encodeURIComponent(buildRunId)}/submit`, {
      method: "POST",
      body: input,
    });
  },

  listTeamRuns(this: ApiClient, teamID: string, options: { limit: number; offset: number }): Promise<TeamRunListResponse> {
    const query = new URLSearchParams({
      view: "team",
      team_id: teamID,
      aggregation_mode: "all-exclusive",
      limit: String(options.limit),
      offset: String(options.offset),
    });
    return this.request<TeamRunListResponse>(`/v1/runs?${query.toString()}`);
  },

  getUsage(this: ApiClient, projectId?: string, signal?: AbortSignal): Promise<UsageReport> {
    const query = projectId ? `?project_id=${encodeURIComponent(projectId)}` : "";
    return this.request<UsageReport>(`/v1/usage${query}`, { signal });
  },
};
