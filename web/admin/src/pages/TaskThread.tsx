import { createUUID } from '../lib/ids'
import { ArrowUp } from 'lucide-react'
import { useEffect, useState, type KeyboardEvent } from 'react'
import { Badge, InlineError } from '../components/ui'
import { errorMessage } from '../lib/api'
import { readThread, submitFollowUp } from '../lib/environments'
import { executionLabel, executionTone, terminal } from '../lib/tasks'

type ThreadRun = Awaited<ReturnType<typeof readThread>>[number]

// The rounds of one task: the original request and its follow-ups.
export function TaskThread({ runId, status, navigate }: { runId: string; status: string; navigate(path: string): void }) {
  const [runs, setRuns] = useState<ThreadRun[]>([])
  useEffect(() => {
    void readThread(runId).then(setRuns).catch(() => setRuns([]))
  }, [runId, status])
  if (runs.length < 2) return null
  return <section aria-label="轮次">
    <h2 className="section-title">轮次</h2>
    <ol className="thread">{runs.map((run, index) => <li key={run.run_id}>
      <button type="button" className={`thread__item${run.run_id === runId ? ' is-selected' : ''}`} aria-current={run.run_id === runId ? 'page' : undefined} onClick={() => navigate(`/tasks/${encodeURIComponent(run.run_id)}`)}>
        <span className="thread__index">{index + 1}</span>
        <span className="thread__title">{run.title || '团队任务'}</span>
        <Badge tone={executionTone(run.status)}>{executionLabel(run.status, run.wait_kind)}</Badge>
      </button>
    </li>)}</ol>
  </section>
}

export function FollowUpComposer({ runId, status, navigate }: { runId: string; status: string; navigate(path: string): void }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [requestId, setRequestId] = useState(() => createUUID())
  if (!terminal(status)) return null
  const submit = async () => {
    if (!text.trim()) { setError('请描述这一轮要做的事'); return }
    setBusy(true)
    setError('')
    try {
      const result = await submitFollowUp(runId, text.trim(), requestId)
      setText('')
      setRequestId(createUUID())
      navigate(`/tasks/${encodeURIComponent(result.run_id)}`)
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) { event.preventDefault(); void submit() }
  }
  return <form className="follow-up" onSubmit={(event) => { event.preventDefault(); void submit() }}>
    <label className="sr-only" htmlFor="follow-up">跟进</label>
    <textarea id="follow-up" className="input textarea" rows={2} placeholder="继续要求，例如：修复失败的测试" value={text} onChange={(event) => { setText(event.target.value); setError('') }} onKeyDown={onKeyDown} />
    <button type="submit" className="button button--primary" disabled={busy} aria-label="提交跟进"><ArrowUp size={14} />{busy ? '正在提交…' : '跟进'}</button>
    {error ? <InlineError message={error} /> : null}
  </form>
}
