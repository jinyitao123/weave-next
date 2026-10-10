import { Bot, CheckCircle2, FileInput, GitBranch, GitMerge, Maximize2, Repeat2, RotateCcw, UsersRound, ZoomIn, ZoomOut } from 'lucide-react'
import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from 'react'
import type { Graph } from '../../lib/graph'
import type { DevelopmentMember } from '../../lib/teams'
import type { StepFeeds } from '../../lib/workflow-graph'

export const stepLabel = (type: string): string => ({ lead: '负责人处理', worker: '成员执行', parallel: '并行分工', join: '汇合', deliver: '交付结果', loop: '验证回路', condition: '条件分支', wait: '等待', transform: '转换', handoff: '交接' }[type] ?? '流程步骤')
const width = 172, height = 64, column = width + 24, row = height + 32
// Input references run to the left of the vertical flow, outside the nodes.
const lane = 64
const pill = { y: 8, width: 104, height: 24 }

// A loop leaves through its exit only after the latch step, so the exit
// target is ordered behind the latch instead of beside the loop body.
function exitAfterLatch(graph: Graph) {
  return graph.edges.flatMap(edge => {
    const loop = graph.nodes.find(node => node.id === edge.from_node_id)
    const latch = loop?.type === 'loop' && edge.route === 'exit' ? String(loop.config?.latch_node_id ?? '') : ''
    return latch ? [{ from_node_id: latch, to_node_id: edge.to_node_id }] : []
  })
}

export function loopRounds(graph: Graph, id: string): number | undefined {
  const loop = graph.nodes.find(node => node.id === id && node.type === 'loop')
  return loop ? Number(loop.config?.max_iterations) || 1 : undefined
}

// The main flow runs downward; parallel branches share a row.
// Back edges keep a separate lane on the right.
export function layout(graph: Graph) {
  const forward = [...graph.edges.filter(edge => edge.route !== 'back'), ...exitAfterLatch(graph)]
  const remaining = new Set(graph.nodes.map(node => node.id)), levels: Graph['nodes'][] = []
  while (remaining.size) {
    let nodes = graph.nodes.filter(node => remaining.has(node.id) && !forward.some(edge => edge.to_node_id === node.id && remaining.has(edge.from_node_id)))
    if (!nodes.length) nodes = graph.nodes.filter(node => remaining.has(node.id)).slice(0, 1)
    levels.push(nodes); nodes.forEach(node => remaining.delete(node.id))
  }
  const columns = Math.max(1, ...levels.map(level => level.length))
  const top = 56
  const positions = new Map<string, { x: number; y: number }>()
  levels.forEach((level, index) => level.forEach((node, branch) => positions.set(node.id, { x: lane + (columns - level.length) * column / 2 + branch * column, y: top + index * row })))
  const bottom = top + Math.max(0, levels.length - 1) * row + height
  const hasBack = graph.edges.some(edge => edge.route === 'back')
  const backLane = lane + (columns - 1) * column + width + 28
  return { width: backLane + (hasBack ? 32 : 12), height: bottom + 24, positions, backLane }
}

const minScale = 0.25, maxScale = 2.5
type Viewport = { x: number; y: number; scale: number }

// Start at readable size. Fitting the whole graph is an explicit overview action.
export function FlowCanvas({ graph, members, selected, feeds, highlight, onSelect }: {
  graph: Graph
  members: DevelopmentMember[]
  selected?: string
  /** What the selected step receives; drawn as arcs and tags. */
  feeds?: StepFeeds
  /** A source being pointed at in the inspector: 'task' or a node id. */
  highlight?: string
  onSelect(id: string): void
}) {
  const box = layout(graph), arrow = useId().replace(/:/g, '')
  const task = { ...pill, x: (box.width - pill.width) / 2 }
  const canvas = useRef<HTMLDivElement>(null)
  const pan = useRef<{ x: number; y: number; left: number; top: number } | undefined>(undefined)
  const initialized = useRef(false)
  const [panning, setPanning] = useState(false)
  const [view, setView] = useState<Viewport>({ x: 0, y: 0, scale: 1 })
  const [overview, setOverview] = useState(false)
  useLayoutEffect(() => {
    const element = canvas.current
    if (!element) return
    const measure = () => {
      if (initialized.current || !element.clientWidth || !element.clientHeight) return
      initialized.current = true
      setView({ x: (element.clientWidth - box.width) / 2, y: Math.max(12, (element.clientHeight - box.height) / 2), scale: 1 })
    }
    measure()
    if (typeof ResizeObserver !== 'function') return
    const observer = new ResizeObserver(measure)
    observer.observe(element)
    return () => observer.disconnect()
  }, [box.width, box.height])
  const zoomAt = useCallback((change: (scale: number) => number, point?: { x: number; y: number }) => {
    const element = canvas.current
    if (!element) return
    const at = point ?? { x: element.clientWidth / 2, y: element.clientHeight / 2 }
    setView(current => {
      const scale = Math.max(minScale, Math.min(maxScale, change(current.scale))), ratio = scale / current.scale
      return { scale, x: at.x - (at.x - current.x) * ratio, y: at.y - (at.y - current.y) * ratio }
    })
    setOverview(false)
  }, [])
  useEffect(() => {
    const element = canvas.current
    if (!element) return
    const wheel = (event: WheelEvent) => {
      event.preventDefault()
      const rect = element.getBoundingClientRect()
      const delta = event.deltaY * (event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? element.clientHeight : 1)
      zoomAt(scale => scale * Math.exp(-delta * 0.002), { x: event.clientX - rect.left, y: event.clientY - rect.top })
    }
    element.addEventListener('wheel', wheel, { passive: false })
    return () => element.removeEventListener('wheel', wheel)
  }, [zoomAt])
  const center = (fit = false) => {
    const element = canvas.current
    if (!element) return
    const scale = fit && element.clientWidth && element.clientHeight
      ? Math.max(minScale, Math.min(1, (element.clientWidth - 24) / box.width, (element.clientHeight - 24) / box.height)) : 1
    setView({ scale, x: element.clientWidth ? (element.clientWidth - box.width * scale) / 2 : 0, y: element.clientHeight ? Math.max(12, (element.clientHeight - box.height * scale) / 2) : 0 })
    setOverview(fit)
  }
  const scale = view.scale
  const backs = graph.edges.flatMap(edge => {
    const from = box.positions.get(edge.from_node_id), to = box.positions.get(edge.to_node_id)
    if (!from || !to || !(edge.route === 'back' || to.y <= from.y)) return []
    const rounds = loopRounds(graph, edge.to_node_id)
    return [{ edge, fx: from.x + width, tx: to.x + width + 2, fy: from.y + height / 2, ty: to.y + height / 2, label: rounds ? `未通过退回 · 最多 ${rounds} 轮` : '' }]
  })
  const target = selected ? box.positions.get(selected) : undefined
  const sources = target ? [
    ...(feeds?.task ? [{ key: 'task', fx: task.x + task.width / 2, fy: task.y + task.height, from: undefined as { x: number; y: number } | undefined }] : []),
    ...(feeds?.nodes ?? []).flatMap(id => { const from = box.positions.get(id); return from && id !== selected ? [{ key: id, fx: from.x + width / 2, fy: from.y, from }] : [] }),
  ] : []
  // Rise into the gap above a source, then use a separate left-hand lane.
  // This keeps a reference from crossing its source's parallel neighbour.
  const arcs = target ? sources.map((arc, index) => {
    const track = 14 + index * Math.min(6, 44 / Math.max(1, sources.length - 1))
    const tx = target.x + width * (index + 1) / (sources.length + 1)
    if (!arc.from && target.y === 56) return { key: arc.key, d: `M ${arc.fx} ${arc.fy} C ${arc.fx} 44, ${tx} 44, ${tx} ${target.y - 2}` }
    return { key: arc.key, d: `M ${arc.fx} ${arc.fy} V ${arc.fy + (arc.from ? -12 : 10)} H ${track} V ${target.y - 16} H ${tx} V ${target.y - 2}` }
  }) : []
  return <div className="flow-editor__stage">
    <div className="flow-editor__zoom" role="group" aria-label="画布缩放">
      <button type="button" className="icon-button" aria-label="缩小" disabled={scale <= minScale + 0.001} onClick={() => zoomAt(current => current / 1.25)}><ZoomOut size={15} /></button>
      <span className="flow-editor__zoom-value" aria-live="polite">{Math.round(scale * 100)}%</span>
      <button type="button" className="icon-button" aria-label="放大" disabled={scale >= maxScale - 0.001} onClick={() => zoomAt(current => current * 1.25)}><ZoomIn size={15} /></button>
      <button type="button" className="icon-button" aria-label="查看全图" title="查看全图" aria-pressed={overview} onClick={() => center(true)}><Maximize2 size={15} /></button>
      <button type="button" className="icon-button" aria-label="重置视图" title="重置视图" onClick={() => center()}><RotateCcw size={15} /></button>
    </div>
    <div ref={canvas} className={`flow-editor__canvas${panning ? ' is-panning' : ''}`} role="region" aria-label="流程画布" tabIndex={0}
      style={{ backgroundPosition: `${view.x}px ${view.y}px` }}
      onKeyDown={event => {
        if (event.target !== event.currentTarget) return
        const directions: Record<string, [number, number]> = { ArrowLeft: [40, 0], ArrowRight: [-40, 0], ArrowUp: [0, 40], ArrowDown: [0, -40] }
        if (directions[event.key]) { const [x, y] = directions[event.key]; setView(current => ({ ...current, x: current.x + x, y: current.y + y })); setOverview(false) }
        else if (event.key === '+' || event.key === '=') zoomAt(current => current * 1.25)
        else if (event.key === '-') zoomAt(current => current / 1.25)
        else if (event.key === 'Home') center()
        else return
        event.preventDefault()
      }}
      onPointerDown={event => {
        if (event.button !== 0 || (event.target as Element).closest('button')) return
        const element = event.currentTarget
        pan.current = { x: event.clientX, y: event.clientY, left: view.x, top: view.y }
        setOverview(false)
        element.setPointerCapture?.(event.pointerId); setPanning(true); event.preventDefault()
      }}
      onPointerMove={event => { const start = pan.current; if (start) setView(current => ({ ...current, x: start.left + event.clientX - start.x, y: start.top + event.clientY - start.y })) }}
      onPointerUp={() => { pan.current = undefined; setPanning(false) }}
      onPointerCancel={() => { pan.current = undefined; setPanning(false) }}>
    <div className="flow-editor__sizer">
    <div className="flow-editor__graph" style={{ width: box.width, height: box.height, transform: `translate(${view.x}px, ${view.y}px) scale(${scale})` }}>
      <svg width={box.width} height={box.height} aria-hidden="true"><defs>
        <marker id={arrow} viewBox="0 0 8 8" refX="7" refY="4" markerWidth="6" markerHeight="6" orient="auto"><path d="M0 0L8 4L0 8Z" /></marker>
        <marker id={`${arrow}-feed`} viewBox="0 0 8 8" refX="7" refY="4" markerWidth="6" markerHeight="6" orient="auto"><path className="is-feed-head" d="M0 0L8 4L0 8Z" /></marker>
      </defs>
        {graph.edges.map((edge, index) => {
          const from = box.positions.get(edge.from_node_id), to = box.positions.get(edge.to_node_id)
          if (!from || !to || backs.some(back => back.edge === edge)) return null
          const sx = from.x + width / 2, sy = from.y + height, ex = to.x + width / 2, ey = to.y - 2
          return <path key={edge.id || index} d={`M ${sx} ${sy} C ${sx} ${(sy + ey) / 2}, ${ex} ${(sy + ey) / 2}, ${ex} ${ey}`} markerEnd={`url(#${arrow})`} />
        })}
        {backs.map(({ edge, fx, tx, fy, ty }, index) => <path key={edge.id || `back-${index}`} d={`M ${fx} ${fy} H ${box.backLane} V ${ty} H ${tx}`} className="is-back" markerEnd={`url(#${arrow})`} />)}
        {arcs.map(arc => <path key={`feed-${arc.key}`} d={arc.d} className={`is-feed${highlight === arc.key ? ' is-hot' : ''}`} markerEnd={`url(#${arrow}-feed)`} />)}
      </svg>
      {feeds?.task && target ? <span className={`flow-editor__task${highlight === 'task' ? ' is-hot' : ''}`} style={{ left: task.x, top: task.y, width: task.width, height: task.height }}><FileInput size={13} />任务输入</span> : null}
      {backs.filter(back => back.label).map(({ edge, fy, ty, label }, index) => <span key={edge.id || `label-${index}`} className="flow-editor__edge-label" style={{ left: box.backLane, top: (fy + ty) / 2 }}>{label}</span>)}
      {graph.nodes.map(node => {
        const position = box.positions.get(node.id)!
        const member = node.type === 'lead' ? members.find(item => item.configuration.role === 'avatar' && item.relationship.enabled) : members.find(item => item.id === node.config?.agent_id)
        const Icon = node.type === 'deliver' ? CheckCircle2 : node.type === 'lead' ? UsersRound : node.type === 'parallel' ? GitBranch : node.type === 'join' ? GitMerge : node.type === 'loop' ? Repeat2 : Bot
        const title = node.label || stepLabel(node.type), subtitle = member?.configuration.display_name || stepLabel(node.type)
        const feeding = selected !== node.id && Boolean(feeds?.nodes.includes(node.id))
        const classes = ['flow-editor__node', selected === node.id ? 'is-selected' : '', feeding ? 'is-feeding' : '', highlight === node.id ? 'is-hot' : ''].filter(Boolean).join(' ')
        return <button type="button" key={node.id} style={{ left: position.x, top: position.y, width, height }} className={classes} aria-label={`编辑步骤 ${title}`} aria-pressed={selected === node.id} onClick={() => onSelect(node.id)}>
          <Icon size={17} /><span><strong>{title}</strong>{subtitle !== title ? <small>{subtitle}</small> : null}</span>
          {feeding ? <em className="flow-editor__tag">输入</em> : null}
        </button>
      })}
    </div>
    </div>
    </div>
  </div>
}
