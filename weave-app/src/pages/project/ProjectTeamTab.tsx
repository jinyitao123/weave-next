import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { api, apiErrorMessage, type ProjectCollaborator, type TeamDispatchRules, type TeamRoster, type User, type WorkflowSummary } from "../../api";
import { useAuth } from "../../auth/useAuth";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { ConfirmModal } from "../../ui/ConfirmModal";
import { Card } from "../../ui/Card";
import { Field } from "../../ui/Field";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { useWorkspace } from "../../workspace/useWorkspace";
import { isPlatformAsset } from "../../workspace/identity";

interface ProjectTeamTabProps {
  projectId: string;
  teamId: string;
}

const when = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });

export function ProjectTeamTab({ projectId, teamId }: ProjectTeamTabProps) {
  const { user } = useAuth();
  const workspace = useWorkspace();
  const [roster, setRoster] = useState<TeamRoster | null>(null);
  const [dispatchRules, setDispatchRules] = useState<TeamDispatchRules | null>(null);
  const [workflows, setWorkflows] = useState<WorkflowSummary[]>([]);
  const [collaborators, setCollaborators] = useState<ProjectCollaborator[]>([]);
  const [users, setUsers] = useState<User[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [addingTeamId, setAddingTeamId] = useState("");
  const [busyTeamId, setBusyTeamId] = useState("");
  const [removingCollaborator, setRemovingCollaborator] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const canManage = user?.role === "owner" || user?.role === "admin";

  const activeTeams = useMemo(() => workspace.teams.filter((team) => {
    if (team.status !== "active") return false;
    const lead = workspace.agents.find((agent) => agent.id === team.lead_avatar_id);
    return !!lead && !lead.deleted && !isPlatformAsset(lead);
  }), [workspace.agents, workspace.teams]);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [rosterRecord, rules, workflowResponse, collaboratorResponse, userRecords] = await Promise.all([
        api.getTeam(teamId),
        api.getTeamDispatchRules(teamId),
        api.listTeamWorkflows(teamId),
        api.listProjectCollaborators(projectId),
        api.listUsers(),
      ]);
      setRoster(rosterRecord);
      setDispatchRules(rules);
      setWorkflows(workflowResponse.workflows);
      setCollaborators(collaboratorResponse.collaborators);
      setUsers(userRecords);
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setLoading(false);
    }
  }, [projectId, teamId]);

  useEffect(() => { void load(); }, [load, workspace.invalidationVersion]);

  const userById = useMemo(() => new Map(users.map((item) => [item.id, item])), [users]);
  const candidateTeams = useMemo(() => activeTeams.filter((team) => (
    team.id !== teamId && !collaborators.some((collaborator) => collaborator.team_id === team.id)
  )), [activeTeams, collaborators, teamId]);

  async function addCollaborator(targetTeamId: string) {
    setActionError(null);
    try {
      await api.addProjectCollaborator(projectId, targetTeamId);
      await load();
    } catch (requestError) {
      setActionError(apiErrorMessage(requestError));
    } finally {
      setAddingTeamId("");
    }
  }

  async function removeCollaborator() {
    const targetTeamId = removingCollaborator;
    if (!targetTeamId) return;
    setBusyTeamId(targetTeamId);
    setActionError(null);
    try {
      await api.removeProjectCollaborator(projectId, targetTeamId);
      setRemovingCollaborator(null);
      await load();
    } catch (requestError) {
      setActionError(apiErrorMessage(requestError));
    } finally {
      setBusyTeamId("");
    }
  }

  if (loading && !roster) return <LoadingView label="正在读取团队配置" />;

  const publishedWorkflows = workflows.filter((workflow) => workflow.status === "active" && workflow.published_version);
  const ownerTeamName = roster?.team.name || workspace.teams.find((team) => team.id === teamId)?.name || "未知团队";

  return (
    <div className="project-team-tab">
      {error && <ErrorNotice message={error} onRetry={() => void load()} />}
      {actionError && <ErrorNotice message={actionError} />}

      <Card
        className="project-team-owner"
        header={<div className="project-team-owner__head"><div><p className="eyebrow">归属团队</p><h2>{ownerTeamName}</h2></div><Link className="ui-button ui-button--secondary ui-button--small" to={`/control/teams/${encodeURIComponent(teamId)}`}>后台管理团队</Link></div>}
      >
        {roster && (
          <div className="project-team-grid">
            <div className="project-team-fact">
              <h3>负责人</h3>
              <p>{roster.lead ? `${roster.lead.display_name || roster.lead.name} · ${roster.lead.engine || "未配置引擎"}` : "未配置负责人"}</p>
            </div>
            <div className="project-team-fact">
              <h3>员工</h3>
              <p>{roster.workers.length > 0 ? roster.workers.map((worker) => worker.name).join("、") : "未配置员工"}</p>
            </div>
            <div className="project-team-fact">
              <h3>派发规则</h3>
              <p>{dispatchRules ? `并行 ${dispatchRules.execution} · leg ${dispatchRules.leg_timeout_sec}s · 组截止 ${dispatchRules.group_deadline_sec}s · quorum ${dispatchRules.quorum}` : "未配置"}</p>
            </div>
            <div className="project-team-fact">
              <h3>协作流程</h3>
              <p>{publishedWorkflows.length > 0 ? `${publishedWorkflows.length} 条已发布 · ${publishedWorkflows.map((workflow) => workflow.name).join("、")}` : "尚未发布流程"}</p>
            </div>
          </div>
        )}
      </Card>

      <section className="project-team-section" aria-labelledby="project-collaborators-title">
        <div className="section-heading">
          <div><h2 id="project-collaborators-title">协作团队</h2><p>其他团队可共同参与该项目；增删需要所有者或管理员权限。</p></div>
          {canManage && candidateTeams.length > 0 && (
            <form className="project-team-add" onSubmit={(event) => { event.preventDefault(); if (addingTeamId) void addCollaborator(addingTeamId); }}>
              <Field label="添加协作团队">{(control) => <select {...control} value={addingTeamId} onChange={(event) => setAddingTeamId(event.target.value)}><option value="">选择团队</option>{candidateTeams.map((team) => <option key={team.id} value={team.id}>{team.name}</option>)}</select>}</Field>
              <Button variant="primary" type="submit" disabled={!addingTeamId}>添加</Button>
            </form>
          )}
        </div>
        {collaborators.length === 0 ? (
          <p className="project-team-none">当前没有协作团队。</p>
        ) : (
          <div className="project-collaborator-list">
            <div className="project-collaborator-row project-collaborator-row--head" aria-hidden="true"><span>团队</span><span>状态</span><span>添加人</span><span>添加时间</span><span /></div>
            {collaborators.map((collaborator) => {
              const team = workspace.teams.find((item) => item.id === collaborator.team_id);
              const addedBy = userById.get(collaborator.added_by);
              return (
                <div className="project-collaborator-row" key={collaborator.team_id}>
                  <strong>{team?.name || collaborator.team_id}</strong>
                  <Badge tone={collaborator.removed_at ? "neutral" : "success"}>{collaborator.removed_at ? "已移除" : "协作中"}</Badge>
                  <span>{addedBy ? (addedBy.display_name || addedBy.username) : "未知成员"}</span>
                  <time dateTime={collaborator.added_at}>{when.format(new Date(collaborator.added_at))}</time>
                  <span className="project-collaborator-row__actions">{canManage && !collaborator.removed_at && (
                    <Button variant="ghost-danger" size="small" loading={busyTeamId === collaborator.team_id} onClick={() => setRemovingCollaborator(collaborator.team_id)}>移除</Button>
                  )}</span>
                </div>
              );
            })}
          </div>
        )}
      </section>
      <ConfirmModal
        open={!!removingCollaborator}
        title="移除协作团队"
        description="移除后该团队不再参与此项目的协作，历史运行记录保留。"
        confirmLabel="移除"
        busy={!!busyTeamId}
        onConfirm={() => void removeCollaborator()}
        onClose={() => setRemovingCollaborator(null)}
      ><p className="confirm-copy">确定移除团队「{workspace.teams.find((team) => team.id === removingCollaborator)?.name || "该团队"}」的协作关系？</p>{actionError && <ErrorNotice message={actionError} />}</ConfirmModal>
    </div>
  );
}
