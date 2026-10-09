import { useEffect, useRef } from 'react'

// Runs task now and then every interval while enabled and the tab is visible.
export function usePolling(task: () => Promise<void> | void, intervalMs: number, enabled = true) {
  const latest = useRef(task)
  latest.current = task
  useEffect(() => {
    if (!enabled) return
    let stopped = false
    let timer: ReturnType<typeof setTimeout> | undefined
    const tick = async () => {
      if (stopped) return
      if (document.visibilityState !== 'hidden') await Promise.resolve(latest.current()).catch(() => undefined)
      if (!stopped) timer = setTimeout(() => void tick(), intervalMs)
    }
    void tick()
    const onVisible = () => { if (document.visibilityState === 'visible') { clearTimeout(timer); void tick() } }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      stopped = true
      clearTimeout(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [intervalMs, enabled])
}
