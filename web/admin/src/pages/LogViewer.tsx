import { Link2, Search } from 'lucide-react'
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { api } from '../lib/api'
import { readLogs, streamLabel, type LogLine } from '../lib/tasks'

const anchor = (node: string, stream: string, seq: number) => `L-${node}-${stream}-${seq}`

interface StreamSummary { node_id: string; stream: string; lines: number; last_seq: number }

const readStreams = (runId: string, nodeId: string) =>
  api<{ streams: StreamSummary[] }>(`/v1/admin/tasks/${encodeURIComponent(runId)}/logs?summary=1&node=${encodeURIComponent(nodeId)}`).then((body) => body.streams)

// Complete log of one stage. Each stream pages forward from the last line
// seen, so updates only fetch new lines.
export function LogViewer({ runId, nodeId, version, preferredStream, fallback }: { runId: string; nodeId: string; version: number; preferredStream?: string; fallback?: ReactNode }) {
  const [streams, setStreams] = useState<StreamSummary[]>([])
  const [stream, setStream] = useState(preferredStream ?? '')
  const [lines, setLines] = useState<Record<string, LogLine[]>>({})
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<LogLine[]>()
  const [copied, setCopied] = useState('')
  const body = useRef<HTMLDivElement>(null)
  const follow = useRef(true)
  const loading = useRef(false)

  useEffect(() => { setStreams([]); setLines({}); setResults(undefined) }, [runId, nodeId])
  useEffect(() => { if (preferredStream) setStream(preferredStream) }, [preferredStream])
  // Opening a line link selects its stream.
  useEffect(() => {
    const target = decodeURIComponent(window.location.hash.slice(1))
    const prefix = `L-${nodeId}-`
    if (target.startsWith(prefix)) setStream(target.slice(prefix.length).replace(/-\d+$/, ''))
  }, [nodeId])

  const known = streams.map((item) => item.stream)
  const active = stream && known.includes(stream) ? stream : known[0] ?? ''

  const load = useCallback(async () => {
    if (loading.current) return
    loading.current = true
    try {
      const summary = await readStreams(runId, nodeId).catch(() => undefined)
      if (!summary) return
      setStreams(summary)
      const current = stream && summary.some((item) => item.stream === stream) ? stream : summary[0]?.stream
      if (!current) return
      const target = summary.find((item) => item.stream === current)!
      let have = lines[current]?.at(-1)?.seq ?? 0
      const added: LogLine[] = []
      while (have < target.last_seq) {
        const page = await readLogs(runId, { node: nodeId, stream: current, after: have, limit: 2000 }).catch(() => undefined)
        if (!page || !page.lines.length) break
        added.push(...page.lines)
        have = page.lines.at(-1)!.seq
        if (page.complete) break
      }
      if (added.length) setLines((previous) => ({ ...previous, [current]: [...(previous[current] ?? []), ...added] }))
    } finally {
      loading.current = false
    }
  }, [runId, nodeId, stream, lines])
  useEffect(() => { void load() }, [version, active]) // eslint-disable-line react-hooks/exhaustive-deps

  const shown = results ? results.filter((line) => line.stream === active) : lines[active] ?? []
  useEffect(() => {
    if (follow.current && body.current && !results) body.current.scrollTop = body.current.scrollHeight
  }, [shown.length, results])
  useEffect(() => {
    const target = decodeURIComponent(window.location.hash.slice(1))
    if (!target.startsWith(`L-${nodeId}-${active}-`)) return
    const element = document.getElementById(target)
    if (element) { follow.current = false; element.scrollIntoView({ block: 'center' }); element.classList.add('is-target') }
  }, [shown.length, nodeId, active])

  const search = async () => {
    if (!query.trim()) { setResults(undefined); return }
    const response = await readLogs(runId, { node: nodeId, q: query.trim(), limit: 2000 }).catch(() => undefined)
    setResults(response?.lines ?? [])
  }
  const copyLink = async (line: LogLine) => {
    const id = anchor(line.node_id, line.stream, line.seq)
    const url = `${window.location.origin}${window.location.pathname}#${id}`
    try { await navigator.clipboard.writeText(url) } catch { /* the address bar still shows it */ }
    window.history.replaceState(null, '', `#${id}`)
    setCopied(id)
    setTimeout(() => setCopied(''), 1500)
  }
  if (!streams.length) return <>{fallback ?? null}</>
  return <div className="log">
    <div className="log__bar">
      <div className="segmented" role="group" aria-label="日志">{streams.map((item) => <button key={item.stream} type="button" aria-pressed={item.stream === active} onClick={() => setStream(item.stream)}>{streamLabel(item.stream)} <span className="muted small">{item.lines}</span></button>)}</div>
      <form className="log__search" onSubmit={(event) => { event.preventDefault(); void search() }}>
        <Search size={14} aria-hidden="true" />
        <input className="input" value={query} placeholder="搜索日志" aria-label="搜索日志" onChange={(event) => { setQuery(event.target.value); if (!event.target.value) setResults(undefined) }} />
      </form>
      {results ? <span className="muted small">{shown.length} 条匹配</span> : null}
    </div>
    <div className="log__body" ref={body} onScroll={(event) => { const element = event.currentTarget; follow.current = element.scrollTop + element.clientHeight >= element.scrollHeight - 24 }}>
      {shown.map((line) => {
        const id = anchor(line.node_id, line.stream, line.seq)
        return <div key={id} id={id} className="log__line">
          <button type="button" className="log__number" aria-label={copied === id ? '已复制链接' : `复制第 ${line.seq} 行的链接`} onClick={() => void copyLink(line)}>{copied === id ? <Link2 size={11} /> : line.seq}</button>
          <span>{line.text}</span>
        </div>
      })}
    </div>
  </div>
}
