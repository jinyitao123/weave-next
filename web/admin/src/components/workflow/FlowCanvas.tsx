import { Bot, CheckCircle2, FileInput, GitBranch, GitMerge, Maximize2, Repeat2, UsersRound, ZoomIn, ZoomOut } from 'lucide-react'
import { useId, useLayoutEffect, useRef, useState } from 'react'
import type { Graph } from '../../lib/graph'
import type { DevelopmentMember } from '../../lib/teams'
import type { StepFeeds } from '../../lib/workflow-graph'

export const stepLabel = (type: string): string => ({ lead: '负责人处理', worker: '成员执行', parallel: '并行分工', join: '汇合', deliver: '交付结果', loop: '验证回路', condition: '条件分支', wait: '等待', transform: '转换', handoff: '交接' }[type] ?? '流程步骤')
const width = 172, height = 60, column = width + 48, row = height + 36
// Above the first row: the task pill and the arcs that show what the selected
// step receives. Always reserved, so selecting a step never moves the nodes.
const lane = 52
const pill = { x: 24, y: 12, width: 104, height: 26 }

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

// GooeyPi's layered automatic layout, transposed for the console's wide canvas.
// Back edges run in a lane below the nodes.
export function layout(graph: Graph) {
  const forward = [...graph.edges.filter(edge => edge.route !== 'back'), ...exitAfterLatch(graph)]
  const remaining = new Set(graph.nodes.map(node => node.id)), levels: Graph['nodes'][] = []
  while (remaining.size) {
    let nodes = graph.nodes.filter(node => remaining.has(node.id) && !forward.some(edge => edge.to_node_id === node.id && remaining.has(edge.from_node_id)))
    if (!nodes.length) nodes = graph.nodes.filter(node => remaining.has(node.id)).slice(0, 1)
    levels.push(nodes); nodes.forEach(node => remaining.delete(node.id))
  }
  const rows = Math.max(1, ...levels.map(level => level.length))
  const top = lane + 8
  const positions = new Map<string, { x: number; y: number }>()
  levels.forEach((level, index) => level.forEach((node, branch) => positions.set(node.id, { x: 24 + index * column, y: top + (rows - level.length) * row / 2 + branch * row })))
  const bottom = top + (rows - 1) * row + height
  const hasBack = graph.edges.some(edge => edge.route === 'back')
  const backLane = bottom + 24
  return { width: Math.max(width + 48, levels.length * column), height: hasBack ? backLane + 28 : bottom + 24, positions, backLane }
}

const zoomSteps = [0.5, 0.6, 0.75, 0.9, 1, 1.25, 1.5]
const minFit = 0.5

// 'fit' shrinks a wide graph to the canvas width (never below minFit, never
// above 100%); a number is the scale the user chose.
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
  const canvas = useRef<HTMLDivElement>(null)
  const [available, setAvailable] = useState(0)
  const [zoom, setZoom] = useState<'fit' | number>('fit')
  useLayoutEffect(() => {
    const element = canvas.current
    if (!element) return
    const measure = () => setAvailable(element.clientWidth)
    measure()
    if (typeof ResizeObserver !== 'function') return
    const observer = new ResizeObserver(measure)
    observer.observe(element)
    return () => observer.disconnect()
  }, [])
  const fitted = available > 0 ? Math.min(1, Math.max(minFit, available / box.width)) : 1
  const scale = zoom === 'fit' ? fitted : zoom
  const step = (direction: 1 | -1) => setZoom(direction > 0 ? zoomSteps.find(value => value > scale + 0.001) ?? zoomSteps[zoomSteps.length - 1] : [...zoomSteps].reverse().find(value => value < scale - 0.001) ?? zoomSteps[0])
  const backs = graph.edges.flatMap(edge => {
    const from = box.positions.get(edge.from_node_id), to = box.positions.get(edge.to_node_id)
    if (!from || !to || !(edge.route === 'back' || to.x <= from.x)) return []
    const rounds = loopRounds(graph, edge.to_node_id)
    return [{ edge, fx: from.x + width / 2, tx: to.x + width / 2, fy: from.y + height, ty: to.y + height + 2, label: rounds ? `未通过退回 · 最多 ${rounds} 轮` : '' }]
  })
  const target = selected ? box.positions.get(selected) : undefined
  // Arcs run in the lane above the nodes, one level per source so they do not
  // lie on top of each other.
  const arcs = target ? [
    ...(feeds?.task ? [{ key: 'task', fx: pill.x + pill.width / 2, fy: pill.y + pill.height }] : []),
    ...(feeds?.nodes ?? []).flatMap(id => { const from = box.positions.get(id); return from && id !== selected ? [{ key: id, fx: from.x + width / 2, fy: from.y }] : [] }),
  ].map((arc, index) => ({ ...arc, level: 40 + (index % 4) * 3, tx: target.x + width / 2, ty: target.y - 2 })) : []
  return <div className="flow-editor__stage">
    <div className="flow-editor__zoom" role="group" aria-label="画布缩放">
      <button type="button" className="icon-button" aria-label="缩小" disabled={scale <= zoomSteps[0] + 0.001} onClick={() => step(-1)}><ZoomOut size={15} /></button>
      <span className="flow-editor__zoom-value" aria-live="polite">{Math.round(scale * 100)}%</span>
      <button type="button" className="icon-button" aria-label="放大" disabled={scale >= zoomSteps[zoomSteps.length - 1] - 0.001} onClick={() => step(1)}><ZoomIn size={15} /></button>
      <button type="button" className="icon-button" aria-label="适应宽度" aria-pressed={zoom === 'fit'} onClick={() => setZoom('fit')}><Maximize2 size={15} /></button>
    </div>
    <div ref={canvas} className="flow-editor__canvas" role="region" aria-label="流程画布" tabIndex={0}>
    <div className="flow-editor__sizer" style={{ width: box.width * scale, height: box.height * scale }}>
    <div className="flow-editor__graph" style={{ width: box.width, height: box.height, transform: `scale(${scale})` }}>
      <svg width={box.width} height={box.height} aria-hidden="true"><defs>
        <marker id={arrow} viewBox="0 0 8 8" refX="7" refY="4" markerWidth="6" markerHeight="6" orient="auto"><path d="M0 0L8 4L0 8Z" /></marker>
        <marker id={`${arrow}-feed`} viewBox="0 0 8 8" refX="7" refY="4" markerWidth="6" markerHeight="6" orient="auto"><path className="is-feed-head" d="M0 0L8 4L0 8Z" /></marker>
      </defs>
        {graph.edges.map((edge, index) => {
          const from = box.positions.get(edge.from_node_id), to = box.positions.get(edge.to_node_id)
          if (!from || !to || backs.some(back => back.edge === edge)) return null
          const sx = from.x + width, sy = from.y + height / 2, ex = to.x - 2, ey = to.y + height / 2
          return <path key={edge.id || index} d={`M ${sx} ${sy} C ${(sx + ex) / 2} ${sy}, ${(sx + ex) / 2} ${ey}, ${ex} ${ey}`} markerEnd={`url(#${arrow})`} />
        })}
        {backs.map(({ edge, fx, tx, fy, ty }, index) => <path key={edge.id || `back-${index}`} d={`M ${fx} ${fy} V ${box.backLane} H ${tx} V ${ty}`} className="is-back" markerEnd={`url(#${arrow})`} />)}
        {arcs.map(arc => <path key={`feed-${arc.key}`} d={`M ${arc.fx} ${arc.fy} V ${arc.level} H ${arc.tx} V ${arc.ty}`} className={`is-feed${highlight === arc.key ? ' is-hot' : ''}`} markerEnd={`url(#${arrow}-feed)`} />)}
      </svg>
      {feeds?.task && target ? <span className={`flow-editor__task${highlight === 'task' ? ' is-hot' : ''}`} style={{ left: pill.x, top: pill.y, width: pill.width, height: pill.height }}><FileInput size={13} />任务输入</span> : null}
      {backs.filter(back => back.label).map(({ edge, fx, tx, label }, index) => <span key={edge.id || `label-${index}`} className="flow-editor__edge-label" style={{ left: (fx + tx) / 2, top: box.backLane }}>{label}</span>)}
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
