/** Personal task creation uses the Host's account-bound default directory. */
import type { ISessions } from '@deepseek-ai/dsh-api-session-controller/client'
import type { HostObservable } from '@deepseek-ai/dsh-client-ui-slots'
import type { AccountView } from './account-controller.ts'

/**
 * Deduplicate a pending click and discard navigation if the account changes.
 * @param account - the product-owned current account projection.
 * @param sessions - existing Session creation and selection authority.
 * @returns a personal task command with no shared directory override.
 */
export function personalSessionStarter(account: HostObservable<AccountView>, sessions: Pick<ISessions, 'create' | 'open'>): () => Promise<void> {
  let pending: Promise<void> | undefined
  return () => {
    const view = account.getSnapshot()
    if (view.status !== 'authenticated' || view.user === null) return Promise.reject(new Error('Account is unavailable'))
    if (pending !== undefined) return pending
    const user = view.user
    pending = sessions.create({}).then(id => {
      const current = account.getSnapshot()
      if (current.status !== 'authenticated' || current.user?.id !== user.id || current.user.workspace_id !== user.workspace_id) {
        throw new Error('Account has changed')
      }
      sessions.open(id)
    }).finally(() => { pending = undefined })
    return pending
  }
}
