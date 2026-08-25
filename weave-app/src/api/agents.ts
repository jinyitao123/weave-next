import type { ApiClient } from "./client";
import type {
  AgentChannel,
  AgentLink,
  AgentMemoryCreateInput,
  AgentMemoryListResponse,
  AgentMemoryProfile,
  AgentMemorySearchInput,
  AgentMemorySearchResponse,
  AgentMemorySlot,
  AgentRecord,
  AgentRunSummaryResponse,
  AgentTeamMembershipsResponse,
  AgentTopologyResponse,
  AgentWriteInput,
  CreateAgentLinkInput,
  LegacySkillImportInput,
  LegacySkillImportResponse,
  OrphanWorkerScanResponse,
  PromptPreviewInput,
  PromptPreviewResponse,
  Skill,
  SkillWriteInput,
} from "./types";

/** Agent 注册表、链接、渠道、技能与 Agent 级记忆。 */
export const agentsMethods = {
  listAgents(this: ApiClient, signal?: AbortSignal): Promise<AgentRecord[]> {
    return this.request<AgentRecord[]>("/v1/agents", { signal });
  },

  getAgent(this: ApiClient, name: string, signal?: AbortSignal): Promise<AgentRecord> {
    return this.request<AgentRecord>(`/v1/agents/${encodeURIComponent(name)}`, { signal });
  },

  getAgentTeamMemberships(this: ApiClient, name: string, signal?: AbortSignal): Promise<AgentTeamMembershipsResponse> {
    return this.request<AgentTeamMembershipsResponse>(`/v1/agents/${encodeURIComponent(name)}/team-memberships`, { signal });
  },

  getAgentRunSummary(this: ApiClient, name: string, signal?: AbortSignal): Promise<AgentRunSummaryResponse> {
    return this.request<AgentRunSummaryResponse>(`/v1/agents/${encodeURIComponent(name)}/run-summary`, { signal });
  },

  createAgent(this: ApiClient, input: AgentWriteInput): Promise<AgentRecord> {
    return this.request<AgentRecord>("/v1/agents", { method: "POST", body: input });
  },

  updateAgent(this: ApiClient, name: string, input: AgentWriteInput): Promise<AgentRecord> {
    return this.request<AgentRecord>(`/v1/agents/${encodeURIComponent(name)}`, { method: "PUT", body: input });
  },

  deleteAgent(this: ApiClient, name: string): Promise<void> {
    return this.request<void>(`/v1/agents/${encodeURIComponent(name)}`, { method: "DELETE" });
  },

  listAgentLinks(this: ApiClient, signal?: AbortSignal): Promise<AgentLink[]> {
    return this.request<AgentLink[]>("/v1/agent-links", { signal });
  },

  createAgentLink(this: ApiClient, input: CreateAgentLinkInput): Promise<AgentLink> {
    return this.request<AgentLink>("/v1/agent-links", { method: "POST", body: input });
  },

  updateAgentLinkInstruction(this: ApiClient, id: string, instruction: string): Promise<AgentLink> {
    return this.request<AgentLink>(`/v1/agent-links/${encodeURIComponent(id)}`, { method: "PATCH", body: { instruction } });
  },

  deleteAgentLink(this: ApiClient, id: string): Promise<AgentLink> {
    return this.request<AgentLink>(`/v1/agent-links/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  scanOrphanWorkers(this: ApiClient): Promise<OrphanWorkerScanResponse> {
    return this.request<OrphanWorkerScanResponse>("/v1/agents/cleanup-orphans", { method: "POST" });
  },

  listAgentChannels(this: ApiClient, name: string, signal?: AbortSignal): Promise<AgentChannel[]> {
    return this.request<AgentChannel[]>(`/v1/agents/${encodeURIComponent(name)}/channels`, { signal });
  },

  createAgentChannel(this: ApiClient, name: string, channelName: string): Promise<AgentChannel> {
    return this.request<AgentChannel>(`/v1/agents/${encodeURIComponent(name)}/channels`, { method: "POST", body: { name: channelName } });
  },

  renameAgentChannel(this: ApiClient, name: string, id: string, channelName: string): Promise<AgentChannel> {
    return this.request<AgentChannel>(`/v1/agents/${encodeURIComponent(name)}/channels/${encodeURIComponent(id)}`, { method: "PATCH", body: { name: channelName } });
  },

  deleteAgentChannel(this: ApiClient, name: string, id: string): Promise<{ ok: boolean }> {
    return this.request<{ ok: boolean }>(`/v1/agents/${encodeURIComponent(name)}/channels/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  reorderAgentChannels(this: ApiClient, name: string, orderedIds: string[]): Promise<void> {
    return this.request<void>(`/v1/agents/${encodeURIComponent(name)}/channels/reorder`, { method: "POST", body: { ordered_ids: orderedIds } });
  },

  previewAgentPrompt(this: ApiClient, name: string, input: PromptPreviewInput): Promise<PromptPreviewResponse> {
    return this.request<PromptPreviewResponse>(`/v1/agents/${encodeURIComponent(name)}/preview-prompt`, { method: "POST", body: input });
  },

  getAgentTopology(this: ApiClient, name: string, signal?: AbortSignal): Promise<AgentTopologyResponse> {
    return this.request<AgentTopologyResponse>(`/v1/agents/${encodeURIComponent(name)}/topology`, { signal });
  },

  listSkills(this: ApiClient, signal?: AbortSignal): Promise<Skill[]> {
    return this.request<Skill[]>("/v1/skills", { signal });
  },

  getSkill(this: ApiClient, id: string, signal?: AbortSignal): Promise<Skill> {
    return this.request<Skill>(`/v1/skills/${encodeURIComponent(id)}`, { signal });
  },

  createSkill(this: ApiClient, input: SkillWriteInput): Promise<Skill> {
    return this.request<Skill>("/v1/skills", { method: "POST", body: input });
  },

  updateSkill(this: ApiClient, id: string, input: SkillWriteInput): Promise<Skill> {
    return this.request<Skill>(`/v1/skills/${encodeURIComponent(id)}`, { method: "PUT", body: input });
  },

  deleteSkill(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/skills/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  importLegacySkill(this: ApiClient, id: string, input: LegacySkillImportInput): Promise<LegacySkillImportResponse> {
    return this.request<LegacySkillImportResponse>(`/v1/skills/${encodeURIComponent(id)}/import-legacy`, { method: "POST", body: input });
  },

  listAgentMemories(this: ApiClient, name: string, signal?: AbortSignal): Promise<AgentMemoryListResponse> {
    return this.request<AgentMemoryListResponse>(`/v1/agents/${encodeURIComponent(name)}/memories`, { signal });
  },

  createAgentMemory(this: ApiClient, name: string, input: AgentMemoryCreateInput): Promise<{ id: string }> {
    return this.request<{ id: string }>(`/v1/agents/${encodeURIComponent(name)}/memories`, { method: "POST", body: input });
  },

  deleteAgentMemory(this: ApiClient, name: string, id: string): Promise<void> {
    return this.request<void>(`/v1/agents/${encodeURIComponent(name)}/memories/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  searchAgentMemories(this: ApiClient, name: string, input: AgentMemorySearchInput, signal?: AbortSignal): Promise<AgentMemorySearchResponse> {
    return this.request<AgentMemorySearchResponse>(`/v1/agents/${encodeURIComponent(name)}/memories/search`, { method: "POST", body: input, signal });
  },

  getAgentMemorySlots(this: ApiClient, name: string, signal?: AbortSignal): Promise<AgentMemorySlot[]> {
    return this.request<AgentMemorySlot[]>(`/v1/agents/${encodeURIComponent(name)}/memory-slots`, { signal });
  },

  updateAgentMemorySlots(this: ApiClient, name: string, slots: AgentMemorySlot[]): Promise<AgentMemorySlot[]> {
    return this.request<AgentMemorySlot[]>(`/v1/agents/${encodeURIComponent(name)}/memory-slots`, { method: "PUT", body: slots });
  },

  getAgentMemoryProfile(this: ApiClient, name: string, userId: string, signal?: AbortSignal): Promise<AgentMemoryProfile> {
    return this.request<AgentMemoryProfile>(`/v1/agents/${encodeURIComponent(name)}/memory-profile?user=${encodeURIComponent(userId)}`, { signal });
  },
};

