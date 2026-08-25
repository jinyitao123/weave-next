import "../../styles/project-data.css";

import { useCallback, useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Brain, Plus, Search, Trash2, X } from "lucide-react";
import { ApiError, api, apiErrorMessage, type Project, type ProjectMemory } from "../../api";
import { Button } from "../../ui/Button";
import { Card } from "../../ui/Card";
import { Field } from "../../ui/Field";
import { ConfirmModal } from "../../ui/ConfirmModal";
import { Modal } from "../../ui/Modal";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";

const when = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });

interface ProjectMemoryPanelProps {
  project: Project;
  open: boolean;
  onClose(): void;
}

export function ProjectMemoryPanel({ project, open, onClose }: ProjectMemoryPanelProps) {
  const projectId = project.id;
  const [memories, setMemories] = useState<ProjectMemory[]>([]);
  const [memoryEnabled, setMemoryEnabled] = useState(true);
  const [searching, setSearching] = useState(false);
  const [query, setQuery] = useState("");
  const [content, setContent] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [removing, setRemoving] = useState<ProjectMemory | null>(null);

  const loadMemories = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const response = await api.listProjectMemories(projectId);
      if (Array.isArray(response)) {
        setMemories(response);
        setMemoryEnabled(true);
      } else {
        setMemories(response.memories);
        setMemoryEnabled(false);
      }
      setSearching(false);
    } catch (requestError) {
      if (requestError instanceof ApiError && requestError.status === 501) setMemoryEnabled(false);
      else setError(apiErrorMessage(requestError));
      setMemories([]);
    } finally {
      setLoading(false);
    }
  }, [projectId]);

  useEffect(() => {
    if (!open) return;
    setContent("");
    setQuery("");
    setError(null);
    void loadMemories();
  }, [loadMemories, open]);

  async function addMemory(event: FormEvent) {
    event.preventDefault();
    if (!content.trim()) return;
    setBusy(true);
    setError(null);
    try {
      await api.createProjectMemory(projectId, content.trim());
      setContent("");
      await loadMemories();
    } catch (requestError) {
      if (requestError instanceof ApiError && requestError.status === 501) setMemoryEnabled(false);
      else setError(apiErrorMessage(requestError));
    } finally {
      setBusy(false);
    }
  }

  async function searchMemory(event: FormEvent) {
    event.preventDefault();
    if (!query.trim()) return;
    setBusy(true);
    setError(null);
    try {
      const response = await api.searchProjectMemories(projectId, { query: query.trim() });
      if (Array.isArray(response)) {
        setMemories(response);
        setMemoryEnabled(true);
        setSearching(true);
      } else {
        setMemories(response.memories);
        setMemoryEnabled(false);
        setSearching(false);
      }
    } catch (requestError) {
      if (requestError instanceof ApiError && requestError.status === 501) setMemoryEnabled(false);
      else setError(apiErrorMessage(requestError));
    } finally {
      setBusy(false);
    }
  }

  async function removeMemory() {
    if (!removing) return;
    setBusy(true);
    setError(null);
    try {
      await api.deleteProjectMemory(projectId, removing.id);
      setRemoving(null);
      await loadMemories();
    } catch (requestError) {
      if (requestError instanceof ApiError && requestError.status === 501) setMemoryEnabled(false);
      else setError(apiErrorMessage(requestError));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal open={open} title={`项目事实 · ${project.name}`} size="wide" onClose={onClose} footer={<Button onClick={onClose}>关闭</Button>}>
      <div className="project-data-memory-workspace project-memory-panel">
        {error && <ErrorNotice message={error} onRetry={() => void loadMemories()} />}
        {loading ? (
          <LoadingView label="正在读取项目事实" />
        ) : !memoryEnabled ? (
          <ProjectDataEmpty icon={<Brain size={28} />} title="项目事实暂不可用" description="当前部署未启用项目事实检索，稍后再试或联系管理员。" />
        ) : (
          <>
            <div className="project-data-memory-tools">
              <form onSubmit={(event) => void searchMemory(event)}>
                <Card
                  className="project-data-tool-card"
                  header={<h2>搜索</h2>}
                  footer={
                    <div className="project-data-tool-actions">
                      <Button type="submit" disabled={busy || !query.trim()}><Search size={16} /> 搜索</Button>
                      {searching && <Button variant="ghost" type="button" onClick={() => { setQuery(""); void loadMemories(); }}><X size={16} /> 清除</Button>}
                    </div>
                  }
                >
                  <Field label="关键词">
                    {(control) => <input {...control} value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索项目事实" />}
                  </Field>
                </Card>
              </form>
              <form onSubmit={(event) => void addMemory(event)}>
                <Card
                  className="project-data-tool-card"
                  header={<h2>添加事实</h2>}
                  footer={<div className="project-data-tool-actions"><Button variant="primary" type="submit" disabled={busy || !content.trim()}><Plus size={16} /> 添加</Button></div>}
                >
                  <Field label="内容">
                    {(control) => <textarea {...control} rows={3} value={content} onChange={(event) => setContent(event.target.value)} placeholder="记录一条项目事实（决策、约定、约束）" />}
                  </Field>
                </Card>
              </form>
            </div>

            {!memories.length ? (
              <ProjectDataEmpty
                icon={<Brain size={28} />}
                title={searching ? "没有匹配的事实" : "还没有项目事实"}
                description={searching ? "换个关键词，或清除搜索返回列表。" : "添加第一条项目事实，团队会在后续会话中使用。"}
              />
            ) : (
              <div className="project-data-memory-list">
                {memories.map((memory) => (
                  <Card
                    className="project-data-memory-item"
                    key={memory.id}
                    header={<strong>{memory.content}</strong>}
                    footer={<Button variant="danger" type="button" disabled={busy} onClick={() => setRemoving(memory)}><Trash2 size={14} /> 删除</Button>}
                  >
                    <dl>
                      <div><dt>记录于</dt><dd>{when.format(new Date(memory.created_at))}</dd></div>
                      <div><dt>最近使用</dt><dd>{when.format(new Date(memory.accessed_at))}</dd></div>
                    </dl>
                  </Card>
                ))}
              </div>
            )}
          </>
        )}
      </div>
      <ConfirmModal
        open={!!removing}
        title="删除项目事实"
        description="删除后这条事实不再参与项目检索，不可恢复。"
        busy={busy}
        onConfirm={() => void removeMemory()}
        onClose={() => setRemoving(null)}
      ><p className="confirm-copy">确定删除这条项目事实？</p></ConfirmModal>
    </Modal>
  );
}

function ProjectDataEmpty({ icon, title, description }: { icon: ReactNode; title: string; description: string }) {
  return (
    <Card className="project-data-empty">
      {icon}
      <h2>{title}</h2>
      <p>{description}</p>
    </Card>
  );
}
