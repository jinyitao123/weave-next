import type { ApiClient } from "./client";
import type { Runtime, RuntimeCreateResponse } from "./types";

/** Runtime registration and scheduling configuration for deployment maintenance. */
export const runtimeMethods = {
  listRuntimes(this: ApiClient, signal?: AbortSignal): Promise<{ runtimes: Runtime[] }> {
    return this.request<{ runtimes: Runtime[] }>("/v1/runtimes", { signal });
  },

  createRuntime(this: ApiClient, name: string): Promise<RuntimeCreateResponse> {
    return this.request<RuntimeCreateResponse>("/v1/runtimes", { method: "POST", body: { name } });
  },

  configureRuntime(this: ApiClient, id: string, input: { name: string; pool_id: string }): Promise<void> {
	return this.request<void>(`/v1/runtimes/${encodeURIComponent(id)}`, { method: "PUT", body: input });
  },

  deleteRuntime(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/runtimes/${encodeURIComponent(id)}`, { method: "DELETE" });
  },
};
