import { Plus, RefreshCw } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { Badge, CopyField, Dot, Drawer, EmptyState, InlineError } from '../components/ui'
import { errorMessage, type AdminSession } from '../lib/api'
import { authModeText, engineName, reasonText, relativeTime } from '../lib/format'
import { createNode, deleteNode, installCommands, listNodes, probeNode, readNode, renameNode, rotateNodeToken, setNodePaused, type RuntimeNode } from '../lib/nodes'
import { usePolling } from '../lib/polling'

const canManage = (session: AdminSession) => session.role === 'admin' || session.role === 'owner'

export function NodesPage({ session }: { session: AdminSession }) {
  const [nodes, setNodes] = useState<RuntimeNode[]>()
  const [error, setError] = useState('')
  const [connecting, setConnecting] = useState(false)
  const [selected, setSelected] = useState<string>()
  const refresh = useCallback(async () => {
    try {
      setNodes(await listNodes())
      setError('')
    } catch (failure) {
      setError(errorMessage(failure))
    }
  }, [])
  usePolling(refresh, 5000)

  const accepting = nodes?.filter((node) => node.accepting).length ?? 0
  const online = nodes?.filter((node) => node.online).length ?? 0
  const current = nodes?.find((node) => node.id === selected)
  return <section className="page">
    <header className="page__header">
      <div><h1>节点</h1>{nodes?.length ? <p className="muted">{accepting} 个可接任务 · {online} 个在线 · 共 {nodes.length} 个</p> : null}</div>
      <div className="toolbar">
        <button type="button" className="icon-button" aria-label="刷新" onClick={() => void refresh()}><RefreshCw size={15} /></button>
        {canManage(session) ? <button type="button" className="button button--primary" onClick={() => setConnecting(true)}><Plus size={15} />接入节点</button> : null}
      </div>
    </header>
    {error ? <InlineError message={error} onRetry={() => void refresh()} /> : null}
    {!nodes ? null : nodes.length === 0 ? <EmptyState title="还没有接入节点" action={canManage(session) ? <button type="button" className="button button--primary" onClick={() => setConnecting(true)}><Plus size={15} />接入节点</button> : undefined}>
      节点上运行 Claude、Codex 等引擎，团队任务由它们执行。
    </EmptyState> : <div className="table-wrap"><table className="table">
      <thead><tr><th>节点</th><th>状态</th><th>引擎</th><th>槽位</th></tr></thead>
      <tbody>{nodes.map((node) => <tr key={node.id} className="table__row" tabIndex={0} onClick={() => setSelected(node.id)} onKeyDown={(event) => { if (event.key === 'Enter') setSelected(node.id) }}>
        <td><strong>{node.name}</strong><div className="muted small">心跳 {relativeTime(node.last_heartbeat_at)}</div></td>
        <td><NodeStatus node={node} /></td>
        <td><div className="chips">{node.engine_readiness.length ? node.engine_readiness.map((engine) => <Badge key={engine.engine} tone={engine.accepting ? 'success' : 'neutral'} title={engine.accepting ? '可接任务' : reasonText(engine.reason)}>
          {engineName(engine.engine)}{engine.accepting ? '' : ` · ${reasonText(engine.reason)}`}
        </Badge>) : <span className="muted">未上报</span>}</div></td>
        <td>{node.active_slots}/{node.total_slots}</td>
      </tr>)}</tbody>
    </table></div>}
    {connecting ? <ConnectDrawer onConnected={refresh} onClose={() => { setConnecting(false); void refresh() }} /> : null}
    {current ? <NodeDrawer node={current} manage={canManage(session)} onClose={() => setSelected(undefined)} onChanged={refresh} /> : null}
  </section>
}

function NodeStatus({ node }: { node: RuntimeNode }) {
  const busy = node.accepting && node.active_slots >= node.total_slots
  return <div className="status-stack">
    <Dot on={node.online} label={node.online ? '在线' : '离线'} />
    {node.accepting ? <Badge tone={busy ? 'warning' : 'success'}>{busy ? '可接任务 · 忙碌' : '可接任务'}</Badge>
      : <Badge>{!node.enabled ? '已停用' : node.paused ? '已暂停' : node.health_status === 'quarantined' ? '已隔离' : '不可接任务'}</Badge>}
  </div>
}

function Commands({ token }: { token: string }) {
  const commands = installCommands(window.location.origin, token)
  return <div className="stack">
    <CopyField label="macOS / Linux" value={commands.unix} />
    <CopyField label="Windows PowerShell" value={commands.windows} />
    <CopyField label="已安装 weave 时直接运行" value={commands.direct} />
    <p className="muted small">令牌只显示这一次。</p>
  </div>
}

function ConnectDrawer({ onClose, onConnected }: { onClose(): void; onConnected(): Promise<void> }) {
  const [name, setName] = useState('')
  const [created, setCreated] = useState<{ id: string; token: string }>()
  const [node, setNode] = useState<RuntimeNode>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const create = async () => {
    if (!name.trim()) {
      setError('请输入节点名称')
      return
    }
    setBusy(true)
    setError('')
    try {
      const result = await createNode(name.trim())
      setCreated({ id: result.id, token: result.token })
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
    }
  }
  const poll = useCallback(async () => {
    if (created) setNode(await readNode(created.id).catch(() => undefined))
  }, [created])
  usePolling(poll, 2000, Boolean(created) && !node?.online)
  const connected = Boolean(node?.online)
  useEffect(() => { if (connected) void onConnected() }, [connected, onConnected])

  return <Drawer title="接入节点" onClose={onClose}>
    {!created ? <form className="stack" onSubmit={(event) => { event.preventDefault(); void create() }}>
      <label className="field"><span>节点名称</span><input className="input" value={name} onChange={(event) => setName(event.target.value)} placeholder="mac-studio-1" autoFocus /></label>
      {error ? <InlineError message={error} /> : null}
      <button type="submit" className="button button--primary" disabled={busy}>{busy ? '正在创建…' : '创建并获取安装命令'}</button>
    </form> : <div className="stack">
      <p>在目标机器上运行其中一条命令：</p>
      <Commands token={created.token} />
      <div className="connect-status" role="status">
        {!node?.online ? <><span className="spinner" aria-hidden="true" />等待节点连接…</> : <>
          <strong>节点已连接</strong>
          <EngineTable node={node} />
        </>}
      </div>
    </div>}
  </Drawer>
}

function EngineTable({ node }: { node: RuntimeNode }) {
  if (!node.engine_readiness.length) return <p className="muted">节点尚未上报引擎。</p>
  return <table className="table table--compact">
    <thead><tr><th>引擎</th><th>版本</th><th>登录方式</th><th>状态</th></tr></thead>
    <tbody>{node.engine_readiness.map((engine) => <tr key={engine.engine}>
      <td>{engineName(engine.engine)}</td>
      <td className="mono">{engine.binary_version || '—'}</td>
      <td>{authModeText(engine.auth_mode)}</td>
      <td>{engine.accepting ? <Badge tone="success">可接任务</Badge> : <Badge>{reasonText(engine.reason)}</Badge>}</td>
    </tr>)}</tbody>
  </table>
}

function NodeDrawer({ node, manage, onClose, onChanged }: { node: RuntimeNode; manage: boolean; onClose(): void; onChanged(): Promise<void> }) {
  const [name, setName] = useState(node.name)
  const [token, setToken] = useState<string>()
  const [confirm, setConfirm] = useState<'rotate' | 'delete'>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  useEffect(() => { setName(node.name) }, [node.name])
  const run = async (action: () => Promise<unknown>, close = false) => {
    setBusy(true)
    setError('')
    try {
      await action()
      await onChanged()
      if (close) onClose()
    } catch (failure) {
      setError(errorMessage(failure))
    } finally {
      setBusy(false)
      setConfirm(undefined)
    }
  }
  return <Drawer title={node.name} onClose={onClose} footer={manage ? <div className="toolbar">
    <button type="button" className="button" disabled={busy} onClick={() => void run(() => setNodePaused(node.id, !node.paused))}>{node.paused ? '恢复接任务' : '暂停接任务'}</button>
    <button type="button" className="button" disabled={busy || !node.online || node.probe_pending} onClick={() => void run(() => probeNode(node.id))}>{node.probe_pending ? '正在重新检测…' : '重新检测引擎'}</button>
    <button type="button" className="button" disabled={busy} onClick={() => setConfirm('rotate')}>轮换令牌</button>
    <button type="button" className="button button--danger" disabled={busy} onClick={() => setConfirm('delete')}>删除</button>
  </div> : undefined}>
    <div className="stack">
      <dl className="facts">
        <div><dt>状态</dt><dd><NodeStatus node={node} /></dd></div>
        {node.paused ? <div><dt>暂停</dt><dd>不接新任务，进行中的任务继续完成</dd></div> : null}
        <div><dt>槽位</dt><dd>{node.active_slots}/{node.total_slots}</dd></div>
        <div><dt>最近心跳</dt><dd>{relativeTime(node.last_heartbeat_at)}</dd></div>
        {node.last_failure_reason ? <div><dt>最近故障</dt><dd>{node.health_status === 'quarantined' ? '连续执行环境故障' : '执行环境故障'}</dd></div> : null}
      </dl>
      <h3>引擎</h3>
      <EngineTable node={node} />
      {manage ? <form className="inline-form" onSubmit={(event) => { event.preventDefault(); if (name.trim() && name.trim() !== node.name) void run(() => renameNode(node.id, name.trim())) }}>
        <label className="field"><span>名称</span><input className="input" value={name} onChange={(event) => setName(event.target.value)} /></label>
        <button type="submit" className="button" disabled={busy || !name.trim() || name.trim() === node.name}>保存</button>
      </form> : null}
      {confirm === 'rotate' ? <div className="confirm" role="alertdialog" aria-label="确认轮换令牌">
        <p>旧令牌立即失效，节点需用新命令重新连接。</p>
        <div className="toolbar"><button type="button" className="button" onClick={() => setConfirm(undefined)}>取消</button>
          <button type="button" className="button button--primary" disabled={busy} onClick={() => void run(async () => setToken((await rotateNodeToken(node.id)).token))}>轮换</button></div>
      </div> : null}
      {confirm === 'delete' ? <div className="confirm" role="alertdialog" aria-label="确认删除节点">
        <p>删除后令牌失效，节点不能再接入，此操作不能撤销。</p>
        <div className="toolbar"><button type="button" className="button" onClick={() => setConfirm(undefined)}>取消</button>
          <button type="button" className="button button--danger" disabled={busy} onClick={() => void run(() => deleteNode(node.id), true)}>删除</button></div>
      </div> : null}
      {token ? <Commands token={token} /> : null}
      {error ? <InlineError message={error} /> : null}
    </div>
  </Drawer>
}
