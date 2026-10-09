import { executionLabel, terminal, type TaskSummary } from './tasks'

// Completion notices are a per-viewer convenience: the switch lives in this
// browser only, and nothing is sent anywhere.
const enabledKey = 'weave-admin:notify'

export function notificationsEnabled(): boolean {
  try { return localStorage.getItem(enabledKey) === '1' && typeof Notification !== 'undefined' && Notification.permission === 'granted' } catch { return false }
}

export async function enableNotifications(): Promise<boolean> {
  if (typeof Notification === 'undefined') return false
  const permission = Notification.permission === 'granted' ? 'granted' : await Notification.requestPermission()
  try { localStorage.setItem(enabledKey, permission === 'granted' ? '1' : '0') } catch { /* convenience only */ }
  return permission === 'granted'
}

export function disableNotifications() {
  try { localStorage.setItem(enabledKey, '0') } catch { /* convenience only */ }
}

const seen = new Map<string, string>()
let unseen = 0
const baseTitle = typeof document !== 'undefined' ? document.title : 'Weave'

function updateTitle() {
  document.title = unseen > 0 ? `(${unseen}) ${baseTitle}` : baseTitle
}

if (typeof window !== 'undefined') {
  window.addEventListener('focus', () => { unseen = 0; updateTitle() })
}

// trackTasks notices tasks that finished since the previous call.
export function trackTasks(tasks: TaskSummary[], open: (runId: string) => void) {
  const first = seen.size === 0
  for (const task of tasks) {
    const previous = seen.get(task.run_id)
    seen.set(task.run_id, task.status)
    if (first || previous === undefined || terminal(previous) || !terminal(task.status)) continue
    if (!document.hasFocus()) {
      unseen += 1
      updateTitle()
    }
    if (notificationsEnabled()) {
      const notice = new Notification(task.title || task.team_name || '团队任务', { body: executionLabel(task.status), tag: task.run_id })
      notice.onclick = () => { window.focus(); open(task.run_id); notice.close() }
    }
  }
}
