import { useEffect } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

/**
 * /project-data 与 /project-execution 退役重定向：优先取 ?project=，其次
 * localStorage 中最近访问的项目，最后落回收件箱。
 */
export function LegacyProjectRedirect({ tab }: { tab: "runs" | "conversations" }) {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const projectId = searchParams.get("project");

  useEffect(() => {
    const resolved = projectId || window.localStorage.getItem("weave.lastProjectId") || "";
    if (resolved) {
      navigate(`/project/${encodeURIComponent(resolved)}/${tab}`, { replace: true });
    } else {
      navigate("/inbox", { replace: true });
    }
  }, [navigate, projectId, tab]);

  return null;
}

