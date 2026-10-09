import { useCallback, useEffect, useState } from 'react'

// Minimal history router for the console's few top-level routes.
const base = '/admin'

export function currentPath(): string {
  const path = window.location.pathname.startsWith(base) ? window.location.pathname.slice(base.length) : '/'
  return path === '' ? '/' : path
}

export function useRoute(): [string, (path: string) => void] {
  const [path, setPath] = useState(currentPath)
  useEffect(() => {
    const update = () => setPath(currentPath())
    window.addEventListener('popstate', update)
    return () => window.removeEventListener('popstate', update)
  }, [])
  const navigate = useCallback((next: string) => {
    if (next === currentPath()) return
    window.history.pushState(null, '', base + next)
    setPath(next)
  }, [])
  return [path, navigate]
}
