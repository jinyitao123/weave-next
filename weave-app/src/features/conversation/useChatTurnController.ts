import { useCallback, useEffect, useRef, useState, type Dispatch, type MutableRefObject, type SetStateAction } from "react";
import type { PendingTurn } from "./ConversationMessage";

const PENDING_CHAT_STORAGE_KEY = "weave.pending-chat-request.v1";

export interface ChatTurnController {
  pending: PendingTurn | null;
  pendingRef: MutableRefObject<PendingTurn | null>;
  setPending: Dispatch<SetStateAction<PendingTurn | null>>;
  clearPending: () => void;
  readStoredPending: () => PendingTurn | null;
}

function storedPendingTurn(pending: PendingTurn): PendingTurn {
  return {
    ...pending,
    streamedContent: "",
    toolCalls: [],
    segments: [],
    agentOutputs: {},
  };
}

export function useChatTurnController(): ChatTurnController {
  const [pending, setPendingState] = useState<PendingTurn | null>(null);
  const pendingRef = useRef<PendingTurn | null>(null);

  const setPending: Dispatch<SetStateAction<PendingTurn | null>> = useCallback((next) => {
    setPendingState((current) => {
      const resolved = typeof next === "function" ? next(current) : next;
      pendingRef.current = resolved;
      return resolved;
    });
  }, []);

  const clearPending = useCallback(() => {
    pendingRef.current = null;
    window.sessionStorage.removeItem(PENDING_CHAT_STORAGE_KEY);
    setPendingState(null);
  }, []);

  const readStoredPending = useCallback((): PendingTurn | null => {
    const encoded = window.sessionStorage.getItem(PENDING_CHAT_STORAGE_KEY);
    if (!encoded) return null;
    try {
      return JSON.parse(encoded) as PendingTurn;
    } catch {
      window.sessionStorage.removeItem(PENDING_CHAT_STORAGE_KEY);
      return null;
    }
  }, []);

  useEffect(() => {
    pendingRef.current = pending;
    if (!pending) return;
    try {
      window.sessionStorage.setItem(PENDING_CHAT_STORAGE_KEY, JSON.stringify(storedPendingTurn(pending)));
    } catch {
      // Status recovery remains available through the server even when
      // private browsing or storage quota disables this tab-local hint.
    }
  }, [pending]);

  return { pending, pendingRef, setPending, clearPending, readStoredPending };
}
