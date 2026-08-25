import type { ApiClient } from "./client";
import type {
  CreateProjectInput,
  CreateProjectResourceInput,
  MoveProjectInput,
  Project,
  ProjectCollaborator,
  ProjectCollaboratorListResponse,
  ProjectListResponse,
  ProjectMemoryResponse,
  ProjectMemorySearchInput,
  ProjectResource,
  UpdateProjectInput,
} from "./types";

/** 项目目录、资源、协作团队与项目级记忆。 */
export const projectsMethods = {
  listProjects(this: ApiClient, filters: { avatarId?: string; teamId?: string; includeArchived?: boolean } = {}, signal?: AbortSignal): Promise<ProjectListResponse> {
    const query = new URLSearchParams();
    if (filters.avatarId) query.set("avatar_id", filters.avatarId);
    if (filters.teamId) query.set("team_id", filters.teamId);
    if (filters.includeArchived) query.set("include_archived", "true");
    const suffix = query.size ? `?${query.toString()}` : "";
    return this.request<ProjectListResponse>(`/v1/projects${suffix}`, { signal });
  },

  createProject(this: ApiClient, input: CreateProjectInput): Promise<Project> {
    return this.request<Project>("/v1/projects", { method: "POST", body: input });
  },

  ensureUnclassifiedProject(this: ApiClient, ownerId: string, ownerKind: "team" | "avatar" = "team"): Promise<Project> {
    const body = ownerKind === "team" ? { team_id: ownerId } : { avatar_id: ownerId };
    return this.request<Project>("/v1/projects/ensure-unclassified", { method: "POST", body });
  },

  updateProject(this: ApiClient, projectId: string, input: UpdateProjectInput): Promise<Project> {
    return this.request<Project>(`/v1/projects/${encodeURIComponent(projectId)}`, { method: "PUT", body: input });
  },

  archiveProject(this: ApiClient, projectId: string): Promise<void> {
    return this.request<void>(`/v1/projects/${encodeURIComponent(projectId)}`, { method: "DELETE" });
  },

  restoreProject(this: ApiClient, projectId: string): Promise<Project> {
    return this.request<Project>(`/v1/projects/${encodeURIComponent(projectId)}/restore`, { method: "POST", body: {} });
  },

  moveProject(this: ApiClient, projectId: string, input: MoveProjectInput): Promise<Project> {
    return this.request<Project>(`/v1/projects/${encodeURIComponent(projectId)}/move`, { method: "POST", body: input });
  },

  listProjectCollaborators(this: ApiClient, projectId: string, signal?: AbortSignal): Promise<ProjectCollaboratorListResponse> {
    return this.request<ProjectCollaboratorListResponse>(`/v1/projects/${encodeURIComponent(projectId)}/collaborators`, { signal });
  },

  addProjectCollaborator(this: ApiClient, projectId: string, teamId: string): Promise<ProjectCollaborator> {
    return this.request<ProjectCollaborator>(`/v1/projects/${encodeURIComponent(projectId)}/collaborators`, { method: "POST", body: { team_id: teamId } });
  },

  removeProjectCollaborator(this: ApiClient, projectId: string, teamId: string): Promise<void> {
    return this.request<void>(`/v1/projects/${encodeURIComponent(projectId)}/collaborators/${encodeURIComponent(teamId)}`, { method: "DELETE" });
  },

  listProjectResources(this: ApiClient, projectId: string, signal?: AbortSignal): Promise<{ resources: ProjectResource[] }> {
    return this.request<{ resources: ProjectResource[] }>(`/v1/projects/${encodeURIComponent(projectId)}/resources`, { signal });
  },

  createProjectResource(this: ApiClient, projectId: string, input: CreateProjectResourceInput): Promise<ProjectResource> {
    return this.request<ProjectResource>(`/v1/projects/${encodeURIComponent(projectId)}/resources`, { method: "POST", body: input });
  },

  deleteProjectResource(this: ApiClient, projectId: string, resourceId: string): Promise<void> {
    return this.request<void>(`/v1/projects/${encodeURIComponent(projectId)}/resources/${encodeURIComponent(resourceId)}`, { method: "DELETE" });
  },

  listProjectMemories(this: ApiClient, projectId: string, signal?: AbortSignal): Promise<ProjectMemoryResponse> {
    return this.request<ProjectMemoryResponse>(`/v1/projects/${encodeURIComponent(projectId)}/memories`, { signal });
  },

  createProjectMemory(this: ApiClient, projectId: string, content: string): Promise<{ id: string }> {
    return this.request<{ id: string }>(`/v1/projects/${encodeURIComponent(projectId)}/memories`, { method: "POST", body: { content } });
  },

  deleteProjectMemory(this: ApiClient, projectId: string, memoryId: string): Promise<void> {
    return this.request<void>(`/v1/projects/${encodeURIComponent(projectId)}/memories/${encodeURIComponent(memoryId)}`, { method: "DELETE" });
  },

  searchProjectMemories(this: ApiClient, projectId: string, input: ProjectMemorySearchInput, signal?: AbortSignal): Promise<ProjectMemoryResponse> {
    return this.request<ProjectMemoryResponse>(`/v1/projects/${encodeURIComponent(projectId)}/memories/search`, { method: "POST", body: input, signal });
  },
};

