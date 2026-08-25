import type { ApiClient } from "./client";
import type {
  CreateDeliveryTargetInput,
  DeliveryTargetConfigInput,
  DeliveryTargetDetailResponse,
  DeliveryTargetListResponse,
  DeliveryTargetMutation,
  DeliveryTargetRevisionResponse,
  EmbedderConfig,
  MCPProbeResult,
  MCPServer,
  MCPServerUpsertInput,
  ProviderConfigInput,
  ProviderHead,
  ProviderMirrorResult,
  Runtime,
  RuntimeCreateResponse,
  SystemProviderSummary,
} from "./types";

/** MCP、Provider、Embedder、交付目标与 Runtime 配置。 */
export const settingsMethods = {
  listMCPServers(this: ApiClient, signal?: AbortSignal): Promise<MCPServer[]> {
    return this.request<MCPServer[]>("/v1/mcp-servers", { signal });
  },

  createMCPServer(this: ApiClient, input: MCPServerUpsertInput): Promise<MCPServer> {
    return this.request<MCPServer>("/v1/mcp-servers", { method: "POST", body: input });
  },

  getMCPServer(this: ApiClient, id: string, signal?: AbortSignal): Promise<MCPServer> {
    return this.request<MCPServer>(`/v1/mcp-servers/${encodeURIComponent(id)}`, { signal });
  },

  updateMCPServer(this: ApiClient, id: string, input: MCPServerUpsertInput): Promise<MCPServer> {
    return this.request<MCPServer>(`/v1/mcp-servers/${encodeURIComponent(id)}`, { method: "PUT", body: input });
  },

  deleteMCPServer(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/mcp-servers/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  probeMCPServer(this: ApiClient, id: string): Promise<MCPProbeResult> {
    return this.request<MCPProbeResult>(`/v1/mcp-servers/${encodeURIComponent(id)}/probe`, { method: "POST" });
  },

  getMCPServerTools(this: ApiClient, id: string, signal?: AbortSignal): Promise<MCPProbeResult> {
    return this.request<MCPProbeResult>(`/v1/mcp-servers/${encodeURIComponent(id)}/tools`, { signal });
  },

  async listProviders(this: ApiClient, signal?: AbortSignal): Promise<ProviderHead[]> {
    return (await this.request<ProviderHead[] | null>("/v1/providers", { signal })) ?? [];
  },

  createProvider(this: ApiClient, input: ProviderConfigInput): Promise<{ status: "created" }> {
    return this.request<{ status: "created" }>("/v1/providers", { method: "POST", body: input });
  },

  updateProvider(this: ApiClient, id: string, input: ProviderConfigInput): Promise<{ status: "updated" }> {
    return this.request<{ status: "updated" }>(`/v1/providers/${encodeURIComponent(id)}`, { method: "PUT", body: input });
  },

  deleteProvider(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/providers/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  mirrorSystemProvider(this: ApiClient, id: string, reason: string): Promise<ProviderMirrorResult> {
    return this.request<ProviderMirrorResult>(`/v1/providers/system/${encodeURIComponent(id)}/mirror`, { method: "POST", body: { reason } });
  },
  listSystemProviders(this: ApiClient, signal?: AbortSignal): Promise<SystemProviderSummary[]> {
    return this.request<SystemProviderSummary[]>("/v1/providers/system", { signal });
  },

  getEmbedder(this: ApiClient, signal?: AbortSignal): Promise<EmbedderConfig> {
    return this.request<EmbedderConfig>("/v1/embedder", { signal });
  },

  updateEmbedder(this: ApiClient, input: EmbedderConfig): Promise<{ status: "updated" }> {
    return this.request<{ status: "updated" }>("/v1/embedder", { method: "PUT", body: input });
  },

  deleteEmbedder(this: ApiClient): Promise<void> {
    return this.request<void>("/v1/embedder", { method: "DELETE" });
  },

  async listDeliveryTargets(this: ApiClient, signal?: AbortSignal): Promise<DeliveryTargetListResponse> {
    return this.request<DeliveryTargetListResponse>("/v1/delivery-targets", { signal });
  },

  createDeliveryTarget(this: ApiClient, input: CreateDeliveryTargetInput): Promise<DeliveryTargetMutation> {
    return this.request<DeliveryTargetMutation>("/v1/delivery-targets", { method: "POST", body: input });
  },

  getDeliveryTarget(this: ApiClient, id: string, signal?: AbortSignal): Promise<DeliveryTargetDetailResponse> {
    return this.request<DeliveryTargetDetailResponse>(`/v1/delivery-targets/${encodeURIComponent(id)}`, { signal });
  },

  updateDeliveryTarget(this: ApiClient, id: string, input: DeliveryTargetConfigInput): Promise<DeliveryTargetMutation> {
    return this.request<DeliveryTargetMutation>(`/v1/delivery-targets/${encodeURIComponent(id)}`, { method: "PUT", body: input });
  },

  getDeliveryTargetRevision(this: ApiClient, id: string, revision: number, signal?: AbortSignal): Promise<DeliveryTargetRevisionResponse> {
    return this.request<DeliveryTargetRevisionResponse>(`/v1/delivery-targets/${encodeURIComponent(id)}/revisions/${revision}`, { signal });
  },

  rotateDeliveryTargetHeaders(this: ApiClient, id: string, headers: Record<string, string>): Promise<DeliveryTargetMutation> {
    return this.request<DeliveryTargetMutation>(`/v1/delivery-targets/${encodeURIComponent(id)}/rotate-headers`, { method: "POST", body: { headers } });
  },

  disableDeliveryTarget(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/delivery-targets/${encodeURIComponent(id)}/disable`, { method: "POST" });
  },

  revokeDeliveryTarget(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/delivery-targets/${encodeURIComponent(id)}/revoke`, { method: "POST" });
  },

  deleteDeliveryTarget(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/delivery-targets/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

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
