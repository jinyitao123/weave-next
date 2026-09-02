import { useEffect, useState } from "react";
import { CheckCircle2, FileCode2, FileText, LoaderCircle, MessagesSquare, Paperclip, Users } from "lucide-react";
import { api, apiErrorMessage, type AssistantExecutionMetadata, type AssistantMessageMetadata, type BuildRunOperationStep, type BuildRunProgress, type BuildRunSummary, type CandidateEvaluation, type ChatRequest, type ExecutionSegment, type Message, type MessageAttachment, type RuntimeAssignment, type TeamArchitectReport, type TeamTemplateDraft, type ToolCallRecord } from "../../api";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { ExecutionProcess } from "./ExecutionProcess";
import { MarkdownText } from "./MarkdownText";
import { MessageContent } from "./MessageContent";
import { RuntimeAssignmentNotice } from "./RuntimeAssignmentNotice";
import { agentRoleLabel, displayName, isTechnicalIdentifier } from "../../workspace/labels";
import { buildRunPhaseLabel } from "./buildRunPhase";
import { blueprintFooterPresentation, buildOperationFailureDescription } from "./blueprintProposalFooter";
import { extractedAnswerSegmentID, finalAnswerFromExecutionSegments, isBlueprintWaitingPlaceholder } from "./conversationMessageOutcome";
import { terminalBlockedFollowupNote, terminalBuildStatusDescription, terminalOutcomePresentation, terminalTurnStateTitle } from "./terminalOutcome";

export interface PendingTurn {
  clientRequestId: string;
  content: string;
  state: "sending" | "failed";
  error?: string;
  /** 用户主动中止（AbortError）时为 true，区别于真正的执行失败。 */
  interrupted?: boolean;
  interruptedReason?: "stopped" | "disconnected";
  streamedContent: string;
  activity?: string;
  toolCalls: ToolCallRecord[];
  segments: ExecutionSegment[];
  agentOutputs: Record<string, string>;
  request: ChatRequest;
  attachments: MessageAttachment[];
  runtimeAssignment?: RuntimeAssignment;
  persistedUserMessageId?: string;
  startedAt: number;
}

interface MessageActionsProps {
  message: Message;
  canCreateThread: boolean;
  threadBusy: boolean;
  creatingThread: boolean;
  promoted: boolean;
  promoteBusy: boolean;
  promoting: boolean;
  onCreateThread(messageId: string): void;
  onPromote(messageId: string): void;
  onSubmitBuildRun?(buildRunID: string, token: NonNullable<AssistantMessageMetadata["blueprint_revision_token"]>): Promise<void> | void;
  onRequestBlueprintChanges?(): void;
  onReplanBuildRun?(): void;
  onAbandonBuildRun?(): void;
}

interface ConversationMessageProps extends MessageActionsProps {
  previousUserCreatedAt?: string;
  promoteError?: string;
  teamId?: string;
  blueprintBuildRun?: BuildRunSummary;
  blueprintCardMode?: "full" | "reference";
  buildProgressInvalidationVersion?: number;
  onOpenTeam?(teamId: string): void;
}

interface PendingTurnMessagesProps {
  pending: PendingTurn;
  teamId?: string;
  hideUserMessage?: boolean;
  onRestoreDraft(): void;
  onRetry(): void;
}

function elapsedLabel(totalSeconds: number): string {
  const seconds = Math.max(0, Math.floor(totalSeconds));
  const minutes = Math.floor(seconds / 60);
  const remainingSeconds = seconds % 60;
  return minutes > 0 ? `${minutes}m ${remainingSeconds}s` : `${remainingSeconds}s`;
}

function completionLabel(assistantCreatedAt: string, previousUserCreatedAt?: string): string {
  if (!previousUserCreatedAt) return "已完成";
  const assistantTime = Date.parse(assistantCreatedAt);
  const userTime = Date.parse(previousUserCreatedAt);
  if (!Number.isFinite(assistantTime) || !Number.isFinite(userTime) || assistantTime < userTime) return "已完成";
  return `已完成 · ${elapsedLabel((assistantTime - userTime) / 1000)}`;
}

function messageTimeLabel(createdAt: string): string | null {
  const createdTime = Date.parse(createdAt);
  if (!Number.isFinite(createdTime)) return null;
  return new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit" }).format(new Date(createdTime));
}

function MessageActions({
  message,
  canCreateThread,
  threadBusy,
  creatingThread,
  promoted,
  promoteBusy,
  promoting,
  onCreateThread,
  onPromote,
  suppressPromotion = false,
}: MessageActionsProps & { suppressPromotion?: boolean }) {
  const timeLabel = messageTimeLabel(message.created_at);
  const hasInteractiveAction = canCreateThread || (message.role === "assistant" && !promoted);
  const busy = threadBusy || promoteBusy;
  return <div className={`message-actions${busy ? " message-actions--busy" : ""}`} role="group" aria-label="消息操作" tabIndex={hasInteractiveAction ? undefined : 0}>
    {canCreateThread && <Button
      variant="ghost"
      size="small"
      disabled={threadBusy}
      aria-label={creatingThread ? "正在创建讨论" : "围绕此消息讨论"}
      title={creatingThread ? "正在创建讨论" : "围绕此消息讨论"}
      onClick={() => onCreateThread(message.id)}
    >
      {creatingThread ? <LoaderCircle className="spin" size={14} aria-hidden="true" /> : <MessagesSquare size={14} aria-hidden="true" />}
    </Button>}
    {message.role === "assistant" && !suppressPromotion && (promoted ? <Badge tone="success">已存为交付物</Badge> : <Button
      variant="ghost"
      size="small"
      disabled={promoteBusy}
      aria-label={promoting ? "正在存为交付物" : "将此消息存为交付物"}
      title={promoting ? "正在存为交付物" : "存为交付物"}
      onClick={() => onPromote(message.id)}
    >
      {promoting ? <LoaderCircle className="spin" size={14} aria-hidden="true" /> : <FileText size={14} aria-hidden="true" />}
    </Button>)}
    {timeLabel && <time dateTime={message.created_at}>{timeLabel}</time>}
  </div>;
}

export function ConversationMessage({
  message,
  previousUserCreatedAt,
  promoteError,
  teamId,
  blueprintBuildRun,
  blueprintCardMode = "full",
  buildProgressInvalidationVersion = 0,
  onOpenTeam,
  ...actions
}: ConversationMessageProps) {
  const metadata = typeof message.metadata === "object" && message.metadata !== null ? message.metadata as AssistantMessageMetadata : {};
  const execution = metadata.execution?.schema_version === 1 ? metadata.execution : undefined;
  const terminalOutcome = metadata.terminal_outcome?.schema_version === 1 ? metadata.terminal_outcome : undefined;
  const structuredBlocked = terminalOutcome?.turn_state === "blocked";
  const suppressBlueprintPlaceholder = message.role === "assistant" && blueprintBuildRun && isBlueprintWaitingPlaceholder(message.content);
  const extractedAnswer = message.role === "assistant" ? finalAnswerFromExecutionSegments(message.content, metadata.execution_segments || [], !!suppressBlueprintPlaceholder || structuredBlocked, !!terminalOutcome) : "";
  const terminalAnswerSegmentID = terminalOutcome ? extractedAnswerSegmentID(metadata.execution_segments || []) : "";
  const traceSegments = extractedAnswer || terminalAnswerSegmentID
    ? (metadata.execution_segments || []).filter((segment) => segment.id !== (terminalAnswerSegmentID || extractedAnswerSegmentID(metadata.execution_segments || [])))
    : (metadata.execution_segments || []);
  const terminalTitle = terminalOutcome ? terminalTurnStateTitle(terminalOutcome.turn_state) : undefined;
  const executionTitle = terminalTitle || (message.role === "assistant" && execution && blueprintBuildRun?.status === "planning"
    ? buildRunPhaseLabel(blueprintBuildRun.status).summary
    : undefined);
  const completion = terminalTitle
    ? completionLabel(message.created_at, previousUserCreatedAt).replace("已完成", terminalTitle)
    : completionLabel(message.created_at, previousUserCreatedAt);
  const blockedFollowupNote = terminalBlockedFollowupNote(terminalOutcome, !!extractedAnswer);
  return <article className={`message message--${message.role}`}>
    {message.role === "assistant" && execution ? <ExecutionProcess execution={execution} segments={traceSegments} teamId={teamId} titleOverride={executionTitle} terminalOutcome={terminalOutcome} /> : message.role === "assistant" && <div className="message-process message-process--complete">
      <span className={`message-process__dot message-process__dot--${structuredBlocked ? "failed" : "complete"}`} aria-hidden="true" />
      <strong>{completion}</strong>
    </div>}
    <div className="message__body">
      {blockedFollowupNote && <div className="message-terminal-status-note" role="status"><small>{blockedFollowupNote}</small></div>}
      {terminalOutcome?.report && <StructuredTeamReport report={terminalOutcome.report} />}
      {message.role === "assistant" && metadata.runtime_assignment && <RuntimeAssignmentNotice assignment={metadata.runtime_assignment} />}
      {extractedAnswer && <MarkdownText text={normalizeAssistantAnswer(extractedAnswer, blueprintBuildRun)} />}
      {message.role === "assistant" && metadata.team_template_draft && <TeamTemplateDraftCard draft={metadata.team_template_draft} onOpenTeam={onOpenTeam} />}
      {message.role === "assistant" && <BlueprintProposalCard metadata={metadata} buildRun={blueprintBuildRun} mode={blueprintCardMode} invalidationVersion={buildProgressInvalidationVersion} onOpenTeam={onOpenTeam} onSubmit={actions.onSubmitBuildRun} onRequestChanges={actions.onRequestBlueprintChanges} onReplan={actions.onReplanBuildRun} onAbandon={actions.onAbandonBuildRun} />}
      {!terminalOutcome && !extractedAnswer && !suppressBlueprintPlaceholder && <MessageContent message={message} />}
      {promoteError && <div className="pending-error" role="alert"><p>{promoteError}</p></div>}
    </div>
    <MessageActions message={message} {...actions} suppressPromotion={structuredBlocked} />
  </article>;
}

function TeamTemplateDraftCard({ draft, onOpenTeam }: { draft: TeamTemplateDraft; onOpenTeam?: (teamId: string) => void }) {
  const [submitting, setSubmitting] = useState(false);
  const [buildRunID, setBuildRunID] = useState("");
  const [status, setStatus] = useState("");
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!buildRunID || status === "authorization_required" || status === "passed") return;
    const controller = new AbortController();
    let timer = 0;
    const poll = async () => {
      try {
        const progress = await api.getTeamBuildRunProgress(buildRunID, controller.signal);
        if (controller.signal.aborted) return;
        setStatus(progress.run_status);
        if (progress.run_status === "passed") {
          if (progress.final_ref?.team_id) onOpenTeam?.(progress.final_ref.team_id);
          else setError("团队已创建，但响应中缺少团队标识。请前往团队列表查看。");
          return;
        }
        if (progress.run_status === "blocked" || progress.run_status === "cancelled") {
          setError(`模板构建已${progress.run_status === "blocked" ? "暂停" : "取消"}，请查看构建记录。`);
          return;
        }
        timer = window.setTimeout(() => void poll(), 1_200);
      } catch (requestError) {
        if (!controller.signal.aborted) setError(apiErrorMessage(requestError));
      }
    };
    void poll();
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [buildRunID, onOpenTeam, status]);

  async function submit() {
    setSubmitting(true);
    setError(null);
    try {
      const outcome = await api.createTeamFromTemplate({ yaml: draft.yaml, idempotency_key: draft.idempotency_key });
      setBuildRunID(outcome.build_run_id);
      setStatus(outcome.status);
      if (outcome.status === "ready" && outcome.team_id) onOpenTeam?.(outcome.team_id);
      if (outcome.status === "authorization_required") {
        setError("模板预算或工作区额度超出自动授权范围。构建已保留，请由管理员审核后继续。");
      }
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setSubmitting(false);
    }
  }

  const complete = status === "ready" || status === "passed";
  return <section className="template-draft-card" aria-label="团队模板草稿">
    <header>
      <FileCode2 size={16} aria-hidden="true" />
      <div><strong>{draft.preview.display_name}</strong><small>{draft.preview.topology} · {draft.preview.members.length} 位成员</small></div>
      <Badge tone={complete ? "success" : status ? "accent" : "warning"}>{complete ? "已创建" : status ? "创建中" : "待审阅"}</Badge>
    </header>
    <p>{draft.preview.purpose}</p>
    <ul>{draft.preview.members.map((member) => <li key={member.name}><span>{member.display_name}</span><small>{agentRoleLabel(member.role)}</small></li>)}</ul>
    <details><summary>审阅 team.yaml</summary><pre><code>{draft.yaml}</code></pre></details>
    {error && <p className="template-draft-card__error" role="alert">{error}</p>}
    <footer>
      <small>预算上限 ${draft.preview.max_cost_usd.toFixed(2)} · 创建后状态为待评测</small>
      <Button variant="primary" size="small" loading={submitting} disabled={submitting || !!buildRunID} onClick={() => void submit()}>
        <CheckCircle2 size={14} aria-hidden="true" />
        {submitting ? "正在提交…" : buildRunID ? "已提交" : "确认并创建团队"}
      </Button>
    </footer>
  </section>;
}

function StructuredTeamReport({ report }: { report: TeamArchitectReport }) {
  return <section className="message-structured-report" aria-label="团队架构师汇报">
    <p><strong>{report.conclusion}</strong></p>
    {report.team_name && <p><small>团队</small> {report.team_name}</p>}
    {report.members.length > 0 && <ul>
      {report.members.map((member, index) => <li key={`${member.name}:${index}`}>
        <strong>{member.name}</strong> <span>{agentRoleLabel(member.role)}</span>
        <p>{member.duty}</p>
      </li>)}
    </ul>}
    <p><small>下一步</small> {report.next_action}</p>
  </section>;
}

function normalizeAssistantAnswer(answer: string, buildRun?: BuildRunSummary): string {
  if (buildRun?.status !== "planning") return answer;
  return answer
    .replace(/回复[“"]可以[”"]即提交蓝图编译执行[。.]?/g, "点击卡片上的「继续构建」开始。")
    .replace(/回复[“"]可以[”"]继续执行[。.]?/g, "点击卡片上的「继续构建」开始。");
}

function BlueprintProposalCard({ metadata, buildRun, mode = "full", invalidationVersion, onOpenTeam, onSubmit, onRequestChanges, onReplan, onAbandon }: { metadata: AssistantMessageMetadata; buildRun?: BuildRunSummary; mode?: "full" | "reference"; invalidationVersion: number; onOpenTeam?: (teamId: string) => void; onSubmit?: MessageActionsProps["onSubmitBuildRun"]; onRequestChanges?: MessageActionsProps["onRequestBlueprintChanges"]; onReplan?: MessageActionsProps["onReplanBuildRun"]; onAbandon?: MessageActionsProps["onAbandonBuildRun"] }) {
  const summary = metadata.team_blueprint_summary;
  const token = metadata.blueprint_revision_token;
  const buildRunID = metadata.blueprint_build_run_id;
  const terminalOutcome = metadata.terminal_outcome?.schema_version === 1 ? metadata.terminal_outcome : undefined;
  const [progress, setProgress] = useState<BuildRunProgress | null>(null);
  useEffect(() => {
    if (!buildRunID || mode !== "full") {
      setProgress(null);
      return;
    }
    const controller = new AbortController();
    void api.getTeamBuildRunProgress(buildRunID, controller.signal)
      .then(setProgress)
      .catch((error) => {
        if (!controller.signal.aborted) console.warn("Failed to load team build progress", error);
      });
    return () => controller.abort();
  }, [buildRunID, invalidationVersion, mode]);
  if (!summary || !token || !buildRunID) return null;
  const status = progress?.run_status || buildRun?.status || terminalOutcome?.build_run_status;
  const state = buildRunPhaseLabel(status);
  const buildMode = summary.mode;
  if (mode === "reference") {
    return <section className="blueprint-proposal-card blueprint-proposal-card--reference" aria-label="历史团队方案引用">
      <span>方案：{summary.team_name || "团队方案"}</span>
      <small>当前状态见最新方案卡片</small>
    </section>;
  }
  const criteria = userFacingCriteria(summary.acceptance_criteria || []);
  const failedStep = progress?.steps.find((step) => step.status === "failed");
  const activeStep = failedStep || progress?.steps.find((step) => step.status === "running") || progress?.steps.find((step) => step.status === "pending");
  const candidateEvaluation = progress?.steps.find((step) => step.operation_type === "candidate_run")?.candidate_evaluation;
  const footer = blueprintFooterPresentation(status, activeStep ? operationStepDisplay(activeStep) : undefined);
  const terminalPresentation = terminalOutcome ? terminalOutcomePresentation(terminalOutcome) : undefined;
  const blockedDescription = terminalBuildStatusDescription(terminalOutcome, status, !!candidateEvaluation);
  const modeNote = terminalPresentation?.modeDescription;
  return <section className="blueprint-proposal-card" aria-label="团队方案">
    <header>
      <Users size={16} aria-hidden="true" />
      <div>
        <div className="blueprint-proposal-card__identity">
          <strong>{summary.team_name || "团队方案"}</strong>
          {buildMode && <Badge tone={buildMode === "optimize" ? "accent" : "neutral"}>{buildMode === "optimize" ? "优化" : "新建"}</Badge>}
          <Badge tone={state.tone}>{footer.statusLabel}</Badge>
        </div>
        {summary.template && <small>{summary.template}</small>}
      </div>
    </header>
    {summary.goal && <p>{summary.goal}</p>}
    {!!summary.members?.length && <ul>
      {summary.members.map((member) => {
        const responsibilities = member.responsibilities?.length ? member.responsibilities : member.duty || [];
        const capability = member.capabilities?.[0];
        return <li key={member.ref}>
          <div>
            <span>{displayName(member.display_name || member.name, "一位团队成员")}</span>
            <small>{agentRoleLabel(member.role)}</small>
          </div>
          {responsibilities[0] && <p>{responsibilities[0]}</p>}
          {capability && <em>{capability}</em>}
        </li>;
      })}
    </ul>}
    {!!criteria.length && <div className="blueprint-proposal-card__criteria">{criteria.map((item) => <span key={item}>{item}</span>)}</div>}
    <footer className={`blueprint-proposal-card__footer blueprint-proposal-card__footer--${footer.kind}`}>
      {footer.kind === "planning" && <>
        <div className="blueprint-proposal-card__phase-chain" aria-label="规划已完成，等待继续构建">
          <span>规划 ✓</span><i aria-hidden="true">·</i><strong aria-current="step">继续</strong><i aria-hidden="true">·</i><span>构建</span><i aria-hidden="true">·</i><span>发布</span>
        </div>
        <div className="blueprint-proposal-card__actions">
          {onRequestChanges && <Button variant="ghost" size="small" onClick={onRequestChanges}>提出修改意见</Button>}
          {onSubmit && <Button variant="primary" size="small" onClick={() => void onSubmit(buildRunID, token)}><CheckCircle2 size={14} />继续构建</Button>}
        </div>
      </>}
      {footer.kind === "building" && <BuildOperationFooter progress={progress} headline={footer.headline} />}
      {footer.kind === "passed" && <PublishedTeamFooter progress={progress} teamName={summary.team_name || "新团队"} memberCount={summary.members?.length || 0} onOpenTeam={onOpenTeam} />}
      {footer.kind === "blocked" && <>
        <div className="blueprint-proposal-card__blocked-copy">
          <strong>{blockedDescription || state.summary}</strong>
          {terminalOutcome?.turn_state === "blocked" && !terminalOutcome.report && terminalOutcome.agent_summary && <small>{terminalOutcome.agent_summary}</small>}
        </div>
        <div className="blueprint-proposal-card__actions">
          {onAbandon && <Button variant="ghost" size="small" onClick={onAbandon}>放弃此次构建</Button>}
          {onReplan && <Button variant="primary" size="small" onClick={onReplan}>重新规划</Button>}
        </div>
        {candidateEvaluation && <CandidateEvaluationSummary evaluation={candidateEvaluation} />}
        <BuildOperationDetails progress={progress} showEvaluation={false} />
      </>}
      {footer.kind === "idle" && <strong>{footer.headline}</strong>}
      {modeNote && <small className="blueprint-proposal-card__mode-note">{modeNote}</small>}
    </footer>
  </section>;
}

function BuildOperationFooter({ progress, headline }: { progress: BuildRunProgress | null; headline: string }) {
  return <div className="build-operation-steps" aria-label="构建操作步骤">
    <div className="build-operation-steps__head">
      <LoaderCircle className="spin" size={14} aria-hidden="true" />
      <strong>{headline}</strong>
    </div>
    <BuildOperationDetails progress={progress} />
  </div>;
}

function BuildOperationDetails({ progress, showEvaluation = true }: { progress: BuildRunProgress | null; showEvaluation?: boolean }) {
  if (!progress?.steps.length) return null;
  const evaluation = progress.steps.find((step) => step.operation_type === "candidate_run")?.candidate_evaluation;
  return <details className="build-operation-steps__details">
      <summary>查看操作清单{progress.revision_no ? <small>第 {progress.revision_no} 版方案</small> : null}</summary>
      <ol className="build-operation-steps__list">
        {progress.steps.map((step) => {
          const failureDescription = step.status === "failed"
            ? buildOperationFailureDescription(step.error_detail, step.error_code)
            : undefined;
          return <li key={step.operation_id} className={`build-operation-step build-operation-step--${step.status}`}>
            <span className="build-operation-step__dot" aria-hidden="true">{step.status === "running" ? <LoaderCircle className="spin" size={14} /> : operationStepGlyph(step.status)}</span>
            <span className="build-operation-step__label">{operationStepDisplay(step)}</span>
            {failureDescription && <small className="build-operation-step__error">{failureDescription}</small>}
          </li>;
        })}
      </ol>
      {showEvaluation && evaluation && <CandidateEvaluationSummary evaluation={evaluation} />}
    </details>;
}

function CandidateEvaluationSummary({ evaluation }: { evaluation: CandidateEvaluation }) {
  const conclusion = evaluation.conclusion === "pass" ? "通过" : evaluation.conclusion === "revise" ? "需返工" : "受阻";
  const artifactIsLink = /^https?:\/\//i.test(evaluation.artifact_ref || "");
  return <section className="candidate-evaluation" aria-label="业务验收摘要">
    <header><strong>业务验收</strong><Badge tone={evaluation.conclusion === "pass" ? "success" : "danger"}>{conclusion}</Badge></header>
    {evaluation.scenario_input && <p>{evaluation.scenario_input}</p>}
    {!!evaluation.rubric_scores.length && <ul>
      {evaluation.rubric_scores.map((rubric) => <li key={rubric.name} className={rubric.score >= rubric.pass ? "is-passed" : "is-failed"}>
        <span>{rubric.name}</span><strong>{rubric.score}/{rubric.max}</strong>
      </li>)}
    </ul>}
    {!!evaluation.gate_failures.length && <div className="candidate-evaluation__gates"><strong>未通过硬门禁</strong><span>{evaluation.gate_failures.join("、")}</span></div>}
    {evaluation.artifact_ref && (artifactIsLink
      ? <a href={evaluation.artifact_ref} target="_blank" rel="noreferrer">查看试写产物</a>
      : <small className="candidate-evaluation__artifact">产物引用：{evaluation.artifact_ref}</small>)}
  </section>;
}

function PublishedTeamFooter({ progress, teamName, memberCount, onOpenTeam }: { progress: BuildRunProgress | null; teamName: string; memberCount: number; onOpenTeam?: (teamId: string) => void }) {
  const teamID = progress?.final_ref?.team_id;
  return <div className="build-complete-card">
    <strong>团队 {teamName} 已发布 · {memberCount} 名成员</strong>
    {teamID && onOpenTeam && <Button variant="primary" size="small" onClick={() => onOpenTeam(teamID)}>进入该团队的工作会话</Button>}
  </div>;
}

const OPERATION_LABELS: Record<string, string> = {
  agent_create: "创建成员", agent_update: "更新成员", team_create: "创建团队", team_update: "更新团队",
  roster_set: "绑定成员", agent_graph_compile: "编译成员图", workflow_compile: "编译工作流",
  candidate_run: "业务验收", publish: "发布团队",
};

function operationStepDisplay(step: BuildRunOperationStep): string {
  if (step.display_label) return step.display_label;
  const base = OPERATION_LABELS[step.operation_type] || "处理团队配置";
  const rawTarget = (step.target_name || step.target || "").replace(/^candidate\//, "");
  const target = isTechnicalIdentifier(rawTarget) ? "" : rawTarget;
  if (step.operation_type === "candidate_run") return target ? `业务验收（试运行：${target}）` : "业务验收（试运行）";
  return target ? `${base}：${target}` : base;
}

function operationStepGlyph(status: string): string {
  if (status === "succeeded") return "✓";
  if (status === "skipped") return "↷";
  if (status === "failed") return "!";
  return "";
}

function userFacingCriteria(criteria: string[]): string[] {
  const blocked = /(蓝图|编译|持久化|BuildRun|ChangeSet|revision|create 模式|schema|落库|平台确定性)/i;
  return criteria
    .map((item) => item.trim())
    .filter((item) => item && !blocked.test(item))
    .slice(0, 3);
}

function PendingAttachments({ attachments }: { attachments: MessageAttachment[] }) {
  if (attachments.length === 0) return null;
  return <ul className="message-attachments" aria-label="消息附件">
    {attachments.map((attachment) => <li className="message-attachment" key={attachment.id}>
      <Paperclip size={14} aria-hidden="true" />
      <span>{attachment.filename}</span>
    </li>)}
  </ul>;
}

function PendingProcess({ pending, teamId }: { pending: PendingTurn; teamId?: string }) {
  const interrupted = pending.state === "failed" && pending.interrupted;
  const activity = pending.state === "sending" ? pending.activity || "等待服务端响应" : pending.interruptedReason === "disconnected" ? "连接已断开" : "已停止";
  const execution: AssistantExecutionMetadata = {
    schema_version: 1,
    agent: pending.request.agent,
    status: pending.state === "sending" ? "running" : interrupted ? "stopped" : "failed",
    input: pending.content,
    output: pending.streamedContent,
    started_at: new Date(pending.startedAt).toISOString(),
    tool_calls: pending.toolCalls,
  };
  return <ExecutionProcess execution={execution} live={pending.state === "sending"} activity={activity} agentOutputs={pending.agentOutputs} segments={pending.segments} teamId={teamId} />;
}

function interruptedMessage(pending: PendingTurn): string {
  if (pending.interruptedReason === "disconnected") return "连接已断开，该请求仍在服务端执行。";
  return "已停止等待。服务端仍在执行；继续等待只会读取同一请求的持久状态，恢复为草稿会创建一个新请求。";
}

function interruptedRetryLabel(pending: PendingTurn): string {
  return pending.interruptedReason === "disconnected" ? "重新连接并读取结果" : "继续等待结果";
}

export function PendingTurnMessages({ pending, teamId, hideUserMessage = false, onRestoreDraft, onRetry }: PendingTurnMessagesProps) {
  return <>
    {!hideUserMessage && <article className="message message--user message--pending">
      <div className="message__body"><p>{pending.content}</p><PendingAttachments attachments={pending.attachments} /></div>
    </article>}
    <article className="message message--assistant message--pending">
      <PendingProcess pending={pending} teamId={teamId} />
      <div className="message__body">
        {pending.state === "failed" && !pending.interrupted && pending.streamedContent && <MarkdownText text={pending.streamedContent} />}
        {pending.state === "failed" && <div className={`pending-error${pending.interrupted ? " pending-error--interrupted" : ""}`} role="alert">
          <p>{pending.interrupted ? interruptedMessage(pending) : pending.error}</p>
          <div><Button variant="ghost" size="small" onClick={onRestoreDraft}>恢复为草稿</Button><Button variant={pending.interrupted ? "secondary" : "primary"} size="small" onClick={onRetry}>{pending.interrupted ? interruptedRetryLabel(pending) : "以同一请求重试"}</Button></div>
        </div>}
      </div>
    </article>
  </>;
}
