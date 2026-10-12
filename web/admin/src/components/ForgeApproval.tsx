import { useEffect, useRef, useState } from 'react'
import { ApiError } from '../lib/api'
import { withForgeApproval, type PendingApproval } from '../lib/forge-approval'
import { approvesThroughForgePage, withForgePageApproval } from '../lib/forge-embed'

/** Whether a failure is the person giving up, which is not something to report back to them. */
export const approvalCancelled = (failure: unknown) => failure instanceof ApiError && failure.code === 'forge_approval_cancelled'

/** Runs approvals on Forge for one screen: at most one at a time, dropped when the screen goes away. */
export function useForgeApproval(forgeOrigin: string | undefined) {
  const [pending, setPending] = useState<PendingApproval>()
  const running = useRef<AbortController | undefined>(undefined)
  useEffect(() => () => running.current?.abort(), [])

  const approve = async <T,>(use: (forgeToken: string) => Promise<T>): Promise<T> => {
    if (!forgeOrigin) throw new ApiError(503, 'external identity is not configured')
    running.current?.abort()
    const controller = new AbortController()
    running.current = controller
    // Inside the Forge it signs in with, that page confirms for the person already signed in there.
    if (approvesThroughForgePage()) {
      try {
        return await withForgePageApproval(forgeOrigin, controller.signal, use)
      } finally {
        if (running.current === controller) running.current = undefined
      }
    }
    // Opened inside the click so the browser allows it, and pointed at Forge once the page is known.
    const tab = window.open('about:blank', '_blank')
    let shown = false
    try {
      return await withForgeApproval(forgeOrigin, {
        signal: controller.signal,
        onPending: (next) => {
          shown = true
          setPending(next)
          if (tab) { tab.opener = null; tab.location.replace(next.page) }
        },
      }, use)
    } catch (failure) {
      if (!shown) tab?.close()
      throw failure
    } finally {
      if (running.current === controller) { running.current = undefined; setPending(undefined) }
    }
  }
  return { pending, approve, cancel: () => running.current?.abort() }
}

/** Shows the code to compare on Forge's page while its approval is awaited. */
export function ForgeApprovalPending({ pending, onCancel }: { pending: PendingApproval; onCancel(): void }) {
  return <div className="approval" role="status">
    <p>请在 Forge 页面确认，并核对确认码</p>
    <output className="approval__code" aria-label="确认码">{pending.userCode}</output>
    <div className="approval__actions">
      <a className="button" href={pending.page} target="_blank" rel="noopener noreferrer">打开 Forge 确认页</a>
      <button type="button" className="button" onClick={onCancel}>取消</button>
    </div>
  </div>
}
