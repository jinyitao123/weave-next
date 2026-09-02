import { useCallback, useEffect, useMemo, useState } from "react";
import { RefreshCw } from "lucide-react";
import { Link } from "react-router-dom";
import { api, apiErrorMessage, type RunSummary } from "../../api";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { useWorkspace } from "../../workspace/useWorkspace";
import { costLabel, usageLabel } from "../../workspace/labels";

interface ProjectRunsTabProps {
  projectId: string;
  teamName: string;
}

const when = new Intl.DateTimeFormat("zh-CN", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });

function statusTone(status: string): "neutral" | "success" | "danger" | "warning" | "accent" {
  if (status === "completed" || status === "success") return "success";
  if (status === "failed" || status === "cancelled" || status === "timed_out") return "danger";
  if (status === "running" || status === "active" || status === "queued") return "accent";
  return "neutral";
}

function statusLabel(status: string): string {
  const labels: Record<string, string> = {
    completed: "已完成", success: "成功", failed: "失败", running: "运行中", active: "执行中",
    queued: "排队中", cancelled: "已取消", timed_out: "超时", superseded: "已取代",
  };
  return labels[status] || "状态未知";
}

function formatTime(value?: string): string {
  if (!value) return "未记录";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : when.format(date);
}

export function ProjectRunsTab({ projectId, teamName }: ProjectRunsTabProps) {
  const workspace = useWorkspace();
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [conversationFilter, setConversationFilter] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const projectConversations = useMemo(() => workspace.conversations
    .filter((conversation) => conversation.project_id === projectId && !conversation.parent_message_id)
    .sort((left, right) => Date.parse(right.updated_at) - Date.parse(left.updated_at)),
  [projectId, workspace.conversations]);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const runResponse = await api.listRuns({ projectId, conversationId: conversationFilter || undefined });
      setRuns(runResponse.runs);
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setLoading(false);
    }
  }, [conversationFilter, projectId]);

  useEffect(() => { void load(); }, [load, workspace.invalidationVersion]);

  const conversationById = useMemo(() => new Map(projectConversations.map((conversation) => [conversation.id, conversation])), [projectConversations]);

  return (
    <div className="project-runs-tab">
      <header className="project-tab-heading">
        <div>
          <h2>运行记录</h2>
          <p>这里列出该项目下会话产生的运行。</p>
        </div>
        <Button variant="ghost" size="small" onClick={() => void load()}><RefreshCw size={16} />重读</Button>
      </header>
      <div className="project-runs-toolbar">
        <label className="compact-field"><span>来源会话</span>
          <select value={conversationFilter} onChange={(event) => setConversationFilter(event.target.value)}>
            <option value="">全部会话</option>
            {projectConversations.map((conversation) => <option key={conversation.id} value={conversation.id}>{conversation.title || "未命名会话"}</option>)}
          </select>
        </label>
        <span className="catalog-count">{runs.length} 条运行</span>
      </div>
      {error && <ErrorNotice message={error} onRetry={() => void load()} />}
      {loading && !runs.length ? <LoadingView label="正在读取运行记录" /> : runs.length === 0 ? (
        <div className="empty-surface"><h2>暂无运行记录</h2><p>在该项目发起会话后，对应的运行会出现在这里。</p></div>
      ) : (
        <div className="project-runs-table" role="table" aria-label="项目运行记录">
          <div className="project-runs-row project-runs-row--head" role="row" aria-hidden="true">
            <span role="columnheader">状态</span>
            <span role="columnheader">承担团队</span>
            <span role="columnheader">来源会话</span>
            <span role="columnheader">开始 / 结束</span>
            <span role="columnheader">模型用量</span>
          </div>
          {runs.map((run) => {
            const conversation = run.conversation_id ? conversationById.get(run.conversation_id) : undefined;
            return (
              <div className="project-runs-row" role="row" key={run.run_id}>
                <span role="cell"><Badge tone={statusTone(run.status)}>{statusLabel(run.status)}</Badge></span>
                <span role="cell">{teamName}</span>
                <span role="cell">{run.conversation_id ? <Link className="text-link" to={`/project/${encodeURIComponent(projectId)}/conversations/${encodeURIComponent(run.conversation_id)}`}>{conversation?.title || "未命名会话"}</Link> : "未归属会话"}</span>
                <span role="cell">{formatTime(run.started_at)} → {formatTime(run.ended_at)}</span>
                <span role="cell">{usageLabel(run.tokens_in, run.tokens_out)} · {costLabel(run.cost_usd)}</span>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
