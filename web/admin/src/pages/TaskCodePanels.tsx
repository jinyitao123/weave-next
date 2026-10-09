import { CircleCheck, CircleX, Download, ExternalLink, GitBranch, GitPullRequest } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Badge, InlineError } from '../components/ui'
import { errorMessage } from '../lib/api'
import { createPullRequest, parsePatch, shortSHA, verdictLabel, verdictReason, type CodeCommand, type TaskCode } from '../lib/environments'

export function CodeSummary({ code }: { code: TaskCode }) {
  const head = code.final?.version.head_sha
  return <span className="mono small">{code.ref} → {head ? shortSHA(head) : '尚无提交'}</span>
}

function PullRequestAction({ code, runId, done, onChanged }: { code: TaskCode; runId: string; done: boolean; onChanged(): void }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  if (code.pull_request) return <a className="button" href={code.pull_request.url} target="_blank" rel="noopener noreferrer"><ExternalLink size={14} />合并请求 #{code.pull_request.number}</a>
  const pushed = code.stages.some((stage) => stage.version.push?.status === 'pushed')
  if (!done || !pushed) return null
  const open = async () => {
    setBusy(true)
    setError('')
    try {
      await createPullRequest(runId)
      onChanged()
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  return <>
    <button type="button" className="button" disabled={busy} onClick={() => void open()}><GitPullRequest size={14} />{busy ? '正在创建…' : '创建合并请求'}</button>
    {error ? <InlineError message={error} /> : null}
  </>
}

export function CodeChanges({ code, runId, done, onChanged }: { code: TaskCode; runId: string; done: boolean; onChanged(): void }) {
  const final = code.final
  const files = useMemo(() => (code.patch ? parsePatch(code.patch) : []), [code.patch])
  const [selected, setSelected] = useState<string>()
  if (!final) return <p className="muted">还没有阶段产出代码版本。</p>
  const version = final.version
  const push = version.push
  const shown = files.find((file) => file.path === selected) ?? files[0]
  const download = () => {
    const url = URL.createObjectURL(new Blob([code.patch ?? ''], { type: 'text/x-diff' }))
    const link = document.createElement('a')
    link.href = url
    link.download = `changes-${shortSHA(version.head_sha)}.patch`
    link.click()
    URL.revokeObjectURL(url)
  }
  return <div className="stack">
    <div className="code-head">
      <span className="mono">{shortSHA(version.base_sha)} → {shortSHA(version.head_sha)}</span>
      <span className="muted small">{version.files.length} 个文件 · +{version.files.reduce((sum, file) => sum + file.added, 0)} −{version.files.reduce((sum, file) => sum + file.deleted, 0)}</span>
      {push ? <span className="small"><GitBranch size={13} aria-hidden="true" /> {push.status === 'pushed' ? `已推送到 ${push.branch}` : push.status === 'failed' ? `推送 ${push.branch} 失败` : '没有改动，未推送'}</span> : null}
      {code.patch ? <button type="button" className="button" onClick={download}><Download size={14} />下载补丁</button> : null}
      <PullRequestAction code={code} runId={runId} done={done} onChanged={onChanged} />
    </div>
    {push?.status === 'failed' && push.error ? <p className="notice">{push.error}</p> : null}
    {!version.changed ? <p className="muted">这个任务没有改动代码。</p>
      : version.patch === 'too_large' ? <p className="notice">改动超过可展示的大小，请在推送的分支上查看。</p>
      : version.patch === 'not_utf8' ? <p className="notice">改动包含非 UTF-8 文本，请在推送的分支上查看。</p>
      : <div className="diff-layout">
        <ul className="diff-files">{version.files.map((file) => <li key={file.path}>
          <button type="button" className={file.path === shown?.path ? 'is-selected' : ''} onClick={() => setSelected(file.path)}>
            <span className="mono">{file.path}</span>
            <span className="small">{file.binary ? '二进制' : <><span className="diff-add-count">+{file.added}</span> <span className="diff-del-count">−{file.deleted}</span></>}</span>
          </button>
        </li>)}</ul>
        <div className="diff-view" aria-label={shown ? `${shown.path} 的改动` : '改动'}>
          {!shown ? null : shown.binary ? <p className="muted">二进制文件</p> : shown.lines.map((line, index) => <div key={index} className={`diff-line diff-line--${line.kind}`}>
            <span aria-hidden="true">{line.kind === 'add' ? '+' : line.kind === 'del' ? '−' : line.kind === 'hunk' ? '' : ' '}</span>{line.text}
          </div>)}
        </div>
      </div>}
  </div>
}

function CommandRow({ command }: { command: CodeCommand }) {
  const passed = command.exit_code === 0 && !command.timed_out
  return <details className="command">
    <summary>
      {passed ? <CircleCheck size={14} className="icon--success" aria-hidden="true" /> : <CircleX size={14} className="icon--danger" aria-hidden="true" />}
      <span className="mono">{command.command}</span>
      <span className="muted small">{command.timed_out ? '超时' : `退出码 ${command.exit_code}`} · {(command.duration_ms / 1000).toFixed(1)} 秒</span>
    </summary>
    <div className="small muted">输出 {command.output_bytes} 字节 · 摘要 <span className="mono">{command.output_sha256.slice(0, 16)}</span></div>
    {command.output_tail ? <pre>{command.output_tail}</pre> : null}
  </details>
}

export function CodeEvidencePanel({ code }: { code: TaskCode }) {
  const evidence = code.final?.evidence
  if (!code.verify_commands.length) return <p className="muted">环境没有声明验证命令。</p>
  if (!evidence) return <p className="muted">还没有节点执行的验证记录。</p>
  return <div className="stack">
    <p className="small">验证提交 <span className="mono">{shortSHA(evidence.commit)}</span>{evidence.commit === code.final?.version.head_sha ? '（交付的提交）' : '（不是交付的提交）'}{evidence.reused_from ? ' · 提交未变化，沿用上一阶段的验证记录' : ''}</p>
    {evidence.setup ? <><h3>启动脚本</h3><CommandRow command={evidence.setup} /></> : null}
    <h3>验证命令</h3>
    <div className="stack">{evidence.commands.map((command, index) => <CommandRow key={index} command={command} />)}</div>
    <p className="muted small">命令由节点在该提交上直接执行，结果不经过模型。</p>
  </div>
}

export function CodeVerdict({ code, executionLabel, onOpenCommand }: { code: TaskCode; executionLabel: string; onOpenCommand?(nodeId: string, index: number): void }) {
  const verdict = code.verdict
  const label = verdictLabel(verdict)
  const evidence = code.final?.evidence
  return <section className={`verdict verdict--${label.tone}`} aria-label="结论">
    <h2 className="section-title">结论</h2>
    <dl className="verdict__rows">
      <div><dt>执行</dt><dd>{executionLabel}</dd></div>
      <div><dt>验证</dt><dd><Badge tone={label.tone}>{label.label}</Badge></dd></div>
      {code.final ? <div><dt>交付提交</dt><dd className="mono">{shortSHA(code.final.version.head_sha)}</dd></div> : null}
      {evidence ? <div><dt>被验证提交</dt><dd className="mono">{shortSHA(evidence.commit)}</dd></div> : null}
    </dl>
    {verdictReason(verdict.reason) ? <p className="small">{verdictReason(verdict.reason)}</p> : null}
    {evidence?.commands.length ? <ul className="check-list">{evidence.commands.map((command, index) => <li key={index}>
      {command.exit_code === 0 && !command.timed_out ? <CircleCheck size={14} className="icon--success" aria-hidden="true" /> : <CircleX size={14} className="icon--danger" aria-hidden="true" />}
      {onOpenCommand ? <button type="button" className="link-button mono small" onClick={() => onOpenCommand(evidence.reused_from || evidence.node_id, index)}>{command.command}</button> : <span className="mono small">{command.command}</span>}
    </li>)}</ul> : null}
    <p className="muted small">成员在输出里的结论不计入验证。</p>
  </section>
}
