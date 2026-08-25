import { useCallback, useEffect, useState, type ChangeEvent } from "react";
import { Cable, FileUp, FolderOpen, GitCompare, HardDrive, Link2, LoaderCircle, Paperclip, Trash2 } from "lucide-react";
import { api, apiErrorMessage, type Attachment, type MCPServer, type Project, type ProjectResource, type Runtime } from "../../api";
import { isOpaqueUUIDHandle, platform } from "../../platform";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { ErrorNotice } from "../../ui/StatusViews";

export function ProjectResources({ project }: { project: Project }) {
  const [resources, setResources] = useState<ProjectResource[]>([]);
  const [attachments, setAttachments] = useState<Attachment[]>([]);
  const [servers, setServers] = useState<MCPServer[]>([]);
  const [runtimes, setRuntimes] = useState<Runtime[]>([]);
  const [selectedRuntimeId, setSelectedRuntimeId] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [pendingWorkspaceActions, setPendingWorkspaceActions] = useState<Set<string>>(() => new Set());
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    try {
      const [resourceResponse, attachmentResponse, mcpResponse, runtimeResponse] = await Promise.all([
        api.listProjectResources(project.id, signal),
        api.listAttachments(signal),
        api.listMCPServers(signal),
        api.listRuntimes(signal),
      ]);
      setResources(resourceResponse.resources);
      setAttachments(attachmentResponse.attachments);
      setServers(mcpResponse);
      setRuntimes(runtimeResponse.runtimes);
      setSelectedRuntimeId((current) => runtimeResponse.runtimes.some((item) => item.id === current && item.enabled && !item.revoked_at && !item.deleted_at) ? current : "");
      setError(null);
    } catch (requestError) {
      if (!signal?.aborted) setError(apiErrorMessage(requestError));
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [project.id]);

  useEffect(() => {
    const controller = new AbortController();
    void refresh(controller.signal);
    return () => controller.abort();
  }, [refresh]);

  async function bind(kind: "attachment" | "mcp_server", resourceRef: string) {
    const source = kind === "attachment" ? attachments.find((item) => item.id === resourceRef) : servers.find((item) => item.id === resourceRef);
    const displayName = source && "filename" in source ? source.filename : source?.display_name;
    if (!resourceRef || !displayName) return;
    setBusy(true);
    try {
      await api.createProjectResource(project.id, { kind, resource_ref: resourceRef, display_name: displayName });
      await refresh();
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setBusy(false);
    }
  }

  async function upload(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file) return;
    setBusy(true);
    try {
      const attachment = await api.uploadAttachment(file);
      setAttachments((current) => [attachment, ...current]);
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setBusy(false);
    }
  }

  const availableRuntimes = runtimes.filter((item) => item.enabled && !item.revoked_at && !item.deleted_at);

  async function bindLocalWorkspace() {
    const runtime = availableRuntimes.find((item) => item.id === selectedRuntimeId);
    if (!runtime) {
      setError("请先选择一个可用的 Runtime");
      return;
    }
    setBusy(true);
    try {
      const selection = await platform.chooseLocalWorkspace();
      if (!selection) return;
      if (!isOpaqueUUIDHandle(selection.handle)) throw new Error("桌面适配器必须返回 opaque UUID handle");
      if (/^(?:[a-z]:[\\/]|[\\/]|~[\\/])/i.test(selection.displayName) || selection.displayName.includes("/") || selection.displayName.includes("\\")) throw new Error("桌面适配器不得把本地绝对路径作为 display_name");
      await api.createProjectResource(project.id, {
        kind: "local_workspace",
        resource_ref: selection.handle,
        display_name: selection.displayName,
        runtime_id: runtime.id,
      });
      await refresh();
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setBusy(false);
    }
  }

  async function runWorkspaceAction(resource: ProjectResource, action: "folder" | "diff") {
    const actionKey = `${resource.id}:${action}`;
    setPendingWorkspaceActions((current) => new Set(current).add(actionKey));
    setError(null);
    try {
      if (!isOpaqueUUIDHandle(resource.resource_ref)) throw new Error("本地工作目录句柄无效");
      if (action === "folder") {
        await platform.openLocalWorkspace(resource.resource_ref);
      } else {
        await platform.openDiff(resource.resource_ref);
      }
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setPendingWorkspaceActions((current) => {
        const next = new Set(current);
        next.delete(actionKey);
        return next;
      });
    }
  }

  const boundAttachments = new Set(resources.filter((item) => item.kind === "attachment").map((item) => item.resource_ref));
  const boundServers = new Set(resources.filter((item) => item.kind === "mcp_server").map((item) => item.resource_ref));

  return <section className="project-resources">
    <div className="section-heading"><div><h2>项目资料</h2><p>绑定真实附件、MCP Server 与桌面工作目录句柄。</p></div><span>{resources.length}</span></div>
    {error && <ErrorNotice message={error} onRetry={() => void refresh()} />}
    {loading ? <div className="resource-loading"><LoaderCircle className="spin" size={16} /> 正在读取资料目录</div> : <>
      <div className="resource-actions">
        <label className="ui-button ui-button--secondary"><FileUp size={16} /> 上传附件<input className="sr-only" type="file" disabled={busy} onChange={(event) => void upload(event)} /></label>
        {platform.capabilities.localWorkspace && <>
          <label className="runtime-picker"><span>Runtime</span><select value={selectedRuntimeId} disabled={busy || availableRuntimes.length === 0} onChange={(event) => setSelectedRuntimeId(event.target.value)}><option value="">选择可用 Runtime…</option>{availableRuntimes.map((runtime) => <option key={runtime.id} value={runtime.id}>{runtime.name}{runtime.online ? " · 在线" : " · 离线"}</option>)}</select></label>
          <Button disabled={busy || !selectedRuntimeId} onClick={() => void bindLocalWorkspace()}><FolderOpen size={16} /> 选择本地工作目录</Button>
        </>}
        {platform.kind === "browser" && <span className="honest-boundary">浏览器不读取本地目录；仅桌面适配器可提供不透明句柄。</span>}
      </div>

      {resources.length === 0 ? <div className="resource-empty"><Link2 size={20} /><p>该项目尚未绑定资料。</p></div> : <div className="resource-list">{resources.map((resource) => {
        const openingFolder = pendingWorkspaceActions.has(`${resource.id}:folder`);
        const openingDiff = pendingWorkspaceActions.has(`${resource.id}:diff`);
        const workspaceActionsVisible = resource.kind === "local_workspace" && platform.capabilities.folderOpener && platform.capabilities.diffViewer;
        return <article key={resource.id}>
          <span className="resource-icon">{resource.kind === "attachment" ? <Paperclip size={16} /> : resource.kind === "mcp_server" ? <Cable size={16} /> : <HardDrive size={16} />}</span>
          <div className="resource-details"><strong>{resource.display_name}</strong><small>{resource.kind === "attachment" ? "附件" : resource.kind === "mcp_server" ? "MCP Server" : `本地工作目录 · Runtime ${resource.runtime_id}`}</small></div>
          {workspaceActionsVisible && <div className="workspace-actions">
            <Button variant="ghost" loading={openingFolder} aria-label={`打开目录 ${resource.display_name}`} onClick={() => void runWorkspaceAction(resource, "folder")}>{!openingFolder && <FolderOpen size={14} />} 打开目录</Button>
            <Button variant="ghost" loading={openingDiff} aria-label={`查看差异 ${resource.display_name}`} onClick={() => void runWorkspaceAction(resource, "diff")}>{!openingDiff && <GitCompare size={14} />} 查看差异</Button>
          </div>}
          <Badge tone={resource.available ? "success" : "neutral"}>{resource.available ? "可用" : "不可用"}</Badge>
          <button className="icon-button icon-button--danger" type="button" disabled={busy} aria-label={`移除 ${resource.display_name}`} onClick={async () => { setBusy(true); try { await api.deleteProjectResource(project.id, resource.id); setResources((current) => current.filter((item) => item.id !== resource.id)); } catch (requestError) { setError(apiErrorMessage(requestError)); } finally { setBusy(false); } }}><Trash2 size={16} /></button>
        </article>;
      })}</div>}

      <div className="catalog-bindings">
        <label><span>绑定已上传附件</span><select defaultValue="" disabled={busy} onChange={(event) => { const value = event.target.value; event.target.value = ""; void bind("attachment", value); }}><option value="">选择附件…</option>{attachments.filter((item) => !boundAttachments.has(item.id)).map((item) => <option key={item.id} value={item.id}>{item.filename} · {Math.max(1, Math.round(item.size / 1024))} KB</option>)}</select></label>
        <label><span>绑定 MCP Server</span><select defaultValue="" disabled={busy} onChange={(event) => { const value = event.target.value; event.target.value = ""; void bind("mcp_server", value); }}><option value="">选择服务…</option>{servers.filter((item) => item.enabled && !item.revoked_at && !boundServers.has(item.id)).map((item) => <option key={item.id} value={item.id}>{item.display_name} · {item.status}</option>)}</select></label>
      </div>
    </>}
  </section>;
}
