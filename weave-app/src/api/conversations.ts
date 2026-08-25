import { ApiError, apiErrorFromResponse, normalizeThrownError } from "./errors";
import type { ApiClient } from "./client";
import type {
  Attachment,
  ChatRequest,
  ChatRequestRecord,
  ChatStreamEvent,
  ChatStreamResult,
  Conversation,
  CreateThreadInput,
  CreateThreadResponse,
  ExecutionSegment,
  FinalDeliverable,
  Message,
  ThreadListResponse,
  UnreadConversation,
} from "./types";

/** 会话、线程、消息与流式对话。 */
export const conversationsMethods = {
	getChatRequest(this: ApiClient, clientRequestId: string, signal?: AbortSignal): Promise<ChatRequestRecord> {
		return this.request<ChatRequestRecord>(`/v1/chat-requests/${encodeURIComponent(clientRequestId)}`, { signal });
	},

	getConversationChatRequest(this: ApiClient, conversationId: string, signal?: AbortSignal): Promise<ChatRequestRecord> {
		return this.request<ChatRequestRecord>(`/v1/conversations/${encodeURIComponent(conversationId)}/chat-request`, { signal });
	},

  listConversations(this: ApiClient, projectId?: string, signal?: AbortSignal): Promise<Conversation[]> {
    const query = projectId ? `?project_id=${encodeURIComponent(projectId)}` : "";
    return this.request<Conversation[]>(`/v1/conversations${query}`, { signal });
  },

  renameConversation(this: ApiClient, conversationId: string, title: string): Promise<Conversation> {
    return this.request<Conversation>(`/v1/conversations/${encodeURIComponent(conversationId)}`, { method: "PATCH", body: { title } });
  },

  listThreads(this: ApiClient, conversationId: string, signal?: AbortSignal): Promise<ThreadListResponse> {
    return this.request<ThreadListResponse>(`/v1/conversations/${encodeURIComponent(conversationId)}/threads`, { signal });
  },

  createThread(this: ApiClient, conversationId: string, input: CreateThreadInput): Promise<CreateThreadResponse> {
    return this.request<CreateThreadResponse>(`/v1/conversations/${encodeURIComponent(conversationId)}/threads`, { method: "POST", body: input });
  },

  async listMessages(this: ApiClient, conversationId: string, signal?: AbortSignal): Promise<Message[]> {
    const pageSize = 100;
    let offset = 0;
    let latest: Message[] = [];
    while (true) {
      const page = await this.request<Message[]>(`/v1/conversations/${encodeURIComponent(conversationId)}/messages?limit=${pageSize}&offset=${offset}`, { signal });
      if (page.length === 0) return latest;
      latest = page.length === pageSize ? page : [...latest.slice(-(pageSize - page.length)), ...page];
      if (page.length < pageSize) return latest;
      offset += pageSize;
    }
  },

  updateAssistantExecutionSegments(this: ApiClient, conversationId: string, messageId: string, segments: ExecutionSegment[]): Promise<Message> {
    return this.request<Message>(`/v1/conversations/${encodeURIComponent(conversationId)}/messages/${encodeURIComponent(messageId)}/assistant-execution-segments`, {
      method: "PATCH",
      body: { segments },
    });
  },

  markConversationRead(this: ApiClient, conversationId: string, lastMessageId: string): Promise<{ status: string }> {
    return this.request<{ status: string }>(`/v1/conversations/${encodeURIComponent(conversationId)}/read`, { method: "POST", body: { last_message_id: lastMessageId } });
  },

  listUnread(this: ApiClient, signal?: AbortSignal): Promise<UnreadConversation[]> {
    return this.request<UnreadConversation[]>("/v1/inbox/unread", { signal });
  },

  listAttachments(this: ApiClient, signal?: AbortSignal): Promise<{ attachments: Attachment[] }> {
    return this.request<{ attachments: Attachment[] }>("/v1/attachments", { signal });
  },

  async uploadAttachment(this: ApiClient, file: File): Promise<Attachment> {
    const token = await this.tokens.read();
    const form = new FormData();
    form.append("file", file);
    try {
      const response = await fetch(`${this.baseUrl}/v1/attachments`, {
        method: "POST",
        headers: token ? { Authorization: `Bearer ${token}`, Accept: "application/json" } : { Accept: "application/json" },
        body: form,
      });
      if (!response.ok) throw await apiErrorFromResponse(response);
      return (await response.json()) as Attachment;
    } catch (error) {
      throw normalizeThrownError(error);
    }
  },

  getConversationDeliverable(this: ApiClient, conversationId: string, signal?: AbortSignal): Promise<FinalDeliverable> {
    return this.request<FinalDeliverable>(`/v1/conversations/${encodeURIComponent(conversationId)}/deliverable`, { signal });
  },

  promoteMessageDeliverable(this: ApiClient, messageId: string, title?: string): Promise<FinalDeliverable> {
    return this.request<FinalDeliverable>(`/v1/messages/${encodeURIComponent(messageId)}/deliverable`, {
      method: "POST",
      body: title ? { title } : {},
    });
  },

  async streamChat(this: ApiClient, input: ChatRequest, onEvent: (event: ChatStreamEvent) => void, signal?: AbortSignal): Promise<ChatStreamResult> {
    const execute = async (retryAfterRefresh: boolean): Promise<ChatStreamResult> => {
      const token = await this.tokens.read();
      try {
        const response = await fetch(`${this.baseUrl}/v1/chat`, {
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
        if (!response.body) throw new ApiError("服务未返回对话事件流", { kind: "server" });

        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";
        let content = "";
        let terminal: ChatStreamResult["terminal"] | null = null;
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
          if (type === "done" || type === "yield") terminal = data as ChatStreamResult["terminal"];
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
        if (!terminal) throw new ApiError("对话事件流未返回 done 或 yield", { kind: "server" });
        return { terminal, content };
      } catch (error) {
        throw normalizeThrownError(error);
      }
    };
    return execute(true);
  },
};
