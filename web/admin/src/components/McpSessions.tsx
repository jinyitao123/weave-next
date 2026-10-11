import { useCallback, useEffect, useState } from 'react'
import { errorMessage } from '../lib/api'
import { relativeTime } from '../lib/format'
import { listMcpSessions, revokeMcpSession, type McpSession } from '../lib/mcp'
import { CopyField, Dialog, InlineError } from './ui'

// The MCP clients people signed in from. Each row is one sign-in; taking it
// back ends it at once. An administrator sees the whole workspace's.
export function McpSessions() {
  const [entry, setEntry] = useState('')
  const [sessions, setSessions] = useState<McpSession[]>()
  const [error, setError] = useState('')
  const [ending, setEnding] = useState<McpSession>()
  const [busy, setBusy] = useState(false)
  const load = useCallback(async () => {
    try {
      const listed = await listMcpSessions()
      setEntry(listed.entry)
      setSessions(listed.sessions)
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [])
  useEffect(() => { void load() }, [load])
  const end = async () => {
    if (!ending) return
    setBusy(true)
    try {
      await revokeMcpSession(ending.id)
      setEnding(undefined)
      await load()
    } catch (failure) {
      setError(errorMessage(failure))
      setEnding(undefined)
    } finally {
      setBusy(false)
    }
  }
  return <section className="integration" aria-label="MCP 客户端">
    <header className="integration__header"><h2>MCP 客户端</h2></header>
    {entry ? <CopyField label="接入地址" value={entry} /> : null}
    {error ? <InlineError message={error} onRetry={() => void load()} /> : null}
    {sessions && sessions.length === 0 ? <p className="muted small">还没有客户端登录。</p> : null}
    {sessions && sessions.length > 0 ? <div className="table-wrap"><table className="table">
      <thead><tr><th>客户端</th><th>账号</th><th>登录</th><th>最近使用</th><th>到期</th><th aria-label="操作" /></tr></thead>
      <tbody>{sessions.map((session) => <tr key={session.id}>
        <td>{session.client_name}</td>
        <td>{session.user_name || '未知账号'}{session.own ? '（我）' : ''}</td>
        <td>{relativeTime(session.created_at)}</td>
        <td>{session.last_used_at ? relativeTime(session.last_used_at) : '尚未使用'}</td>
        <td>{new Date(session.expires_at).toLocaleDateString()}</td>
        <td><button type="button" className="button" onClick={() => setEnding(session)}>收回</button></td>
      </tr>)}</tbody>
    </table></div> : null}
    {ending ? <Dialog title="收回登录" onClose={() => setEnding(undefined)} footer={<>
      <button type="button" className="button" onClick={() => setEnding(undefined)}>取消</button>
      <button type="button" className="button button--danger" disabled={busy} onClick={() => void end()}>{busy ? '正在收回…' : '收回'}</button>
    </>}>
      <p>收回后，「{ending.client_name}」不能再以{ending.own ? '你' : ` ${ending.user_name || '该账号'} `}的身份使用 Weave，需要重新登录。</p>
    </Dialog> : null}
  </section>
}
