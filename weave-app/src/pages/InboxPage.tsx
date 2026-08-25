import "../styles/inbox.css";

import { useMemo, useState } from "react";
import { MessageSquarePlus, PanelLeftOpen, RefreshCw, UserPlus, Users } from "lucide-react";
import { Link, useNavigate, useOutletContext } from "react-router-dom";
import { api, apiErrorMessage } from "../api";
import { Card } from "../ui/Card";
import { ErrorNotice, LoadingView } from "../ui/StatusViews";
import { useWorkspace } from "../workspace/useWorkspace";
import { isPlatformAsset, TEAM_ARCHITECT_AGENT_NAME } from "../workspace/identity";

export function InboxPage() {
  const workspace = useWorkspace();
  const navigate = useNavigate();
  const { sidebarCollapsed, setSidebarCollapsed } = useOutletContext<{ sidebarCollapsed: boolean; setSidebarCollapsed: (v: boolean) => void }>();
  const [startBusy, setStartBusy] = useState("");
  const [startError, setStartError] = useState<string | null>(null);

  const activeTeams = useMemo(() => workspace.teams.filter((team) => {
    if (team.status === "building" || team.status === "archived") return false;
    const lead = workspace.agents.find((agent) => agent.id === team.lead_avatar_id);
    return !!lead && !lead.deleted && !isPlatformAsset(lead);
  }), [workspace.agents, workspace.teams]);

  async function openTeamConversation(teamId: string, leadAvatarId: string) {
    setStartBusy(teamId); setStartError(null);
    try {
      const quickProject = workspace.projects.find((project) => project.team_id === teamId && !project.archived_at && (project.system_kind === "unclassified" || project.name.startsWith("未分类")))
        || await workspace.ensureUnclassifiedProject(leadAvatarId);
      navigate(`/project/${encodeURIComponent(quickProject.id)}/conversations`);
    } catch (requestError) { setStartError(apiErrorMessage(requestError)); }
    finally { setStartBusy(""); }
  }

  async function openTeamBuilder() {
    setStartBusy("builder"); setStartError(null);
    try {
      const architect = await api.getAgent(TEAM_ARCHITECT_AGENT_NAME);
      if (architect.role !== "avatar" || architect.deleted) throw new Error("元团队架构师不可用");
      const target = await workspace.ensureUnclassifiedProject(architect.id);
      navigate(`/project/${encodeURIComponent(target.id)}/conversations?intent=create_team`);
    } catch (requestError) { setStartError(apiErrorMessage(requestError)); }
    finally { setStartBusy(""); }
  }

  const entries = useMemo(() => workspace.unread
    .map((unread) => ({ updatedAt: unread.updated_at, unread }))
    .sort((left, right) => Date.parse(right.updatedAt) - Date.parse(left.updatedAt)), [workspace.unread]);

  return (
    <div className="page inbox-page">
      <header className="shell-toolbar">{sidebarCollapsed && <button className="icon-button" type="button" aria-label="展开侧栏" onClick={() => setSidebarCollapsed(false)}><PanelLeftOpen size={16} /></button>}<strong className="shell-toolbar__title">收件箱</strong><div className="shell-toolbar__actions"><button className="icon-button inbox-refresh" type="button" aria-label="刷新" title="刷新" onClick={() => void workspace.refresh()}><RefreshCw size={14} /></button></div></header>
      {workspace.loading && !entries.length ? <LoadingView label="正在读取收件箱" /> : !entries.length ? (
        <div className="empty-surface inbox-start">
          {activeTeams.length ? <MessageSquarePlus size={28} /> : <UserPlus size={28} />}
          <h2>{activeTeams.length ? "要做什么？" : "描述你想要的结果"}</h2>
          <p>{activeTeams.length ? "把任务交给你的团队，过程和结果都会回到这里。" : "还没有团队可用。说出你需要什么，会帮你组建一支团队来完成。"}</p>
          {startError && <ErrorNotice message={startError} />}
          <div className="inbox-start__actions">
            {activeTeams.slice(0, 3).map((team) => {
              const lead = workspace.agents.find((agent) => agent.id === team.lead_avatar_id);
              return <button key={team.id} type="button" className="inbox-start__team" disabled={!!startBusy} onClick={() => void openTeamConversation(team.id, team.lead_avatar_id)}>
                <Users size={16} aria-hidden="true" />
                <span><strong>{team.name}</strong><small>{lead?.display_name || lead?.name || "团队负责人"}</small></span>
                <span className="inbox-start__go">{startBusy === team.id ? "正在进入…" : "交给它"}</span>
              </button>;
            })}
            <button type="button" className="inbox-start__builder" disabled={!!startBusy} onClick={() => void openTeamBuilder()}>
              <UserPlus size={16} aria-hidden="true" /> {startBusy === "builder" ? "正在进入…" : activeTeams.length ? "再组建一支团队" : "组建我的第一支团队"}
            </button>
          </div>
        </div>
      ) : <div className="inbox-stream">{entries.map((entry) => {
        const conversation = workspace.conversations.find(({ id }) => id === entry.unread.conversation_id);
        const project = workspace.projects.find(({ id }) => id === conversation?.project_id);
        const destination = conversation ? `/project/${encodeURIComponent(conversation.project_id)}/conversations/${encodeURIComponent(conversation.id)}` : "";
        return <Card className="unread-item" padding="compact" key={`unread:${entry.unread.conversation_id}`}><span className="unread-dot" /><div><span className="item-kicker">未读消息</span><h2>{destination ? <Link to={destination}>{conversation?.title || "未命名会话"}</Link> : "会话当前不可见"}</h2><p>{entry.unread.unread_count} 条未读 · {project?.name || "项目归属不可见"}</p></div><time dateTime={entry.unread.updated_at}>{new Intl.DateTimeFormat("zh-CN", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }).format(new Date(entry.unread.updated_at))}</time></Card>;
      })}</div>}
    </div>
  );
}
