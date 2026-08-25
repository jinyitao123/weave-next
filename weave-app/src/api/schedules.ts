import type { ApiClient } from "./client";
import type { AgentSchedule, AgentScheduleInput, ConnectorSchedule } from "./types";

/** Agent 定时任务与连接器定时任务。 */
export const schedulesMethods = {
  listAgentSchedules(this: ApiClient, signal?: AbortSignal): Promise<AgentSchedule[]> {
    return this.request<AgentSchedule[]>("/v1/agent-schedules", { signal });
  },

  upsertAgentSchedule(this: ApiClient, input: AgentScheduleInput): Promise<AgentSchedule> {
    return this.request<AgentSchedule>("/v1/agent-schedules", { method: "PUT", body: input });
  },

  deleteAgentSchedule(this: ApiClient, id: string): Promise<void> {
    return this.request<void>(`/v1/agent-schedules?id=${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  listConnectorSchedules(this: ApiClient, signal?: AbortSignal): Promise<ConnectorSchedule[]> {
    return this.request<ConnectorSchedule[]>("/v1/schedules", { signal });
  },

  getConnectorSchedule(this: ApiClient, sourceUrl: string, signal?: AbortSignal): Promise<ConnectorSchedule> {
    return this.request<ConnectorSchedule>(`/v1/schedules/one?source_url=${encodeURIComponent(sourceUrl)}`, { signal });
  },

  upsertConnectorSchedule(this: ApiClient, input: ConnectorSchedule): Promise<ConnectorSchedule> {
    return this.request<ConnectorSchedule>("/v1/schedules", { method: "PUT", body: input });
  },

  deleteConnectorSchedule(this: ApiClient, sourceUrl: string): Promise<void> {
    return this.request<void>(`/v1/schedules?source_url=${encodeURIComponent(sourceUrl)}`, { method: "DELETE" });
  },
};

