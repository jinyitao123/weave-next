import { useEffect, useState, type FormEvent } from "react";
import { CheckCircle2, FileCode2 } from "lucide-react";
import {
  api,
  apiErrorMessage,
  normalizeThrownError,
  type BuildRunProgress,
  type TeamTemplateSample,
} from "../../api";
import { createClientUUID } from "../../platform/uuid";
import { Button } from "../../ui/Button";
import { ErrorNotice } from "../../ui/StatusViews";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";

interface TeamTemplateModalProps {
  open: boolean;
  onClose(): void;
  onReady(teamID: string): void;
}

export function TeamTemplateModal({ open, onClose, onReady }: TeamTemplateModalProps) {
  const [samples, setSamples] = useState<TeamTemplateSample[]>([]);
  const [selectedSample, setSelectedSample] = useState("");
  const [yaml, setYAML] = useState("");
  const [idempotencyKey, setIdempotencyKey] = useState("");
  const [loadingSamples, setLoadingSamples] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [buildRunID, setBuildRunID] = useState("");
  const [buildStatus, setBuildStatus] = useState("");
  const [progress, setProgress] = useState<BuildRunProgress | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    const controller = new AbortController();
    setIdempotencyKey(createClientUUID());
    setBuildRunID("");
    setBuildStatus("");
    setProgress(null);
    setError(null);
    setSelectedSample("");
    setYAML("");
    setLoadingSamples(true);
    void api.listTeamTemplateSamples(controller.signal)
      .then(({ samples: nextSamples }) => {
        setSamples(nextSamples);
        if (nextSamples.length) {
          setSelectedSample(nextSamples[0].name);
          setYAML(nextSamples[0].yaml);
        }
      })
      .catch((requestError: unknown) => {
        const normalized = normalizeThrownError(requestError);
        if (normalized.kind !== "aborted") setError(`读取模板样例失败：${apiErrorMessage(normalized)}`);
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoadingSamples(false);
      });
    return () => controller.abort();
  }, [open]);

  useEffect(() => {
    if (!open || !buildRunID || buildStatus === "authorization_required") return;
    const controller = new AbortController();
    let timer = 0;
    const poll = async () => {
      try {
        const next = await api.getTeamBuildRunProgress(buildRunID, controller.signal);
        if (controller.signal.aborted) return;
        setProgress(next);
        setBuildStatus(next.run_status);
        if (next.run_status === "passed") {
          const teamID = next.final_ref?.team_id;
          if (!teamID) {
            setError("构建已结束，但没有返回团队标识。请查看构建进度详情。");
            return;
          }
          onReady(teamID);
          return;
        }
        if (next.run_status === "blocked" || next.run_status === "cancelled") {
          setError(`模板构建已${next.run_status === "blocked" ? "暂停" : "取消"}，请检查失败步骤后重试。`);
          return;
        }
        timer = window.setTimeout(() => void poll(), 1_200);
      } catch (requestError) {
        const normalized = normalizeThrownError(requestError);
        if (normalized.kind !== "aborted") setError(`读取构建进度失败：${apiErrorMessage(normalized)}`);
      }
    };
    void poll();
    return () => {
      controller.abort();
      window.clearTimeout(timer);
    };
  }, [buildRunID, buildStatus, onReady, open]);

  function chooseSample(name: string) {
    setSelectedSample(name);
    const sample = samples.find((item) => item.name === name);
    if (sample) setYAML(sample.yaml);
    setError(null);
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!yaml.trim() || !idempotencyKey) return;
    setSubmitting(true);
    setError(null);
    try {
      const declarativeSpec = samples.find((sample) => sample.name === selectedSample)?.declarative_spec;
      const outcome = await api.createTeamFromTemplate({
        yaml,
        idempotency_key: idempotencyKey,
        ...(declarativeSpec ? { declarative_spec: declarativeSpec } : {}),
      });
      setBuildRunID(outcome.build_run_id);
      setBuildStatus(outcome.status);
      if (outcome.status === "ready" && outcome.team_id) {
        onReady(outcome.team_id);
      } else if (outcome.status === "authorization_required") {
        setError("模板预算或工作区额度超出自动授权范围。构建已保留，请由管理员在构建记录中审核后继续。");
      }
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setSubmitting(false);
    }
  }

  const completedSteps = progress?.steps.filter((step) => step.status === "succeeded").length || 0;
  const progressLocked = !!buildRunID;
  const requestClose = () => {
    if (!submitting) onClose();
  };

  return <Modal
    open={open}
    size="wide"
    title="从模板创建团队"
    description="选择行业样例并直接修改 YAML。平台完成静态校验后会立即发布首个工作流版本，团队可以直接接活。"
    onClose={requestClose}
    footer={<>
      <Button disabled={submitting} onClick={requestClose}>关闭</Button>
      <Button variant="primary" type="submit" form="team-template-create" loading={submitting} disabled={loadingSamples || progressLocked || !yaml.trim()}>
        <FileCode2 size={16} aria-hidden="true" />
        {submitting ? "正在提交…" : "创建团队"}
      </Button>
    </>}
  >
    <form id="team-template-create" className="team-template-editor" onSubmit={(event) => void submit(event)}>
      {error && <ErrorNotice message={error} />}
      <div className="team-template-toolbar">
        <Field label="行业样例" help="切换样例会替换下方编辑器内容。">
          {(control) => <select {...control} value={selectedSample} disabled={loadingSamples || progressLocked} onChange={(event) => chooseSample(event.target.value)}>
            {!samples.length && <option value="">{loadingSamples ? "正在读取样例" : "暂无样例，可直接粘贴 YAML"}</option>}
            {samples.map((sample) => <option key={sample.name} value={sample.name}>{sample.display_name}</option>)}
          </select>}
        </Field>
        {selectedSample && <p>{samples.find((sample) => sample.name === selectedSample)?.description}</p>}
      </div>
      <Field label="团队模板 YAML" help="业务字段必填；模型与执行策略可留空，由平台补全。">
        {(control) => <textarea
          {...control}
          className="team-template-yaml"
          rows={24}
          spellCheck={false}
          value={yaml}
          readOnly={progressLocked}
          onChange={(event) => { setYAML(event.target.value); setError(null); }}
        />}
      </Field>
      {buildRunID && <div className="team-template-progress" role="status">
        <CheckCircle2 size={18} aria-hidden="true" />
        <span>
          <strong>{buildStatus === "authorization_required" ? "等待管理员授权" : "正在创建团队"}</strong>
          <small>{progress ? `已完成 ${completedSteps}/${progress.steps.length} 个步骤 · ${progress.run_status}` : `构建记录 ${buildRunID}`}</small>
        </span>
      </div>}
    </form>
  </Modal>;
}
