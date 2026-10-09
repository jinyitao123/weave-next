import { Bot, CheckCircle2, GitBranch, GitMerge, Repeat2, UsersRound } from 'lucide-react'
import { useId } from 'react'
import type { Graph } from '../../lib/graph'
import type { DevelopmentMember } from '../../lib/teams'

export const stepLabel = (type: string): string => ({ lead: '负责人处理', worker: '成员执行', parallel: '并行分工', join: '汇合', deliver: '交付结果', loop: '验证回路', condition: '条件分支', wait: '等待', transform: '转换', handoff: '交接' }[type] ?? '流程步骤')
const width = 172, height = 60, column = width + 48, row = height + 36

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
  const positions = new Map<string, { x: number; y: number }>()
  levels.forEach((level, index) => level.forEach((node, branch) => positions.set(node.id, { x: 24 + index * column, y: 24 + (rows - level.length) * row / 2 + branch * row })))
  const backLane = 24 + (rows - 1) * row + height + 24
  const hasBack = graph.edges.some(edge => edge.route === 'back')
  return { width: Math.max(width + 48, levels.length * column), height: hasBack ? backLane + 24 : rows * row + 32, positions, backLane }
}

export function FlowCanvas({ graph, members, selected, onSelect }: { graph: Graph; members: DevelopmentMember[]; selected?: string; onSelect(id: string): void }) {
  const box = layout(graph), arrow = useId().replace(/:/g, '')
  const backs = graph.edges.flatMap(edge => {
    const from = box.positions.get(edge.from_node_id), to = box.positions.get(edge.to_node_id)
    if (!from || !to || !(edge.route === 'back' || to.x <= from.x)) return []
    const rounds = loopRounds(graph, edge.to_node_id)
    return [{ edge, fx: from.x + width / 2, tx: to.x + width / 2, fy: from.y + height, ty: to.y + height + 2, label: rounds ? `未通过退回 · 最多 ${rounds} 轮` : '' }]
  })
  return <div className="flow-editor__canvas" role="region" aria-label="流程画布" tabIndex={0}>
    <div className="flow-editor__graph" style={{ width: box.width, height: box.height }}>
      <svg width={box.width} height={box.height} aria-hidden="true"><defs><marker id={arrow} viewBox="0 0 8 8" refX="7" refY="4" markerWidth="6" markerHeight="6" orient="auto"><path d="M0 0L8 4L0 8Z" /></marker></defs>
        {graph.edges.map((edge, index) => {
          const from = box.positions.get(edge.from_node_id), to = box.positions.get(edge.to_node_id)
          if (!from || !to || backs.some(back => back.edge === edge)) return null
          const sx = from.x + width, sy = from.y + height / 2, ex = to.x - 2, ey = to.y + height / 2
          return <path key={edge.id || index} d={`M ${sx} ${sy} C ${(sx + ex) / 2} ${sy}, ${(sx + ex) / 2} ${ey}, ${ex} ${ey}`} markerEnd={`url(#${arrow})`} />
        })}
        {backs.map(({ edge, fx, tx, fy, ty }, index) => <path key={edge.id || `back-${index}`} d={`M ${fx} ${fy} V ${box.backLane} H ${tx} V ${ty}`} className="is-back" markerEnd={`url(#${arrow})`} />)}
      </svg>
      {backs.filter(back => back.label).map(({ edge, fx, tx, label }, index) => <span key={edge.id || `label-${index}`} className="flow-editor__edge-label" style={{ left: (fx + tx) / 2, top: box.backLane }}>{label}</span>)}
      {graph.nodes.map(node => {
        const position = box.positions.get(node.id)!
        const member = node.type === 'lead' ? members.find(item => item.configuration.role === 'avatar' && item.relationship.enabled) : members.find(item => item.id === node.config?.agent_id)
        const Icon = node.type === 'deliver' ? CheckCircle2 : node.type === 'lead' ? UsersRound : node.type === 'parallel' ? GitBranch : node.type === 'join' ? GitMerge : node.type === 'loop' ? Repeat2 : Bot
        const title = node.label || stepLabel(node.type), subtitle = member?.configuration.display_name || stepLabel(node.type)
        return <button type="button" key={node.id} style={{ left: position.x, top: position.y, width, height }} className={`flow-editor__node${selected === node.id ? ' is-selected' : ''}`} aria-label={`编辑步骤 ${title}`} aria-pressed={selected === node.id} onClick={() => onSelect(node.id)}>
          <Icon size={17} /><span><strong>{title}</strong>{subtitle !== title ? <small>{subtitle}</small> : null}</span>
        </button>
      })}
    </div>
  </div>
}
