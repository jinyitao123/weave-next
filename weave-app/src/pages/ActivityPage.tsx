import "../styles/activity.css";

import { useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Activity, Ban, BriefcaseBusiness, ChevronRight, CircleDot, RefreshCw, Send, Square, Workflow } from "lucide-react";
import { Link, useSearchParams } from "react-router-dom";
import { api, apiErrorMessage, type ForkRunInput, type ForkStreamEvent, type Job, type ResumeRunInput, type ResumeStreamEvent, type RunCheckpoint, type RunState, type RunSummary, type TaskGroup, type TaskGroupDetail, type ToolCallRecord } from "../api";
import { MarkdownText } from "../features/conversation/MarkdownText";
import { Badge } from "../ui/Badge";
import { Button } from "../ui/Button";
import { Card } from "../ui/Card";
import { Field } from "../ui/Field";
import { ErrorNotice, LoadingView } from "../ui/StatusViews";
import { useWorkspace } from "../workspace/useWorkspace";
import { executionStepLabel, isTechnicalIdentifier, stopReasonLabel, usageLabel } from "../workspace/labels";
import { toolDisplayName } from "../features/conversation/executionToolAction";

type EvidenceKind = "group" | "job" | "run";
type RunEvidenceErrors = Partial<Record<"trace" | "state" | "checkpoints", string>>;

const statusLabels: Record<string, string> = {
  active: "执行中", resolving: "正在汇总", resolved: "已完成",
  queued: "排队中", dispatched: "已分派", running: "运行中", cancel_requested: "正在取消",
  completed: "已完成", failed: "失败", cancelled: "已取消", superseded: "已取代", cut: "已裁切", timed_out: "超时",
};
const cancellable: Record<string, true> = { queued: true, dispatched: true, running: true, cancel_requested: true };

const jobSourceLabels: Record<string, string> = {
  chat: "对话", schedule: "定时任务", api: "API 调用", event: "业务事件",
};

const jobKindLabels: Record<string, string> = {
  agent: "Agent 任务",
  workflow: "工作流任务",
  team_workflow: "团队工作流任务",
  sub_agent: "委托任务",
};

function statusText(status: string) {
  return statusLabels[status] || "状态未知";
}

function statusTone(status: string): "neutral" | "accent" | "success" | "warning" | "danger" {
  if (["completed", "resolved"].includes(status)) return "success";
  if (["failed", "cancelled", "timed_out"].includes(status)) return "danger";
  if (["queued", "dispatched", "cancel_requested", "resolving"].includes(status)) return "warning";
  if (["active", "running"].includes(status)) return "accent";
  return "neutral";
}

function formatTime(value?: string) {
  if (!value) return "时间未记录";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat("zh-CN", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }).format(date);
}

function formatDuration(durationMs?: number) {
  if (durationMs === undefined || durationMs < 0) return "未记录";
  if (durationMs < 1000) return `${durationMs} ms`;
  const seconds = durationMs / 1000;
  if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)} 秒`;
  const minutes = Math.floor(seconds / 60);
  return `${minutes} 分 ${Math.round(seconds % 60)} 秒`;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function stringField(record: Record<string, unknown>, key: string) {
  return typeof record[key] === "string" ? record[key] as string : "";
}

function evidenceError(result: PromiseRejectedResult) {
  return apiErrorMessage(result.reason);
}

function groupTitle(originalRequest?: string) {
  return originalRequest && !isTechnicalIdentifier(originalRequest) ? originalRequest : "未命名任务组";
}

function attributionText(attribution?: string) {
  if (attribution === "project_attributed") return "已归属项目";
  if (attribution === "legacy_unattributed") return "历史未归属";
  return "归属状态未知";
}

function jobTitle(job: Job) {
  if (job.agent && !isTechnicalIdentifier(job.agent)) return job.agent;
  if (job.kind) return jobKindLabels[job.kind] || "任务";
  return "任务";
}

function runTitle(run: RunSummary) {
  if (run.agent && !isTechnicalIdentifier(run.agent)) return run.agent;
  if (run.step) { const step = executionStepLabel(run.step); return step.mapped ? step.label : "一次运行"; }
  return "一次运行";
}

export function ActivityPage() {
  const workspace = useWorkspace();
  const [searchParams, setSearchParams] = useSearchParams();
  const projectId = searchParams.get("project") || "";
  const selectedKind = (searchParams.get("kind") as EvidenceKind | null) || null;
  const selectedId = searchParams.get("id") || "";
  const [groups, setGroups] = useState<TaskGroup[]>([]);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [detail, setDetail] = useState<unknown>(null);
  const [trace, setTrace] = useState<unknown[] | null>(null);
  const [state, setState] = useState<RunState | null>(null);
  const [checkpoints, setCheckpoints] = useState<RunCheckpoint[] | null>(null);
  const [runEvidenceErrors, setRunEvidenceErrors] = useState<RunEvidenceErrors>({});
  const [loading, setLoading] = useState(true);
  const [detailLoading, setDetailLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [detailError, setDetailError] = useState<string | null>(null);
  const [cancelling, setCancelling] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [groupResponse, jobResponse, runResponse] = await Promise.all([
        api.listTaskGroups(projectId || undefined, undefined, ["active", "resolving", "resolved"]),
        api.listJobs(projectId || undefined),
        api.listRuns(projectId ? { projectId } : {}),
      ]);
      setGroups(groupResponse.groups);
      setJobs(jobResponse.jobs);
      setRuns(runResponse.runs);
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setLoading(false);
    }
  }, [projectId]);

  useEffect(() => { void refresh(); }, [refresh, workspace.invalidationVersion]);

  const selectedRun = selectedKind === "run" ? runs.find(({ run_id }) => run_id === selectedId) : undefined;
  const selectedRunAgent = selectedRun?.agent;

  useEffect(() => {
    if (!selectedKind || !selectedId) {
      setDetail(null); setTrace(null); setState(null); setCheckpoints(null); setRunEvidenceErrors({}); setDetailError(null); return;
    }
    const controller = new AbortController();
    setDetailLoading(true); setDetailError(null); setTrace(null); setState(null); setCheckpoints(null); setRunEvidenceErrors({});
    void (async () => {
      try {
        if (selectedKind === "group") setDetail(await api.getTaskGroup(selectedId, controller.signal));
        if (selectedKind === "job") setDetail(await api.getJob(selectedId, controller.signal));
        if (selectedKind === "run") {
          const runAgent = selectedRunAgent;
          const record = await api.getRun(selectedId, controller.signal);
          setDetail(record);
          const [traceResult, stateResult, checkpointResult] = await Promise.allSettled([
            api.getRunTrace(selectedId, controller.signal),
            runAgent ? api.getRunState(selectedId, runAgent, controller.signal) : Promise.resolve(null),
            runAgent ? api.getRunCheckpoints(selectedId, runAgent, controller.signal) : Promise.resolve(null),
          ]);
          if (traceResult.status === "fulfilled") setTrace(traceResult.value);
          if (stateResult.status === "fulfilled") setState(stateResult.value);
          if (checkpointResult.status === "fulfilled") setCheckpoints(checkpointResult.value);
          setRunEvidenceErrors({
            ...(traceResult.status === "rejected" ? { trace: evidenceError(traceResult) } : {}),
            ...(stateResult.status === "rejected" ? { state: evidenceError(stateResult) } : {}),
            ...(checkpointResult.status === "rejected" ? { checkpoints: evidenceError(checkpointResult) } : {}),
          });
        }
      } catch (requestError) {
        if (!controller.signal.aborted) setDetailError(apiErrorMessage(requestError));
      } finally {
        if (!controller.signal.aborted) setDetailLoading(false);
      }
    })();
    return () => controller.abort();
  }, [selectedId, selectedKind, selectedRunAgent]);

  const refreshRunEvidence = useCallback(async (runId: string, agent: string) => {
    const [runResponse, record] = await Promise.all([
      api.listRuns(projectId ? { projectId } : {}),
      api.getRun(runId),
    ]);
    const [traceResult, stateResult, checkpointResult] = await Promise.allSettled([
      api.getRunTrace(runId),
      api.getRunState(runId, agent),
      api.getRunCheckpoints(runId, agent),
    ]);
    setRuns(runResponse.runs);
    setDetail(record);
    setTrace(traceResult.status === "fulfilled" ? traceResult.value : null);
    setState(stateResult.status === "fulfilled" ? stateResult.value : null);
    setCheckpoints(checkpointResult.status === "fulfilled" ? checkpointResult.value : null);
    setRunEvidenceErrors({
      ...(traceResult.status === "rejected" ? { trace: evidenceError(traceResult) } : {}),
      ...(stateResult.status === "rejected" ? { state: evidenceError(stateResult) } : {}),
      ...(checkpointResult.status === "rejected" ? { checkpoints: evidenceError(checkpointResult) } : {}),
    });
    if (runId !== selectedId) {
      const next = new URLSearchParams(searchParams);
      next.set("kind", "run");
      next.set("id", runId);
      setSearchParams(next);
    }
  }, [projectId, searchParams, selectedId, setSearchParams]);

  const visibleJobs = jobs;
  const selectedGroup = selectedKind === "group" ? groups.find((group) => group.id === selectedId) : undefined;
  const projectNames = new Map(workspace.projects.map((project) => [project.id, project.name]));

  function select(kind: EvidenceKind, id: string) {
    const next = new URLSearchParams(searchParams);
    next.set("kind", kind); next.set("id", id); setSearchParams(next);
  }

  async function cancelSelected() {
    if (selectedKind !== "job" || !selectedId) return;
    setCancelling(true); setDetailError(null);
    try {
      await api.cancelJob(selectedId);
      setDetail(await api.getJob(selectedId));
      await refresh();
    } catch (requestError) {
      setDetailError(apiErrorMessage(requestError));
    } finally {
      setCancelling(false);
    }
  }

  return <div className="page activity-page">
    <div className="activity-toolbar">
      <Field label="项目范围">
        {(control) => <select {...control} value={projectId} onChange={(event) => { const next = new URLSearchParams(); if (event.target.value) next.set("project", event.target.value); setSearchParams(next); }}><option value="">全部可见项目</option>{workspace.projects.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}</select>}
      </Field>
    </div>
    {error && <ErrorNotice message={error} onRetry={() => void refresh()} />}
    {loading && !groups.length && !jobs.length && !runs.length ? <LoadingView label="正在读取执行证据" /> : <div className="activity-layout">
      <div className="activity-master">
        <EvidenceSection icon={CircleDot} title="运行记录" count={runs.length}>{runs.map((run) => <EvidenceRow key={run.run_id} active={selectedKind === "run" && selectedId === run.run_id} title={runTitle(run)} status={run.status} meta={`${projectNames.get(run.project_id || "") || (run.attribution === "project_attributed" ? "项目名称不可见" : "历史未归属")} · ${formatTime(run.started_at)}`} onClick={() => select("run", run.run_id)} />)}</EvidenceSection>
        {groups.length > 0 && <EvidenceSection icon={Workflow} title="任务组" count={groups.length}>{groups.map((group) => <EvidenceRow key={group.id} active={selectedKind === "group" && selectedId === group.id} title={groupTitle(group.original_request)} status={group.status} meta={`${group.project_id ? projectNames.get(group.project_id) || "项目名称不可见" : "项目归属未记录"} · ${group.legs.length} 个执行分支 · ${formatTime(group.updated_at)}`} onClick={() => select("group", group.id)} />)}</EvidenceSection>}
        {visibleJobs.length > 0 && <EvidenceSection icon={BriefcaseBusiness} title="任务" count={visibleJobs.length}>{visibleJobs.map((job) => <EvidenceRow key={job.id} active={selectedKind === "job" && selectedId === job.id} title={jobTitle(job)} status={job.status} meta={`${job.kind || "类型未记录"} · ${job.project_id ? projectNames.get(job.project_id) || "项目名称不可见" : "项目归属未记录"} · ${formatTime(job.updated_at)}`} onClick={() => select("job", job.id)} />)}</EvidenceSection>}
      </div>
      <aside className="activity-detail">
        {!selectedId ? <div className="activity-detail__empty"><Activity size={24} /><strong>选择一项证据</strong><p>查看持久详情、入口和可用操作。</p></div> : detailLoading ? <LoadingView label="正在读取详情" /> : <>
          {detailError && <ErrorNotice message={detailError} />}
          {selectedKind === "group" && detail && <TaskGroupEvidence detail={detail as TaskGroupDetail} projectId={selectedGroup?.project_id} onSelect={select} />}
          {selectedKind === "job" && detail && <JobEvidence job={detail as Job} projectName={projectNames.get((detail as Job).project_id || "")} onSelect={select} onCancel={() => void cancelSelected()} cancelling={cancelling} />}
          {selectedKind === "run" && detail && <RunEvidence key={selectedId} runId={selectedId} summary={selectedRun} projectName={projectNames.get(selectedRun?.project_id || "")} detail={detail} trace={trace} state={state} checkpoints={checkpoints} evidenceErrors={runEvidenceErrors} onRefresh={refreshRunEvidence} />}
        </>}
      </aside>
    </div>}
  </div>;
}

function EvidenceSection({ icon: Icon, title, count, children }: { icon: typeof Activity; title: string; count: number; children: ReactNode }) {
  return <section className="evidence-section"><header><Icon size={16} /><strong>{title}</strong><Badge>{count} 条</Badge></header><div>{children || <p className="activity-empty">暂无记录</p>}</div></section>;
}
function EvidenceRow({ active, title, status, meta, onClick }: { active: boolean; title: string; status: string; meta: string; onClick(): void }) {
  return <div className={active ? "evidence-row evidence-row--active" : "evidence-row"}><Button variant="ghost" size="small" onClick={onClick}><span className="evidence-row__copy"><strong>{title}</strong><span className="evidence-row__meta"><Badge tone={statusTone(status)}>{statusText(status)}</Badge><small>{meta}</small></span></span><ChevronRight size={16} /></Button></div>;
}
function TaskGroupEvidence({ detail, projectId, onSelect }: { detail: TaskGroupDetail; projectId?: string; onSelect(kind: EvidenceKind, id: string): void }) {
  return <div className="evidence-detail"><Badge tone={statusTone(detail.status)}>{statusText(detail.status)}</Badge><h2>{groupTitle(detail.original_request)}</h2><p>需 {detail.quorum} 个分支完成 · {formatTime(detail.updated_at)}</p>{detail.conversation_id && projectId ? <Link className="text-link" to={`/project/${encodeURIComponent(projectId)}/conversations/${encodeURIComponent(detail.conversation_id)}`}>进入对应会话</Link> : detail.conversation_id ? <p className="readonly-note">暂时无法定位该会话所属的项目。</p> : null}<TechnicalInfo rows={[['任务组 ID', detail.id], ['Avatar Agent', detail.avatar_agent], ['原始请求', isTechnicalIdentifier(detail.original_request) ? detail.original_request : ''], ['会话 ID', detail.conversation_id]]} /><h3>执行分支</h3><div className="activity-leg-list">{detail.legs.map((leg, index) => <article key={`${leg.job_id}:${index}`}><div><strong>{isTechnicalIdentifier(leg.agent) ? `执行分支 ${index + 1}` : leg.agent}</strong><Badge tone={statusTone(leg.status)}>{statusText(leg.status)}</Badge></div>{leg.error && <p className="danger-text">{leg.error}</p>}{leg.result_summary && <p>{leg.result_summary}</p>}<TechnicalInfo rows={[['Agent', leg.agent], ['任务 ID', leg.job_id], ['运行 ID', leg.run_id]]} /><footer>{leg.job_id && <Button variant="ghost" size="small" onClick={() => onSelect("job", leg.job_id)}>查看任务</Button>}{leg.run_id && <Button variant="ghost" size="small" onClick={() => onSelect("run", leg.run_id)}>查看运行</Button>}</footer></article>)}</div></div>;
}
function JobEvidence({ job, projectName, onSelect, onCancel, cancelling }: { job: Job; projectName?: string; onSelect(kind: EvidenceKind, id: string): void; onCancel(): void; cancelling: boolean }) {
  const runId = job.run_id;
  const taskGroupId = job.task_group_id;
  return <div className="evidence-detail"><Badge tone={statusTone(job.status)}>{statusText(job.status)}</Badge><h2>{jobTitle(job)}</h2><p>{jobKindLabels[job.kind] || job.kind || "任务类型未记录"} · {projectName || (job.project_id ? "项目名称不可见" : "项目归属未记录")} · 来源 {jobSourceLabels[job.source || ""] || "其他"} · {formatTime(job.created_at)}</p>{job.error && <ErrorNotice message={job.error} />}<TechnicalInfo rows={[['任务 ID', job.id], ['Agent', job.agent], ['项目 ID', job.project_id || '未记录'], ['运行时 ID', job.runtime_id || '未记录']]} /><div className="activity-actions">{runId && <Button onClick={() => onSelect("run", runId)}>查看运行记录</Button>}{taskGroupId && <Button onClick={() => onSelect("group", taskGroupId)}>查看任务组</Button>}{cancellable[job.status] && <Button variant="danger" loading={cancelling} onClick={onCancel}><Ban size={14} />取消任务</Button>}</div></div>;
}

function TechnicalInfo({ rows }: { rows: Array<[string, string]> }) {
  return <details className="activity-technical"><summary>技术信息</summary><dl>{rows.filter(([, value]) => value).map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl></details>;
}
interface StreamAttempt<Request> {
  request: Request;
  phase: "streaming" | "failed" | "stopped" | "terminal";
  output: string;
  activity: string;
  steps: string[];
  tools: ToolCallRecord[];
  error?: string;
  terminal?: { type: "done" | "yield"; data: Record<string, unknown> };
}

type ResumeAttempt = StreamAttempt<ResumeRunInput>;
interface ForkAttempt extends StreamAttempt<ForkRunInput> {
  parentRunId: string;
}

function updateToolResult(tools: ToolCallRecord[], name: string, status: string, result: string): ToolCallRecord[] {
  let index = -1;
  for (let toolIndex = tools.length - 1; toolIndex >= 0; toolIndex -= 1) {
    if (tools[toolIndex].name === name && tools[toolIndex].status === "running") {
      index = toolIndex;
      break;
    }
  }
  if (index < 0) return [...tools, { name, status, result }];
  return tools.map((tool, toolIndex) => toolIndex === index ? { ...tool, status, result } : tool);
}

function applyStreamEvent<Attempt extends StreamAttempt<unknown>>(event: ResumeStreamEvent | ForkStreamEvent, setAttempt: (updater: (current: Attempt | null) => Attempt | null) => void) {
  if (event.type === "step_start" && typeof event.data.step === "string") {
    const stepLabel = executionStepLabel(event.data.step).label;
    setAttempt((current) => current ? { ...current, activity: stepLabel, steps: [...current.steps, stepLabel] } : current);
  }
  if (event.type === "step_end" && typeof event.data.step === "string") {
    const stepLabel = executionStepLabel(event.data.step).label;
    setAttempt((current) => current ? { ...current, activity: `${stepLabel}已结束` } : current);
  }
  if (event.type === "chunk" && typeof event.data.content === "string") {
    setAttempt((current) => current ? { ...current, output: current.output + event.data.content, activity: "正在接收输出" } : current);
  }
  if (event.type === "tool_call") {
    const name = typeof event.data.name === "string" ? event.data.name : "";
    const callId = typeof event.data.call_id === "string" ? event.data.call_id : undefined;
    setAttempt((current) => current ? { ...current, activity: `正在调用${toolDisplayName(name)}`, tools: [...current.tools, { call_id: callId, name, status: "running" }] } : current);
  }
  if (event.type === "tool_result") {
    const name = typeof event.data.name === "string" ? event.data.name : "";
    const status = String(event.data.status || "success");
    const content = String(event.data.content || "");
    setAttempt((current) => current ? { ...current, activity: `${toolDisplayName(name)} · ${status === "success" ? "成功" : "失败"}`, tools: updateToolResult(current.tools, name, status, content) } : current);
  }
}

function StreamAttemptView({ attempt }: { attempt: StreamAttempt<unknown> }) {
  return <div className="run-resume__stream" aria-live="polite"><div className="run-resume__status"><CircleDot size={14} /><strong>{attempt.activity}</strong></div>{attempt.error && <p className="run-resume__error" role="alert">{attempt.error}</p>}{attempt.steps.length > 0 && <div className="run-resume__steps" aria-label="执行步骤">{attempt.steps.map((step, index) => <span key={`${step}:${index}`}>{step}</span>)}</div>}{attempt.tools.length > 0 && <div className="run-resume__tools" aria-label="工具活动">{attempt.tools.map((tool, index) => <span key={`${tool.call_id || tool.name}:${index}`}><b>{toolDisplayName(tool.name)}</b><Badge tone={tool.status === "success" ? "success" : tool.status === "running" ? "accent" : "danger"}>{tool.status === "success" ? "成功" : tool.status === "running" ? "执行中" : "失败"}</Badge></span>)}</div>}{attempt.output && <pre className="run-resume__output">{attempt.output}</pre>}{attempt.terminal && <section className="run-resume__terminal"><header><strong>{attempt.terminal.type === "done" ? "运行已结束" : "等待输入"}</strong></header><details><summary>技术细节</summary><pre>{JSON.stringify(attempt.terminal.data, null, 2)}</pre></details></section>}</div>;
}

function RunEvidence({ runId, summary, projectName, detail, trace, state, checkpoints, evidenceErrors, onRefresh }: { runId: string; summary?: RunSummary; projectName?: string; detail: unknown; trace: unknown[] | null; state: RunState | null; checkpoints: RunCheckpoint[] | null; evidenceErrors: RunEvidenceErrors; onRefresh(runId: string, agent: string): Promise<void> }) {
  const attributed = summary?.attribution === "project_attributed";
  const detailRecord = isRecord(detail) ? detail : {};
  const requestSummary = stringField(detailRecord, "summary");
  const output = stringField(detailRecord, "output");
  const stateEntries = Object.entries(state?.state || {});
  const [message, setMessage] = useState("");
  const [advancedInput, setAdvancedInput] = useState("{}");
  const [inputError, setInputError] = useState<string | null>(null);
  const [attempt, setAttempt] = useState<ResumeAttempt | null>(null);
  const [forkMessage, setForkMessage] = useState("");
  const [forkAdvancedInput, setForkAdvancedInput] = useState("{}");
  const [forkInputError, setForkInputError] = useState<string | null>(null);
  const [forkAttempt, setForkAttempt] = useState<ForkAttempt | null>(null);
  const positiveCheckpoints = (checkpoints || []).filter((checkpoint) => checkpoint.seq > 0).sort((left, right) => left.seq - right.seq);
  const [forkSeq, setForkSeq] = useState<number | null>(null);
  const selectedForkSeq = positiveCheckpoints.some(({ seq }) => seq === forkSeq) ? forkSeq : positiveCheckpoints.at(-1)?.seq ?? null;
  const resumeAbortRef = useRef<AbortController | null>(null);
  const forkAbortRef = useRef<AbortController | null>(null);
  const resumable = !!state && !!summary?.agent && !!state.yield_phase.trim();
  const forkable = !!summary?.agent && positiveCheckpoints.length > 0;

  useEffect(() => () => { resumeAbortRef.current?.abort(); forkAbortRef.current?.abort(); }, []);

  async function executeResume(request: ResumeRunInput) {
    const controller = new AbortController();
    resumeAbortRef.current = controller;
    setInputError(null);
    setAttempt({ request, phase: "streaming", output: "", activity: "等待服务端响应", tools: [], steps: [] });
    let terminalEvent: ResumeAttempt["terminal"];
    try {
      const result = await api.streamResume(request, (event: ResumeStreamEvent) => {
        applyStreamEvent(event, setAttempt);
        if (event.type === "done" || event.type === "yield") terminalEvent = { type: event.type, data: event.data };
      }, controller.signal);
      const terminal: NonNullable<ResumeAttempt["terminal"]> = terminalEvent || { type: "done", data: { ...result.terminal } };
      const terminalError = typeof terminal.data.error === "string" ? terminal.data.error : "";
      setAttempt((current) => current ? { ...current, phase: terminalError ? "failed" : "terminal", activity: terminal.type === "done" ? "运行已结束" : "已转入等待输入", terminal, error: terminalError || undefined } : current);
      const returnedRunId = typeof terminal.data.run_id === "string" && terminal.data.run_id ? terminal.data.run_id : request.run_id;
      try {
        await onRefresh(returnedRunId, request.agent);
      } catch (refreshError) {
        setAttempt((current) => current ? { ...current, phase: "failed", error: `服务端已返回 ${terminal.type}，但刷新运行证据失败：${apiErrorMessage(refreshError)}` } : current);
      }
    } catch (requestError) {
      if (controller.signal.aborted) setAttempt((current) => current ? { ...current, phase: "stopped", activity: "已停止在此页面等待", error: "仅停止了页面等待；后端执行未被声明为已取消。可使用同一请求重试读取结果。" } : current);
      else setAttempt((current) => current ? { ...current, phase: "failed", activity: "继续执行失败", error: apiErrorMessage(requestError) } : current);
    } finally {
      if (resumeAbortRef.current === controller) resumeAbortRef.current = null;
    }
  }

  async function executeFork(parentRunId: string, request: ForkRunInput) {
    const controller = new AbortController();
    forkAbortRef.current = controller;
    setForkInputError(null);
    setForkAttempt({ parentRunId, request, phase: "streaming", output: "", activity: "等待服务端创建分叉", tools: [], steps: [] });
    let terminalEvent: ForkAttempt["terminal"];
    try {
      const result = await api.streamForkRun(parentRunId, request, (event: ForkStreamEvent) => {
        applyStreamEvent(event, setForkAttempt);
        if (event.type === "done" || event.type === "yield") terminalEvent = { type: event.type, data: event.data };
      }, controller.signal);
      const terminal: NonNullable<ForkAttempt["terminal"]> = terminalEvent || { type: "done", data: { ...result.terminal } };
      const terminalError = typeof terminal.data.error === "string" ? terminal.data.error : "";
      setForkAttempt((current) => current ? { ...current, phase: terminalError ? "failed" : "terminal", activity: terminal.type === "done" ? "运行已结束" : "已转入等待输入", terminal, error: terminalError || undefined } : current);
      const childRunId = typeof terminal.data.run_id === "string" ? terminal.data.run_id : "";
      if (childRunId) {
        try {
          await onRefresh(childRunId, request.agent);
        } catch (refreshError) {
          setForkAttempt((current) => current ? { ...current, phase: "failed", error: `服务端已返回 ${terminal.type}，但刷新子运行证据失败：${apiErrorMessage(refreshError)}` } : current);
        }
      }
    } catch (requestError) {
      if (controller.signal.aborted) setForkAttempt((current) => current ? { ...current, phase: "stopped", activity: "已停止在此页面等待", error: "仅停止了页面等待；后端分叉执行未被声明为已取消。可使用相同父运行、检查点、Agent 和输入重试等待。" } : current);
      else setForkAttempt((current) => current ? { ...current, phase: "failed", activity: "创建分叉失败", error: apiErrorMessage(requestError) } : current);
    } finally {
      if (forkAbortRef.current === controller) forkAbortRef.current = null;
    }
  }

  function parseObject(raw: string, setError: (value: string | null) => void): Record<string, unknown> | null {
    let parsed: unknown;
    try { parsed = JSON.parse(raw); } catch { setError("高级输入不是有效的 JSON。请输入一个 JSON 对象。"); return null; }
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) { setError("高级输入必须是 JSON 对象，不能是数组或 null。"); return null; }
    return { ...(parsed as Record<string, unknown>) };
  }

  function submitResume(event: FormEvent) {
    event.preventDefault();
    if (!resumable || !summary?.agent) return;
    const input = parseObject(advancedInput, setInputError);
    if (!input) return;
    if (message.trim()) input.message = message.trim();
    void executeResume({ run_id: runId, agent: summary.agent, input, stream: true });
  }

  function submitFork(event: FormEvent) {
    event.preventDefault();
    if (!forkable || !summary?.agent || selectedForkSeq === null) return;
    const input = parseObject(forkAdvancedInput, setForkInputError);
    if (!input) return;
    if (forkMessage.trim()) input.message = forkMessage.trim();
    void executeFork(runId, { seq: selectedForkSeq, agent: summary.agent, input, stream: true });
  }

  return <div className="evidence-detail">
    <div className="activity-badges"><Badge tone={attributed ? "success" : "neutral"}>{attributionText(summary?.attribution)}</Badge>{summary && <Badge tone={statusTone(summary.status)}>{statusText(summary.status)}</Badge>}</div>
    <h2>{summary ? runTitle(summary) : "运行详情"}</h2>
    <p>{summary ? `${projectName || (attributed ? "项目名称不可见" : "历史未归属")} · ${formatTime(summary.started_at)}` : "服务端运行详情"}</p>
    {summary && <dl className="run-facts">
      <div><dt>耗时</dt><dd>{formatDuration(summary.duration_ms)}</dd></div>
      <div><dt>模型用量</dt><dd>{usageLabel(summary.tokens_in, summary.tokens_out)}</dd></div>
      <div><dt>结束方式</dt><dd>{stopReasonLabel(summary.stop_reason)}</dd></div>
    </dl>}
    {requestSummary && <section className="run-readable"><h3>运行摘要</h3><MarkdownText text={requestSummary} /></section>}
    {output && output !== requestSummary && <section className="run-readable"><h3>运行输出</h3><MarkdownText text={output} /></section>}
    {trace && trace.length > 0 && <section className="run-readable"><h3>调用轨迹</h3><ol className="run-trace-list">{trace.map((entry, index) => {
      const record = isRecord(entry) ? entry : {};
      const label = stringField(record, "step_name") || stringField(record, "name") || stringField(record, "type") || `轨迹 ${index + 1}`;
      const status = stringField(record, "status");
      const time = stringField(record, "timestamp") || stringField(record, "created_at");
      return <li key={`${label}:${index}`}><strong>{label}</strong>{(status || time) && <small>{[status && statusText(status), time && formatTime(time)].filter(Boolean).join(" · ")}</small>}</li>;
    })}</ol></section>}
    {stateEntries.length > 0 && <section className="run-readable"><h3>业务状态</h3><dl className="run-state-list">{stateEntries.map(([key, value]) => <div key={key}><dt>{key}</dt><dd>{typeof value === "string" ? <MarkdownText text={value} /> : JSON.stringify(value)}</dd></div>)}</dl></section>}
    {positiveCheckpoints.length > 0 && <section className="run-readable"><h3>检查点</h3><ol className="run-checkpoint-list">{positiveCheckpoints.map((checkpoint) => <li key={checkpoint.seq}><Badge tone="neutral">#{checkpoint.seq}</Badge><span><strong>{checkpoint.last_step || "步骤未记录"}</strong><small>{formatTime(checkpoint.saved_at)}</small></span></li>)}</ol></section>}
    {Object.keys(evidenceErrors).length > 0 && <section className="run-evidence-errors" aria-label="部分取证不可用">{evidenceErrors.trace && <p>调用轨迹读取失败：{evidenceErrors.trace}</p>}{evidenceErrors.state && <p>业务状态读取失败：{evidenceErrors.state}</p>}{evidenceErrors.checkpoints && <p>检查点读取失败：{evidenceErrors.checkpoints}</p>}</section>}
    <TechnicalInfo rows={[['运行 ID', runId], ['Agent', summary?.agent || '未记录'], ['项目 ID', summary?.project_id || '未记录'], ['父运行 ID', summary?.parent_run_id || '未记录']]} />
    {(resumable || attempt) && <Card className="run-resume" header={<div className="run-card__heading"><div><h3 id="run-resume-title">{resumable ? "继续执行" : "继续执行结果"}</h3><p>{resumable ? "此运行正在等待输入。继续后，以服务端终止事件和刷新后的运行证据为准。" : attempt?.terminal ? "当前操作已收到服务端终止事件；下方事实来自该事件和刷新后的运行证据。" : "本页面已停止等待或继续执行失败；后端状态仍以刷新后的运行证据为准。"}</p></div>{resumable && <Badge tone="warning">等待输入</Badge>}</div>}>
      {resumable && <form onSubmit={submitResume}>
        <Field label="继续说明">{(control) => <textarea {...control} rows={3} value={message} disabled={attempt?.phase === "streaming"} placeholder="用自然语言说明接下来要做什么" onChange={(event) => { setMessage(event.target.value); setInputError(null); }} />}</Field>
        <details className="run-resume__advanced"><summary>高级输入（可选）</summary><Field label="JSON 对象" help="用于需要结构化字段的工作流；继续说明非空时会合并为 message。" error={inputError}>{(control) => <textarea {...control} className="code-input" rows={6} value={advancedInput} disabled={attempt?.phase === "streaming"} spellCheck={false} onChange={(event) => { setAdvancedInput(event.target.value); setInputError(null); }} />}</Field></details>
        <div className="run-resume__actions">{attempt?.phase === "streaming" ? <Button onClick={() => resumeAbortRef.current?.abort()}><Square size={14} /> 停止等待</Button> : <Button variant="primary" type="submit"><Send size={14} /> 继续执行</Button>}{attempt && (attempt.phase === "failed" || attempt.phase === "stopped") && <Button onClick={() => void executeResume(attempt.request)}><RefreshCw size={14} /> 使用同一输入重试</Button>}</div>
      </form>}
      {attempt && <StreamAttemptView attempt={attempt} />}
    </Card>}
    {(forkable || forkAttempt) && <Card className="run-fork" header={<div className="run-card__heading"><div><h3 id="run-fork-title">创建分叉</h3><p>{forkAttempt?.terminal ? "服务端已返回分叉终止事实；子运行证据已按返回的 run_id 刷新。" : "从真实历史检查点创建新运行。执行仅通过事件流发起。"}</p></div>{selectedForkSeq !== null && <Badge tone="accent">seq {selectedForkSeq}</Badge>}</div>}>
      {forkable && <form onSubmit={submitFork}>
        <Field label="检查点">{(control) => <select {...control} value={selectedForkSeq ?? ""} disabled={forkAttempt?.phase === "streaming"} onChange={(event) => setForkSeq(Number(event.target.value))}>{positiveCheckpoints.map((checkpoint) => <option key={checkpoint.seq} value={checkpoint.seq}>#{checkpoint.seq}{checkpoint.last_step ? ` · ${checkpoint.last_step}` : ""}{checkpoint.saved_at ? ` · ${checkpoint.saved_at}` : ""}</option>)}</select>}</Field>
        <Field label="分叉说明">{(control) => <textarea {...control} rows={3} value={forkMessage} disabled={forkAttempt?.phase === "streaming"} placeholder="用自然语言说明分叉后要执行什么" onChange={(event) => { setForkMessage(event.target.value); setForkInputError(null); }} />}</Field>
        <details className="run-resume__advanced"><summary>高级输入（可选）</summary><Field label="JSON 对象" help="必须是 JSON 对象；分叉说明非空时会合并为 input.message。" error={forkInputError}>{(control) => <textarea {...control} className="code-input" rows={6} value={forkAdvancedInput} disabled={forkAttempt?.phase === "streaming"} spellCheck={false} onChange={(event) => { setForkAdvancedInput(event.target.value); setForkInputError(null); }} />}</Field></details>
        <div className="run-resume__actions">{forkAttempt?.phase === "streaming" ? <Button onClick={() => forkAbortRef.current?.abort()}><Square size={14} /> 停止等待</Button> : <Button variant="primary" type="submit"><Workflow size={14} /> 创建分叉</Button>}{forkAttempt && (forkAttempt.phase === "failed" || forkAttempt.phase === "stopped") && <Button onClick={() => void executeFork(forkAttempt.parentRunId, forkAttempt.request)}><RefreshCw size={14} /> 使用同一输入重试</Button>}</div>
      </form>}
      {forkAttempt && <StreamAttemptView attempt={forkAttempt} />}
    </Card>}
    <EvidenceJSON title="原始运行数据" value={detail} />
    {trace !== null && <EvidenceJSON title="原始调用轨迹" value={trace} />}
    {state !== null && <EvidenceJSON title="原始业务状态" value={state} />}
    {checkpoints !== null && <EvidenceJSON title="原始检查点" value={checkpoints} />}
  </div>;
}
function EvidenceJSON({ title, value }: { title: string; value: unknown }) { return <details className="activity-json"><summary>{title}</summary><pre>{JSON.stringify(value, null, 2)}</pre></details>; }
