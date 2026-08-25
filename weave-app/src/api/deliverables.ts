import { apiErrorFromResponse, normalizeThrownError } from "./errors";
import type { ApiClient } from "./client";
import type { DeliverableListResponse, DownloadedContent } from "./types";

/** 阶段产物与最终产物的只读列表及下载。 */
export const deliverablesMethods = {
  listDeliverables(this: ApiClient, projectId: string, signal?: AbortSignal): Promise<DeliverableListResponse> {
    return this.request<DeliverableListResponse>(`/v1/deliverables?project_id=${encodeURIComponent(projectId)}`, { signal });
  },

  listConversationDeliverables(this: ApiClient, conversationId: string, signal?: AbortSignal): Promise<DeliverableListResponse> {
    return this.request<DeliverableListResponse>(`/v1/deliverables?conversation_id=${encodeURIComponent(conversationId)}`, { signal });
  },

  async downloadDeliverable(this: ApiClient, id: string, signal?: AbortSignal): Promise<DownloadedContent> {
    const token = await this.tokens.read();
    try {
      const response = await fetch(`${this.baseUrl}/v1/deliverables/${encodeURIComponent(id)}/content`, {
        headers: token ? { Authorization: `Bearer ${token}` } : {},
        signal,
      });
      if (!response.ok) throw await apiErrorFromResponse(response);
      const disposition = response.headers.get("Content-Disposition") || "";
      const filename = disposition.match(/filename="?([^";]+)"?/)?.[1] || `weave-deliverable-${id}.md`;
      return { blob: await response.blob(), filename };
    } catch (error) {
      throw normalizeThrownError(error);
    }
  },
};
