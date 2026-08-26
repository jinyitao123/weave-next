import "../styles/human-tasks.css";

import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { CheckCircle2, ClipboardCheck, Clock3, PanelLeftOpen, RefreshCw } from "lucide-react";
import { api, apiErrorMessage, normalizeThrownError, type HumanTask, type RuntimeSchema } from "../api";
import { humanTasksChangedEvent } from "../api/humanTasks";
import { createClientUUID } from "../platform/uuid";
import { Button } from "../ui/Button";
import { Field } from "../ui/Field";
import { ErrorNotice, LoadingView } from "../ui/StatusViews";
import { useOutletContext } from "react-router-dom";

function initialSchemaValue(schema: RuntimeSchema): unknown {
  if (Object.prototype.hasOwnProperty.call(schema, "const")) return schema.const;
  if (schema.enum?.length) return schema.enum[0];
  switch (schema.type) {
    case "object":
      return Object.fromEntries(Object.entries(schema.properties || {}).map(([name, child]) => [name, initialSchemaValue(child)]));
    case "array":
      return [];
    case "boolean":
      return false;
    case "integer":
    case "number":
      return 0;
    case "null":
      return null;
    default:
      return "";
  }
}

function enumLabel(value: unknown): string {
  if (value === "approve") return "通过";
  if (value === "reject") return "驳回";
  return typeof value === "string" ? value : JSON.stringify(value);
}

function JSONEditor({ label, value, onChange }: { label: string; value: unknown; onChange(value: unknown): void }) {
  const [text, setText] = useState(() => JSON.stringify(value, null, 2));
  const [error, setError] = useState<string | null>(null);
  return <Field label={label} error={error}>
    {(control) => <textarea {...control} rows={6} value={text} onChange={(event) => {
      const next = event.target.value;
      setText(next);
      try {
        onChange(JSON.parse(next));
        setError(null);
      } catch {
        setError("请输入有效 JSON");
      }
    }} />}
  </Field>;
}

function SchemaField({ name, schema, value, required, onChange }: {
  name: string;
  schema: RuntimeSchema;
  value: unknown;
  required: boolean;
  onChange(value: unknown): void;
}) {
  const label = `${name}${required ? " *" : ""}`;
  if (Object.prototype.hasOwnProperty.call(schema, "const")) {
    return <div className="human-schema-const"><span>{label}</span><code>{JSON.stringify(schema.const)}</code></div>;
  }
  if (schema.enum?.length) {
    return <Field label={label}>{(control) => <select {...control} value={JSON.stringify(value)} onChange={(event) => onChange(JSON.parse(event.target.value))}>
      {schema.enum?.map((option) => <option key={JSON.stringify(option)} value={JSON.stringify(option)}>{enumLabel(option)}</option>)}
    </select>}</Field>;
  }
  if (schema.type === "object" && Object.keys(schema.properties || {}).length) {
    const objectValue = value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {};
    const requiredNames = new Set(schema.required || []);
    return <fieldset className="human-schema-group"><legend>{label}</legend>
      {Object.entries(schema.properties || {}).map(([childName, childSchema]) => <SchemaField
        key={childName}
        name={childName}
        schema={childSchema}
        value={objectValue[childName]}
        required={requiredNames.has(childName)}
        onChange={(next) => onChange({ ...objectValue, [childName]: next })}
      />)}
    </fieldset>;
  }
  if (schema.type === "array" || schema.type === "object" || !schema.type) {
    return <JSONEditor label={label} value={value} onChange={onChange} />;
  }
  if (schema.type === "boolean") {
    return <Field label={label}>{(control) => <select {...control} value={value === true ? "true" : "false"} onChange={(event) => onChange(event.target.value === "true")}>
      <option value="true">是</option><option value="false">否</option>
    </select>}</Field>;
  }
  if (schema.type === "number" || schema.type === "integer") {
    return <Field label={label}>{(control) => <input {...control} type="number" step={schema.type === "integer" ? 1 : "any"} value={typeof value === "number" ? value : 0} onChange={(event) => onChange(Number(event.target.value))} />}</Field>;
  }
  if (schema.type === "null") {
    return <div className="human-schema-const"><span>{label}</span><code>null</code></div>;
  }
  return <Field label={label}>{(control) => name.toLowerCase().includes("comment") || name.includes("批注")
    ? <textarea {...control} rows={4} value={typeof value === "string" ? value : ""} onChange={(event) => onChange(event.target.value)} />
    : <input {...control} value={typeof value === "string" ? value : ""} onChange={(event) => onChange(event.target.value)} />}</Field>;
}

function ResumeForm({ task, busy, onComplete }: { task: HumanTask; busy: boolean; onComplete(payload: unknown): Promise<void> }) {
  const [payload, setPayload] = useState<unknown>(() => initialSchemaValue(task.resume_schema));
  const schema = task.resume_schema;
  const required = new Set(schema.required || []);
  const properties = Object.entries(schema.properties || {});

  useEffect(() => setPayload(initialSchemaValue(task.resume_schema)), [task]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    await onComplete(payload);
  }

  return <form className="human-resume-form" onSubmit={(event) => void submit(event)}>
    <h3>提交终审</h3>
    {schema.type === "object" && properties.length
      ? properties.map(([name, child]) => <SchemaField
        key={name}
        name={name}
        schema={child}
        value={(payload as Record<string, unknown> | null)?.[name]}
        required={required.has(name)}
        onChange={(next: unknown) => setPayload((current: unknown) => ({ ...(current as Record<string, unknown>), [name]: next }))}
      />)
      : <SchemaField name="payload" schema={schema} value={payload} required onChange={setPayload} />}
    <Button variant="primary" type="submit" loading={busy}>{busy ? "正在提交" : "提交并继续流程"}</Button>
  </form>;
}

function OutputPreview({ outputs }: { outputs: Record<string, unknown> }) {
  const entries = Object.entries(outputs);
  if (!entries.length) return <p className="human-output-empty">当前节点没有可预览的上游交付物。</p>;
  return <div className="human-output-list">{entries.map(([nodeID, output]) => <article key={nodeID}>
    <strong>{nodeID}</strong>
    <pre>{typeof output === "string" ? output : JSON.stringify(output, null, 2)}</pre>
  </article>)}</div>;
}

export function HumanTasksPage() {
  const { sidebarCollapsed, setSidebarCollapsed } = useOutletContext<{ sidebarCollapsed: boolean; setSidebarCollapsed(value: boolean): void }>();
  const [tasks, setTasks] = useState<HumanTask[]>([]);
  const [selectedRunID, setSelectedRunID] = useState("");
  const [nextCursor, setNextCursor] = useState("");
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [completedMessage, setCompletedMessage] = useState("");

  const selected = useMemo(() => tasks.find((task) => task.run_id === selectedRunID) || tasks[0] || null, [selectedRunID, tasks]);

  const load = useCallback(async (cursor = "") => {
    if (cursor) { setLoadingMore(true); } else { setLoading(true); }
    setError(null);
    try {
      const response = await api.listHumanTasks({ limit: 20, cursor: cursor || undefined });
      setTasks((current) => cursor ? [...current, ...response.tasks] : response.tasks);
      setNextCursor(response.next_cursor);
      setTotal(response.total);
      if (!cursor) setSelectedRunID((current) => response.tasks.some((task) => task.run_id === current) ? current : response.tasks[0]?.run_id || "");
    } catch (requestError) {
      setError(apiErrorMessage(requestError));
    } finally {
      setLoading(false);
      setLoadingMore(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function complete(payload: unknown) {
    if (!selected) return;
    setBusy(true);
    setError(null);
    setCompletedMessage("");
    try {
      await api.completeHumanTask(selected.run_id, { payload, idempotency_key: createClientUUID() });
      setTasks((current) => current.filter((task) => task.run_id !== selected.run_id));
      setTotal((current) => Math.max(0, current - 1));
      setSelectedRunID("");
      setCompletedMessage("终审已提交，流程恢复任务已进入队列。完全恢复后交付物会回到项目中。");
      window.dispatchEvent(new Event(humanTasksChangedEvent));
    } catch (requestError) {
      const normalized = normalizeThrownError(requestError);
      setError(normalized.kind === "conflict" ? "该待办已被其他成员处理，正在刷新列表。" : apiErrorMessage(normalized));
      if (normalized.kind === "conflict") void load();
    } finally {
      setBusy(false);
    }
  }

  return <div className="page human-tasks-page">
    <header className="shell-toolbar">
      {sidebarCollapsed && <button className="icon-button" type="button" aria-label="展开侧栏" onClick={() => setSidebarCollapsed(false)}><PanelLeftOpen size={16} /></button>}
      <strong className="shell-toolbar__title">人工待办</strong>
      <span className="human-task-total">{total} 项待处理</span>
      <div className="shell-toolbar__actions"><button className="icon-button" type="button" aria-label="刷新" title="刷新" onClick={() => void load()}><RefreshCw size={14} /></button></div>
    </header>
    {error && <ErrorNotice message={error} />}
    {completedMessage && <div className="notice human-task-success" role="status"><CheckCircle2 size={16} />{completedMessage}</div>}
    {loading && !tasks.length ? <LoadingView label="正在读取人工待办" /> : !tasks.length ? <div className="empty-surface human-tasks-empty">
      <ClipboardCheck size={30} /><h2>没有待处理的人工节点</h2><p>团队流程挂起等待人工决定后，会出现在这里。</p>
    </div> : <div className="human-tasks-layout">
      <nav className="human-task-list" aria-label="人工待办列表">
        {tasks.map((task) => <button key={task.run_id} type="button" aria-current={selected?.run_id === task.run_id ? "page" : undefined} onClick={() => { setSelectedRunID(task.run_id); setCompletedMessage(""); }}>
          <span><strong>{task.title}</strong><small>{task.workflow_id} · v{task.workflow_version}</small></span>
          <time dateTime={task.updated_at}>{new Intl.DateTimeFormat("zh-CN", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }).format(new Date(task.updated_at))}</time>
        </button>)}
        {nextCursor && <Button size="small" loading={loadingMore} onClick={() => void load(nextCursor)}>加载更多</Button>}
      </nav>
      {selected && <section className="human-task-detail" aria-labelledby="human-task-title">
        <header><span className="eyebrow">等待人工决定</span><h1 id="human-task-title">{selected.title}</h1><p>{selected.instructions}</p>
          {selected.deadline_at && <span className="human-task-deadline"><Clock3 size={14} />截止 {new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(new Date(selected.deadline_at))}</span>}
        </header>
        <section className="human-task-outputs"><h2>待审交付物与上下文</h2><OutputPreview outputs={selected.completed_outputs} /></section>
        <ResumeForm key={selected.run_id} task={selected} busy={busy} onComplete={complete} />
      </section>}
    </div>}
  </div>;
}
