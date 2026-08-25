import { useEffect, useState } from "react";
import { ArrowRight, Users } from "lucide-react";
import { apiErrorMessage, type AgentRecord, type Project, type Team } from "../../api";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { ErrorNotice } from "../../ui/StatusViews";

interface MoveProjectFormProps {
  open: boolean;
  project: Project;
  teams: Team[];
  agents: AgentRecord[];
  onClose(): void;
  onMove(id: string, input: { team_id: string }): Promise<Project>;
}

export function MoveProjectForm({ open, project, teams, agents, onClose, onMove }: MoveProjectFormProps) {
  const [teamId, setTeamId] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const current = teams.find((team) => team.id === project.team_id);
  const target = teams.find((team) => team.id === teamId);

  useEffect(() => {
    if (open) {
      setTeamId(teams.find((team) => team.id !== project.team_id)?.id || "");
      setError(null);
    }
  }, [open, project.avatar_id, project.team_id, teams]);

  function teamOptionLabel(team: Team): string {
    const lead = agents.find((agent) => agent.id === team.lead_avatar_id);
    return lead ? `${team.name} · ${lead.display_name || lead.name}` : team.name;
  }

  return (
    <Modal
      open={open}
      title="迁移项目"
      description="迁移只改变项目当前归属团队；历史会话与运行不会被改写。"
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button
            variant="primary"
            loading={saving}
            disabled={!target}
            onClick={async () => {
              if (!target) return;
              setSaving(true);
              setError(null);
              try {
                await onMove(project.id, { team_id: target.id });
                onClose();
              } catch (requestError) {
                setError(apiErrorMessage(requestError));
              } finally {
                setSaving(false);
              }
            }}
          >
            {saving ? "正在迁移…" : "确认迁移"}
          </Button>
        </>
      }
    >
      <div className="form-stack">
        {error && <ErrorNotice message={error} />}
        <div className="transfer-path" aria-label="项目迁移路径">
          <span><Users size={16} /> {current?.name || "当前团队"}</span>
          <ArrowRight size={16} />
          <strong>{target?.name || "选择目标"}</strong>
        </div>
        <Field label="目标团队">{(control) => <select {...control} value={teamId} onChange={(event) => setTeamId(event.target.value)}>
            <option value="" disabled>没有其他可用团队</option>
            {teams.filter((team) => team.id !== project.team_id).map((team) => (
              <option key={team.id} value={team.id}>{teamOptionLabel(team)}</option>
            ))}
          </select>}</Field>
      </div>
    </Modal>
  );
}
