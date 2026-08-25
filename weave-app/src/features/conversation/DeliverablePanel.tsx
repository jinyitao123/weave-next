import { useEffect, useState } from "react";
import { Check, Clipboard, Download, FileText, MessageSquareText, RefreshCw } from "lucide-react";
import { Link } from "react-router-dom";
import { api, apiErrorMessage, type FinalDeliverable } from "../../api";
import { Badge } from "../../ui/Badge";
import { Button } from "../../ui/Button";
import { Card } from "../../ui/Card";
import { ErrorNotice, LoadingView } from "../../ui/StatusViews";
import { MarkdownText } from "./MarkdownText";
import { useWorkspace } from "../../workspace/useWorkspace";
import { deliverableTypeLabel } from "../../workspace/labels";
import { useDeliverables, type DeliverablesState } from "./useDeliverables";

const runIdentityCache = new Map<string, string>();

function looksLikeHtmlDocument(content: string): boolean {
  const head = content.trimStart().slice(0, 200).toLowerCase();
  return head.startsWith("<!doctype html") || head.startsWith("<html");
}

function looksLikeSvgDocument(content: string): boolean {
  return content.trimStart().slice(0, 80).toLowerCase().startsWith("<svg");
}

function deliverableMetadata(item: FinalDeliverable): Record<string, unknown> {
  return item.metadata && typeof item.metadata === "object" && !Array.isArray(item.metadata)
    ? item.metadata as Record<string, unknown>
    : {};
}

type ArtifactCategory = "final" | "stage" | "evidence";

function artifactCategory(item: FinalDeliverable): ArtifactCategory {
  const metadata = deliverableMetadata(item);
  if (metadata.artifact_kind === "final") return "final";
  if (metadata.artifact_kind === "evidence") return "evidence";
  const nodeType = String(metadata.node_type || "").trim().toLowerCase();
  const nodeId = String(metadata.node_id || "").trim().toLowerCase();
  const nodeLabel = String(metadata.node_label || item.title || "").trim().toLowerCase();
  const controlNode = ["parallel", "join", "loop", "condition", "wait"].includes(nodeType);
  const latchNode = /(^|[-_])latch($|[-_])/.test(nodeId);
  const processEvidence = /(quality[-_ ]?gate|qa|验收|质量门)/.test(`${nodeId} ${nodeLabel}`);
  return controlNode || latchNode || processEvidence ? "evidence" : "stage";
}

function useRunIdentity(runId: string): string {
  const [label, setLabel] = useState("读取中…");
  useEffect(() => {
    let cancelled = false;
    if (!runId) {
      setLabel("未记录");
      return () => { cancelled = true; };
    }
    const cached = runIdentityCache.get(runId);
    if (cached) {
      setLabel(cached);
      return () => { cancelled = true; };
    }
    api.getRun(runId)
      .then((detail) => {
        const agent = typeof detail.agent === "string" && detail.agent.trim() ? detail.agent.trim() : "";
        const resolved = agent || "运行信息不可用";
        runIdentityCache.set(runId, resolved);
        if (!cancelled) setLabel(resolved);
      })
      .catch(() => {
        runIdentityCache.set(runId, "运行信息不可用");
        if (!cancelled) setLabel("运行信息不可用");
      });
    return () => { cancelled = true; };
  }, [runId]);
  return label;
}

function DeliverableProvenance({ item }: { item: FinalDeliverable }) {
  const { avatars, conversations } = useWorkspace();
  const runLabel = useRunIdentity(item.run_id);
  const avatar = avatars.find((record) => record.id === item.lead_avatar_id);
  const avatarLabel = avatar?.display_name || avatar?.name || "已删除分身";
  const snapshotLabel = item.run_snapshot_id && item.run_snapshot_id === item.run_id ? "同一次运行" : runLabel;
  const conversation = item.conversation_id ? conversations.find((record) => record.id === item.conversation_id) : undefined;
  const conversationHref = item.project_id && item.conversation_id ? `/project/${encodeURIComponent(item.project_id)}/conversations/${encodeURIComponent(item.conversation_id)}` : "";
  return <dl className="deliverable-provenance">
    <div><dt>来源运行</dt><dd>{runLabel}</dd></div>
    <div><dt>运行</dt><dd>{snapshotLabel}</dd></div>
    <div><dt>主分身</dt><dd>{avatarLabel}</dd></div>
    {item.conversation_id && <div><dt>来源会话</dt><dd>{conversationHref ? <Link className="text-link" to={conversationHref}>{conversation?.title || item.conversation_id}</Link> : conversation?.title || item.conversation_id}</dd></div>}
  </dl>;
}

interface DeliverableBodyProps {
  state: DeliverablesState;
  projectId: string;
  conversationId?: string;
  previewOpen?: boolean;
  onImprove?(): void;
  /** 整页模式：覆盖「继续完善/改进」链接目标（默认跳回当前会话）。 */
  improveTo?: string;
}

export function DeliverableBody({ state, projectId, conversationId, previewOpen = true, onImprove, improveTo }: DeliverableBodyProps) {
  const { items, loading, notGenerated, error, refresh } = state;
  return <>
    {error && <ErrorNotice message={error} onRetry={() => void refresh()} />}
    {loading ? <LoadingView label="正在读取交付物" /> : notGenerated || !items.length ? <div className="deliverable-empty"><FileText size={20} /><div><strong>尚未生成交付物</strong><p>{conversationId ? "工作流阶段产物和最终产物会集中显示在这里。" : "该项目当前还没有交付物。"}</p></div></div> : <DeliverableList items={items} projectId={projectId} conversationId={conversationId} previewOpen={previewOpen} onImprove={onImprove} improveTo={improveTo} />}
  </>;
}

interface DeliverableListProps {
  items: FinalDeliverable[];
  projectId: string;
  conversationId?: string;
  previewOpen: boolean;
  onImprove?(): void;
  improveTo?: string;
}

function DeliverableList({ items, projectId, conversationId, previewOpen, onImprove, improveTo }: DeliverableListProps) {
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [downloadingId, setDownloadingId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  async function copy(item: FinalDeliverable) {
    try {
      await navigator.clipboard.writeText(item.content);
      setCopiedId(item.id);
      window.setTimeout(() => setCopiedId((current) => current === item.id ? null : current), 1800);
    } catch (copyError) {
      setActionError(apiErrorMessage(copyError));
    }
  }

  async function download(item: FinalDeliverable) {
    setDownloadingId(item.id);
    setActionError(null);
    try {
      const content = await api.downloadDeliverable(item.id);
      const href = URL.createObjectURL(content.blob);
      const anchor = document.createElement("a");
      anchor.href = href;
      anchor.download = content.filename;
      anchor.click();
      URL.revokeObjectURL(href);
    } catch (downloadError) {
      setActionError(apiErrorMessage(downloadError));
    } finally {
      setDownloadingId(null);
    }
  }

  const outcomeItems = items.filter((item) => artifactCategory(item) !== "evidence");
  const evidenceItems = items.filter((item) => artifactCategory(item) === "evidence");

  function renderItem(item: FinalDeliverable) {
    const category = artifactCategory(item);
    const destinationConversation = item.conversation_id || conversationId;
    const contentType = (item.content_type || "text/markdown").split(";", 1)[0].trim().toLowerCase();
    const isMarkdown = contentType === "text/markdown";
    const isHtml = contentType === "text/html";
    const looksLikeHtml = isHtml || looksLikeHtmlDocument(item.content);
    const isSvg = contentType === "image/svg+xml" || looksLikeSvgDocument(item.content);
    const artifactKind = category === "final" ? "最终成果" : category === "evidence" ? "过程资料" : "阶段成果";
    const artifactTone = category === "final" ? "success" : "neutral";
    const displayTitle = category === "evidence"
      ? (item.title || "未命名过程资料").replace(/^阶段产物/, "过程资料")
      : item.title || "未命名交付物";
    const improveLink = improveTo || (destinationConversation
      ? `/conversation?project=${encodeURIComponent(projectId)}&conversation=${encodeURIComponent(destinationConversation)}#composer`
      : `/conversation?project=${encodeURIComponent(projectId)}#composer`);
    return <Card key={item.id} className="deliverable-card" header={<div className="deliverable-card__header"><div><div className="deliverable-card__badges"><Badge tone={artifactTone}>{artifactKind}</Badge><Badge>{deliverableTypeLabel(item.content_type)}</Badge></div><h3>{displayTitle}</h3></div><time dateTime={item.created_at}>{new Intl.DateTimeFormat("zh-CN", { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }).format(new Date(item.created_at))}</time></div>} footer={<><Button variant="ghost" size="small" onClick={() => void copy(item)}>{copiedId === item.id ? <Check size={14} /> : <Clipboard size={14} />} {copiedId === item.id ? "已复制" : "复制"}</Button><Button variant="ghost" size="small" loading={downloadingId === item.id} onClick={() => void download(item)}>{downloadingId !== item.id && <Download size={14} />} 下载原文</Button>{category !== "evidence" && <Link className="ui-button ui-button--small" to={improveLink} onClick={onImprove}><MessageSquareText size={14} /> {improveTo ? "改进（新建会话）" : "继续完善"}</Link>}</>}>
      <DeliverableProvenance item={item} />
      <details className="deliverable-preview" open={previewOpen && category !== "evidence"}><summary>预览内容</summary>{isSvg ? <iframe className="deliverable-html-preview deliverable-svg-preview" sandbox="" title={displayTitle} srcDoc={item.content} /> : isMarkdown && !looksLikeHtml ? <MarkdownText text={item.content} /> : looksLikeHtml ? <iframe className="deliverable-html-preview" sandbox="" title={displayTitle} srcDoc={item.content} /> : <pre>{item.content}</pre>}</details>
    </Card>;
  }

  return <>
    {actionError && <ErrorNotice message={actionError} />}
    <div className="deliverable-list">{outcomeItems.map(renderItem)}</div>
    {evidenceItems.length > 0 && <details className="deliverable-evidence-group">
      <summary><span><strong>过程资料</strong><small>控制记录、质量检查与中间锁存，不计入交付成果</small></span><Badge>{evidenceItems.length}</Badge></summary>
      <div className="deliverable-list">{evidenceItems.map(renderItem)}</div>
    </details>}
  </>;
}

interface DeliverablePanelProps {
  projectId: string;
  conversationId?: string;
  title?: string;
  invalidationVersion?: number;
}

export function DeliverablePanel({ projectId, conversationId, title = "交付物", invalidationVersion = 0 }: DeliverablePanelProps) {
  const state = useDeliverables(projectId, conversationId, invalidationVersion);
  return <section className="deliverable-panel plain-section">
    <div className="section-heading"><div><h2>{title}</h2><p>产物创建后只读，不会随后续配置变更而改变。</p></div><Button variant="ghost" size="small" aria-label="重读交付物" onClick={() => void state.refresh()}><RefreshCw size={16} /></Button></div>
    <DeliverableBody state={state} projectId={projectId} conversationId={conversationId} />
  </section>;
}
