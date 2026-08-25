import { useCallback, useEffect, useState } from "react";
import { ApiError, api, apiErrorMessage, type FinalDeliverable } from "../../api";

export function useDeliverables(projectId: string, conversationId: string | undefined, invalidationVersion = 0, enabled = true) {
  const [items, setItems] = useState<FinalDeliverable[]>([]);
  const [loading, setLoading] = useState(true);
  const [notGenerated, setNotGenerated] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    if (!enabled) {
      setItems([]);
      setLoading(false);
      setError(null);
      setNotGenerated(false);
      return;
    }
    setLoading(true);
    setError(null);
    setNotGenerated(false);
    try {
      if (conversationId) {
        const response = await api.listConversationDeliverables(conversationId);
        setItems(response.deliverables);
        if (!response.deliverables.length) setNotGenerated(true);
      } else {
        const response = await api.listDeliverables(projectId);
        setItems(response.deliverables);
      }
    } catch (requestError) {
      if (requestError instanceof ApiError && requestError.kind === "not_found" && conversationId) {
        setItems([]);
        setNotGenerated(true);
      } else {
        setError(apiErrorMessage(requestError));
      }
    } finally {
      setLoading(false);
    }
  }, [conversationId, enabled, projectId]);

  useEffect(() => { void refresh(); }, [refresh, invalidationVersion]);

  return { items, loading, notGenerated, error, refresh };
}

export type DeliverablesState = ReturnType<typeof useDeliverables>;
