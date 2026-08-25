import { useEffect, useMemo, useRef, useState } from "react";
import { Archive, ArrowRightLeft, Brain, FolderKanban, ListChecks, MoreHorizontal, PanelLeftOpen, Pencil, RotateCcw, Users } from "lucide-react";
import { useLocation, useNavigate, useOutletContext, useParams, useSearchParams } from "react-router-dom";
import { apiErrorMessage } from "../../api";
import { MoveProjectForm } from "../../features/projects/MoveProjectForm";
import { ProjectForm } from "../../features/projects/ProjectForm";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { ConfirmModal } from "../../ui/ConfirmModal";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { useWorkspace } from "../../workspace/useWorkspace";
import { isPlatformAsset, isTeamBuildProject, isUnclassifiedProject, projectDisplayName } from "../../workspace/identity";
import { ProjectConversationsTab } from "./ProjectConversationsTab";
import { ProjectDeliverablesTab } from "./ProjectDeliverablesTab";
import { ProjectMemoryPanel } from "./ProjectMemoryPanel";
import { ProjectRunsTab } from "./ProjectRunsTab";
import { ProjectTeamTab } from "./ProjectTeamTab";
import "../../styles/project-container.css";

type ProjectTab = "conversations" | "team" | "runs" | "deliverables";

function activeTab(pathname: string, projectId: string): ProjectTab {
  const base = `/project/${encodeURIComponent(projectId)}`;
  if (pathname.startsWith(`${base}/conversations`)) return "conversations";
  if (pathname.startsWith(`${base}/team`)) return "team";
  if (pathname.startsWith(`${base}/runs`)) return "runs";
  if (pathname.startsWith(`${base}/deliverables`)) return "deliverables";
  return "conversations";
}

export function ProjectContainer() {
  const params = useParams();
  const projectId = params.projectId || "";
  const conversationId = params.conversationId;
  const [searchParams] = useSearchParams();
  const threadRootId = searchParams.get("thread_root") || "";
  const conversationIntent = searchParams.get("intent") === "create_team" ? "create_team" : undefined;
  const location = useLocation();
  const navigate = useNavigate();
  const workspace = useWorkspace();
  const { sidebarCollapsed, setSidebarCollapsed } = useOutletContext<{ sidebarCollapsed: boolean; setSidebarCollapsed: (v: boolean) => void }>();
  const [editorOpen, setEditorOpen] = useState(false);
  const [moving, setMoving] = useState(false);
  const [memoryOpen, setMemoryOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDetailsElement>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [deliverablesOpen, setDeliverablesOpen] = useState(false);
  const [confirmArchive, setConfirmArchive] = useState(false);

  const activeTeams = useMemo(() => workspace.teams.filter((team) => {
    if (team.status !== "active" && team.status !== "needs_repair") return false;
    const lead = workspace.agents.find((agent) => agent.id === team.lead_avatar_id);
    return !!lead && !lead.deleted && !isPlatformAsset(lead);
  }), [workspace.agents, workspace.teams]);

  const project = workspace.projects.find((item) => item.id === projectId);
  const team = project ? activeTeams.find((item) => item.id === project.team_id) : undefined;
  const teamBuild = project ? isTeamBuildProject(project, workspace.teams, workspace.agents) : false;
  const projectName = project ? projectDisplayName(project, workspace.teams, workspace.agents) : "";
  const tab = activeTab(location.pathname, projectId);

  async function runProjectAction(action: () => Promise<unknown>) {
    setBusy(true);
    setActionError(null);
    try {
      await action();
    } catch (error) {
      setActionError(apiErrorMessage(error));
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    if (!menuOpen) return;
    function handleOutsideClick(event: globalThis.MouseEvent) {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) {
        setMenuOpen(false);
      }
    }
    document.addEventListener("mousedown", handleOutsideClick);
    return () => document.removeEventListener("mousedown", handleOutsideClick);
  }, [menuOpen]);

  if (workspace.loading && !workspace.projects.length && !activeTeams.length) {
    return <LoadingView label="正在读取项目" />;
  }

  if (!project) {
    return (
      <div className="page">
        <ErrorNotice message={projectId ? "项目不存在，或已被删除、没有访问权限。" : "缺少项目标识。"} />
        <Button variant="primary" onClick={() => navigate("/inbox")}>返回收件箱</Button>
      </div>
    );
  }

  const lead = workspace.agents.find((agent) => agent.id === (team?.lead_avatar_id || project.avatar_id));

  return (
    <div className="project-container">
      <header className="project-container__header">
        {sidebarCollapsed && <button className="sidebar-reveal" type="button" aria-label="展开侧栏" onClick={() => setSidebarCollapsed(false)}><PanelLeftOpen size={16} /></button>}
        <div className="project-container__title">
          <p className="eyebrow">{teamBuild ? "元团队 · 团队架构师" : team ? `${team.name} · ${lead?.display_name || lead?.name || "负责人"}` : "未归属团队"}</p>
          <h1>
            <FolderKanban size={20} aria-hidden="true" />
            <span>{projectName}</span>
            {project.archived_at && <Badge tone="neutral">已归档</Badge>}
            {teamBuild ? <Badge>团队搭建</Badge> : isUnclassifiedProject(project) && <Badge>快速会话</Badge>}
          </h1>
        </div>
        <div className="project-container__actions">
          <button className="project-container__deliverables" type="button" aria-label="查看交付物" aria-expanded={deliverablesOpen} onClick={() => setDeliverablesOpen((current) => !current)}><svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M3 8 12 3l9 5v8l-9 5-9-5z" /><path d="M3 8l9 5 9-5" /><path d="M12 13v8" /></svg></button>
          <details className="row-menu" ref={menuRef} open={menuOpen} onToggle={(event) => setMenuOpen(event.currentTarget.open)}>
            <summary aria-label={`${projectName} 操作`}><MoreHorizontal size={16} /></summary>
            <div className="row-menu__popover">
              <button type="button" onClick={() => { setMenuOpen(false); setEditorOpen(true); }}><Pencil size={16} />重命名</button>
              <button type="button" disabled={activeTeams.length < 2 || !!project.archived_at} onClick={() => { setMenuOpen(false); setMoving(true); }}><ArrowRightLeft size={16} />移动团队</button>
              <button type="button" onClick={() => { setMenuOpen(false); setMemoryOpen(true); }}><Brain size={16} />项目事实</button>
              {team && <button type="button" onClick={() => { setMenuOpen(false); navigate(`/project/${encodeURIComponent(project.id)}/team`); }}><Users size={16} />参与团队</button>}
              <button type="button" onClick={() => { setMenuOpen(false); navigate(`/project/${encodeURIComponent(project.id)}/runs`); }}><ListChecks size={16} />运行记录</button>
              {project.archived_at ? (
                <button type="button" disabled={busy} onClick={() => { setMenuOpen(false); void runProjectAction(() => workspace.restoreProject(project.id)); }}><RotateCcw size={16} />恢复</button>
              ) : (
                <button type="button" disabled={busy} onClick={() => { setMenuOpen(false); setConfirmArchive(true); }}><Archive size={16} />归档</button>
              )}
            </div>
          </details>
        </div>
      </header>

      {actionError && <div className="project-container__notice"><ErrorNotice message={actionError} /></div>}

      <div className="project-container__content">
        {tab === "conversations" && <ProjectConversationsTab projectId={project.id} conversationId={conversationId} conversationIntent={conversationIntent} threadRootId={threadRootId} deliverablesOpen={deliverablesOpen} onDeliverablesOpenChange={setDeliverablesOpen} />}
        {tab === "team" && team && <ProjectTeamTab projectId={project.id} teamId={team.id} />}
        {tab === "team" && !team && <div className="project-tab-empty"><FolderKanban size={28} /><h2>团队归属不可解析</h2><p>项目当前未关联到可用团队，请在后台检查团队与负责人配置。</p></div>}
        {tab === "runs" && <ProjectRunsTab projectId={project.id} teamName={team?.name || lead?.display_name || lead?.name || "未知团队"} />}
        {tab === "deliverables" && <ProjectDeliverablesTab projectId={project.id} />}
      </div>

      <ConfirmModal
        open={confirmArchive}
        title="归档项目"
        description="归档后项目从侧栏隐藏，会话与运行记录保留，可随时恢复。"
        confirmLabel="归档"
        busy={busy}
        onConfirm={() => { setConfirmArchive(false); void runProjectAction(() => workspace.archiveProject(project.id)); }}
        onClose={() => setConfirmArchive(false)}
      ><p className="confirm-copy">确定归档「{projectName}」？</p></ConfirmModal>
      <ProjectForm
        open={editorOpen}
        project={project}
        teams={activeTeams}
        agents={workspace.agents}
        onClose={() => setEditorOpen(false)}
        onCreate={workspace.createProject}
        onUpdate={workspace.updateProject}
      />
      {moving && project && (
        <MoveProjectForm
          open
          project={project}
          teams={activeTeams}
          agents={workspace.agents}
          onClose={() => setMoving(false)}
          onMove={workspace.moveProject}
        />
      )}
      <ProjectMemoryPanel project={project} open={memoryOpen} onClose={() => setMemoryOpen(false)} />
    </div>
  );
}
