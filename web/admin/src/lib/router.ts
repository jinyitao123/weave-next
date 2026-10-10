import { useCallback, useEffect, useState } from 'react'

// Minimal history router for the console's few top-level routes.
const base = '/admin'

export function currentPath(): string {
  const path = window.location.pathname.startsWith(base) ? window.location.pathname.slice(base.length) : '/'
  return path === '' ? '/' : path
}

// A page with unsaved edits registers a guard: navigation is handed to it
// instead of happening, and the page decides whether to go on.
let leaveGuard: ((path: string) => void) | undefined

export function useLeaveGuard(active: boolean, onBlocked: (path: string) => void) {
  useEffect(() => {
    if (!active) return
    leaveGuard = onBlocked
    // Closing or reloading the tab can only be confirmed by the browser itself.
    const warn = (event: BeforeUnloadEvent) => { event.preventDefault() }
    window.addEventListener('beforeunload', warn)
    return () => {
      if (leaveGuard === onBlocked) leaveGuard = undefined
      window.removeEventListener('beforeunload', warn)
    }
  }, [active, onBlocked])
}

export function useRoute(): [string, (path: string, options?: { force?: boolean }) => void] {
  const [path, setPath] = useState(currentPath)
  useEffect(() => {
    const update = () => setPath(currentPath())
    window.addEventListener('popstate', update)
    return () => window.removeEventListener('popstate', update)
  }, [])
  const navigate = useCallback((next: string, options?: { force?: boolean }) => {
    if (next === currentPath()) return
    if (leaveGuard && !options?.force) { leaveGuard(next); return }
    window.history.pushState(null, '', base + next)
    setPath(next)
  }, [])
  return [path, navigate]
}
