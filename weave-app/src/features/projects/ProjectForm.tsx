import { useEffect, useState, type FormEvent } from "react";
import { apiErrorMessage, type AgentRecord, type Project, type Team } from "../../api";
import { Button } from "../../ui/Button";
import { Field } from "../../ui/Field";
import { Modal } from "../../ui/Modal";
import { ErrorNotice } from "../../ui/StatusViews";

interface ProjectFormProps {
  open: boolean;
  teams: Team[];
  agents: AgentRecord[];
  project?: Project;
  initialTeamId?: string;
  onClose(): void;
  onCreate(input: { team_id: string; name: string; description: string }): Promise<Project>;
  onUpdate(id: string, input: { name: string; description: string }): Promise<Project>;
}

export function ProjectForm({ open, teams, agents, project, initialTeamId, onClose, onCreate, onUpdate }: ProjectFormProps) {
  const [teamId, setTeamId] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const selectedTeam = teams.find((team) => team.id === teamId);

  useEffect(() => {
    if (!open) return;
    const projectTeam = project ? teams.find((team) => team.id === project.team_id) : undefined;
    setTeamId(projectTeam?.id || initialTeamId || teams[0]?.id || "");
    setName(project?.name || "");
    setDescription(project?.description || "");
    setError(null);
  }, [initialTeamId, open, project, teams]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!name.trim() || (!project && !selectedTeam)) return;
    setSaving(true);
    setError(null);
    try {
      if (project) {
        await onUpdate(project.id, { name, description });
      } else if (selectedTeam) {
        await onCreate({ team_id: selectedTeam.id, name, description });
      }
      onClose();
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal
      open={open}
      title={project ? "编辑项目" : "创建项目"}
      description={project ? "项目归属需通过单独的迁移动作修改。" : "项目归属于一个团队，由该团队负责人承接会话并调度员工。"}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>取消</Button>
          <Button variant="primary" type="submit" form="project-form" loading={saving} disabled={!name.trim() || (!project && !selectedTeam)}>
            {project ? "保存更改" : "创建项目"}
          </Button>
        </>
      }
    >
      <form id="project-form" className="form-stack" onSubmit={submit}>
        {error && <ErrorNotice message={error} />}
        {!project && (
          <Field label="所属团队">
            {({ id, ...control }) => (
              <select id={id} {...control} value={teamId} onChange={(event) => setTeamId(event.target.value)} required>
                <option value="" disabled>选择团队</option>
                {teams.map((team) => {
                  const lead = agents.find((agent) => agent.id === team.lead_avatar_id);
                  return <option key={team.id} value={team.id}>{team.name}{lead ? ` · ${lead.display_name || lead.name}` : ""}</option>;
                })}
              </select>
            )}
          </Field>
        )}
        <Field label="项目名称">
          {({ id, ...control }) => (
            <input id={id} {...control} value={name} onChange={(event) => setName(event.target.value)} autoFocus maxLength={120} required placeholder="例如：产品发布" />
          )}
        </Field>
        <Field label="说明">
          {({ id, ...control }) => (
            <textarea id={id} {...control} value={description} onChange={(event) => setDescription(event.target.value)} rows={4} placeholder="项目目标、边界或约定" />
          )}
        </Field>
      </form>
    </Modal>
  );
}
