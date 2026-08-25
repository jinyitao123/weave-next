import type { Dispatch, MutableRefObject, SetStateAction } from "react";
import { api, apiErrorMessage, normalizeThrownError, type ChatRequestRecord, type Message } from "../../api";
import type { PendingTurn } from "./ConversationMessage";

export interface ChatTurnRecoveryContext {
  projectId: string;
  conversationId: string;
  isSelectedThread: boolean;
  threadRootId: string;
  pendingRef: MutableRefObject<PendingTurn | null>;
  abortRef: MutableRefObject<AbortController | null>;
  setPending: Dispatch<SetStateAction<PendingTurn | null>>;
  clearPending: () => void;
  routeKey: () => string;
  setCorrectingConversation: (conversationId: string) => void;
  navigateConversation: (conversationId: string, threadRootId?: string, replace?: boolean) => void;
  reloadUntilMessage: (conversationId: string, userMessageId?: string, assistantContent?: string) => Promise<Message | undefined>;
  refreshConversations: (projectId?: string) => Promise<unknown> | unknown;
}

export async function convergeChatRequest(
  context: ChatTurnRecoveryContext,
  record: ChatRequestRecord,
  source: PendingTurn,
): Promise<boolean> {
  const current = context.pendingRef.current;
  if (!current || current.clientRequestId !== source.clientRequestId) return true;
  if (record.status === "failed") {
    const failed: PendingTurn = {
      ...source,
      state: "failed",
      interrupted: false,
      interruptedReason: undefined,
      persistedUserMessageId: persistedUserMessageId(record) || source.persistedUserMessageId,
      error: record.response?.error || "服务端执行失败。可恢复为草稿后重新发送。",
    };
    context.pendingRef.current = failed;
    context.setPending(failed);
    return true;
  }
  if (record.status !== "completed" && record.status !== "yielded") return false;
  const terminal = record.response || {};
  const correctedConversationId = record.conversation_id || terminal.conversation_id || source.request.conversation_id;
  if (!correctedConversationId) throw new Error("服务端执行已结束，但没有返回会话标识");
  const assistantContent = typeof terminal.output === "string" && terminal.output.trim() ? terminal.output : undefined;
  await context.reloadUntilMessage(correctedConversationId, record.user_message_id || terminal.user_message_id, assistantContent);
  await context.refreshConversations(context.projectId);
  context.pendingRef.current = null;
  context.clearPending();
  if (correctedConversationId !== context.conversationId) {
    context.setCorrectingConversation(correctedConversationId);
    const threadRootQuery = context.isSelectedThread && context.threadRootId ? context.threadRootId : undefined;
    context.navigateConversation(correctedConversationId, threadRootQuery, true);
  }
  return true;
}

export function mergeChatRequestStatus(current: PendingTurn, record: ChatRequestRecord): PendingTurn {
  return {
    ...current,
    activity: chatRequestActivity(record),
    persistedUserMessageId: persistedUserMessageId(record) || current.persistedUserMessageId,
    runtimeAssignment: record.runtime_assignment || current.runtimeAssignment,
  };
}

export async function adoptAdmittedConversation(
  context: ChatTurnRecoveryContext,
  record: ChatRequestRecord,
  source: PendingTurn,
): Promise<void> {
  const admittedConversationId = record.conversation_id || record.response?.conversation_id;
  if (!admittedConversationId || context.routeKey() === `${context.projectId}:${admittedConversationId}`) return;
  if (source.request.conversation_id && source.request.conversation_id !== admittedConversationId) {
    throw new Error("服务端返回了不一致的会话归属");
  }
  const current = context.pendingRef.current;
  if (!current || current.clientRequestId !== source.clientRequestId) return;
  const rebound: PendingTurn = {
    ...current,
    request: { ...current.request, conversation_id: admittedConversationId },
    persistedUserMessageId: persistedUserMessageId(record) || current.persistedUserMessageId,
  };
  context.pendingRef.current = rebound;
  context.setPending(rebound);
  await context.refreshConversations(context.projectId);
  context.setCorrectingConversation(admittedConversationId);
  context.navigateConversation(admittedConversationId, undefined, true);
}

export async function pollAttachedWorkflowProgress(
  context: ChatTurnRecoveryContext,
  source: PendingTurn,
  controller: AbortController,
): Promise<void> {
  while (!controller.signal.aborted) {
    try {
      const record = await api.getChatRequest(source.clientRequestId, controller.signal);
      if (record.status === "completed" || record.status === "failed" || record.status === "yielded") {
        if (await convergeChatRequest(context, record, source)) controller.abort();
        return;
      }
      await adoptAdmittedConversation(context, record, source);
      context.setPending((current) => current?.clientRequestId === source.clientRequestId
        ? mergeChatRequestStatus(current, record)
        : current);
    } catch (error) {
      if (normalizeThrownError(error).kind === "aborted") return;
    }
    await delay(1000);
  }
}

export async function reconnectPendingTurn(
  context: ChatTurnRecoveryContext,
  source = context.pendingRef.current,
): Promise<void> {
  if (!source) return;
  const reconnectRouteKey = context.routeKey();
  const controller = new AbortController();
  context.abortRef.current?.abort();
  context.abortRef.current = controller;
  const reconnecting: PendingTurn = {
    ...source,
    state: "sending",
    interrupted: undefined,
    interruptedReason: undefined,
    error: undefined,
    activity: "正在恢复执行状态",
  };
  context.pendingRef.current = reconnecting;
  context.setPending(reconnecting);
  try {
    while (!controller.signal.aborted) {
      const record = await api.getChatRequest(reconnecting.clientRequestId, controller.signal);
      if (await convergeChatRequest(context, record, reconnecting)) return;
      await adoptAdmittedConversation(context, record, reconnecting);
      context.setPending((current) => current ? mergeChatRequestStatus(current, record) : current);
      await delay(1000);
    }
    if (context.routeKey() === reconnectRouteKey) {
      setStopped(context, reconnecting);
    }
  } catch (error) {
    if (normalizeThrownError(error).kind === "aborted") {
      if (context.routeKey() === reconnectRouteKey) {
        setStopped(context, reconnecting);
      }
      return;
    }
    if (context.routeKey() !== reconnectRouteKey) return;
    const disconnected: PendingTurn = {
      ...reconnecting,
      state: "failed",
      interrupted: true,
      interruptedReason: "disconnected",
      error: apiErrorMessage(error),
    };
    context.pendingRef.current = disconnected;
    context.setPending(disconnected);
  } finally {
    if (context.abortRef.current === controller) context.abortRef.current = null;
  }
}

function chatRequestActivity(record: ChatRequestRecord): string {
  const progress = record.workflow_progress;
  if (!progress) return "后台仍在执行";
  const count = progress.total_stages > 0
    ? `${progress.completed_stages}/${progress.total_stages} 个阶段`
    : `${progress.completed_stages} 个阶段`;
  if (progress.latest_stage) return `已完成 ${count} · 最近：${progress.latest_stage}`;
  if (progress.status === "queued") return `工作流已进入队列 · ${count}`;
  return `工作流正在执行 · 已完成 ${count}`;
}

function persistedUserMessageId(record: ChatRequestRecord): string | undefined {
  return record.user_message_id || record.response?.user_message_id;
}

function setStopped(context: ChatTurnRecoveryContext, reconnecting: PendingTurn): void {
  const stopped = {
    ...reconnecting,
    state: "failed" as const,
    interrupted: true,
    interruptedReason: "stopped" as const,
  };
  context.pendingRef.current = stopped;
  context.setPending(stopped);
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}
