import type { ApiClient } from "./client";
import type { CompleteHumanTaskInput, CompleteHumanTaskOutcome, HumanTasksResponse } from "./types";

export const humanTasksChangedEvent = "weave:human-tasks-changed";

export const humanTasksMethods = {
  listHumanTasks(this: ApiClient, input: { limit?: number; cursor?: string } = {}, signal?: AbortSignal): Promise<HumanTasksResponse> {
    const query = new URLSearchParams();
    if (input.limit) query.set("limit", String(input.limit));
    if (input.cursor) query.set("cursor", input.cursor);
    const suffix = query.size ? `?${query.toString()}` : "";
    return this.request<HumanTasksResponse>(`/v1/human-tasks${suffix}`, { signal });
  },

  completeHumanTask(this: ApiClient, runID: string, input: CompleteHumanTaskInput): Promise<CompleteHumanTaskOutcome> {
    return this.request<CompleteHumanTaskOutcome>(`/v1/human-tasks/${encodeURIComponent(runID)}/complete`, {
      method: "POST",
      body: input,
    });
  },
};
