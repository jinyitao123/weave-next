import { ApiError, apiErrorFromResponse, normalizeThrownError } from "./errors";
import type { ApiClient } from "./client";
import type {
  ForkRunInput,
  ForkStreamEvent,
  ForkStreamResult,
  Job,
  JobListResponse,
  ResumeRunInput,
  ResumeStreamEvent,
  ResumeStreamResult,
  RunCheckpoint,
  RunDetail,
  RunListResponse,
  RunState,
  TaskGroupDetail,
  TaskGroupListResponse,
} from "./types";

/** 运行、状态/检查点、分叉/恢复与任务组/任务。 */
export const runsMethods = {
  getRun(this: ApiClient, id: string, signal?: AbortSignal): Promise<RunDetail> {
    return this.request<RunDetail>(`/v1/runs/${encodeURIComponent(id)}`, { signal });
  },

  getRunTrace(this: ApiClient, id: string, signal?: AbortSignal): Promise<unknown[]> {
    return this.request<unknown[]>(`/v1/runs/${encodeURIComponent(id)}/trace`, { signal });
  },

  listRuns(this: ApiClient, filters: { projectId?: string; conversationId?: string } = {}, signal?: AbortSignal): Promise<RunListResponse> {
    const query = new URLSearchParams();
    if (filters.projectId) query.set("project_id", filters.projectId);
    if (filters.conversationId) query.set("conversation_id", filters.conversationId);
    const suffix = query.size ? `?${query.toString()}` : "";
    return this.request<RunListResponse>(`/v1/runs${suffix}`, { signal });
  },

  getRunState(this: ApiClient, id: string, agent: string, signal?: AbortSignal): Promise<RunState> {
    return this.request<RunState>(`/v1/runs/${encodeURIComponent(id)}/state?agent=${encodeURIComponent(agent)}`, { signal });
  },

  getRunCheckpoints(this: ApiClient, id: string, agent: string, signal?: AbortSignal): Promise<RunCheckpoint[]> {
    return this.request<RunCheckpoint[]>(`/v1/runs/${encodeURIComponent(id)}/checkpoints?agent=${encodeURIComponent(agent)}`, { signal });
  },

  async streamForkRun(this: ApiClient, parentRunId: string, input: ForkRunInput, onEvent: (event: ForkStreamEvent) => void, signal?: AbortSignal): Promise<ForkStreamResult> {
    const execute = async (retryAfterRefresh: boolean): Promise<ForkStreamResult> => {
      const token = await this.tokens.read();
      try {
        const response = await fetch(`${this.baseUrl}/v1/runs/${encodeURIComponent(parentRunId)}/fork`, {
          method: "POST",
          headers: {
            Accept: "text/event-stream",
            "Content-Type": "application/json",
            ...(token ? { Authorization: `Bearer ${token}` } : {}),
          },
          body: JSON.stringify({ ...input, stream: true }),
          signal,
        });
        if (response.status === 401 && retryAfterRefresh && token) {
          await this.refreshToken();
          return execute(false);
        }
        if (!response.ok) throw await apiErrorFromResponse(response);
        if (!response.body) throw new ApiError("服务未返回分叉事件流", { kind: "server" });

        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        let content = "";
        const terminalHolder: { value: ForkStreamResult["terminal"] | null } = { value: null };
        const consume = (rawFrame: string) => {
          let type = "message";
          const dataLines: string[] = [];
          for (const line of rawFrame.split("\n")) {
            if (line.startsWith("event:")) type = line.slice(6).trim();
            if (line.startsWith("data:")) dataLines.push(line.slice(5).trimStart());
          }
          if (!dataLines.length) return;
          const data = JSON.parse(dataLines.join("\n")) as Record<string, unknown>;
          if (type === "chunk" && typeof data.content === "string") content += data.content;
          if (type === "done" || type === "yield") terminalHolder.value = data as unknown as ForkStreamResult["terminal"];
          onEvent({ type, data });
        };

        while (true) {
          const { done, value } = await reader.read();
          buffer += decoder.decode(value, { stream: !done }).replaceAll("\r\n", "\n");
          let boundary = buffer.indexOf("\n\n");
          while (boundary >= 0) {
            consume(buffer.slice(0, boundary));
            buffer = buffer.slice(boundary + 2);
            boundary = buffer.indexOf("\n\n");
          }
          if (done) break;
        }
        if (buffer.trim()) consume(buffer);
        const terminal = terminalHolder.value;
        if (!terminal) throw new ApiError("分叉事件流未返回 done 或 yield", { kind: "server" });
        if (typeof terminal.parent_run_id !== "string" || terminal.parent_run_id !== parentRunId || terminal.parent_seq !== input.seq) {
          throw new ApiError("分叉终止事件的 parent_run_id 或 parent_seq 不一致", { kind: "server" });
        }
        if (typeof terminal.run_id !== "string") throw new ApiError("分叉终止事件缺少 run_id", { kind: "server" });
        return { terminal, content };
      } catch (error) {
        throw normalizeThrownError(error);
      }
    };
    return execute(true);
  },

  async streamResume(this: ApiClient, input: ResumeRunInput, onEvent: (event: ResumeStreamEvent) => void, signal?: AbortSignal): Promise<ResumeStreamResult> {
    const execute = async (retryAfterRefresh: boolean): Promise<ResumeStreamResult> => {
      const token = await this.tokens.read();
      try {
        const response = await fetch(`${this.baseUrl}/v1/resume`, {
          method: "POST",
          headers: {
            Accept: "text/event-stream",
            "Content-Type": "application/json",
            ...(token ? { Authorization: `Bearer ${token}` } : {}),
          },
          body: JSON.stringify({ ...input, stream: true }),
          signal,
        });
        if (response.status === 401 && retryAfterRefresh && token) {
          await this.refreshToken();
          return execute(false);
        }
        if (!response.ok) throw await apiErrorFromResponse(response);
        if (!response.body) throw new ApiError("服务未返回继续执行事件流", { kind: "server" });

        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        let content = "";
        let terminal: ResumeStreamResult["terminal"] | null = null;
        const consume = (rawFrame: string) => {
          let type = "message";
          const dataLines: string[] = [];
          for (const line of rawFrame.split("\n")) {
            if (line.startsWith("event:")) type = line.slice(6).trim();
            if (line.startsWith("data:")) dataLines.push(line.slice(5).trimStart());
          }
          if (!dataLines.length) return;
          const data = JSON.parse(dataLines.join("\n")) as Record<string, unknown>;
          if (type === "chunk" && typeof data.content === "string") content += data.content;
          if (type === "done" || type === "yield") terminal = data as ResumeStreamResult["terminal"];
          onEvent({ type, data });
        };

        while (true) {
          const { done, value } = await reader.read();
          buffer += decoder.decode(value, { stream: !done }).replaceAll("\r\n", "\n");
          let boundary = buffer.indexOf("\n\n");
          while (boundary >= 0) {
            consume(buffer.slice(0, boundary));
            buffer = buffer.slice(boundary + 2);
            boundary = buffer.indexOf("\n\n");
          }
          if (done) break;
        }
        if (buffer.trim()) consume(buffer);
        if (!terminal) throw new ApiError("继续执行事件流未返回 done 或 yield", { kind: "server" });
        return { terminal, content };
      } catch (error) {
        throw normalizeThrownError(error);
      }
    };
    return execute(true);
  },

  listTaskGroups(this: ApiClient, projectId?: string, signal?: AbortSignal, statuses?: string[]): Promise<TaskGroupListResponse> {
    const query = new URLSearchParams();
    if (projectId) query.set("project_id", projectId);
    if (statuses?.length) query.set("status", statuses.join(","));
    return this.request<TaskGroupListResponse>(`/v1/task-groups${query.size ? `?${query.toString()}` : ""}`, { signal });
  },

  getTaskGroup(this: ApiClient, id: string, signal?: AbortSignal): Promise<TaskGroupDetail> {
    return this.request<TaskGroupDetail>(`/v1/task-groups/${encodeURIComponent(id)}`, { signal });
  },

  getJob(this: ApiClient, id: string, signal?: AbortSignal): Promise<Job> {
    return this.request<Job>(`/v1/jobs/${encodeURIComponent(id)}`, { signal });
  },

  cancelJob(this: ApiClient, id: string): Promise<{ status: string }> {
    return this.request<{ status: string }>(`/v1/jobs/${encodeURIComponent(id)}/cancel`, { method: "POST", body: {} });
  },

  listJobs(this: ApiClient, projectId?: string, signal?: AbortSignal): Promise<JobListResponse> {
    const query = projectId ? `?project_id=${encodeURIComponent(projectId)}` : "";
    return this.request<JobListResponse>(`/v1/jobs${query}`, { signal });
  },
};
