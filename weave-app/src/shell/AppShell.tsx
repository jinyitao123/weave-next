import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent, type PointerEvent } from "react";
import { createPortal } from "react-dom";
import { ChevronDown, FolderKanban, Inbox, LogOut, Menu, MessageSquarePlus, PanelLeftClose, Pencil, Search, Settings2, UserPlus, Users, X } from "lucide-react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { api, apiErrorMessage, type Conversation, type Project, type TeamBuildRunSummary } from "../api";
import { useAuth } from "../auth/useAuth";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Field } from "../ui/Field";
import { Modal } from "../ui/Modal";
import { useWorkspace } from "../workspace/useWorkspace";
import { isPlatformAsset, isUnclassifiedProject, META_TEAM_NAME, TEAM_ARCHITECT_AGENT_NAME } from "../workspace/identity";
import { useSidebarState } from "./useSidebarState";

const drawerFocusableSelector = "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex]:not([tabindex='-1'])";

function drawerFocusableElements(drawer: HTMLElement): HTMLElement[] {
  return Array.from(drawer.querySelectorAll<HTMLElement>(drawerFocusableSelector))
    .filter((element) => element.getClientRects().length > 0);
}

function teamBuildStatus(run: TeamBuildRunSummary | undefined, ready: boolean): { label: string; tone: "neutral" | "accent" | "success" | "warning" | "danger" } | null {
  if (!ready) return null;
  if (!run) return { label: "需求讨论", tone: "neutral" };
  switch (run.status) {
    case "planning":
      return { label: "待确认", tone: "warning" };
    case "authorized":
    case "round_running":
    case "publishing":
      return { label: "构建中", tone: "accent" };
    case "passed":
      return { label: "已完成", tone: "success" };
    case "blocked":
      return { label: "失败", tone: "danger" };
    case "cancelled":
      return { label: "已取消", tone: "neutral" };
  }
}

export function AppShell() {
  const { user, logout } = useAuth();
  const { agents, teams, projects, conversations, unread, connected, ensureUnclassifiedProject, invalidationVersion, renameConversation } = useWorkspace();
  const location = useLocation();
  const navigate = useNavigate();
  const [navOpen, setNavOpen] = useState(false);
  const { width: sidebarWidth, setWidth: setSidebarWidth, collapsed: sidebarCollapsed, setCollapsed: setSidebarCollapsed } = useSidebarState();
  const sidebarDragRef = useRef<{ pointerId: number; startX: number; startWidth: number } | null>(null);
  const [sidebarQuery, setSidebarQuery] = useState("");
  const [renamingConversation, setRenamingConversation] = useState<Conversation | null>(null);
  const [conversationTitle, setConversationTitle] = useState("");
  const [renameError, setRenameError] = useState<string | null>(null);
  const [renameBusy, setRenameBusy] = useState(false);
  const [teamFilterId, setTeamFilterId] = useState("");
  const [expandedProjectIds, setExpandedProjectIds] = useState<Set<string>>(() => new Set());
  const [newConversationBusy, setNewConversationBusy] = useState("");
  const [newConversationOpen, setNewConversationOpen] = useState(false);
  const [newTeamError, setNewTeamError] = useState<string | null>(null);
  const [teamBuildRuns, setTeamBuildRuns] = useState<TeamBuildRunSummary[]>([]);
  const [teamBuildRunsReady, setTeamBuildRunsReady] = useState(false);
  const newConversationTriggerRef = useRef<HTMLElement>(null);
  const [newConversationMenuPos, setNewConversationMenuPos] = useState<{ top: number; left: number; width: number } | null>(null);
  const [accountMenuOpen, setAccountMenuOpen] = useState(false);
  const accountMenuRef = useRef<HTMLDetailsElement>(null);
  const navigationTriggerRef = useRef<HTMLButtonElement>(null);
  const drawerRef = useRef<HTMLElement>(null);
  const restoreNavigationFocusRef = useRef(false);

  useEffect(() => {
    setNavOpen(false);
  }, [location.pathname]);

  useEffect(() => {
    if (!accountMenuOpen) return;
    function handleOutsideClick(event: globalThis.MouseEvent) {
      if (accountMenuRef.current && !accountMenuRef.current.contains(event.target as Node)) {
        setAccountMenuOpen(false);
      }
    }
    document.addEventListener("mousedown", handleOutsideClick);
    return () => document.removeEventListener("mousedown", handleOutsideClick);
  }, [accountMenuOpen]);

  useEffect(() => {
    if (!newConversationOpen) return;
    function handleOutsideClick(event: globalThis.MouseEvent) {
      const target = event.target as Node;
      const inTrigger = newConversationTriggerRef.current?.contains(target);
      const inMenu = (event.target as HTMLElement)?.closest?.(".shell-new-conversation__menu");
      if (!inTrigger && !inMenu) setNewConversationOpen(false);
    }
    document.addEventListener("mousedown", handleOutsideClick);
    return () => document.removeEventListener("mousedown", handleOutsideClick);
  }, [newConversationOpen]);

  function startSidebarDrag(event: PointerEvent<HTMLDivElement>) {
    if (event.button !== 0 || sidebarCollapsed) return;
    event.preventDefault();
    sidebarDragRef.current = { pointerId: event.pointerId, startX: event.clientX, startWidth: sidebarWidth };
    event.currentTarget.setPointerCapture(event.pointerId);
  }

  function dragSidebar(event: PointerEvent<HTMLDivElement>) {
    const drag = sidebarDragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    const next = drag.startWidth + (event.clientX - drag.startX);
    setSidebarWidth(Math.max(200, Math.min(380, next)));
  }

  function stopSidebarDrag(event: PointerEvent<HTMLDivElement>) {
    if (!sidebarDragRef.current || sidebarDragRef.current.pointerId !== event.pointerId) return;
    sidebarDragRef.current = null;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
  }

  useEffect(() => {
    if (navOpen) {
      restoreNavigationFocusRef.current = true;
      drawerRef.current?.querySelector<HTMLButtonElement>(".drawer-close")?.focus();
      return;
    }
    if (restoreNavigationFocusRef.current) {
      restoreNavigationFocusRef.current = false;
      navigationTriggerRef.current?.focus();
    }
  }, [navOpen]);

  function handleDrawerKeyDown(event: KeyboardEvent<HTMLElement>) {
    if (event.key === "Escape") {
      event.preventDefault();
      setNavOpen(false);
      return;
    }
    if (event.key !== "Tab" || !drawerRef.current) return;

    const focusable = drawerFocusableElements(drawerRef.current);
    if (!focusable.length) {
      event.preventDefault();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && (document.activeElement === first || !drawerRef.current.contains(document.activeElement))) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  function beginConversationRename(conversation: Conversation) {
    setRenamingConversation(conversation);
    setConversationTitle(conversation.title);
    setRenameError(null);
  }

  function closeConversationRename() {
    if (renameBusy) return;
    setRenamingConversation(null);
    setConversationTitle("");
    setRenameError(null);
  }

  async function submitConversationRename(event: FormEvent) {
    event.preventDefault();
    if (!renamingConversation || !conversationTitle.trim() || [...conversationTitle.trim()].length > 80) return;
    setRenameBusy(true);
    setRenameError(null);
    try {
      await renameConversation(renamingConversation.id, conversationTitle);
      setRenamingConversation(null);
      setConversationTitle("");
    } catch (error) {
      setRenameError(apiErrorMessage(error));
    } finally {
      setRenameBusy(false);
    }
  }

  async function openQuickConversation(busyKey: string, project: Project | null, avatarId?: string) {
    setNewConversationBusy(busyKey);
    try {
      const target = project || (avatarId ? await ensureUnclassifiedProject(avatarId) : null);
      if (target) navigate(`/project/${encodeURIComponent(target.id)}/conversations`);
    } catch {
      navigate("/inbox");
    } finally {
      setNewConversationBusy("");
    }
  }

  async function openNewTeamConversation() {
    setNewConversationBusy(TEAM_ARCHITECT_AGENT_NAME);
    setNewTeamError(null);
    try {
      const architect = await api.getAgent(TEAM_ARCHITECT_AGENT_NAME);
      if (architect.role !== "avatar" || architect.deleted) throw new Error("元团队架构师不可用");
      const target = await ensureUnclassifiedProject(architect.id);
      setNewConversationOpen(false);
      setNavOpen(false);
      navigate(`/project/${encodeURIComponent(target.id)}/conversations?intent=create_team`);
    } catch (error) {
      setNewTeamError(apiErrorMessage(error));
    } finally {
      setNewConversationBusy("");
    }
  }

  const unreadCount = unread.reduce((sum, item) => sum + item.unread_count, 0);

  const { activeProjectId, activeConversationId } = useMemo(() => {
    const match = location.pathname.match(/^\/project\/([^/]+)\/conversations(?:\/([^/]+))?/);
    return { activeProjectId: match?.[1], activeConversationId: match?.[2] };
  }, [location.pathname]);
  const treeQuery = sidebarQuery.trim().toLowerCase();

  const metaTeam = useMemo(() => teams.find((team) => team.name === META_TEAM_NAME), [teams]);
  const teamArchitect = useMemo(() => agents.find((agent) => agent.name === TEAM_ARCHITECT_AGENT_NAME), [agents]);
  const activeProject = useMemo(() => projects.find((project) => project.id === activeProjectId), [activeProjectId, projects]);
  const shouldLoadTeamBuildRuns = useMemo(() => {
    if (treeQuery) return true;
    if (location.search.includes("intent=create_team")) return true;
    if (location.pathname.startsWith("/control/teams") || location.pathname.startsWith("/control/workflows")) return true;
    if (!activeProject) return false;
    return activeProject.team_id === metaTeam?.id || activeProject.avatar_id === teamArchitect?.id;
  }, [activeProject, location.pathname, location.search, metaTeam?.id, teamArchitect?.id, treeQuery]);

  useEffect(() => {
    if (!shouldLoadTeamBuildRuns) return;
    const controller = new AbortController();
    let disposed = false;
    async function loadTeamBuildRuns() {
      try {
        const response = await api.listTeamBuildRuns({ limit: 200, offset: 0 }, controller.signal);
        if (disposed) return;
        setTeamBuildRuns(response.items);
        setTeamBuildRunsReady(true);
      } catch {
        if (!disposed) setTeamBuildRunsReady(false);
      }
    }
    void loadTeamBuildRuns();
    const interval = window.setInterval(() => void loadTeamBuildRuns(), 10_000);
    return () => {
      disposed = true;
      controller.abort();
      window.clearInterval(interval);
    };
  }, [invalidationVersion, shouldLoadTeamBuildRuns]);

  const teamBuildProjectIds = useMemo(() => {
    const metaTeam = teams.find((team) => team.name === META_TEAM_NAME);
    const architect = agents.find((agent) => agent.name === TEAM_ARCHITECT_AGENT_NAME);
    return new Set(projects
      .filter((project) => project.team_id === metaTeam?.id || project.avatar_id === architect?.id)
      .map((project) => project.id));
  }, [agents, projects, teams]);

  const teamBuildRunByConversation = useMemo(() => {
    const latest = new Map<string, TeamBuildRunSummary>();
    for (const run of teamBuildRuns) {
      if (!run.conversation_id) continue;
      const existing = latest.get(run.conversation_id);
      if (!existing || Date.parse(run.updated_at) > Date.parse(existing.updated_at)) {
        latest.set(run.conversation_id, run);
      }
    }
    return latest;
  }, [teamBuildRuns]);

  const activeProjects = useMemo(() => projects.filter((project) => {
    if (project.archived_at) return false;
    if (teamBuildProjectIds.has(project.id)) return true;
    const team = teams.find((item) => item.id === project.team_id);
    const owner = agents.find((agent) => agent.id === (team?.lead_avatar_id || project.avatar_id));
    return !!team && (!owner || !isPlatformAsset(owner));
  }), [agents, projects, teamBuildProjectIds, teams]);
  const mainConversations = useMemo(() => conversations.filter((conversation) => !conversation.parent_message_id), [conversations]);

  const activeTeams = useMemo(() => teams.filter((team) => {
    if (team.status === "building" || team.status === "archived") return false;
    const lead = agents.find((agent) => agent.id === team.lead_avatar_id);
    return !!lead && !lead.deleted && !isPlatformAsset(lead);
  }), [agents, teams]);

  function projectActivity(project: Project): number {
    return Date.parse(project.last_activity_at || project.updated_at);
  }

  const teamById = useMemo(() => new Map(activeTeams.map((team) => [team.id, team])), [activeTeams]);

  const sidebarProjects = useMemo(() => {
    const filtered = activeProjects.filter((project) => {
      if (teamFilterId && project.team_id !== teamFilterId) return false;
      if (!treeQuery) return true;
      const projectName = teamBuildProjectIds.has(project.id) ? "团队搭建" : isUnclassifiedProject(project) ? "快速会话" : project.name;
      if (projectName.toLowerCase().includes(treeQuery)) return true;
      const team = teamById.get(project.team_id);
      if (team?.name.toLowerCase().includes(treeQuery)) return true;
      return mainConversations.some((conversation) => conversation.project_id === project.id && (conversation.title || "未命名会话").toLowerCase().includes(treeQuery));
    });
    return [...filtered].sort((left, right) => {
      const leftQuick = isUnclassifiedProject(left) ? 1 : 0;
      const rightQuick = isUnclassifiedProject(right) ? 1 : 0;
      if (leftQuick !== rightQuick) return leftQuick - rightQuick;
      return projectActivity(right) - projectActivity(left);
    });
  }, [activeProjects, mainConversations, teamBuildProjectIds, teamById, teamFilterId, treeQuery]);

  function projectConversations(projectId: string, projectName: string) {
    const rows = mainConversations.filter((conversation) => conversation.project_id === projectId);
    if (!treeQuery || projectName.toLowerCase().includes(treeQuery)) return rows;
    return rows.filter((conversation) => (conversation.title || "").toLowerCase().includes(treeQuery));
  }

  function renderProject(project: Project) {
    const isTeamBuild = teamBuildProjectIds.has(project.id);
    const isUnclassified = isUnclassifiedProject(project);
    const projectName = isTeamBuild ? "团队搭建" : isUnclassified ? "快速会话" : project.name;
    const team = teamById.get(project.team_id);
    const rows = projectConversations(project.id, projectName);
    const expanded = expandedProjectIds.has(project.id) || (treeQuery.length > 0 && rows.length > 0);
    return (
      <div className="shell-project-group" key={project.id}>
        <Link
          to={`/project/${encodeURIComponent(project.id)}/conversations`}
          aria-current={project.id === activeProjectId ? "page" : undefined}
          aria-expanded={expanded}
          onClick={() => {
            setNavOpen(false);
            setExpandedProjectIds((current) => {
              if (current.has(project.id)) return current;
              const next = new Set(current);
              next.add(project.id);
              return next;
            });
          }}
        ><FolderKanban size={16} /><span>{projectName}</span>{team && !isTeamBuild && <span className="shell-project-team">{team.name}{team.status === "needs_repair" && <Badge tone="warning">待修复</Badge>}</span>}<MessageSquarePlus className="shell-project-new" size={16} aria-hidden="true" /></Link>
        {expanded && <>
          {rows.map((conversation) => {
            const buildRun = isTeamBuild ? teamBuildRunByConversation.get(conversation.id) : undefined;
            const buildStatus = isTeamBuild ? teamBuildStatus(buildRun, teamBuildRunsReady) : null;
            const title = buildRun?.new_team_name || buildRun?.target_team_name || conversation.title || "未命名会话";
            return (
            <div className="shell-project-conversation-row" key={conversation.id}>
              <Link className="shell-project-conversation" to={`/project/${encodeURIComponent(project.id)}/conversations/${encodeURIComponent(conversation.id)}`} aria-current={conversation.id === activeConversationId ? "page" : undefined} onClick={() => setNavOpen(false)} onDoubleClick={() => beginConversationRename(conversation)}><span>{title}</span>{buildStatus && <Badge tone={buildStatus.tone}>{buildStatus.label}</Badge>}</Link>
              <button className="shell-conversation-rename icon-button" type="button" aria-label={`重命名 ${title}`} title="重命名" onClick={() => beginConversationRename(conversation)}><Pencil size={14} /></button>
            </div>
            );
          })}
        </>}
      </div>
    );
  }

  const content = (
    <>
      <div className="shell-workspace-row">
        <div className="shell-workspace-switcher shell-workspace-switcher--static"><span className="shell-workspace-identity"><span className="brand-mark brand-mark--small">W</span><span><strong>Weave</strong><small>default</small></span></span></div>
        <button className="icon-button sidebar-collapse" type="button" aria-label="收起侧栏" onClick={() => setSidebarCollapsed(true)}><PanelLeftClose size={16} /></button>
      </div>
      <div className="shell-actions">
        <label className="sidebar-search">
          <span className="sr-only">搜索</span>
          <Search size={14} aria-hidden="true" />
          <input type="search" value={sidebarQuery} onChange={(event) => setSidebarQuery(event.target.value)} placeholder="搜索" autoComplete="off" />
        </label>
        <NavLink className="shell-inbox-icon" to="/inbox" onClick={() => setNavOpen(false)} aria-label="收件箱" title="收件箱"><Inbox size={16} />{unreadCount > 0 && <Badge tone="accent">{unreadCount}</Badge>}</NavLink>
      </div>
      <div className="shell-actions shell-actions--new-task">
        <details className="shell-new-conversation" open={newConversationOpen} onToggle={(event) => {
          const open = event.currentTarget.open;
          setNewConversationOpen(open);
          if (open && newConversationTriggerRef.current) {
            const r = newConversationTriggerRef.current.getBoundingClientRect();
            setNewConversationMenuPos({ top: r.bottom + 4, left: r.left, width: Math.max(220, r.width) });
          }
        }}>
          <summary ref={newConversationTriggerRef} className="ui-button ui-button--secondary ui-button--small" aria-haspopup="menu"><MessageSquarePlus size={16} aria-hidden="true" /><span>新任务</span></summary>
        </details>
        <Button variant="secondary" size="small" loading={newConversationBusy === TEAM_ARCHITECT_AGENT_NAME} onClick={() => void openNewTeamConversation()}>
          <UserPlus size={16} aria-hidden="true" />
          <span>新团队</span>
        </Button>
        {newConversationOpen && newConversationMenuPos && createPortal(
          <div className="shell-new-conversation__menu shell-new-conversation__menu--portal" role="menu" aria-label="新对话" style={{ top: newConversationMenuPos.top, left: newConversationMenuPos.left, width: newConversationMenuPos.width }}>
            <span className="shell-new-conversation__label">团队 · 快速会话</span>
            {activeTeams.length === 0 && <span className="shell-new-conversation__empty">暂无可用团队</span>}
            {activeTeams.map((team) => {
              const quickProject = activeProjects.find((project) => project.team_id === team.id && isUnclassifiedProject(project)) || null;
              const lead = agents.find((agent) => agent.id === team.lead_avatar_id);
              return <button key={team.id} type="button" role="menuitem" disabled={newConversationBusy === team.id} onClick={() => { setNewConversationOpen(false); setNavOpen(false); void openQuickConversation(team.id, quickProject, team.lead_avatar_id); }}><Users size={14} aria-hidden="true" /><span>{team.name}</span><small>{lead?.display_name || lead?.name}</small></button>;
            })}
          </div>,
          document.body,
        )}
      </div>
      {newTeamError && <p className="shell-new-team-error" role="alert">{newTeamError}</p>}
      <div className="shell-tree">
      <section className="shell-projects" aria-labelledby="shell-projects-title">
        <div className="shell-projects__head">
          <h2 id="shell-projects-title">项目</h2>
          {activeTeams.length > 1 && (
            <details className="shell-team-filter">
              <summary aria-haspopup="menu">{teamFilterId ? (teamById.get(teamFilterId)?.name || "筛选团队") : "全部团队"}<ChevronDown size={14} aria-hidden="true" /></summary>
              <div className="shell-team-filter__menu" role="menu" aria-label="按团队筛选">
                <button type="button" role="menuitem" aria-current={teamFilterId === "" ? "true" : undefined} onClick={(event) => { setTeamFilterId(""); event.currentTarget.closest("details")?.removeAttribute("open"); }}>全部团队</button>
                {activeTeams.map((team) => <button key={team.id} type="button" role="menuitem" aria-current={teamFilterId === team.id ? "true" : undefined} onClick={(event) => { setTeamFilterId(team.id); event.currentTarget.closest("details")?.removeAttribute("open"); }}>{team.name}</button>)}
              </div>
            </details>
          )}
        </div>
        {!sidebarProjects.length && <p>{treeQuery || teamFilterId ? "无匹配" : "暂无项目"}</p>}
        <nav aria-label="最近项目">{sidebarProjects.map((project) => renderProject(project))}</nav>
      </section>
      </div>
      <div className="sidebar__footer">
        <div className="shell-account-row">
        <details className="account-menu" ref={accountMenuRef} open={accountMenuOpen} onToggle={(event) => setAccountMenuOpen(event.currentTarget.open)}>
          <summary><span className="user-avatar">{(user?.display_name || user?.username || "W").slice(0, 1).toUpperCase()}</span><span><strong>{user?.display_name || user?.username || "当前用户"}</strong><small>{user?.tenant_id}</small></span></summary>
          <div className="account-menu__popover">
            <div className="account-menu__header">
              <Link to="/control/account" state={{ returnTo: `${location.pathname}${location.search}${location.hash}` }} onClick={(event) => { event.currentTarget.closest("details")?.removeAttribute("open"); setNavOpen(false); }}><span className="user-avatar">{(user?.display_name || user?.username || "W").slice(0, 1).toUpperCase()}</span><span><strong>{user?.display_name || user?.username || "当前用户"}</strong></span></Link>
              <button className="account-logout" type="button" aria-label="退出登录" title="退出登录" onClick={() => void logout()}><LogOut size={16} /></button>
            </div>
            <Link to="/control/agents" state={{ returnTo: `${location.pathname}${location.search}${location.hash}` }} onClick={(event) => { event.currentTarget.closest("details")?.removeAttribute("open"); setNavOpen(false); }}><Settings2 size={16} /> 设置</Link>
          </div>
        </details>
        </div>
      </div>
    </>
  );

  return (
    <div className={sidebarCollapsed ? "app-shell app-shell--collapsed" : "app-shell"} style={{ "--sidebar-width": `${sidebarWidth}px` } as React.CSSProperties}>
      <aside className="sidebar" inert={navOpen}>
        {content}
        <div className="sidebar-resizer" role="separator" aria-label="调整侧边栏宽度" aria-orientation="vertical" aria-valuemin={200} aria-valuemax={380} aria-valuenow={sidebarWidth} tabIndex={0} onPointerDown={startSidebarDrag} onPointerMove={dragSidebar} onPointerUp={stopSidebarDrag} onPointerCancel={stopSidebarDrag} onKeyDown={(event) => { if (event.key === "ArrowLeft" || event.key === "ArrowRight") { event.preventDefault(); setSidebarWidth(sidebarWidth + (event.key === "ArrowRight" ? 24 : -24)); } }} />
      </aside>
      <header className="mobile-toolbar" inert={navOpen}><button ref={navigationTriggerRef} className="icon-button" type="button" aria-label="打开导航" onClick={() => setNavOpen(true)}><Menu size={20} /></button><strong>Weave</strong><span className={connected ? "toolbar-status toolbar-status--online" : "toolbar-status"} role="status" aria-label={connected ? "实时连接" : "正在重连"} /></header>
      <main className="workspace" inert={navOpen}>
        <Outlet context={{ sidebarCollapsed, setSidebarCollapsed }} />
      </main>
      {navOpen && <div className="drawer-backdrop" role="presentation" onMouseDown={() => setNavOpen(false)}><aside ref={drawerRef} className="mobile-drawer" role="dialog" aria-modal="true" aria-label="导航" onKeyDown={handleDrawerKeyDown} onMouseDown={(event) => event.stopPropagation()}><button className="icon-button drawer-close" type="button" aria-label="关闭导航" onClick={() => setNavOpen(false)}><X size={20} /></button>{content}</aside></div>}
      <Modal open={!!renamingConversation} title="重命名会话" onClose={closeConversationRename} footer={<><Button disabled={renameBusy} onClick={closeConversationRename}>取消</Button><Button variant="primary" type="submit" form="conversation-rename-form" loading={renameBusy} disabled={!conversationTitle.trim() || [...conversationTitle.trim()].length > 80}>{renameBusy ? "正在保存" : "保存"}</Button></>}>
        <form id="conversation-rename-form" className="form-stack" onSubmit={(event) => void submitConversationRename(event)}>
          <Field label="会话名称" help={`${[...conversationTitle.trim()].length}/80`} error={renameError}>{(control) => <input {...control} autoFocus maxLength={160} value={conversationTitle} onChange={(event) => setConversationTitle(event.target.value)} />}</Field>
        </form>
      </Modal>
    </div>
  );
}
