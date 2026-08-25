import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent, type PointerEvent } from "react";
import { ArrowLeft, Bot, Building2, Cable, Clipboard, GitBranch, KeyRound, LockKeyhole, Menu, Pencil, Plus, RefreshCw, Search, Send, Server, ServerCog, Sparkles, Trash2, UserRound, UsersRound, Workflow, X } from "lucide-react";
import { Link, NavLink, Navigate, Outlet, Route, Routes, useLocation } from "react-router-dom";
import { useSidebarState } from "../shell/useSidebarState";
import { api, apiErrorMessage, normalizeThrownError, type APIKey, type ApiError, type CreateAPIKeyResponse, type User, type WorkspaceMember, type WorkspaceMemberRole, type WorkspaceResponse } from "../api";
import { AgentsPage } from "./control-center/AgentsPage";
import { AgentDetailPage } from "./control-center/AgentDetailPage";
import { AgentGraphPage } from "./control-center/AgentGraphPage";
import { MCPServersPage } from "./control-center/MCPServersPage";
import { TeamsPage } from "./control-center/TeamsPage";
import { TeamDetailPage } from "./control-center/TeamDetailPage";
import { WorkflowsPage } from "./control-center/WorkflowsPage";
import { SkillsPage } from "./control-center/SkillsPage";
import { DeliveryTargetsPage } from "./control-center/DeliveryTargetsPage";
import { ProvidersPage } from "./control-center/ProvidersPage";
import { RuntimesPage } from "./RuntimesPage";
import { useAuth } from "../auth/AuthContext";
import { Badge } from "../ui/Badge";
import { API_SCOPE_OPTIONS, apiScopeLabel } from "../workspace/labels";
import { Button } from "../ui/Button";
import { Card } from "../ui/Card";
import { Field } from "../ui/Field";
import { Modal } from "../ui/Modal";
import { ErrorNotice, LoadingView } from "../ui/StatusViews";
import { Switch } from "../ui/Switch";
import "../styles/control-shell.css";

const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
const keyRoles = ["service", "admin"] as const;
const userRoles = ["admin", "owner", "user"] as const;
const workspaceMemberRoles = ["member", "owner"] as const;
const controlTabGroups = [
  {
    label: "组织",
    tabs: [
      { to: "/control/agents", label: "智能体", icon: Bot },
      { to: "/control/skills", label: "技能", icon: Sparkles },
      { to: "/control/teams", label: "团队", icon: UsersRound },
      { to: "/control/workflows", label: "团队流程", icon: Workflow },
    ],
  },
  {
    label: "接入与运行",
    tabs: [
      { to: "/control/providers", label: "模型供应商", icon: ServerCog },
      { to: "/control/runtimes", label: "运行时", icon: Server },
      { to: "/control/mcp-servers", label: "MCP 服务器", icon: Cable },
      { to: "/control/delivery-targets", label: "交付目标", icon: Send },
    ],
  },
  {
    label: "工作区",
    tabs: [
      { to: "/control/account", label: "账户", icon: UserRound },
      { to: "/control/workspace", label: "工作区", icon: Building2 },
      { to: "/control/users", label: "用户", icon: UsersRound },
      { to: "/control/api-keys", label: "API 密钥", icon: KeyRound },
    ],
  },
  {
    label: "兼容（旧版）",
    tabs: [
      { to: "/control/agent-links", label: "兼容关系", icon: GitBranch },
    ],
  },
];
const settingsFocusableSelector = "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex]:not([tabindex='-1'])";

function settingsFocusableElements(drawer: HTMLElement): HTMLElement[] {
  return Array.from(drawer.querySelectorAll<HTMLElement>(settingsFocusableSelector))
    .filter((element) => element.getClientRects().length > 0);
}

function roleLabel(role: string) {
  return ({ admin: "管理员", owner: "所有者", member: "成员", user: "用户", service: "服务" } as Record<string, string>)[role] || role;
}

function formatDate(value?: string | null, fallback = "—") {
  return value ? dateTime.format(new Date(value)) : fallback;
}

function ForbiddenView({ message }: { message: string }) {
  return <Card className="control-empty control-forbidden"><LockKeyhole size={28} aria-hidden="true" /><h2>没有访问权限</h2><p>{message}</p></Card>;
}

function memberActionMessage(error: ApiError) {
  if (error.status === 403) return "当前账号只能查看工作区和成员，无法修改";
  if (error.status === 409) return `成员变更冲突：${apiErrorMessage(error)}`;
  return apiErrorMessage(error);
}

function safeReturnHref(value: unknown): string {
  if (typeof value !== "string" || !value.startsWith("/") || value.startsWith("//") || value.startsWith("/control")) return "/inbox";
  return value;
}

export function ControlCenterPage() {
  const location = useLocation();
  const returnTo = (location.state as { returnTo?: unknown } | null)?.returnTo;
  const returnHref = safeReturnHref(returnTo);
  const navigationState = returnHref === "/inbox" ? undefined : { returnTo: returnHref };
  const [settingsQuery, setSettingsQuery] = useState("");
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  const navigationRef = useRef<HTMLElement>(null);
  const mobileNavigationTriggerRef = useRef<HTMLButtonElement>(null);
  const mobileDrawerRef = useRef<HTMLElement>(null);
  const restoreMobileNavigationFocusRef = useRef(false);
  const { width: sidebarWidth, setWidth: setSidebarWidth, minWidth, maxWidth } = useSidebarState();
  const settingsDragRef = useRef<{ pointerId: number; startX: number; startWidth: number } | null>(null);
  const normalizedQuery = settingsQuery.trim().toLowerCase();
  const visibleGroups = useMemo(() => controlTabGroups.flatMap((group) => {
    const tabs = normalizedQuery ? group.tabs.filter((tab) => tab.label.toLowerCase().includes(normalizedQuery)) : group.tabs;
    return tabs.length ? [{ ...group, tabs }] : [];
  }), [normalizedQuery]);
  const activeTab = controlTabGroups.flatMap((group) => group.tabs).find((tab) => location.pathname === tab.to);

  useEffect(() => {
    navigationRef.current?.querySelector<HTMLElement>("a.active")?.scrollIntoView({ block: "nearest", inline: "nearest" });
    setMobileNavOpen(false);
  }, [location.pathname]);

  useEffect(() => {
    if (mobileNavOpen) {
      restoreMobileNavigationFocusRef.current = true;
      mobileDrawerRef.current?.querySelector<HTMLButtonElement>(".settings-mobile-drawer__close")?.focus();
      return;
    }
    if (restoreMobileNavigationFocusRef.current) {
      restoreMobileNavigationFocusRef.current = false;
      mobileNavigationTriggerRef.current?.focus();
    }
  }, [mobileNavOpen]);

  function handleMobileDrawerKeyDown(event: KeyboardEvent<HTMLElement>) {
    if (event.key === "Escape") {
      event.preventDefault();
      setMobileNavOpen(false);
      return;
    }
    if (event.key !== "Tab" || !mobileDrawerRef.current) return;

    const focusable = settingsFocusableElements(mobileDrawerRef.current);
    if (!focusable.length) {
      event.preventDefault();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && (document.activeElement === first || !mobileDrawerRef.current.contains(document.activeElement))) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  function settingsNavigation(prefix: string) {
    return <nav className="settings-navigation" aria-label="设置导航">
      {visibleGroups.map((group, groupIndex) => {
        const labelId = `${prefix}-settings-group-${groupIndex}`;
        return <section className="settings-navigation__group" aria-labelledby={labelId} key={group.label}>
          <h2 id={labelId}>{group.label}</h2>
          <div>{group.tabs.map(({ to, label, icon: Icon }) => <NavLink to={to} state={navigationState} key={to} onClick={() => setMobileNavOpen(false)}><Icon size={16} aria-hidden="true" /><span>{label}</span></NavLink>)}</div>
        </section>;
      })}
      {!visibleGroups.length && <p className="settings-navigation__empty">没有匹配的设置</p>}
    </nav>;
  }

  function settingsSidebarContent(prefix: string) {
    return <>
      <Link className="settings-return" to={returnHref}><ArrowLeft size={16} aria-hidden="true" />返回应用</Link>
      <label className="settings-search">
        <span className="sr-only">搜索设置</span>
        <Search size={16} aria-hidden="true" />
        <input type="search" value={settingsQuery} onChange={(event) => setSettingsQuery(event.target.value)} placeholder="搜索设置" autoComplete="off" />
      </label>
      {settingsNavigation(prefix)}
    </>;
  }

  function startSettingsDrag(event: PointerEvent<HTMLDivElement>) {
    if (event.button !== 0) return;
    event.preventDefault();
    settingsDragRef.current = { pointerId: event.pointerId, startX: event.clientX, startWidth: sidebarWidth };
    event.currentTarget.setPointerCapture(event.pointerId);
  }

  function dragSettings(event: PointerEvent<HTMLDivElement>) {
    const drag = settingsDragRef.current;
    if (!drag || drag.pointerId !== event.pointerId) return;
    setSidebarWidth(drag.startWidth + (event.clientX - drag.startX));
  }

  function stopSettingsDrag(event: PointerEvent<HTMLDivElement>) {
    if (!settingsDragRef.current || settingsDragRef.current.pointerId !== event.pointerId) return;
    settingsDragRef.current = null;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
  }

  return (
    <div className="control-center-page settings-layout" style={{ "--sidebar-width": `${sidebarWidth}px` } as React.CSSProperties}>
      <aside ref={navigationRef} className="settings-sidebar">
        {settingsSidebarContent("desktop")}
        <div className="settings-sidebar__resizer" role="separator" aria-label="调整侧边栏宽度" aria-orientation="vertical" aria-valuemin={minWidth} aria-valuemax={maxWidth} aria-valuenow={sidebarWidth} tabIndex={0} onPointerDown={startSettingsDrag} onPointerMove={dragSettings} onPointerUp={stopSettingsDrag} onPointerCancel={stopSettingsDrag} onKeyDown={(event) => { if (event.key === "ArrowLeft" || event.key === "ArrowRight") { event.preventDefault(); setSidebarWidth(sidebarWidth + (event.key === "ArrowRight" ? 24 : -24)); } }} />
      </aside>
      <header className="settings-mobile-toolbar" inert={mobileNavOpen}>
        <button ref={mobileNavigationTriggerRef} className="icon-button" type="button" aria-label="打开设置导航" onClick={() => setMobileNavOpen(true)}><Menu size={20} /></button>
        <strong>{activeTab?.label || "设置"}</strong>
      </header>
      <main className="settings-content" inert={mobileNavOpen}><Outlet /></main>
      {mobileNavOpen && <div className="settings-mobile-backdrop" role="presentation" onMouseDown={() => setMobileNavOpen(false)}>
        <aside ref={mobileDrawerRef} className="settings-mobile-drawer" role="dialog" aria-modal="true" aria-label="设置导航" onKeyDown={handleMobileDrawerKeyDown} onMouseDown={(event) => event.stopPropagation()}>
          <button className="icon-button settings-mobile-drawer__close" type="button" aria-label="关闭设置导航" onClick={() => setMobileNavOpen(false)}><X size={20} /></button>
          {settingsSidebarContent("mobile")}
        </aside>
      </div>}
    </div>
  );
}

export function ControlCenterRoutes() {
  return <Routes>
    <Route element={<ControlCenterPage />}>
      <Route index element={<Navigate to="account" replace />} />
      <Route path="account" element={<AccountPage />} />
      <Route path="workspace" element={<WorkspacePage />} />
      <Route path="agents" element={<AgentsPage />} />
      <Route path="agents/:name" element={<AgentDetailPage />} />
      <Route path="skills" element={<SkillsPage />} />
      <Route path="agent-links" element={<AgentGraphPage />} />
      <Route path="channels" element={<Navigate to="/control/agents" replace />} />
      <Route path="prompt-preview" element={<Navigate to="/control/agents" replace />} />
      <Route path="topology" element={<Navigate to="/control/agents" replace />} />
      <Route path="agent-memory" element={<Navigate to="/control/agents" replace />} />
      <Route path="memory-slots" element={<Navigate to="/control/agents" replace />} />
      <Route path="memory-profile" element={<Navigate to="/control/agents" replace />} />
      <Route path="teams" element={<TeamsPage />} />
      <Route path="teams/:id" element={<TeamDetailPage />} />
      <Route path="workflows" element={<WorkflowsPage />} />
      <Route path="mcp-servers" element={<MCPServersPage />} />
      <Route path="providers" element={<ProvidersPage />} />
      <Route path="delivery-targets" element={<DeliveryTargetsPage />} />
      <Route path="runtimes" element={<RuntimesPage />} />
      <Route path="users" element={<UsersPage />} />
      <Route path="api-keys" element={<APIKeysPage />} />
      <Route path="*" element={<Navigate to="account" replace />} />
    </Route>
  </Routes>;
}

function WorkspacePage() {
  const { user } = useAuth();
  const [workspaceResponse, setWorkspaceResponse] = useState<WorkspaceResponse | null>(null);
  const [members, setMembers] = useState<WorkspaceMember[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<WorkspaceMember | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<ApiError | null>(null);
  const [conflict, setConflict] = useState<string | null>(null);
  const [userId, setUserId] = useState("");
  const [role, setRole] = useState<WorkspaceMemberRole>("member");
  const [userOptions, setUserOptions] = useState<User[]>([]);

  const canManage = user?.role === "admin" || user?.role === "owner";
  const actionForbidden = actionError?.status === 403;
  const canWrite = canManage && !actionForbidden;

  const refresh = useCallback(async (initial = false) => {
    if (initial) setLoading(true); else setRefreshing(true);
    setError(null); setForbidden(false);
    try {
      const [nextWorkspace, nextMembers] = await Promise.all([
        api.getWorkspace(),
        api.listWorkspaceMembers(),
      ]);
      setWorkspaceResponse(nextWorkspace);
      setMembers(nextMembers);
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setForbidden(normalized.status === 403);
      setError(apiErrorMessage(normalized));
    } finally {
      setLoading(false); setRefreshing(false);
    }
  }, []);

  useEffect(() => { void refresh(true); }, [refresh]);

  async function addMember(event: FormEvent) {
    event.preventDefault(); setBusy(true); setActionError(null); setConflict(null);
    try {
      await api.addWorkspaceMember({ user_id: userId.trim(), role });
      setCreating(false); setUserId(""); setRole("member"); await refresh();
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      if (normalized.status === 403) setCreating(false);
      if (normalized.status === 409) setConflict(memberActionMessage(normalized)); else setActionError(normalized);
    } finally { setBusy(false); }
  }

  async function removeMember() {
    if (!deleting) return;
    setBusy(true); setActionError(null); setConflict(null);
    try {
      await api.removeWorkspaceMember(deleting.user_id);
      setDeleting(null);
      await refresh();
    }
    catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      if (normalized.status === 403) setDeleting(null);
      if (normalized.status === 409) setConflict(memberActionMessage(normalized)); else setActionError(normalized);
    } finally { setBusy(false); }
  }

  const workspace = workspaceResponse?.workspace;

  return <section className="control-content" aria-labelledby="workspace-heading">
    <div className="control-section-heading"><div><h2 id="workspace-heading">工作区</h2><p>工作区详情只读；成员关系属于工作区，不代表项目权限。</p></div><div className="heading-actions"><Button variant="ghost" loading={refreshing} disabled={loading} onClick={() => void refresh()}>{!refreshing && <RefreshCw size={16} aria-hidden="true" />}{refreshing ? "正在刷新" : "刷新"}</Button>{canWrite && !forbidden && <Button variant="primary" onClick={() => { setCreating(true); setActionError(null); setConflict(null); setUserId(""); void api.listUsers().then(setUserOptions).catch(() => setUserOptions([])); }}><Plus size={16} aria-hidden="true" />添加成员</Button>}</div></div>
    {error && !forbidden && <ErrorNotice message={error} onRetry={() => void refresh()} />}
    {conflict && !creating && !deleting && <ErrorNotice message={conflict} onRetry={() => void refresh()} />}
    {actionError && !creating && !deleting && <ErrorNotice message={memberActionMessage(actionError)} onRetry={() => void refresh()} />}
    {forbidden ? <ForbiddenView message={error || "当前账号没有查看工作区和成员的权限。"} /> : loading && !workspaceResponse ? <LoadingView label="正在读取工作区与成员" /> : workspace ? <>
      <Card className="workspace-summary settings-section" header={<div className="settings-section__title"><div><h3>{workspace.name}</h3><p>该工作区暂不支持修改名称，仅可查看。</p></div><Badge>只读</Badge></div>}>
        <dl className="workspace-facts"><div><dt>名称</dt><dd>{workspace.name}</dd></div><div><dt>标识</dt><dd><code>{workspace.slug}</code></dd></div><div><dt>工作区 ID</dt><dd><code>{workspace.id}</code></dd></div><div><dt>当前成员角色</dt><dd>{workspaceResponse.role ? roleLabel(workspaceResponse.role) : "未记录"}</dd></div><div><dt>创建时间</dt><dd><time dateTime={workspace.created_at}>{formatDate(workspace.created_at)}</time></dd></div></dl>
      </Card>
      <div className="members-section">
        <div className="settings-section__title members-section__heading"><div><h3>成员</h3><p>{actionForbidden ? "当前账号只能查看成员。" : canManage ? "管理员和所有者可添加或删除成员。" : `${roleLabel("member")}角色只能查看成员。`}</p></div><Badge>{members.length} 位</Badge></div>
        {refreshing && <p className="refresh-note" role="status">正在刷新工作区与成员…</p>}
        {!members.length ? <Card className="control-empty workspace-empty"><UsersRound size={28} aria-hidden="true" /><h2>还没有成员</h2><p>当前工作区还没有成员，可由管理员或所有者添加。</p></Card> : <div className="control-table members-table" role="table" aria-label="工作区成员列表">
          <div className="control-table__header" role="row"><span role="columnheader">成员</span><span role="columnheader">角色</span><span role="columnheader">状态</span><span role="columnheader">加入时间</span><span role="columnheader" aria-label="操作" /></div>
          {members.map((member) => <div className="control-table__row" role="row" key={member.user_id}>
            <div className="table-identity" role="rowheader"><span className="user-avatar">{(member.display_name || member.username || "U").slice(0, 1).toUpperCase()}</span><span><strong>{member.deleted ? "已删除用户" : member.display_name || member.username || "未命名成员"}</strong><small>{member.username || "无用户名"} · {member.user_id}</small><small>工作区 · {member.workspace_id}</small></span></div>
            <span role="cell">{roleLabel(member.role)}</span><span className="control-table__status" role="cell"><Badge tone={member.deleted ? "danger" : "success"}>{member.deleted ? "用户已删除" : "有效"}</Badge></span><time role="cell" dateTime={member.created_at}>{formatDate(member.created_at)}</time>
            <div className="row-actions" role="cell">{canWrite && <Button variant="danger" size="small" aria-label={`删除成员 ${member.display_name || member.username || member.user_id}`} title="删除成员" disabled={busy} onClick={() => { setDeleting(member); setActionError(null); setConflict(null); }}><Trash2 size={16} aria-hidden="true" /></Button>}</div>
          </div>)}
        </div>}
      </div>
    </> : null}
    <Modal open={creating} title="添加工作区成员" description="选择已有用户并指定角色；此处不配置项目权限。" onClose={() => { if (!busy) setCreating(false); }} footer={<><Button disabled={busy} onClick={() => setCreating(false)}>取消</Button><Button variant="primary" type="submit" form="member-create" loading={busy} disabled={!userId.trim()}>{busy ? "正在保存" : "添加成员"}</Button></>}><form id="member-create" className="form-stack" onSubmit={(event) => void addMember(event)}><Field label="用户" help="只列出尚未加入当前工作区的用户。">{(control) => { const candidates = userOptions.filter((option) => !members.some((member) => member.user_id === option.id)); return <select {...control} autoFocus value={userId} onChange={(event) => setUserId(event.target.value)} required><option value="" disabled>选择用户</option>{candidates.map((option) => <option key={option.id} value={option.id}>{option.display_name || option.username}（{option.username}）</option>)}</select>; }}</Field><Field label="成员角色" help="可选所有者或成员。">{(control) => <select {...control} value={role} onChange={(event) => setRole(event.target.value as WorkspaceMemberRole)}>{workspaceMemberRoles.map((value) => <option key={value} value={value}>{roleLabel(value)}</option>)}</select>}</Field>{conflict && <ErrorNotice message={conflict} onRetry={() => void refresh()} />}{actionError && <ErrorNotice message={memberActionMessage(actionError)} onRetry={() => void refresh()} />}</form></Modal>
    <Modal open={!!deleting} title="删除工作区成员" description="删除成员关系后不可恢复；该操作不会删除用户账户。" onClose={() => { if (!busy) setDeleting(null); }} footer={<><Button disabled={busy} onClick={() => setDeleting(null)}>取消</Button><Button variant="danger" loading={busy} onClick={() => void removeMember()}>{busy ? "正在删除" : "确认删除"}</Button></>}><p className="confirm-copy">确定从工作区删除“{deleting?.display_name || deleting?.username || deleting?.user_id}”的成员关系吗？</p>{conflict && <ErrorNotice message={conflict} onRetry={() => void refresh()} />}{actionError && <ErrorNotice message={memberActionMessage(actionError)} onRetry={() => void refresh()} />}</Modal>
  </section>;
}

function AccountPage() {
  const { user, refreshUser } = useAuth();
  const [loading, setLoading] = useState(false);
  const [displayName, setDisplayName] = useState(user?.display_name || "");
  const [profileBusy, setProfileBusy] = useState(false);
  const [profileError, setProfileError] = useState<string | null>(null);
  const [profileNotice, setProfileNotice] = useState<string | null>(null);
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [passwordBusy, setPasswordBusy] = useState(false);
  const [passwordError, setPasswordError] = useState<string | null>(null);
  const [passwordNotice, setPasswordNotice] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true); setProfileError(null);
    try {
      const current = await refreshUser();
      setDisplayName(current.display_name || "");
    } catch (error) { setProfileError(apiErrorMessage(error)); }
    finally { setLoading(false); }
  }, [refreshUser]);

  async function saveProfile(event: FormEvent) {
    event.preventDefault();
    if (!displayName.trim()) return;
    setProfileBusy(true); setProfileError(null); setProfileNotice(null);
    try {
      const updated = await api.updateMe({ display_name: displayName.trim() });
      setDisplayName(updated.display_name || "");
      await refreshUser();
      setProfileNotice("账户资料已保存。");
    } catch (error) { setProfileError(apiErrorMessage(error)); }
    finally { setProfileBusy(false); }
  }

  async function changePassword(event: FormEvent) {
    event.preventDefault();
    setPasswordError(null); setPasswordNotice(null);
    if (newPassword !== confirmPassword) { setPasswordError("两次输入的新密码不一致。"); return; }
    setPasswordBusy(true);
    try {
      await api.changeMyPassword({ current_password: currentPassword, new_password: newPassword });
      setCurrentPassword(""); setNewPassword(""); setConfirmPassword("");
      setPasswordNotice("密码已更新，表单已清空。");
    } catch (error) { setPasswordError(apiErrorMessage(error)); }
    finally { setPasswordBusy(false); }
  }

  if (!user) return <LoadingView label="正在读取账户" />;
  const apiKeyIdentity = user.source === "apikey";

  return <section className="control-content" aria-labelledby="account-heading">
    <div className="control-section-heading"><div><h2 id="account-heading">账户</h2><p>查看当前账户信息，并修改显示名称或密码。</p></div><Button variant="ghost" loading={loading} onClick={() => void refresh()}>{!loading && <RefreshCw size={16} aria-hidden="true" />}{loading ? "正在刷新" : "刷新"}</Button></div>
    {profileError && <ErrorNotice message={profileError} onRetry={() => void refresh()} />}
    {apiKeyIdentity ? <ForbiddenView message="API 密钥身份没有可编辑的账户资料或密码。" /> : <div className="account-grid">
      <Card className="settings-section" header={<div className="settings-section__title"><h3>个人资料</h3><p>用户名不可修改；仅保存显示名称。</p></div>}>
        <form className="form-stack settings-form settings-rows" onSubmit={(event) => void saveProfile(event)}>
          <Field label="显示名">{(control) => <input {...control} value={displayName} onChange={(event) => setDisplayName(event.target.value)} required autoComplete="name" />}</Field>
          <div className="readonly-grid" aria-label="只读身份"><div><span>用户名</span><strong>{user.username || "—"}</strong></div><div><span>角色</span><strong>{roleLabel(user.role)}</strong></div><div><span>工作区</span><strong>{user.tenant_id}</strong></div><div><span>用户 ID</span><code>{user.id}</code></div></div>
          {profileNotice && <p className="success-note" role="status">{profileNotice}</p>}
          <div className="form-actions"><Button variant="primary" type="submit" loading={profileBusy} disabled={!displayName.trim()}>{profileBusy ? "正在保存" : "保存资料"}</Button></div>
        </form>
      </Card>
      <Card className="settings-section" header={<div className="settings-section__title"><h3>修改密码</h3><p>需要验证当前密码；密码不会明文显示或存储。</p></div>}>
        <form className="form-stack settings-form settings-rows" onSubmit={(event) => void changePassword(event)}>
          <Field label="当前密码">{(control) => <input {...control} aria-label="当前密码" type="password" value={currentPassword} onChange={(event) => setCurrentPassword(event.target.value)} required autoComplete="current-password" />}</Field>
          <Field label="新密码">{(control) => <input {...control} aria-label="新密码" type="password" value={newPassword} onChange={(event) => setNewPassword(event.target.value)} required autoComplete="new-password" />}</Field>
          <Field label="确认新密码">{(control) => <input {...control} aria-label="确认新密码" type="password" value={confirmPassword} onChange={(event) => setConfirmPassword(event.target.value)} required autoComplete="new-password" />}</Field>
          {passwordError && <ErrorNotice message={passwordError} />}
          {passwordNotice && <p className="success-note" role="status">{passwordNotice}</p>}
          <div className="form-actions"><Button variant="primary" type="submit" loading={passwordBusy} disabled={!currentPassword || !newPassword || !confirmPassword}>{passwordBusy ? "正在更新" : "更新密码"}</Button></div>
        </form>
      </Card>
    </div>}
  </section>;
}

function UsersPage() {
  const { user: currentUser } = useAuth();
  const [users, setUsers] = useState<User[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<User | null>(null);
  const [deleting, setDeleting] = useState<User | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [newDisplayName, setNewDisplayName] = useState("");
  const [editDisplayName, setEditDisplayName] = useState("");
  const [editRole, setEditRole] = useState("");
  const [editDisabled, setEditDisabled] = useState(false);

  const canManage = currentUser?.role === "admin" || currentUser?.role === "owner";
  const canCreate = currentUser?.role === "admin";

  const refresh = useCallback(async () => {
    setLoading(true); setError(null); setForbidden(false);
    try { setUsers(await api.listUsers()); }
    catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setForbidden(normalized.status === 403);
      setError(apiErrorMessage(normalized));
    } finally { setLoading(false); }
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);

  function beginEdit(target: User) {
    setEditing(target); setEditDisplayName(target.display_name || ""); setEditRole(target.role); setEditDisabled(Boolean(target.disabled)); setActionError(null);
  }

  async function createUser(event: FormEvent) {
    event.preventDefault(); setBusy(true); setActionError(null);
    try {
      await api.registerUser({ username: username.trim(), password, display_name: newDisplayName.trim() });
      setCreating(false); setUsername(""); setPassword(""); setNewDisplayName(""); await refresh();
    } catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function updateUser(event: FormEvent) {
    event.preventDefault(); if (!editing) return;
    setBusy(true); setActionError(null);
    try {
      await api.updateUser(editing.id, { display_name: editDisplayName, role: editRole, disabled: editDisabled });
      setEditing(null); await refresh();
    } catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function deleteUser() {
    if (!deleting) return;
    setBusy(true); setActionError(null);
    try { await api.deleteUser(deleting.id); setDeleting(null); await refresh(); }
    catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  return <section className="control-content" aria-labelledby="users-heading">
    <div className="control-section-heading"><div><h2 id="users-heading">用户</h2><p>管理员和所有者可管理用户；只有管理员可创建，新用户默认为普通成员角色。</p></div><div className="heading-actions"><Button variant="ghost" loading={loading} onClick={() => void refresh()}>{!loading && <RefreshCw size={16} aria-hidden="true" />}{loading ? "正在刷新" : "刷新"}</Button>{canCreate && <Button variant="primary" onClick={() => { setCreating(true); setActionError(null); }}><Plus size={16} aria-hidden="true" />创建用户</Button>}</div></div>
    {error && !forbidden && <ErrorNotice message={error} onRetry={() => void refresh()} />}
    {actionError && !editing && !creating && !deleting && <ErrorNotice message={actionError} onRetry={() => void refresh()} />}
    {!canManage || forbidden ? <ForbiddenView message={error || "当前账号没有管理用户的权限。"} /> : loading && !users.length ? <LoadingView label="正在读取用户" /> : !users.length ? <Card className="control-empty"><UsersRound size={28} aria-hidden="true" /><h2>还没有用户</h2><p>当前工作区还没有用户，可由管理员创建。</p></Card> : <div className="control-table users-table" role="table" aria-label="用户列表">
      <div className="control-table__header" role="row"><span role="columnheader">用户</span><span role="columnheader">角色</span><span role="columnheader">状态</span><span role="columnheader">创建时间</span><span role="columnheader">更新时间</span><span role="columnheader" aria-label="操作" /></div>
      {users.map((listedUser) => {
        const isSelf = listedUser.id === currentUser?.id;
        return <div className="control-table__row" role="row" key={listedUser.id}>
          <div className="table-identity" role="rowheader"><span className="user-avatar">{(listedUser.display_name || listedUser.username || "U").slice(0, 1).toUpperCase()}</span><span><strong>{listedUser.display_name || listedUser.username || "未命名用户"}</strong><small>{listedUser.username} · {listedUser.id}</small></span></div>
          <span role="cell">{roleLabel(listedUser.role)}</span><span className="control-table__status" role="cell"><Badge tone={listedUser.disabled ? "danger" : "success"}>{listedUser.disabled ? "已禁用" : "启用"}</Badge></span><time role="cell" dateTime={listedUser.created_at}>{formatDate(listedUser.created_at)}</time><time role="cell" dateTime={listedUser.updated_at}>{formatDate(listedUser.updated_at)}</time>
          <div className="row-actions" role="cell"><Button variant="ghost" size="small" aria-label={`编辑 ${listedUser.display_name || listedUser.username}`} disabled={busy || isSelf} title={isSelf ? "请到账户页修改显示名；不能修改自己的角色或启用状态。" : "编辑用户"} onClick={() => beginEdit(listedUser)}><Pencil size={16} aria-hidden="true" /></Button><Button variant="danger" size="small" aria-label={`删除 ${listedUser.display_name || listedUser.username}`} disabled={busy || isSelf} title={isSelf ? "不能通过页面删除当前用户" : "删除用户"} onClick={() => { setDeleting(listedUser); setActionError(null); }}><Trash2 size={16} aria-hidden="true" /></Button></div>
        </div>;
      })}
    </div>}
    <Modal open={creating} title="创建用户" description="新建用户默认为普通成员角色。" onClose={() => { if (!busy) setCreating(false); }} footer={<><Button disabled={busy} onClick={() => setCreating(false)}>取消</Button><Button variant="primary" type="submit" form="user-create" loading={busy} disabled={!username.trim() || !password}>{busy ? "正在创建" : "创建"}</Button></>}><form id="user-create" className="form-stack" onSubmit={(event) => void createUser(event)}><Field label="用户名">{(control) => <input {...control} autoFocus value={username} onChange={(event) => setUsername(event.target.value)} required autoComplete="off" />}</Field><Field label="显示名" help="留空时将使用用户名。">{(control) => <input {...control} value={newDisplayName} onChange={(event) => setNewDisplayName(event.target.value)} autoComplete="off" />}</Field><Field label="初始密码">{(control) => <input {...control} type="password" value={password} onChange={(event) => setPassword(event.target.value)} required autoComplete="new-password" />}</Field><Field label="角色">{(control) => <input {...control} value="普通成员" disabled />}</Field>{actionError && <ErrorNotice message={actionError} onRetry={() => void refresh()} />}</form></Modal>
    <Modal open={!!editing} title="修改用户" description="保存将覆盖该用户的名称、角色与启用状态。" onClose={() => { if (!busy) setEditing(null); }} footer={<><Button disabled={busy} onClick={() => setEditing(null)}>取消</Button><Button variant="primary" type="submit" form="user-edit" loading={busy}>{busy ? "正在保存" : "保存"}</Button></>}><form id="user-edit" className="form-stack" onSubmit={(event) => void updateUser(event)}><Field label="用户名">{(control) => <input {...control} value={editing?.username || ""} disabled />}</Field><Field label="显示名">{(control) => <input {...control} autoFocus value={editDisplayName} onChange={(event) => setEditDisplayName(event.target.value)} />}</Field><Field label="角色" help="选择该用户在工作区中的权限角色。">{(control) => <select {...control} value={editRole} onChange={(event) => setEditRole(event.target.value)}>{userRoles.map((value) => <option key={value} value={value}>{roleLabel(value)}</option>)}</select>}</Field><Field label="禁用用户" help="禁用后将无法登录。">{(control) => <Switch {...control} checked={editDisabled} onChange={setEditDisabled} />}</Field>{actionError && <ErrorNotice message={actionError} onRetry={() => void refresh()} />}</form></Modal>
    <Modal open={!!deleting} title="删除用户" description="删除后不可恢复。" onClose={() => { if (!busy) setDeleting(null); }} footer={<><Button disabled={busy} onClick={() => setDeleting(null)}>取消</Button><Button variant="danger" loading={busy} onClick={() => void deleteUser()}>{busy ? "正在删除" : "确认删除"}</Button></>}><p className="confirm-copy">确定删除“{deleting?.display_name || deleting?.username}”吗？同时会移除对应的工作区成员关系。</p>{actionError && <ErrorNotice message={actionError} onRetry={() => void refresh()} />}</Modal>
  </section>;
}

function APIKeysPage() {
  const { user } = useAuth();
  const [keys, setKeys] = useState<APIKey[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<APIKey | null>(null);
  const [created, setCreated] = useState<CreateAPIKeyResponse | null>(null);
  const [copyNotice, setCopyNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [role, setRole] = useState<(typeof keyRoles)[number]>("service");
  const [selectedScopes, setSelectedScopes] = useState<string[]>([]);
  const [expiresAt, setExpiresAt] = useState("");

  const refresh = useCallback(async () => {
    setLoading(true); setError(null); setForbidden(false);
    try { setKeys(await api.listAPIKeys()); }
    catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setForbidden(normalized.status === 403);
      setError(apiErrorMessage(normalized));
    } finally { setLoading(false); }
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);

  async function createKey(event: FormEvent) {
    event.preventDefault(); setBusy(true); setActionError(null);
    try {
      const response = await api.createAPIKey({ name: name.trim(), role, scopes: selectedScopes, expires_at: expiresAt ? new Date(expiresAt).toISOString() : null });
      setCreating(false); setName(""); setRole("service"); setSelectedScopes([]); setExpiresAt("");
      setCreated(response);
      await refresh();
    } catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  async function deleteKey() {
    if (!deleting) return;
    setBusy(true); setActionError(null);
    try { await api.deleteAPIKey(deleting.id); setDeleting(null); await refresh(); }
    catch (requestError) { setActionError(apiErrorMessage(requestError)); }
    finally { setBusy(false); }
  }

  return <section className="control-content" aria-labelledby="keys-heading">
    <div className="control-section-heading"><div><h2 id="keys-heading">API 密钥</h2><p>仅管理员可管理。列表只显示基本信息，原始密钥仅在创建后显示一次。</p></div><div className="heading-actions"><Button variant="ghost" loading={loading} onClick={() => void refresh()}>{!loading && <RefreshCw size={16} aria-hidden="true" />}{loading ? "正在刷新" : "刷新"}</Button>{user?.role === "admin" && <Button variant="primary" onClick={() => { setCreating(true); setActionError(null); }}><Plus size={16} aria-hidden="true" />创建 API 密钥</Button>}</div></div>
    {error && !forbidden && <ErrorNotice message={error} onRetry={() => void refresh()} />}
    {actionError && !creating && !deleting && <ErrorNotice message={actionError} onRetry={() => void refresh()} />}
    {user?.role !== "admin" || forbidden ? <ForbiddenView message={error || "当前账号没有管理 API 密钥的权限。"} /> : loading && !keys.length ? <LoadingView label="正在读取 API 密钥" /> : !keys.length ? <Card className="control-empty"><KeyRound size={28} aria-hidden="true" /><h2>还没有 API 密钥</h2><p>点击“创建 API 密钥”开始添加；原始密钥只显示一次。</p></Card> : <div className="control-table keys-table" role="table" aria-label="API 密钥列表">
      <div className="control-table__header" role="row"><span role="columnheader">名称</span><span role="columnheader">角色</span><span role="columnheader">权限范围</span><span role="columnheader">到期时间</span><span role="columnheader">最近使用</span><span role="columnheader">创建时间</span><span role="columnheader" aria-label="操作" /></div>
      {keys.map((key) => <div className="control-table__row" role="row" key={key.id}><div role="rowheader"><strong>{key.name}</strong><small>{key.id} · 创建者 {key.created_by || "—"}</small></div><span role="cell">{roleLabel(key.role)}</span><span className="scope-list" role="cell">{key.scopes?.length ? key.scopes.map(apiScopeLabel).join("、") : "全部权限"}</span><time role="cell" dateTime={key.expires_at || undefined}>{formatDate(key.expires_at, "永不过期")}</time><time role="cell" dateTime={key.last_used || undefined}>{formatDate(key.last_used, "尚未使用")}</time><time role="cell" dateTime={key.created_at}>{formatDate(key.created_at)}</time><div className="row-actions" role="cell"><Button variant="danger" size="small" aria-label={`删除 ${key.name}`} title="删除 API 密钥" disabled={busy} onClick={() => { setDeleting(key); setActionError(null); }}><Trash2 size={16} aria-hidden="true" /></Button></div></div>)}
    </div>}
    <Modal open={creating} title="创建 API 密钥" description="选择角色和权限范围；不填写权限范围表示不限制。" onClose={() => { if (!busy) setCreating(false); }} footer={<><Button disabled={busy} onClick={() => setCreating(false)}>取消</Button><Button variant="primary" type="submit" form="key-create" loading={busy} disabled={!name.trim()}>{busy ? "正在创建" : "创建"}</Button></>}><form id="key-create" className="form-stack" onSubmit={(event) => void createKey(event)}><Field label="名称">{(control) => <input {...control} autoFocus value={name} onChange={(event) => setName(event.target.value)} required />}</Field><Field label="角色" help="服务适合日常调用；管理员可访问管理功能。">{(control) => <select {...control} value={role} onChange={(event) => setRole(event.target.value as (typeof keyRoles)[number])}>{keyRoles.map((value) => <option key={value} value={value}>{roleLabel(value)}</option>)}</select>}</Field><Field label="权限范围" help="不勾选表示不限制权限。">{<div className="scope-checkbox-list">{API_SCOPE_OPTIONS.map((option) => <label key={option.value} className="scope-checkbox"><input type="checkbox" checked={selectedScopes.includes(option.value)} onChange={(event) => setSelectedScopes((current) => event.target.checked ? [...current, option.value] : current.filter((scope) => scope !== option.value))} /><span><strong>{option.label}</strong><small>{option.description}</small></span></label>)}</div>}</Field><Field label="到期时间" help="留空则永不过期。">{(control) => <input {...control} type="datetime-local" value={expiresAt} onChange={(event) => setExpiresAt(event.target.value)} />}</Field>{actionError && <ErrorNotice message={actionError} onRetry={() => void refresh()} />}</form></Modal>
    <Modal open={!!created} title="立即保存 API 密钥" description="密钥只显示一次，关闭后无法再次查看，请立即妥善保存。" onClose={() => undefined} footer={<Button variant="primary" onClick={() => { setCreated(null); setCopyNotice(null); }}>我已安全保存</Button>}><div className="token-once"><p>请复制到安全位置。之后列表只显示基本信息。</p><code>{created?.key}</code><Button onClick={async () => { if (!created) return; try { await navigator.clipboard.writeText(created.key); setCopyNotice("已复制到剪贴板"); } catch { setCopyNotice("复制失败，请手动选择上方密钥"); } }}><Clipboard size={16} aria-hidden="true" />复制 API 密钥</Button>{copyNotice && <p role="status">{copyNotice}</p>}</div></Modal>
    <Modal open={!!deleting} title="删除 API 密钥" description="删除后将立即失效且不可恢复。" onClose={() => { if (!busy) setDeleting(null); }} footer={<><Button disabled={busy} onClick={() => setDeleting(null)}>取消</Button><Button variant="danger" loading={busy} onClick={() => void deleteKey()}>{busy ? "正在删除" : "确认删除"}</Button></>}><p className="confirm-copy">确定删除“{deleting?.name}”吗？使用该密钥的服务将无法继续认证。</p>{actionError && <ErrorNotice message={actionError} onRetry={() => void refresh()} />}</Modal>
  </section>;
}
