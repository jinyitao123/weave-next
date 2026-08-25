import DOMPurify from "dompurify";
import mermaid from "mermaid";
import { useEffect, useId, useRef, useState } from "react";
import { BarChart3, Box, Code2, Network, Paperclip } from "lucide-react";
import type { AssistantMessageMetadata, ContentBlock, Message, MessageAttachment } from "../../api";
import { Badge } from "../../ui/Badge";
import { Card } from "../../ui/Card";
import { DispatchExecutionProcess } from "./ExecutionProcess";
import { MarkdownText } from "./MarkdownText";

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

const FORBIDDEN_DIAGRAM_TAGS = ["script", "foreignObject", "foreignobject", "iframe", "object", "embed", "link"];

type DiagramState =
  | { status: "loading" }
  | { status: "ready"; markup: string }
  | { status: "error" };

mermaid.initialize({
  startOnLoad: false,
  securityLevel: "strict",
});

function sanitizeSvg(source: string): string {
  const parsed = new DOMParser().parseFromString(source, "image/svg+xml");
  if (parsed.querySelector("parsererror") || parsed.documentElement.localName.toLowerCase() !== "svg") {
    throw new Error("Invalid SVG");
  }

  const sanitized = String(DOMPurify.sanitize(source, {
    USE_PROFILES: { svg: true, svgFilters: true },
    FORBID_TAGS: FORBIDDEN_DIAGRAM_TAGS,
  }));
  const sanitizedDocument = new DOMParser().parseFromString(sanitized, "image/svg+xml");
  if (
    sanitizedDocument.querySelector("parsererror")
    || sanitizedDocument.documentElement.localName.toLowerCase() !== "svg"
    || sanitizedDocument.querySelector(FORBIDDEN_DIAGRAM_TAGS.join(","))
  ) {
    throw new Error("Unsafe SVG");
  }
  return sanitized;
}

function persistedAttachments(metadata: Record<string, unknown>): MessageAttachment[] {
  if (!Array.isArray(metadata.attachments)) return [];
  return metadata.attachments.flatMap((item) => {
    if (!isRecord(item) || typeof item.id !== "string" || !item.id.trim() || typeof item.filename !== "string" || !item.filename.trim()) return [];
    return [{ id: item.id, filename: item.filename }];
  });
}

function ChartBlock({ block }: { block: ContentBlock }) {
  const labels = block.data?.labels || [];
  const datasets = block.data?.datasets || [];
  const maximum = Math.max(1, ...datasets.flatMap(({ values }) => values.map((value) => Math.abs(value))));
  return <Card className="content-block content-block--chart" header={<><BarChart3 size={16} /> {block.title || `${block.chart_type || "bar"} 图表`}</>}><div className="chart-table">{labels.map((label, index) => <div className="chart-row" key={`${label}-${index}`}><strong>{label}</strong><div>{datasets.map((dataset) => { const value = dataset.values[index] ?? 0; return <span className="chart-series" key={dataset.label}><i style={{ width: `${Math.max(2, Math.abs(value) / maximum * 100)}%` }} /><small>{dataset.label || "数值"} {value}</small></span>; })}</div></div>)}</div></Card>;
}

function DiagramBlock({ block }: { block: ContentBlock }) {
  const reactId = useId();
  const renderBaseId = `weave-diagram-${reactId.replace(/[^a-zA-Z0-9_-]/g, "-")}`;
  const requestRef = useRef(0);
  const [state, setState] = useState<DiagramState>({ status: "loading" });
  const format = block.format?.toLowerCase() || "";
  const source = block.source || "";
  const title = block.title || (format === "mermaid" ? "Mermaid 图" : format === "svg" ? "SVG 图" : "图示");
  const accessibleLabel = block.caption || title;

  useEffect(() => {
    const request = ++requestRef.current;
    let active = true;
    setState({ status: "loading" });

    void (async () => {
      try {
        if (!source.trim()) throw new Error("Empty diagram source");

        let markup: string;
        if (format === "mermaid") {
          const result = await mermaid.render(`${renderBaseId}-${request}`, source);
          markup = sanitizeSvg(result.svg);
        } else if (format === "svg") {
          markup = sanitizeSvg(source);
        } else {
          throw new Error("Unsupported diagram format");
        }

        if (active && request === requestRef.current) setState({ status: "ready", markup });
      } catch {
        if (active && request === requestRef.current) setState({ status: "error" });
      }
    })();

    return () => {
      active = false;
    };
  }, [format, renderBaseId, source]);

  return <Card className="content-block content-block--diagram" header={<><Network size={16} /><span><strong>{title}</strong>{block.caption && block.caption !== title && <small>{block.caption}</small>}</span></>}>
    {state.status === "loading" && <p className="diagram-status">正在渲染图示…</p>}
    {state.status === "ready" && <div className="diagram-canvas" role="img" aria-label={accessibleLabel} tabIndex={0} dangerouslySetInnerHTML={{ __html: state.markup }} />}
    {state.status === "error" && <>
      <p className="diagram-error" role="alert">图示无法渲染。请检查源码格式。</p>
      <details className="diagram-source-fallback"><summary>查看图示源码</summary><pre><code>{source}</code></pre></details>
    </>}
  </Card>;
}

function StructuredBlock({ block }: { block: ContentBlock }) {
  if (block.type === "text") return <MarkdownText text={block.text || ""} />;
  if (block.type === "code") return <Card className="content-block content-block--code" header={<><Code2 size={14} /> {block.filename || block.language || "代码"}</>}><pre><code>{block.code || ""}</code></pre></Card>;
  if (block.type === "table") return <div className="content-table" role="region" aria-label="数据表格" tabIndex={0}><table><thead><tr>{(block.headers || []).map((header, index) => <th key={index}>{header}</th>)}</tr></thead><tbody>{(block.rows || []).map((row, rowIndex) => <tr key={rowIndex}>{row.map((cell, cellIndex) => <td key={cellIndex}>{cell}</td>)}</tr>)}</tbody></table></div>;
  if (block.type === "chart") return <ChartBlock block={block} />;
  if (block.type === "diagram") return <DiagramBlock block={block} />;
  if (block.type === "component") {
    const title = typeof block.props?.title === "string" ? block.props.title : block.component_type || "组件";
    const text = typeof block.props?.text === "string" ? block.props.text : typeof block.props?.message === "string" ? block.props.message : "";
    const metrics = Array.isArray(block.props?.metrics) ? block.props.metrics.filter(isRecord) : [];
    return <Card className="content-block component-block" header={<><Box size={16} /><strong>{title}</strong><Badge>{block.component_type}</Badge></>}>{text && <p>{text}</p>}{metrics.length > 0 && <dl>{metrics.map((metric, index) => <div key={index}><dt>{String(metric.label || metric.name || "指标")}</dt><dd>{String(metric.value ?? "—")}</dd></div>)}</dl>}<details><summary>查看组件数据</summary><pre><code>{JSON.stringify(block.props || {}, null, 2)}</code></pre></details></Card>;
  }
  return <Card className="content-block" header={<><Box size={16} /><strong>暂不支持展示的内容</strong></>}><p>这段内容的格式当前版本无法展示。</p><details><summary>查看原始数据</summary><pre><code>{JSON.stringify(block, null, 2)}</code></pre></details></Card>;
}

export function MessageContent({ message }: { message: Message }) {
  const metadata: AssistantMessageMetadata & Record<string, unknown> = isRecord(message.metadata) ? message.metadata : {};
  const blocks = Array.isArray(metadata.blocks) ? metadata.blocks : [];
  const attachments = persistedAttachments(metadata);
  const eventType = typeof metadata.type === "string" ? metadata.type : "event";
  const dispatchGroupID = eventType === "dispatch_card" && typeof metadata.group_id === "string" ? metadata.group_id : "";
  const dispatchLegs = Array.isArray(metadata.legs) ? metadata.legs.filter(isRecord).map((leg) => ({
    agent: typeof leg.agent === "string" ? leg.agent : undefined,
    worker: typeof leg.worker === "string" ? leg.worker : undefined,
    status: typeof leg.status === "string" ? leg.status : undefined,
  })) : [];
  return <>
    {message.role === "event" ? (
      dispatchGroupID ? <DispatchExecutionProcess groupId={dispatchGroupID} terminal={metadata.terminal === true} revision={typeof metadata.revision === "number" ? metadata.revision : 0} fallbackLegs={dispatchLegs} />
        : <div className="event-content"><strong>系统事件</strong><p>{message.content}</p>{Object.keys(metadata).length > 0 && <details><summary>查看事件详情</summary><pre>{JSON.stringify(metadata, null, 2)}</pre></details>}</div>
    ) : blocks.length ? blocks.map((block, index) => <StructuredBlock key={index} block={block} />) : <MarkdownText text={message.content} />}
    {attachments.length > 0 && <ul className="message-attachments" aria-label="消息附件">
      {attachments.map((attachment) => <li className="message-attachment" key={attachment.id}><Paperclip size={14} aria-hidden="true" /><span>{attachment.filename}</span></li>)}
    </ul>}
  </>;
}
