import { useCallback, useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { Archive, Bot, ChevronRight, FileCode2, LockKeyhole, Plus, RefreshCw, ShieldAlert, UserRound, UsersRound } from "lucide-react";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type AgentRecord,
  type CreateTeamInput,
  type Team,
  type TeamDispatchRules,
  type TeamRoster,
  type TeamRosterKind,
  type TeamRosterWorkerInput,
  type UpdateTeamDispatchRulesInput,
} from "../../api";
import { useAuth } from "../../auth/useAuth";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { Switch } from "../../ui/Switch";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { createClientUUID } from "../../platform/uuid";
import { Navigate, useNavigate, useSearchParams } from "react-router-dom";
import { TeamTemplateModal } from "./TeamTemplateModal";

const dateTime = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
const rosterKinds: TeamRosterKind[] = ["consult", "dispatch", "handoff"];
const rosterKindLabels: Record<TeamRosterKind, string> = {
  consult: "同步咨询",
  dispatch: "异步派工",
  handoff: "交接处理",
};

function rosterKind(value: string): TeamRosterKind | "" {
  if (value === "consult" || value === "dispatch" || value === "handoff") return value;
  return "";
}
type EditableWorker = TeamRosterWorkerInput & { key: string };

interface TeamFormState {
  name: string;
  objective: string;
  primary_scenario: string;
  success_criteria: string;
  lead_avatar_id: string;
  workers: EditableWorker[];
}

function formatDate(value?: string) {
  return value ? dateTime.format(new Date(value)) : "—";
}

function agentLabel(agent: Pick<AgentRecord, "name" | "display_name">) {
  return agent.display_name || agent.name;
}

function emptyWorker(workerAgentID = "", enabled = true): EditableWorker {
  return {
    key: createClientUUID(),
    worker_agent_id: workerAgentID,
    duty: "",
    when_to_use: "",
    context_instruction: "",
    allowed_kinds: ["consult"],
    default_kind: "consult",
    result_requirement: "",
    enabled,
  };
}

function emptyTeamForm(avatars: AgentRecord[], workers: AgentRecord[]): TeamFormState {
  return {
    name: "",
    objective: "",
    primary_scenario: "",
    success_criteria: "",
    lead_avatar_id: avatars[0]?.id || "",
    workers: [emptyWorker(workers[0]?.id || "")],
  };
}

function rosterWorker(worker: TeamRoster["workers"][number]): EditableWorker {
  return {
    key: createClientUUID(),
    worker_agent_id: worker.id,
    duty: worker.configured_duty || worker.duty || "",
    when_to_use: worker.when_to_use || "",
    context_instruction: worker.context_instruction || "",
    allowed_kinds: worker.allowed_kinds || [],
    default_kind: worker.default_kind || "",
    result_requirement: worker.result_requirement || "",
    enabled: worker.enabled,
  };
}

function validateWorkers(workers: EditableWorker[], desiredStatus: Team["status"]): string | null {
  if (!workers.length) return "团队至少需要一位成员。";
  const seen = new Set<string>();
  for (const worker of workers) {
    if (!worker.worker_agent_id) return "请为每位成员选择智能体。";
    if (seen.has(worker.worker_agent_id)) return "同一位成员不能重复加入团队。";
    seen.add(worker.worker_agent_id);
    if (!worker.allowed_kinds.length) return "每位成员至少选择一种协作方式。";
    if (!worker.default_kind || !worker.allowed_kinds.includes(worker.default_kind)) return "默认协作方式必须包含在已选方式中。";
  }
  if (desiredStatus === "active" && !workers.some((worker) => worker.enabled)) return "使用中的团队至少需要一位已启用成员。";
  return null;
}

function workerPayload(worker: EditableWorker): TeamRosterWorkerInput {
  return {
    worker_agent_id: worker.worker_agent_id,
    duty: worker.duty,
    when_to_use: worker.when_to_use,
    context_instruction: worker.context_instruction,
    allowed_kinds: worker.allowed_kinds,
    default_kind: worker.default_kind,
    result_requirement: worker.result_requirement,
    enabled: worker.enabled,
  };
}

function initialWorkerPayload(worker: EditableWorker): CreateTeamInput["workers"][number] {
  return {
    worker_agent_id: worker.worker_agent_id,
    duty: worker.duty,
    when_to_use: worker.when_to_use,
    context_instruction: worker.context_instruction,
    allowed_kinds: worker.allowed_kinds,
    default_kind: worker.default_kind,
    result_requirement: worker.result_requirement,
  };
}

function teamError(error: unknown, action: string) {
  const normalized = normalizeThrownError(error);
  const message = apiErrorMessage(normalized);
  if (normalized.status === 403) return `当前账号没有${action}权限：${message}`;
  if (normalized.status === 409) return `${action}冲突：${message}`;
  return message;
}

export function TeamsPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const navigate = useNavigate();
  const requestedTeamID = searchParams.get("team");
  const requestedCreate = searchParams.get("create") === "1";
  const createRequestHandled = useRef(false);
  const initialRefreshStarted = useRef(false);
  const { user } = useAuth();
  const [teams, setTeams] = useState<Team[]>([]);
  const [agents, setAgents] = useState<AgentRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [teamForm, setTeamForm] = useState<TeamFormState>({ name: "", objective: "", primary_scenario: "", success_criteria: "", lead_avatar_id: "", workers: [] });
  const [createStep, setCreateStep] = useState<"basic" | "members">("basic");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);
  const [creatingFromTemplate, setCreatingFromTemplate] = useState(false);

  const avatars = useMemo(() => agents.filter((agent) => !agent.deleted && agent.role === "avatar"), [agents]);
  const workerCandidates = useMemo(() => agents.filter((agent) => !agent.deleted && agent.role === "worker"), [agents]);
  const canEditTeam = user?.role === "admin" || user?.role === "owner";
  const canCreate = canEditTeam;
  const canCreateFromTemplate = user?.role === "admin";

  const refresh = useCallback(async (initial = false) => {
    if (initial) setLoading(true); else setRefreshing(true);
    setError(null);
    setForbidden(false);
    try {
      const [nextTeams, nextAgents] = await Promise.all([api.listTeams(), api.listAgents()]);
      setTeams(nextTeams);
      setAgents(nextAgents);
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setForbidden(normalized.status === 403);
      setError(apiErrorMessage(normalized));
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    if (requestedTeamID || initialRefreshStarted.current) return;
    initialRefreshStarted.current = true;
    void refresh(true);
  }, [refresh, requestedTeamID]);

  const beginCreate = useCallback(() => {
    setTeamForm(emptyTeamForm(avatars, workerCandidates));
    setActionError(null);
    setConflict(false);
    setCreateStep("basic");
    setCreating(true);
  }, [avatars, workerCandidates]);

  const finishTemplateCreate = useCallback((teamID: string) => {
    setCreatingFromTemplate(false);
    navigate(`/control/teams/${encodeURIComponent(teamID)}`);
  }, [navigate]);

  useEffect(() => {
    if (!requestedCreate) {
      createRequestHandled.current = false;
      return;
    }
    if (loading || forbidden || !canCreate || createRequestHandled.current) return;
    createRequestHandled.current = true;
    beginCreate();
    setSearchParams((current) => {
      const next = new URLSearchParams(current);
      next.delete("create");
      return next;
    }, { replace: true });
  }, [beginCreate, canCreate, forbidden, loading, requestedCreate, setSearchParams]);

  function updateTeamWorker(key: string, patch: Partial<EditableWorker>) {
    setTeamForm((current) => ({ ...current, workers: current.workers.map((worker) => worker.key === key ? { ...worker, ...patch } : worker) }));
  }

  async function createTeam(event: FormEvent) {
    event.preventDefault();
    setActionError(null);
    const workerError = validateWorkers(teamForm.workers, "active");
    if (workerError) { setActionError(workerError); return; }
    if (!teamForm.name.trim() || !teamForm.objective.trim() || !teamForm.lead_avatar_id) {
      setActionError("请填写团队名称、团队目标并选择负责人。");
      return;
    }
    const input: CreateTeamInput = {
      name: teamForm.name.trim(),
      objective: teamForm.objective.trim(),
      primary_scenario: teamForm.primary_scenario.trim(),
      success_criteria: teamForm.success_criteria.trim(),
      lead_avatar_id: teamForm.lead_avatar_id,
      workers: teamForm.workers.map(initialWorkerPayload),
    };
    setBusy(true);
    try {
      const created = await api.createTeam(input);
      setCreating(false);
      navigate(`/control/teams/${encodeURIComponent(created.id)}`);
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setConflict(normalized.status === 409);
      setActionError(teamError(normalized, "创建团队"));
    } finally {
      setBusy(false);
    }
  }

  if (requestedTeamID) {
    return <Navigate to={`/control/teams/${encodeURIComponent(requestedTeamID)}`} replace />;
  }

  const usedCreateWorkers = new Set(teamForm.workers.map((worker) => worker.worker_agent_id));

  return <section className="control-content teams-control" aria-labelledby="teams-heading">
    <div className="control-section-heading">
      <div>
        <h2 id="teams-heading">团队</h2>
        <p>查看团队名册、当前状态并创建团队</p>
      </div>
      <div className="heading-actions">
        <Button
          variant="ghost"
          loading={refreshing}
          disabled={loading}
          onClick={() => void refresh(false)}
        >
          {!refreshing && <RefreshCw size={16} aria-hidden="true" />}
          {refreshing ? "正在刷新" : "刷新"}
        </Button>
        {canCreate && !forbidden && <Button
          variant="primary"
          disabled={!avatars.length || !workerCandidates.length}
          onClick={beginCreate}
        >
          <Plus size={16} aria-hidden="true" />
          创建团队
        </Button>}
        {canCreateFromTemplate && !forbidden && <Button
          variant="primary"
          onClick={() => setCreatingFromTemplate(true)}
        >
          <FileCode2 size={16} aria-hidden="true" />
          从模板创建
        </Button>}
      </div>
    </div>
    {error && !forbidden && <ErrorNotice message={error} onRetry={() => void refresh(false)} />}
    {forbidden ? <div className="empty-surface control-forbidden">
      <LockKeyhole size={28} />
      <h2>没有团队管理权限</h2>
      <p>当前账号没有团队管理权限，请联系管理员。</p>
    </div> : loading && !teams.length ? <LoadingView label="正在加载团队与智能体候选" /> : !teams.length ? <div className="empty-surface teams-empty">
      <UsersRound size={28} />
      <h2>还没有团队</h2>
      <p>{canCreate ? "创建团队需要一位负责人和至少一位成员。" : "请联系管理员创建第一个团队。"}</p>
      {canCreate && avatars.length > 0 && workerCandidates.length > 0 && <Button
        variant="primary"
        onClick={beginCreate}
      >
        <Plus size={16} aria-hidden="true" />
        创建第一个团队
      </Button>}
      {canCreateFromTemplate && <Button variant="primary" onClick={() => setCreatingFromTemplate(true)}>
        <FileCode2 size={16} aria-hidden="true" />
        从行业模板创建
      </Button>}
      {(!avatars.length || !workerCandidates.length) && <p className="candidate-warning">
        {!avatars.length ? "手动创建暂无可作为负责人的分身。请先在「智能体」中创建分身。" : ""}
        {!avatars.length && !workerCandidates.length ? " " : ""}
        {!workerCandidates.length ? "手动创建暂无可作为成员的数字员工。请先在「智能体」中创建数字员工。" : ""}
      </p>}
    </div> : <div className="teams-master" role="list" aria-label="团队列表">
        {teams.map((team) => <button
          key={team.id}
          type="button"
          role="listitem"
          className="team-row"
          onClick={() => navigate(`/control/teams/${encodeURIComponent(team.id)}`)}
        >
          <span className="team-row__icon"><UsersRound size={16} /></span>
          <span>
            <strong>{team.name}</strong>
            <small>{team.objective || "未设置目标"}</small>
          </span>
          <span className="team-row__badges">
            {team.evaluation === "unevaluated" && <Badge tone="warning">待评测</Badge>}
            <Badge tone={team.status === "active" ? "success" : "neutral"}>
              {team.status === "active" ? "使用中" : "已归档"}
            </Badge>
          </span>
          <ChevronRight size={16} />
        </button>)}
    </div>}

    <Modal
      open={creating}
      size="wide"
      title="创建团队"
      description="创建后将同时建立负责人和初始成员配置。"
      onClose={() => {
        if (!busy) {
          setCreating(false);
          setActionError(null);
          setConflict(false);
        }
      }}
      footer={<>
        <Button disabled={busy} onClick={() => setCreating(false)}>取消</Button>
        {createStep === "basic" ? <Button
          variant="primary"
          type="button"
          disabled={!teamForm.name.trim() || !teamForm.objective.trim() || !teamForm.lead_avatar_id}
          onClick={() => setCreateStep("members")}
        >下一步：成员配置</Button> : <Button
          variant="primary"
          type="submit"
          form="team-create-form"
          loading={busy}
          disabled={!teamForm.workers.length}
        >
          {busy ? "正在创建…" : "创建团队"}
        </Button>}
      </>}
    >
      <form id="team-create-form" className="team-editor" onSubmit={(event) => void createTeam(event)}>
        {actionError && <ErrorNotice
          message={actionError}
          onRetry={conflict ? () => {
            setActionError(null);
            setConflict(false);
          } : undefined}
        />}
        <ol className="team-create-steps" aria-label="创建步骤">
          <li className={createStep === "basic" ? "is-active" : "is-done"}>1 · 基本信息</li>
          <li className={createStep === "members" ? "is-active" : ""}>2 · 成员与职责</li>
        </ol>
        {createStep === "members" && <p className="team-create-back"><button type="button" className="text-link" onClick={() => setCreateStep("basic")}>返回修改基本信息</button></p>}
        {createStep === "basic" && <section className="team-form-section">
          <header><h3>基本信息</h3></header>
          <div className="team-form-grid">
            <Field label="团队名称">
              {(control) => <input
                {...control}
                autoFocus
                required
                value={teamForm.name}
                onChange={(event) => setTeamForm((current) => ({ ...current, name: event.target.value }))}
              />}
            </Field>
            <Field label="负责人">
              {(control) => <select
                {...control}
                required
                value={teamForm.lead_avatar_id}
                onChange={(event) => setTeamForm((current) => ({ ...current, lead_avatar_id: event.target.value }))}
              >
                <option value="">选择一位分身作为负责人</option>
                {avatars.map((agent) => <option key={agent.id} value={agent.id}>
                  {agentLabel(agent)} · {agent.name}
                </option>)}
              </select>}
            </Field>
          </div>
          <Field label="团队目标">
            {(control) => <textarea
              {...control}
              required
              rows={3}
              value={teamForm.objective}
              onChange={(event) => setTeamForm((current) => ({ ...current, objective: event.target.value }))}
            />}
          </Field>
          <div className="team-form-grid">
            <Field label="主要场景">
              {(control) => <textarea
                {...control}
                rows={3}
                value={teamForm.primary_scenario}
                onChange={(event) => setTeamForm((current) => ({ ...current, primary_scenario: event.target.value }))}
              />}
            </Field>
            <Field label="成功标准">
              {(control) => <textarea
                {...control}
                rows={3}
                value={teamForm.success_criteria}
                onChange={(event) => setTeamForm((current) => ({ ...current, success_criteria: event.target.value }))}
              />}
            </Field>
          </div>
        </section>}
        {createStep === "members" && <section className="team-form-section">
          <header>
            <h3>初始成员</h3>
            <p>新成员创建后默认启用。</p>
          </header>
          {!workerCandidates.length ? <CandidateWarning kind="Worker" /> : <>
            <div className="roster-worker-list">
              {teamForm.workers.map((worker, index) => <WorkerFields
                key={worker.key}
                worker={worker}
                index={index}
                candidates={workerCandidates}
                usedIDs={usedCreateWorkers}
                disabled={busy}
                showEnabled={false}
                onChange={(patch) => updateTeamWorker(worker.key, patch)}
                onRemove={teamForm.workers.length > 1 ? () => setTeamForm((current) => ({
                  ...current,
                  workers: current.workers.filter((item) => item.key !== worker.key),
                })) : undefined}
              />)}
            </div>
            <Button
              variant="ghost"
              disabled={busy || !workerCandidates.some((candidate) => !usedCreateWorkers.has(candidate.id))}
              onClick={() => {
                const candidate = workerCandidates.find((item) => !usedCreateWorkers.has(item.id));
                if (candidate) setTeamForm((current) => ({
                  ...current,
                  workers: [...current.workers, emptyWorker(candidate.id)],
                }));
              }}
            >
              <Plus size={16} aria-hidden="true" />
              添加成员
            </Button>
          </>}
        </section>}
      </form>
    </Modal>

    <TeamTemplateModal
      open={creatingFromTemplate}
      onClose={() => setCreatingFromTemplate(false)}
      onReady={finishTemplateCreate}
    />

  </section>;
}

type TeamDetailSections = "roster" | "dispatch" | "all";

export function TeamDetailPanel({ teamID, onArchived, sections = "all" }: { teamID: string; onArchived?: () => void; sections?: TeamDetailSections }) {
  const { user } = useAuth();
  const [agents, setAgents] = useState<AgentRecord[]>([]);
  const [detail, setDetail] = useState<TeamRoster | null>(null);
  const [detailLoading, setDetailLoading] = useState(true);
  const [detailError, setDetailError] = useState<string | null>(null);
  const [dispatchRules, setDispatchRules] = useState<TeamDispatchRules | null>(null);
  const [dispatchLoading, setDispatchLoading] = useState(false);
  const [dispatchError, setDispatchError] = useState<string | null>(null);
  const [editingRoster, setEditingRoster] = useState(false);
  const [editingDispatch, setEditingDispatch] = useState(false);
  const [dispatchForm, setDispatchForm] = useState<UpdateTeamDispatchRulesInput>({ execution: "parallel", leg_timeout_sec: 180, group_deadline_sec: 480, quorum: 0 });
  const [dispatchBusy, setDispatchBusy] = useState(false);
  const [dispatchActionError, setDispatchActionError] = useState<string | null>(null);
  const [archiving, setArchiving] = useState(false);
  const [archiveReason, setArchiveReason] = useState("");
  const [archiveBusy, setArchiveBusy] = useState(false);
  const [archiveError, setArchiveError] = useState<string | null>(null);
  const [rosterLeadID, setRosterLeadID] = useState("");
  const [rosterWorkers, setRosterWorkers] = useState<EditableWorker[]>([]);
  const [rosterReason, setRosterReason] = useState("");
  const [rosterBaseUpdatedAt, setRosterBaseUpdatedAt] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [conflict, setConflict] = useState(false);

  const avatars = useMemo(() => agents.filter((agent) => !agent.deleted && agent.role === "avatar"), [agents]);
  const workerCandidates = useMemo(() => agents.filter((agent) => !agent.deleted && agent.role === "worker"), [agents]);
  const canEditTeam = user?.role === "admin" || user?.role === "owner";
  const canArchive = canEditTeam;
  const teamWritable = detail?.team.status === "active";

  const readDetail = useCallback(async (nextTeamID: string) => {
    setDetailLoading(true);
    setDispatchLoading(true);
    setDetailError(null);
    setDispatchError(null);
    setDispatchRules(null);
    let next: TeamRoster;
    try {
      const [nextDetail, nextAgents] = await Promise.all([api.getTeam(nextTeamID), api.listAgents()]);
      next = nextDetail;
      setDetail(nextDetail);
      setAgents(nextAgents);
    } catch (requestError) {
      setDetail(null);
      setDetailError(teamError(requestError, "读取团队详情"));
      setDetailLoading(false);
      setDispatchLoading(false);
      return null;
    }
    setDetailLoading(false);
    try {
      setDispatchRules(await api.getTeamDispatchRules(nextTeamID));
    } catch (requestError) {
      setDispatchError(teamError(requestError, "读取并行调度规则"));
    } finally {
      setDispatchLoading(false);
    }
    return next;
  }, []);

  useEffect(() => {
    void readDetail(teamID);
  }, [readDetail, teamID]);

  async function beginRosterEdit() {
    setEditingRoster(true);
    setActionError(null);
    setConflict(false);
    setDetailLoading(true);
    const current = await readDetail(teamID);
    if (current) {
      setRosterLeadID(current.lead?.id || avatars[0]?.id || "");
      setRosterWorkers(current.workers.map(rosterWorker));
      setRosterReason("");
      setRosterBaseUpdatedAt(current.team.updated_at);
    }
  }

  function updateRosterWorker(key: string, patch: Partial<EditableWorker>) {
    setRosterWorkers((current) => current.map((worker) => worker.key === key ? { ...worker, ...patch } : worker));
  }

  function addRosterWorker() {
    const used = new Set(rosterWorkers.map((worker) => worker.worker_agent_id));
    const candidate = workerCandidates.find((worker) => !used.has(worker.id));
    if (candidate) setRosterWorkers((current) => [...current, emptyWorker(candidate.id)]);
  }

  async function reloadRosterEditor() {
    setActionError(null);
    setConflict(false);
    const current = await readDetail(teamID);
    if (!current) return;
    setRosterLeadID(current.lead?.id || avatars[0]?.id || "");
    setRosterWorkers(current.workers.map(rosterWorker));
    setRosterReason("");
    setRosterBaseUpdatedAt(current.team.updated_at);
  }

  async function saveRoster(event: FormEvent) {
    event.preventDefault();
    if (!detail) return;
    setActionError(null);
    setConflict(false);
    const workerError = validateWorkers(rosterWorkers, detail.team.status);
    if (workerError) { setActionError(workerError); return; }
    if (!rosterLeadID) { setActionError("请选择团队负责人。"); return; }
    if (!rosterReason.trim()) { setActionError("请填写变更原因。"); return; }
    setBusy(true);
    try {
      const serverDetail = await api.getTeam(detail.team.id);
      if (serverDetail.team.updated_at !== rosterBaseUpdatedAt) {
        setConflict(true);
        setActionError("团队配置已在服务端更新。当前编辑内容未覆盖服务端，请刷新后重新编辑。");
        return;
      }
      await api.updateTeamRoster(detail.team.id, {
        idempotency_key: createClientUUID(),
        expected_updated_at: serverDetail.team.updated_at,
        desired_team_status: serverDetail.team.status,
        lead_agent_id: rosterLeadID,
        workers: rosterWorkers.map(workerPayload),
        reason: rosterReason.trim(),
      });
      const reread = await api.getTeam(detail.team.id);
      setDetail(reread);
      setEditingRoster(false);
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setConflict(normalized.status === 409);
      setActionError(normalized.status === 409
        ? `团队配置保存冲突：${apiErrorMessage(normalized)}。本地内容未覆盖服务端，请刷新。`
        : teamError(normalized, "保存团队配置"));
    } finally {
      setBusy(false);
    }
  }

  function beginDispatchEdit() {
    if (!dispatchRules || !detail || detail.team.status === "archived") return;
    setDispatchForm({
      execution: dispatchRules.execution,
      leg_timeout_sec: dispatchRules.leg_timeout_sec,
      group_deadline_sec: dispatchRules.group_deadline_sec,
      quorum: dispatchRules.quorum,
    });
    setDispatchActionError(null);
    setEditingDispatch(true);
  }

  async function saveDispatchRules(event: FormEvent) {
    event.preventDefault();
    if (!detail) return;
    setDispatchBusy(true);
    setDispatchActionError(null);
    try {
      await api.updateTeamDispatchRules(detail.team.id, dispatchForm);
      const reread = await api.getTeamDispatchRules(detail.team.id);
      setDispatchRules(reread);
      setEditingDispatch(false);
    } catch (requestError) {
      setDispatchActionError(teamError(requestError, "保存并行调度规则"));
    } finally {
      setDispatchBusy(false);
    }
  }

  async function archiveTeam() {
    if (!detail || !archiveReason.trim()) return;
    setArchiveBusy(true);
    setArchiveError(null);
    try {
      const latest = await api.getTeam(detail.team.id);
      if (latest.team.status === "archived") {
        setArchiving(false);
        await onArchived?.();
        await readDetail(latest.team.id);
        return;
      }
      await api.updateTeamRoster(latest.team.id, {
        idempotency_key: createClientUUID(),
        expected_updated_at: latest.team.updated_at,
        desired_team_status: "archived",
        lead_agent_id: latest.lead?.id || latest.team.lead_avatar_id,
        workers: latest.workers.map(workerPayloadFromSummary),
        reason: archiveReason.trim(),
      });
      setArchiving(false);
      setArchiveReason("");
      await onArchived?.();
      await readDetail(latest.team.id);
    } catch (requestError) {
      setArchiveError(teamError(requestError, "归档团队"));
    } finally {
      setArchiveBusy(false);
    }
  }

  function workerPayloadFromSummary(worker: TeamRoster["workers"][number]): TeamRosterWorkerInput {
    return {
      worker_agent_id: worker.id,
      duty: worker.configured_duty || worker.duty || "",
      when_to_use: worker.when_to_use || "",
      context_instruction: worker.context_instruction || "",
      allowed_kinds: worker.allowed_kinds || [],
      default_kind: worker.default_kind || "",
      result_requirement: worker.result_requirement || "",
      enabled: worker.enabled,
    };
  }

  const usedRosterWorkers = new Set(rosterWorkers.map((worker) => worker.worker_agent_id));
  const rosterHasUnusedCandidate = workerCandidates.some((worker) => !usedRosterWorkers.has(worker.id));

  return <>
    {detailLoading ? <LoadingView label="正在加载团队详情与并行调度规则" /> : detailError ? <ErrorNotice
      message={detailError}
      onRetry={() => void readDetail(teamID)}
    /> : detail ? <TeamDetail
      detail={detail}
      dispatchRules={dispatchRules}
      dispatchLoading={dispatchLoading}
      dispatchError={dispatchError}
      canEdit={Boolean(canEditTeam && teamWritable)}
      canArchive={Boolean(canArchive && teamWritable)}
      onEditRoster={() => void beginRosterEdit()}
      onEditDispatch={beginDispatchEdit}
      onArchive={() => {
        setArchiveReason("");
        setArchiveError(null);
        setArchiving(true);
      }}
      onRetryDispatch={() => void readDetail(teamID)}
      sections={sections}
    /> : <p>选择团队查看详情。</p>}

    <Modal
      open={editingRoster}
      size="wide"
      title={`编辑 ${detail?.team.name || "团队"} 编制`}
      description="保存将整体替换团队配置；若服务端版本已更新，需要刷新后重新编辑。"
      onClose={() => {
        if (!busy) {
          setEditingRoster(false);
          setActionError(null);
          setConflict(false);
        }
      }}
      footer={<>
        <Button disabled={busy} onClick={() => setEditingRoster(false)}>取消</Button>
        <Button
          variant="primary"
          type="submit"
          form="roster-edit-form"
          loading={busy}
          disabled={detailLoading || !rosterLeadID || !rosterReason.trim() || !rosterWorkers.length}
        >
          {busy ? "正在保存…" : "保存团队配置"}
        </Button>
      </>}
    >
      {detailLoading && !rosterBaseUpdatedAt ? <LoadingView label="正在加载最新团队配置" /> : <form
        id="roster-edit-form"
        className="team-editor"
        onSubmit={(event) => void saveRoster(event)}
      >
        {actionError && <ErrorNotice message={actionError} onRetry={conflict ? () => void reloadRosterEditor() : undefined} />}
        {!canEditTeam && <div className="notice">
          <LockKeyhole size={16} />
          <span>当前账号只有查看权限，请联系管理员修改团队配置。</span>
        </div>}
        <section className="team-form-section">
          <header>
            <h3>团队配置</h3>
            <p>保存将整体替换团队配置，团队状态保持不变。</p>
          </header>
          <div className="team-form-grid">
            <Field label="负责人">
              {(control) => <select
                {...control}
                value={rosterLeadID}
                disabled={busy || !canEditTeam}
                onChange={(event) => setRosterLeadID(event.target.value)}
              >
                <option value="">选择一位分身作为负责人</option>
                {avatars.map((agent) => <option key={agent.id} value={agent.id}>
                  {agentLabel(agent)} · {agent.name}
                </option>)}
              </select>}
            </Field>
            <Field label="服务端版本戳" help="保存冲突时用于校验当前编辑是否基于最新版本。">
              {(control) => <input {...control} value={rosterBaseUpdatedAt} readOnly />}
            </Field>
          </div>
          <Field label="变更原因">
            {(control) => <textarea
              {...control}
              required
              rows={2}
              value={rosterReason}
              disabled={busy || !canEditTeam}
              onChange={(event) => setRosterReason(event.target.value)}
              placeholder="说明本次团队配置的变更原因"
            />}
          </Field>
          {!avatars.length && <CandidateWarning kind="Avatar" />}
        </section>
        <section className="team-form-section">
          <header>
            <h3>成员</h3>
            <p>保存将整体替换现有成员配置。</p>
          </header>
          {!workerCandidates.length ? <CandidateWarning kind="Worker" /> : <>
            <div className="roster-worker-list">
              {rosterWorkers.map((worker, index) => <WorkerFields
                key={worker.key}
                worker={worker}
                index={index}
                candidates={workerCandidates}
                usedIDs={usedRosterWorkers}
                disabled={busy || !canEditTeam}
                showEnabled
                onChange={(patch) => updateRosterWorker(worker.key, patch)}
              />)}
            </div>
            <Button
              variant="ghost"
              disabled={busy || !canEditTeam || !rosterHasUnusedCandidate}
              onClick={addRosterWorker}
            >
              <Plus size={16} aria-hidden="true" />
              添加成员
            </Button>
            {!rosterHasUnusedCandidate && <p className="candidate-note">没有更多可加入的成员候选。</p>}
          </>}
        </section>
      </form>}
    </Modal>

    <Modal
      open={editingDispatch}
      title={`编辑 ${detail?.team.name || "团队"} 并行调度规则`}
      description="保存将整体替换团队的并行调度规则。"
      onClose={() => {
        if (!dispatchBusy) {
          setEditingDispatch(false);
          setDispatchActionError(null);
        }
      }}
      footer={<>
        <Button disabled={dispatchBusy} onClick={() => setEditingDispatch(false)}>取消</Button>
        <Button
          variant="primary"
          type="submit"
          form="dispatch-rules-form"
          loading={dispatchBusy}
        >
          {dispatchBusy ? "正在保存…" : "保存规则"}
        </Button>
      </>}
    >
      <form id="dispatch-rules-form" className="form-stack" onSubmit={(event) => void saveDispatchRules(event)}>
        {dispatchActionError && <ErrorNotice
          message={dispatchActionError}
          onRetry={() => void readDetail(teamID).then(() => setEditingDispatch(false))}
        />}
        <Field label="执行方式" help="暂不支持串行执行。">
          <strong>并行执行</strong>
        </Field>
        <Field label="单个成员任务超时（秒）">
          {(control) => <input
            {...control}
            type="number"
            min="0"
            required
            value={dispatchForm.leg_timeout_sec}
            disabled={dispatchBusy}
            onChange={(event) => setDispatchForm((current) => ({
              ...current,
              leg_timeout_sec: Number(event.target.value),
            }))}
          />}
        </Field>
        <Field label="任务组截止时间（秒）">
          {(control) => <input
            {...control}
            type="number"
            min="0"
            required
            value={dispatchForm.group_deadline_sec}
            disabled={dispatchBusy}
            onChange={(event) => setDispatchForm((current) => ({
              ...current,
              group_deadline_sec: Number(event.target.value),
            }))}
          />}
        </Field>
        <Field label="最少成功成员数" help="最少需要 N 个成员返回成功才算组成功；填 0 表示不限制。">
          {(control) => <input
            {...control}
            type="number"
            min="0"
            max={detail?.workers.filter((worker) => worker.enabled).length}
            required
            value={dispatchForm.quorum}
            disabled={dispatchBusy}
            onChange={(event) => setDispatchForm((current) => ({
              ...current,
              quorum: Number(event.target.value),
            }))}
          />}
        </Field>
      </form>
    </Modal>

    <Modal
      open={archiving}
      title="归档团队"
      description="归档后团队及其配置将只读，且无法恢复。"
      onClose={() => {
        if (!archiveBusy) {
          setArchiving(false);
          setArchiveError(null);
        }
      }}
      footer={<>
        <Button disabled={archiveBusy} onClick={() => setArchiving(false)}>取消</Button>
        <Button
          variant="danger"
          type="button"
          loading={archiveBusy}
          disabled={!archiveReason.trim()}
          onClick={() => void archiveTeam()}
        >
          {archiveBusy ? "正在归档…" : "确认归档"}
        </Button>
      </>}
    >
      <div className="form-stack">
        <p className="confirm-copy">
          {"归档“"}{detail?.team.name}{"”后，Team、Roster 与 Dispatch Rules 全部只读，不接受新 command；后端没有恢复 API。"}
        </p>
        <Field label="归档原因">
          {(control) => <textarea
            {...control}
            autoFocus
            required
            rows={3}
            value={archiveReason}
            disabled={archiveBusy}
            onChange={(event) => setArchiveReason(event.target.value)}
            placeholder="请填写归档原因（必填）"
          />}
        </Field>
        {archiveError && <ErrorNotice
          message={archiveError}
          onRetry={() => detail && void readDetail(detail.team.id).then(() => setArchiving(false))}
        />}
      </div>
    </Modal>
  </>;
}

function TeamDetail({
  detail,
  dispatchRules,
  dispatchLoading,
  dispatchError,
  canEdit,
  canArchive,
  onEditRoster,
  onEditDispatch,
  onArchive,
  onRetryDispatch,
  sections,
}: {
  detail: TeamRoster;
  dispatchRules: TeamDispatchRules | null;
  dispatchLoading: boolean;
  dispatchError: string | null;
  canEdit: boolean;
  canArchive: boolean;
  onEditRoster(): void;
  onEditDispatch(): void;
  onArchive(): void;
  onRetryDispatch(): void;
  sections: TeamDetailSections;
}) {
  const { team, lead, workers } = detail;
  const archived = team.status === "archived";
  const showRoster = sections !== "dispatch";
  const showDispatch = sections !== "roster";
  return <article className={archived ? "team-detail-card team-detail-card--archived" : "team-detail-card"}>
    {showRoster && <><header>
      <div>
        <Badge tone={team.status === "active" ? "success" : "neutral"}>
          {team.status === "active" ? "使用中" : "已归档"}
        </Badge>
        <h3>{team.name}</h3>
        <p>{team.objective || "未设置目标"}</p>
      </div>
      <div className="team-detail-actions">
        {canEdit && <Button variant="primary" onClick={onEditRoster}>编辑团队配置</Button>}
        {canArchive && <Button variant="danger" onClick={onArchive}>
          <Archive size={16} aria-hidden="true" />
          归档团队
        </Button>}
      </div>
    </header>
    {archived && <div className="notice team-archive-boundary">
      <LockKeyhole size={16} />
      <span>已归档：团队、编制和并行调度规则均为只读，且无法恢复。</span>
    </div>}
    <dl className="team-facts">
      <div><dt>团队 ID</dt><dd>{team.id}</dd></div>
      <div><dt>更新时间</dt><dd>{formatDate(team.updated_at)}</dd></div>
      <div><dt>主要场景</dt><dd>{team.primary_scenario || "—"}</dd></div>
      <div><dt>成功标准</dt><dd>{team.success_criteria || "—"}</dd></div>
    </dl>
    <section className="team-members">
      <h4>负责人</h4>
      {lead ? <div className="member-summary">
        <span className="member-avatar"><UserRound size={16} /></span>
        <span>
          <strong>{lead.display_name || lead.name}</strong>
          <small>{lead.name} · {lead.id}</small>
        </span>
      </div> : <div className="inline-empty">
        <ShieldAlert size={16} />
        未配置负责人
      </div>}
    </section>
    <section className="team-members">
      <h4>成员 <span>{workers.length}</span></h4>
      {workers.length ? <div className="team-worker-summaries">
        {workers.map((worker) => <div className="worker-summary" key={worker.id}>
          <span className="member-avatar"><Bot size={16} /></span>
          <span>
            <strong>{worker.display_name || worker.name}</strong>
            <small>{worker.configured_duty || worker.duty || "未设置职责"}</small>
            <small>
              {worker.allowed_kinds.map((kind) => rosterKindLabels[kind]).join(" · ")}
              {" · 默认方式："}
              {worker.default_kind ? rosterKindLabels[worker.default_kind] : "—"}
            </small>
          </span>
          <Badge tone={worker.enabled ? "success" : "neutral"}>
            {worker.enabled ? "已启用" : "已停用"}
          </Badge>
        </div>)}
      </div> : <div className="inline-empty">
        <ShieldAlert size={16} />
        未配置成员
      </div>}
    </section>
    </>}
    {showDispatch && <section className="dispatch-rules-section">
      <div className="dispatch-rules-heading">
        <div>
          <h4>并行调度规则</h4>
          <p>设置并行任务的超时、截止时间与最少成功成员数。</p>
        </div>
        {canEdit && dispatchRules && <Button variant="ghost" onClick={onEditDispatch}>
          编辑规则
        </Button>}
      </div>
      {dispatchLoading ? <div className="dispatch-rules-state" role="status">正在加载并行调度规则…</div> : dispatchError ? <ErrorNotice
        message={dispatchError}
        onRetry={onRetryDispatch}
      /> : dispatchRules ? <dl className="dispatch-rules-facts">
        <div><dt>执行方式</dt><dd>并行执行</dd></div>
        <div><dt>单个成员任务超时</dt><dd>{dispatchRules.leg_timeout_sec} 秒</dd></div>
        <div><dt>任务组截止时间</dt><dd>{dispatchRules.group_deadline_sec} 秒</dd></div>
        <div><dt>最少成功成员数</dt><dd>{dispatchRules.quorum || "不限制"}</dd></div>
      </dl> : <div className="dispatch-rules-state">暂未配置并行调度规则。</div>}
      {!canEdit && <p className="readonly-note">
        {archived ? "已归档团队不接受编制或并行调度规则变更。" : "当前账号只有查看权限，请联系管理员修改团队配置。"}
      </p>}
    </section>}
  </article>;
}

function CandidateWarning({ kind }: { kind: "Avatar" | "Worker" }) {
  return <div className="notice notice--warning">
    <ShieldAlert size={16} />
    <span>
      {kind === "Avatar"
        ? "暂无可作为负责人的分身。请先在「智能体」中创建分身。"
        : "暂无可作为成员的数字员工。请先在「智能体」中创建数字员工。"}
    </span>
  </div>;
}

function WorkerFields({
  worker,
  index,
  candidates,
  usedIDs,
  disabled,
  showEnabled,
  onChange,
  onRemove,
}: {
  worker: EditableWorker;
  index: number;
  candidates: AgentRecord[];
  usedIDs: Set<string>;
  disabled: boolean;
  showEnabled: boolean;
  onChange(patch: Partial<EditableWorker>): void;
  onRemove?: () => void;
}) {
  function toggleKind(kind: TeamRosterKind, checked: boolean) {
    const allowed = checked ? [...worker.allowed_kinds, kind] : worker.allowed_kinds.filter((item) => item !== kind);
    const currentDefault = worker.default_kind;
    onChange({ allowed_kinds: allowed, default_kind: currentDefault && allowed.includes(currentDefault) ? currentDefault : allowed[0] || "" });
  }
  return <fieldset className="roster-worker">
    <legend>成员 {index + 1}</legend>
    <div className="roster-worker__heading">
      <Field label="成员智能体" help="选择在团队中承担该成员职责的智能体。">
        {(control) => <select
          {...control}
          value={worker.worker_agent_id}
          disabled={disabled}
          onChange={(event) => onChange({ worker_agent_id: event.target.value })}
        >
          <option value="">选择角色为 worker 的智能体</option>
          {candidates.map((candidate) => <option
            key={candidate.id}
            value={candidate.id}
            disabled={candidate.id !== worker.worker_agent_id && usedIDs.has(candidate.id)}
          >
            {agentLabel(candidate)} · {candidate.name}
          </option>)}
        </select>}
      </Field>
      {showEnabled && <label className="worker-enabled">
        <Switch
          checked={worker.enabled}
          disabled={disabled}
          aria-label="启用成员"
          onChange={(next) => onChange({ enabled: next })}
        />
        <span>启用成员</span>
        <small>关闭后不参与团队协作。</small>
      </label>}
      {onRemove && <Button
        variant="ghost-danger"
        disabled={disabled}
        onClick={onRemove}
      >
        移除
      </Button>}
    </div>
    <div className="team-form-grid">
      <Field label="团队职责" help="说明该成员在团队中的职责范围。">
        {(control) => <input
          {...control}
          value={worker.duty}
          disabled={disabled}
          onChange={(event) => onChange({ duty: event.target.value })}
        />}
      </Field>
      <Field label="适用任务" help="说明哪些任务或情形适合调用该成员。">
        {(control) => <input
          {...control}
          value={worker.when_to_use}
          disabled={disabled}
          onChange={(event) => onChange({ when_to_use: event.target.value })}
        />}
      </Field>
    </div>
    <Field label="协作背景" help="补充该成员执行团队任务时需要了解的背景。">
      {(control) => <textarea
        {...control}
        rows={2}
        value={worker.context_instruction}
        disabled={disabled}
        onChange={(event) => onChange({ context_instruction: event.target.value })}
      />}
    </Field>
    <div className="worker-policy">
      <fieldset>
        <legend>允许的协作方式</legend>
        {rosterKinds.map((kind) => <label key={kind}>
          <Switch
            checked={worker.allowed_kinds.includes(kind)}
            disabled={disabled}
            aria-label={rosterKindLabels[kind]}
            onChange={(next) => toggleKind(kind, next)}
          />
          {kind === "consult" && "同步咨询（结果交回负责人）"}
          {kind === "dispatch" && "异步派工（完成后由负责人综合）"}
          {kind === "handoff" && "交接处理（成员接管本次回复）"}
        </label>)}
        <small>选择允许该成员使用的协作方式，可多选。</small>
      </fieldset>
      <Field label="默认协作方式" help="未明确指定方式时优先使用，且必须包含在已选方式中。">
        {(control) => <select
          {...control}
          value={worker.default_kind}
          disabled={disabled || !worker.allowed_kinds.length}
          onChange={(event) => onChange({ default_kind: rosterKind(event.target.value) })}
        >
          <option value="">选择默认协作方式</option>
          {worker.allowed_kinds.map((kind) => <option key={kind} value={kind}>{rosterKindLabels[kind]}</option>)}
        </select>}
      </Field>
    </div>
    <Field label="结果要求" help="说明成员返回结果应满足的格式或质量要求。">
      {(control) => <textarea
        {...control}
        rows={2}
        value={worker.result_requirement}
        disabled={disabled}
        onChange={(event) => onChange({ result_requirement: event.target.value })}
      />}
    </Field>
  </fieldset>;
}
