import type { ApiClient } from "./client";
import type {
  APIKey,
  AddWorkspaceMemberInput,
  ChangeMyPasswordInput,
  CreateAPIKeyInput,
  CreateAPIKeyResponse,
  RegisterUserInput,
  UpdateMeInput,
  UpdateUserInput,
  User,
  WorkspaceMember,
  WorkspaceResponse,
} from "./types";

/** 认证 / 当前用户 / 工作区成员与 API Key。 */
export const authMethods = {
  updateMe(this: ApiClient, input: UpdateMeInput): Promise<User> {
    return this.request<User>("/v1/auth/me", { method: "PUT", body: input });
  },

  changeMyPassword(this: ApiClient, input: ChangeMyPasswordInput): Promise<void> {
    return this.request<void>("/v1/auth/me/password", { method: "PUT", body: input });
  },

  listUsers(this: ApiClient, signal?: AbortSignal): Promise<User[]> {
    return this.request<User[]>("/v1/users", { signal });
  },

  getWorkspace(this: ApiClient, signal?: AbortSignal): Promise<WorkspaceResponse> {
    return this.request<WorkspaceResponse>("/v1/workspace", { signal });
  },

  listWorkspaceMembers(this: ApiClient, signal?: AbortSignal): Promise<WorkspaceMember[]> {
    return this.request<WorkspaceMember[]>("/v1/workspace/members", { signal });
  },

  addWorkspaceMember(this: ApiClient, input: AddWorkspaceMemberInput): Promise<void> {
    return this.request<void>("/v1/workspace/members", { method: "POST", body: input });
  },

  removeWorkspaceMember(this: ApiClient, userId: string): Promise<void> {
    return this.request<void>(`/v1/workspace/members/${encodeURIComponent(userId)}`, { method: "DELETE" });
  },

  registerUser(this: ApiClient, input: RegisterUserInput): Promise<User> {
    return this.request<User>("/v1/auth/register", { method: "POST", body: input });
  },

  updateUser(this: ApiClient, id: string, input: UpdateUserInput): Promise<void> {
    return this.request<void>(`/v1/users/${encodeURIComponent(id)}`, { method: "PUT", body: input });
  },

  deleteUser(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/users/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  async listAPIKeys(this: ApiClient, signal?: AbortSignal): Promise<APIKey[]> {
    return (await this.request<APIKey[] | null>("/v1/auth/api-keys", { signal })) ?? [];
  },

  createAPIKey(this: ApiClient, input: CreateAPIKeyInput): Promise<CreateAPIKeyResponse> {
    return this.request<CreateAPIKeyResponse>("/v1/auth/api-keys", { method: "POST", body: input });
  },

  deleteAPIKey(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/auth/api-keys/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  getMe(this: ApiClient, signal?: AbortSignal): Promise<User> {
    return this.request<User>("/v1/auth/me", { signal });
  },
};

