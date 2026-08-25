import { useEffect } from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router-dom";

/**
 * 旧版全屏对话 URL 兼容：/conversation?project=P&conversation=C&thread_root=T
 * → /project/P/conversations/C?thread_root=T；无 project → /inbox。
 */
export function RedirectConversation() {
  const navigate = useNavigate();
  const location = useLocation();
  const [searchParams] = useSearchParams();
  const projectId = searchParams.get("project");
  const conversationId = searchParams.get("conversation");
  const threadRootId = searchParams.get("thread_root");

  useEffect(() => {
    if (projectId) {
      const base = `/project/${encodeURIComponent(projectId)}/conversations`;
      const target = conversationId
        ? `${base}/${encodeURIComponent(conversationId)}${threadRootId ? `?thread_root=${encodeURIComponent(threadRootId)}` : ""}`
        : base;
      navigate(`${target}${location.hash}`, { replace: true });
    } else {
      navigate("/inbox", { replace: true });
    }
  }, [conversationId, location.hash, navigate, projectId, threadRootId]);

  return null;
}
