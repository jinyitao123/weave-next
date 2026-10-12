import { ApiError } from './api'
import { waitFor, withForgeApproval } from './forge-approval'

// Shown inside a page of the Forge it signs in with, the console lets that
// page confirm a sign-in for the person already signed in there. The console
// sends the page the confirmation code, the page approves it with the person's
// own Forge session, and the console goes on as if they had approved it by
// hand. Only the code crosses between the two pages; no session does.

export const APPROVE_REQUEST = 'weave-console:approve'
export const APPROVE_ANSWER = 'weave-console:approval'

const ANSWER_MILLISECONDS = 8000

let pageApproves = false

/** Whether the Forge page around the console has confirmed a sign-in for it. */
export const approvesThroughForgePage = () => pageApproves

/** Whether the console is shown inside another page at all. */
export function embedded(): boolean {
  try { return window.parent !== window } catch { return true }
}

/** Runs one approval that the surrounding Forge page confirms. Fails with forge_page_unavailable when no such page answers. */
export async function withForgePageApproval<T>(forgeOrigin: string, signal: AbortSignal, use: (forgeToken: string) => Promise<T>, answerMilliseconds = ANSWER_MILLISECONDS): Promise<T> {
  pageApproves = false
  if (signal.aborted) throw new ApiError(0, 'forge_approval_cancelled')
  const origin = new URL(forgeOrigin).origin
  const controller = new AbortController()
  const stop = () => controller.abort()
  signal.addEventListener('abort', stop, { once: true })
  let failure: ApiError | undefined
  let userCode = ''
  let confirm = () => undefined as void
  const confirmed = new Promise<void>((resolve) => { confirm = resolve })
  const listen = (event: MessageEvent) => {
    // Only this Forge, speaking as the page around the console, about this code.
    if (event.origin !== origin || event.source !== window.parent || !userCode) return
    const data = event.data as { type?: unknown; userCode?: unknown; approved?: unknown } | null
    if (!data || data.type !== APPROVE_ANSWER || data.userCode !== userCode) return
    if (data.approved === true) {
      confirm()
    } else {
      failure = new ApiError(403, 'forge_approval_denied')
      controller.abort()
    }
  }
  window.addEventListener('message', listen)
  let timer: ReturnType<typeof setTimeout> | undefined
  let hurried = false
  try {
    const result = await withForgeApproval(origin, {
      signal: controller.signal,
      onPending: (pending) => {
        userCode = pending.userCode
        timer = setTimeout(() => {
          pageApproves = false
          failure = new ApiError(0, 'forge_page_unavailable')
          controller.abort()
        }, answerMilliseconds)
        void confirmed.then(() => clearTimeout(timer))
        // Delivered only if the surrounding page really is this Forge.
        window.parent.postMessage({ type: APPROVE_REQUEST, userCode }, origin)
      },
      // The first poll goes out as soon as the page says it approved; later ones keep a short pace.
      wait: (milliseconds, waiting) => {
        if (hurried) return waitFor(Math.min(milliseconds, 1000), waiting)
        const timed = waitFor(milliseconds, waiting)
        timed.catch(() => undefined)
        return Promise.race([timed, confirmed.then(() => { hurried = true })])
      },
    }, use)
    if (controller.signal.aborted) throw new ApiError(0, 'forge_approval_cancelled')
    pageApproves = true
    return result
  } catch (error) {
    throw failure ?? error
  } finally {
    clearTimeout(timer)
    window.removeEventListener('message', listen)
    signal.removeEventListener('abort', stop)
  }
}
