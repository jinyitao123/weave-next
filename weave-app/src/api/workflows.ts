import type { ApiClient } from "./client";
import type {
  CreateWorkflowInput,
  CreateWorkflowResponse,
  TeamWorkflowVersion,
  UpdateWorkflowDraftInput,
  WorkflowAdmissionStatusResponse,
  WorkflowArchiveResponse,
  WorkflowDependenciesResponse,
  WorkflowDetailResponse,
  WorkflowLegacyManualRunRequest,
  WorkflowListResponse,
  WorkflowManualRunRequest,
  WorkflowManualRunResponse,
  WorkflowValidationResponse,
  WorkflowVersionResponse,
} from "./types";

/** 团队协作流程（workflow）全生命周期。 */
export const workflowsMethods = {
  listTeamWorkflows(this: ApiClient, teamId: string, includeArchived = false, signal?: AbortSignal): Promise<WorkflowListResponse> {
    const suffix = includeArchived ? "?include=archived" : "";
    return this.request<WorkflowListResponse>(`/v1/teams/${encodeURIComponent(teamId)}/workflows${suffix}`, { signal });
  },

  createWorkflow(this: ApiClient, teamId: string, input: CreateWorkflowInput): Promise<CreateWorkflowResponse> {
    return this.request<CreateWorkflowResponse>(`/v1/teams/${encodeURIComponent(teamId)}/workflows`, { method: "POST", body: input });
  },

  getWorkflow(this: ApiClient, id: string, signal?: AbortSignal): Promise<WorkflowDetailResponse> {
    return this.request<WorkflowDetailResponse>(`/v1/workflows/${encodeURIComponent(id)}`, { signal });
  },

  getWorkflowVersion(this: ApiClient, id: string, version: number, signal?: AbortSignal): Promise<WorkflowVersionResponse> {
    return this.request<WorkflowVersionResponse>(`/v1/workflows/${encodeURIComponent(id)}/versions/${version}`, { signal });
  },

  createWorkflowDraft(this: ApiClient, id: string): Promise<TeamWorkflowVersion> {
    return this.request<TeamWorkflowVersion>(`/v1/workflows/${encodeURIComponent(id)}/drafts`, { method: "POST", body: {} });
  },

  updateWorkflowDraft(this: ApiClient, id: string, version: number, input: UpdateWorkflowDraftInput): Promise<TeamWorkflowVersion> {
    return this.request<TeamWorkflowVersion>(`/v1/workflows/${encodeURIComponent(id)}/versions/${version}`, { method: "PUT", body: input });
  },

  validateWorkflowVersion(this: ApiClient, id: string, version: number): Promise<WorkflowValidationResponse> {
    return this.request<WorkflowValidationResponse>(`/v1/workflows/${encodeURIComponent(id)}/versions/${version}/validate`, { method: "POST", body: {} });
  },

  publishWorkflowVersion(this: ApiClient, id: string, version: number): Promise<TeamWorkflowVersion> {
    return this.request<TeamWorkflowVersion>(`/v1/workflows/${encodeURIComponent(id)}/versions/${version}/publish`, { method: "POST", body: {} });
  },

  getWorkflowAdmission(this: ApiClient, id: string, version: number, signal?: AbortSignal): Promise<WorkflowAdmissionStatusResponse> {
    return this.request<WorkflowAdmissionStatusResponse>(`/v1/workflows/${encodeURIComponent(id)}/versions/${version}/admission`, { signal });
  },

  getWorkflowDependencies(this: ApiClient, id: string, version: number, signal?: AbortSignal): Promise<WorkflowDependenciesResponse> {
    return this.request<WorkflowDependenciesResponse>(`/v1/workflows/${encodeURIComponent(id)}/versions/${version}/dependencies`, { signal });
  },

  archiveWorkflow(this: ApiClient, id: string): Promise<WorkflowArchiveResponse> {
    return this.request<WorkflowArchiveResponse>(`/v1/workflows/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  runWorkflow(this: ApiClient, id: string, request: WorkflowManualRunRequest): Promise<WorkflowManualRunResponse> {
    return this.request<WorkflowManualRunResponse>(`/v1/workflows/${encodeURIComponent(id)}/run`, { method: "POST", body: request });
  },

  runLegacyWorkflow(this: ApiClient, id: string, request: WorkflowLegacyManualRunRequest): Promise<WorkflowManualRunResponse> {
    return this.request<WorkflowManualRunResponse>(`/v1/internal/workflows/${encodeURIComponent(id)}/run`, { method: "POST", body: request });
  },
};
