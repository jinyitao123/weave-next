import { Search } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { listEnvironments } from '../lib/environments'
import { listNodes } from '../lib/nodes'
import { listTasks } from '../lib/tasks'
import { listTeamRecords } from '../lib/teams'

interface Item { id: string; group: string; label: string; detail?: string; path: string }

const pages: Item[] = [
  { id: 'page-tasks', group: '页面', label: '任务', path: '/' },
  { id: 'page-environments', group: '页面', label: '环境', path: '/environments' },
  { id: 'page-teams', group: '页面', label: '团队', path: '/teams' },
  { id: 'page-nodes', group: '页面', label: '节点', path: '/nodes' },
  { id: 'page-integrations', group: '页面', label: '集成', path: '/integrations' },
]

// Ctrl/⌘+K jumps to any page, task, team, environment or node.
export function CommandPalette({ onClose, navigate }: { onClose(): void; navigate(path: string): void }) {
  const [query, setQuery] = useState('')
  const [items, setItems] = useState<Item[]>(pages)
  const [active, setActive] = useState(0)
  const input = useRef<HTMLInputElement>(null)
  useEffect(() => {
    input.current?.focus()
    void Promise.allSettled([listTasks(''), listTeamRecords(), listEnvironments(), listNodes()]).then(([tasks, teams, environments, nodes]) => {
      const loaded: Item[] = [...pages]
      if (tasks.status === 'fulfilled') loaded.push(...tasks.value.tasks.slice(0, 30).map((task) => ({ id: `task-${task.run_id}`, group: '任务', label: task.title || task.team_name || '团队任务', detail: task.team_name, path: `/tasks/${encodeURIComponent(task.run_id)}` })))
      if (teams.status === 'fulfilled') loaded.push(...teams.value.map((team) => ({ id: `team-${team.id}`, group: '团队', label: team.display_name || team.name, path: `/teams/${encodeURIComponent(team.id)}` })))
      if (environments.status === 'fulfilled') loaded.push(...environments.value.map((environment) => ({ id: `environment-${environment.id}`, group: '环境', label: environment.name, detail: environment.repository_url, path: '/environments' })))
      if (nodes.status === 'fulfilled') loaded.push(...nodes.value.map((node) => ({ id: `node-${node.id}`, group: '节点', label: node.name, path: '/nodes' })))
      setItems(loaded)
    })
  }, [])
  const shown = useMemo(() => {
    const needle = query.trim().toLowerCase()
    return (needle ? items.filter((item) => `${item.label} ${item.detail ?? ''} ${item.group}`.toLowerCase().includes(needle)) : items).slice(0, 40)
  }, [items, query])
  useEffect(() => { setActive(0) }, [query])
  const choose = (item?: Item) => {
    if (!item) return
    navigate(item.path)
    onClose()
  }
  return <div className="palette-layer" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose() }}>
    <div className="palette" role="dialog" aria-modal="true" aria-label="快速跳转">
      <div className="palette__input"><Search size={15} aria-hidden="true" />
        <input ref={input} value={query} placeholder="搜索任务、团队、环境或节点" aria-label="搜索" aria-controls="palette-list"
          aria-activedescendant={shown[active] ? `palette-${shown[active].id}` : undefined}
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Escape') { event.preventDefault(); onClose() }
            else if (event.key === 'ArrowDown') { event.preventDefault(); setActive((index) => Math.min(shown.length - 1, index + 1)) }
            else if (event.key === 'ArrowUp') { event.preventDefault(); setActive((index) => Math.max(0, index - 1)) }
            else if (event.key === 'Enter') { event.preventDefault(); choose(shown[active]) }
          }} />
      </div>
      <ul className="palette__list" id="palette-list" role="listbox">{shown.map((item, index) => <li key={item.id} id={`palette-${item.id}`} role="option" aria-selected={index === active}
        className={index === active ? 'is-active' : ''} onMouseEnter={() => setActive(index)} onMouseDown={(event) => { event.preventDefault(); choose(item) }}>
        <span className="palette__group">{item.group}</span><span className="palette__label">{item.label}</span>{item.detail ? <span className="muted small palette__detail">{item.detail}</span> : null}
      </li>)}{!shown.length ? <li className="muted palette__empty">没有匹配项</li> : null}</ul>
    </div>
  </div>
}
