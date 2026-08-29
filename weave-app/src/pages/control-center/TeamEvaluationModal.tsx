import { useEffect, useMemo, useState, type FormEvent } from "react";
import { CheckCircle2, ClipboardCheck, ShieldAlert } from "lucide-react";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type BuildRunProgress,
  type Team,
  type TeamEvaluationReport,
} from "../../api";
import { createClientUUID } from "../../platform/uuid";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { ErrorNotice } from "../../ui/StatusViews";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";

interface TeamEvaluationModalProps {
  team: Team | null;
  onClose(): void;
  onCompleted(): void;
}

function realContract(team: Team): Record<string, unknown> {
  const scenarioID = `team-${team.id}-primary`;
  const rule = "在保持业务目标不变的前提下改变输入细节与表达方式";
  const input = team.primary_scenario.trim() || `请完成“${team.objective}”的典型业务任务，并给出可交付结果。`;
  const expected = team.success_criteria.trim() || `结果应完整、正确并满足“${team.objective}”。`;
  return {
    schema_version: 2,
    hard_gates: [],
    rubric: [{
      id: "business_quality", name: "业务产物质量",
      description: "依据真实输出判断正确性、完整性、可读性与可交付性。",
      max_score: 10, pass_threshold: 7,
    }],
    public_scenarios: [{ id: scenarioID, input, expected }],
    perturbation_rules: [rule],
    perturbation_scenarios: [{
      id: `${scenarioID}-variant`, base_scenario_id: scenarioID, rule,
      input: `${input}\n变化条件：输入细节与措辞已经调整，请仍按相同业务目标完成。`, expected,
    }],
    hidden_scenario_count: 0,
    severe_defect_definition: "输出包含关键事实错误、缺少核心交付物，或无法用于所述业务场景。",
    run_count: 1,
    max_iterations: 3,
    pass_rules: ["全部硬门禁通过且每项 rubric 达到阈值"],
    block_rules: ["出现严重缺陷或业务质量未达到阈值"],
    infra_failure_rules: ["运行环境或依赖故障必须报告并阻断，不得计为通过"],
  };
}

export function TeamEvaluationModal({ team, onClose, onCompleted }: TeamEvaluationModalProps) {
  const [contractJSON, setContractJSON] = useState("");
  const [budget, setBudget] = useState("5");
  const [idempotencyKey, setIdempotencyKey] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [buildRunID, setBuildRunID] = useState("");
  const [progress, setProgress] = useState<BuildRunProgress | null>(null);
  const [report, setReport] = useState<TeamEvaluationReport | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!team) return;
    setContractJSON(JSON.stringify(realContract(team), null, 2));
    setBudget("5");
    setIdempotencyKey(createClientUUID());
    setBuildRunID("");
    setProgress(null);
    setReport(null);
    setError(null);
  }, [team]);

  useEffect(() => {
    if (!team || !buildRunID) return;
    const controller = new AbortController();
    let timer = 0;
    const readReport = async () => {
      try {
        const response = await api.getTeamBuildRunRoundReport(buildRunID, 1, controller.signal);
        if (!controller.signal.aborted) setReport(response.report);
      } catch {
        // Infrastructure/CAS failures may block before a round report exists.
      }
    };
    const poll = async () => {
      try {
        const next = await api.getTeamBuildRunProgress(buildRunID, controller.signal);
        if (controller.signal.aborted) return;
        setProgress(next);
        if (next.run_status === "passed" || next.run_status === "blocked" || next.run_status === "cancelled") {
          await readReport();
          onCompleted();
          if (next.run_status !== "passed") setError(`认证已${next.run_status === "blocked" ? "失败" : "取消"}，不影响团队继续接活。`);
          return;
        }
        timer = window.setTimeout(() => void poll(), 1_200);
      } catch (requestError) {
        const normalized = normalizeThrownError(requestError);
        if (normalized.kind !== "aborted") setError(`读取认证进度失败：${apiErrorMessage(normalized)}`);
      }
    };
    void poll();
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [buildRunID, onCompleted, team]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!team || buildRunID) return;
    setError(null);
    let contract: unknown;
    try {
      contract = JSON.parse(contractJSON);
    } catch {
      setError("合同不是有效的 JSON 对象。");
      return;
    }
    if (!contract || typeof contract !== "object" || Array.isArray(contract)) {
      setError("合同必须是一个 JSON 对象。");
      return;
    }
    const maxCostUSD = Number(budget);
    if (!Number.isFinite(maxCostUSD) || maxCostUSD <= 0) {
      setError("预算必须是大于 0 的美元金额。");
      return;
    }
    setSubmitting(true);
    try {
      const outcome = await api.evaluateTeam(team.id, {
        contract: contract as Record<string, unknown>,
        idempotency_key: idempotencyKey,
        budget: { max_cost_usd: maxCostUSD },
      });
      setBuildRunID(outcome.build_run_id);
      setProgress({ workspace_id: "", build_run_id: outcome.build_run_id, run_status: outcome.status, steps: [], updated_at: new Date().toISOString() });
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setSubmitting(false);
    }
  }

  const completedSteps = useMemo(() => progress?.steps.filter((step) => step.status === "succeeded" || step.status === "skipped").length || 0, [progress]);
  const terminal = progress?.run_status === "passed" || progress?.run_status === "blocked" || progress?.run_status === "cancelled";

  return <Modal
    open={!!team}
    size="wide"
    title={`认证${team ? `“${team.name}”` : "团队"}`}
    description="这是可选的版本质量认证，不是团队接活的前置条件。认证会使用显式预算，失败不会修改或停用团队。"
    onClose={() => { if (!submitting) onClose(); }}
    footer={<>
      <Button disabled={submitting} onClick={onClose}>{terminal ? "完成" : "关闭"}</Button>
      <Button variant="primary" type="submit" form="team-evaluation-form" loading={submitting} disabled={!!buildRunID || !contractJSON.trim()}>
        <ClipboardCheck size={16} aria-hidden="true" />
        {submitting ? "正在提交…" : "开始认证"}
      </Button>
    </>}
  >
    <form id="team-evaluation-form" className="team-evaluation-editor" onSubmit={(event) => void submit(event)}>
      {error && <ErrorNotice message={error} />}
      <div className="team-evaluation-budget">
        <Field label="最高成本预算（USD）" help="由管理员显式授权；当前版本不会自动授予额度。">
          {(control) => <input {...control} type="number" min="0.01" step="0.01" value={budget} readOnly={!!buildRunID} onChange={(event) => setBudget(event.target.value)} />}
        </Field>
        <div className="notice"><ShieldAlert size={16} /><span>质量失败直接 blocked，绝不触发 BlueprintPatch。</span></div>
      </div>
      <Field label="EvaluationContract JSON" help="请用真实业务输入、期望与扰动场景替换示例内容后提交。">
        {(control) => <textarea {...control} className="team-evaluation-contract" rows={24} spellCheck={false} value={contractJSON} readOnly={!!buildRunID} onChange={(event) => setContractJSON(event.target.value)} />}
      </Field>
      {buildRunID && <section className="team-evaluation-progress" aria-live="polite">
        <header>
          <CheckCircle2 size={18} aria-hidden="true" />
          <span><strong>{progress?.run_status === "passed" ? "认证通过" : terminal ? "认证已停止" : "认证进行中"}</strong><small>{progress ? `已完成 ${completedSteps}/${progress.steps.length} 个步骤 · ${progress.run_status}` : buildRunID}</small></span>
          {progress?.run_status && <Badge tone={progress.run_status === "passed" ? "success" : progress.run_status === "blocked" ? "danger" : "neutral"}>{progress.run_status}</Badge>}
        </header>
        {report && <div className="team-evaluation-report">
          <h3>认证报告</h3>
          <p>结论 {report.conclusion}{report.failure_category ? ` · ${report.failure_category}` : ""}</p>
          {!!report.rubric_scores?.length && <ul>{report.rubric_scores.map((score) => <li key={score.dimension_id}><strong>{score.dimension_id}</strong><span>{score.score} 分</span><small>{score.reason}</small></li>)}</ul>}
          {!!report.severe_defects?.length && <p className="team-evaluation-defects">严重缺陷 {report.severe_defects.join("；")}</p>}
        </div>}
      </section>}
    </form>
  </Modal>;
}
