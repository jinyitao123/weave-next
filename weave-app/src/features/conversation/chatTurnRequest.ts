import type { AgentRecord, ChatRequest, MessageAttachment, Project } from "../../api";
import { createClientUUID } from "../../platform/uuid";
import type { PendingTurn } from "./ConversationMessage";

export interface ChatTurnTargetConversation {
  id: string;
  channel: string;
  session_key?: string;
}

export interface ChatTurnBlockInput {
  selectedProject?: Project;
  selectedAvatar?: AgentRecord;
  uploadingAttachments: boolean;
  conversationId: string;
  exactTargetConversation?: ChatTurnTargetConversation;
  currentPending: PendingTurn | null;
}

export function chatTurnBlockedReason(input: ChatTurnBlockInput): string {
  return !input.selectedProject ? "当前项目尚未加载完成，暂时不能发送。"
    : !input.selectedAvatar ? "当前项目没有可用的团队负责人，暂时不能发送。"
      : input.uploadingAttachments ? "附件仍在上传，请稍后再发送。"
        : input.conversationId && !input.exactTargetConversation ? "当前会话归属尚未确认，暂时不能发送。"
          : input.currentPending?.state === "sending" ? "上一条消息仍在发送中。"
            : "";
}

export interface BuildChatTurnRequestInput {
  currentPending: PendingTurn | null;
  selectedProject: Project;
  selectedAvatar: AgentRecord;
  exactTargetConversation?: ChatTurnTargetConversation;
  conversationIntent?: "create_team";
  isCLIEngine: boolean;
  selectedRuntimeId: string;
  messageOverride?: string;
  composerValue?: string;
  draftRefValue: string;
  draft: string;
  selectedAttachments: string[];
  blueprintChangeRequested: boolean;
}

export function buildChatTurnRequest(input: BuildChatTurnRequestInput): ChatRequest {
  if (input.currentPending?.state === "failed") {
    return {
      ...input.currentPending.request,
      client_request_id: createClientUUID(),
      message: input.currentPending.content,
      stream: true,
    };
  }
  return {
    agent: input.selectedAvatar.name,
    project_id: input.selectedProject.id,
    conversation_id: input.exactTargetConversation?.id,
    intent: !input.exactTargetConversation && input.conversationIntent === "create_team" ? "create_team" : undefined,
    runtime_id: input.isCLIEngine && input.selectedRuntimeId ? input.selectedRuntimeId : undefined,
    client_request_id: createClientUUID(),
    session_id: conversationSessionId(input.exactTargetConversation?.session_key),
    message: input.messageOverride?.trim() || input.composerValue?.trim() || input.draftRefValue.trim() || input.draft.trim(),
    channel: input.exactTargetConversation?.channel || "default",
    attachment_ids: [...input.selectedAttachments],
    blueprint_change_requested: input.blueprintChangeRequested || undefined,
    stream: true,
  };
}

export interface CreatePendingTurnInput {
  request: ChatRequest;
  currentPending: PendingTurn | null;
  selectedAttachmentFacts: MessageAttachment[];
}

export function createPendingTurn(input: CreatePendingTurnInput): PendingTurn {
  return {
    clientRequestId: input.request.client_request_id,
    content: input.request.message,
    state: "sending",
    streamedContent: "",
    activity: "等待服务端响应",
    toolCalls: [],
    segments: [],
    agentOutputs: {},
    request: input.request,
    attachments: input.currentPending?.state === "failed"
      ? input.currentPending.attachments
      : input.selectedAttachmentFacts,
    runtimeAssignment: input.currentPending?.state === "failed"
      ? undefined
      : input.currentPending?.runtimeAssignment,
    startedAt: Date.now(),
  };
}

function conversationSessionId(sessionKey?: string): string {
  if (!sessionKey) return "";
  const parts = sessionKey.split(":");
  return parts.length >= 4 ? parts.slice(3).join(":") : "";
}
