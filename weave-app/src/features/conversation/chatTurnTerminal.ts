import type { Dispatch, MutableRefObject, SetStateAction } from "react";
import type { ChatRequest, ChatStreamResult, ExecutionSegment, Message } from "../../api";
import type { PendingTurn } from "./ConversationMessage";

export interface ChatTurnTerminalContext {
  projectId: string;
  conversationId: string;
  isSelectedThread: boolean;
  threadRootId: string;
  pendingRef: MutableRefObject<PendingTurn | null>;
  setPending: Dispatch<SetStateAction<PendingTurn | null>>;
  clearPending: () => void;
  setMessages: Dispatch<SetStateAction<Message[]>>;
  setCorrectingConversation: (conversationId: string) => void;
  navigateConversation: (conversationId: string, threadRootId?: string, replace?: boolean) => void;
  reloadUntilMessage: (conversationId: string, userMessageId?: string, assistantContent?: string) => Promise<Message | undefined>;
  updateAssistantExecutionSegments: (conversationId: string, messageId: string, segments: ExecutionSegment[]) => Promise<Message>;
}

export async function handleChatTurnTerminal(
  context: ChatTurnTerminalContext,
  request: ChatRequest,
  result: ChatStreamResult,
): Promise<void> {
  const terminal = result.terminal;
  if (terminal.runtime_assignment) {
    context.setPending((current) => current ? { ...current, runtimeAssignment: terminal.runtime_assignment } : current);
  }
  if (terminal.error) throw new Error(terminal.error);
  if (request.conversation_id && terminal.conversation_id !== request.conversation_id) {
    throw new Error("服务端返回了不一致的会话归属");
  }
  if (terminal.project_id && terminal.project_id !== context.projectId) {
    throw new Error("服务端返回了不一致的项目归属");
  }
  if (terminal.user_message_id && !terminal.conversation_id) {
    throw new Error("服务端返回了消息标识但缺少会话标识");
  }
  const correctedConversationId = terminal.conversation_id || context.conversationId;
  if (!correctedConversationId) {
    throw new Error("服务端未返回新会话标识");
  }
  const assistantContent = typeof terminal.output === "string" && terminal.output.trim()
    ? terminal.output
    : result.content;
  const completedSegments = context.pendingRef.current?.segments || [];
  const assistantMessage = await context.reloadUntilMessage(
    correctedConversationId,
    terminal.user_message_id,
    assistantContent || undefined,
  );
  if (assistantMessage && completedSegments.length > 0) {
    await persistExecutionSegments(context, correctedConversationId, assistantMessage.id, completedSegments);
  }
  context.pendingRef.current = null;
  context.clearPending();
  if (correctedConversationId !== context.conversationId) {
    context.setCorrectingConversation(correctedConversationId);
    const threadRootQuery = context.isSelectedThread && context.threadRootId ? context.threadRootId : undefined;
    context.navigateConversation(correctedConversationId, threadRootQuery, true);
  }
}

async function persistExecutionSegments(
  context: ChatTurnTerminalContext,
  conversationId: string,
  messageId: string,
  segments: ExecutionSegment[],
): Promise<void> {
  try {
    const updated = await context.updateAssistantExecutionSegments(conversationId, messageId, segments);
    context.setMessages((current) => current
      .map((message) => message.id === updated.id ? updated : message)
      .sort((left, right) => left.seq - right.seq));
  } catch (segmentError) {
    console.warn("persist execution segments failed", segmentError);
  }
}
