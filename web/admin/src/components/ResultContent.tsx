import { Check, CircleAlert, Copy } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import ReactMarkdown, { type Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import './result-content.css'

const plugins = [remarkGfm]
const components: Components = {
  a: ({ href, children }) => href && /^(https?:|mailto:|#)/i.test(href)
    ? <a href={href} target="_blank" rel="noopener noreferrer">{children}</a> : <span>{children}</span>,
  img: ({ alt }) => <span>{alt || '图片'}</span>,
  table: ({ children }) => <div className="result-content__table"><table>{children}</table></div>,
}
const labels: Record<string, string> = { disposition: '处理结论', summary: '汇总', missing_items: '待补材料' }

function Markdown({ text }: { text: string }) {
  return <ReactMarkdown remarkPlugins={plugins} components={components} skipHtml>{text}</ReactMarkdown>
}

function JsonValue({ value }: { value: unknown }) {
  if (typeof value === 'string') return <Markdown text={value} />
  if (value === null) return <span className="muted">null</span>
  if (Array.isArray(value)) return value.length ? <ul className="result-content__items">{value.map((item, index) => <li key={index}><JsonValue value={item} /></li>)}</ul> : <code>[]</code>
  if (typeof value === 'object') {
    const fields = Object.entries(value)
    return fields.length ? <dl className="result-content__fields">{fields.map(([key, item]) => <div key={key}>
      <dt>{Object.hasOwn(labels, key) ? labels[key] : key}</dt><dd><JsonValue value={item} /></dd>
    </div>)}</dl> : <code>{'{}'}</code>
  }
  return <span>{String(value)}</span>
}

function parseJSON(content: string): { value: unknown } | undefined {
  const trimmed = content.trim()
  const fenced = /^```(?:json)?\s*\n([\s\S]*?)\n```$/i.exec(trimmed)
  try { return { value: JSON.parse(fenced?.[1] ?? trimmed) } } catch { return undefined }
}

export function ResultContent({ content, label = '结果', detail }: { content: string; label?: string; detail?: string }) {
  const [copyState, setCopyState] = useState<{ content: string; status: 'copied' | 'failed' }>()
  const status = copyState?.content === content ? copyState.status : undefined
  useEffect(() => {
    if (!copyState) return
    const timer = setTimeout(() => setCopyState(undefined), 2000)
    return () => clearTimeout(timer)
  }, [copyState])
  const copy = async () => {
    try { await navigator.clipboard.writeText(content); setCopyState({ content, status: 'copied' }) }
    catch { setCopyState({ content, status: 'failed' }) }
  }
  const copyLabel = status === 'copied' ? '已复制原文' : status === 'failed' ? '复制失败，点击重试' : '复制原文'
  const json = useMemo(() => parseJSON(content), [content])
  return <div className="result-content">
    <div className="artifact__head">
      <span className="mono">{label}</span>
      <div className="result-content__actions">
        {detail ? <span className="muted small">{detail}</span> : null}
        <button type="button" className="icon-button" aria-label={copyLabel} title={copyLabel} onClick={() => void copy()}>
          {status === 'copied' ? <Check size={15} /> : status === 'failed' ? <CircleAlert size={15} /> : <Copy size={15} />}
        </button>
        <span className="sr-only" role="status">{status ? copyLabel : ''}</span>
      </div>
    </div>
    <div className="result-content__body">{json ? <JsonValue value={json.value} /> : <Markdown text={content} />}</div>
  </div>
}
