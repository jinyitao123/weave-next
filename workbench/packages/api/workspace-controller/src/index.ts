/** Host Workspace Remote owner: explicit commands and reconnect-safe state. */

import { Context } from '@deepseek-ai/cordis'
import { Remote, TypertRemoteFailure, TypertRemoteService } from '@deepseek-ai/dsh-typert-protocol'
import type { SessionId } from '@deepseek-ai/dsh-session/types'
import { WorkspaceCommands } from './commands.ts'
import { DirectoryPickerController } from './directory-picker.ts'
import { WorkspaceFeed } from './feed.ts'
import type {
  WorkspaceArchiveSessionRequest,
  WorkspaceArchiveValue,
  WorkspaceCreateRequest,
  WorkspaceCreateValue,
  WorkspaceDeleteRequest,
  WorkspaceDeleteValue,
  WorkspaceFollowFrame,
  WorkspaceInsertBeforeRequest,
  WorkspaceInsertSessionBeforeRequest,
  WorkspaceOrderValue,
  WorkspaceRenameRequest,
  WorkspaceValue,
  WorkspaceView,
} from './types.ts'

type SessionVisibility = (id: SessionId) => Promise<boolean>

export type * from './types.ts'
export { DirectoryPickerController } from './directory-picker.ts'

declare module '@deepseek-ai/cordis' {
  interface Context {
    /** Host Workspace business API and Remote namespace owner. */
    workspaceController: WorkspaceController
  }
}

/** Host service backing the generated `ctx.remote.workspace` namespace. */
export class WorkspaceController extends TypertRemoteService {
  static inject = ['typert', 'workspaceRegistry']

  private readonly commands: WorkspaceCommands
  private readonly feed: WorkspaceFeed
  private sessionVisibility: (() => SessionVisibility) | undefined

  /**
   * Install the product's request-bound Session visibility policy.
   * @param policy - captures the current caller and returns a live visibility check.
   * @returns disposer that withdraws this policy if it is still installed.
   */
  setSessionVisibility(policy: () => SessionVisibility): () => void {
    this.sessionVisibility = policy
    return () => { if (this.sessionVisibility === policy) this.sessionVisibility = undefined }
  }

  /** @param ctx - Host context containing the Workspace registry. */
  constructor(ctx: Context) {
    super(ctx, 'workspaceController', { namespace: 'workspace' })
    this.commands = new WorkspaceCommands(ctx)
    this.feed = new WorkspaceFeed(ctx)
    // This package is the Loader entry for both Remote owners it hosts: the
    // directory-picking seam is abstract and never an entry itself. The child
    // stays pending until a picking backend is composed, so a host without one
    // registers no picking namespace instead of answering an unservable verb.
    ctx.plugin(DirectoryPickerController)
  }

  /**
   * Create or idempotently resolve one Workspace over an existing directory.
   * @param request - directory path to register.
   * @returns the Workspace and whether this call created it.
   */
  @Remote('create')
  async create(request: WorkspaceCreateRequest): Promise<WorkspaceCreateValue> {
    const visible = this.sessionVisibility?.()
    const result = await this.commands.create(request)
    return { ...result, workspace: await visibleWorkspace(result.workspace, visible) }
  }

  /**
   * Rename one Workspace to a unique non-blank title.
   * @param request - Workspace identity and proposed title.
   * @returns the updated Workspace projection.
   */
  @Remote('rename')
  async rename(request: WorkspaceRenameRequest): Promise<WorkspaceValue> {
    const visible = this.sessionVisibility?.()
    const result = await this.commands.rename(request)
    return { workspace: await visibleWorkspace(result.workspace, visible) }
  }

  /**
   * Remove one Workspace registration while retaining files and Sessions.
   * @param request - Workspace identity to remove.
   * @returns deletion confirmation.
   */
  @Remote('delete')
  delete(request: WorkspaceDeleteRequest): Promise<WorkspaceDeleteValue> {
    return this.commands.delete(request)
  }

  /**
   * Move one Workspace within the registry display order.
   * @param request - moved Workspace and optional anchor.
   * @returns the complete resulting Workspace order.
   */
  @Remote('insertBefore')
  insertBefore(request: WorkspaceInsertBeforeRequest): Promise<WorkspaceOrderValue> {
    return this.commands.insertBefore(request)
  }

  /**
   * Move one accounted Session within a Workspace.
   * @param request - Workspace, Session, and optional anchor identities.
   * @returns the updated Workspace projection.
   */
  @Remote('insertSessionBefore')
  async insertSessionBefore(request: WorkspaceInsertSessionBeforeRequest): Promise<WorkspaceValue> {
    const visible = this.sessionVisibility?.()
    await requireVisibleSession(request.sessionId, visible)
    if (request.beforeSessionId !== undefined) await requireVisibleSession(request.beforeSessionId, visible)
    const result = await this.commands.insertSessionBefore(request)
    return { workspace: await visibleWorkspace(result.workspace, visible) }
  }

  /**
   * Hide one known Session from Workspace grouping surfaces.
   * @param request - Session identity to archive.
   * @returns the complete resulting archive set.
   */
  @Remote('archiveSession')
  async archiveSession(request: WorkspaceArchiveSessionRequest): Promise<WorkspaceArchiveValue> {
    const visible = this.sessionVisibility?.()
    await requireVisibleSession(request.sessionId, visible)
    const result = await this.commands.archiveSession(request)
    return { archivedSessionIds: await visibleSessionIds(result.archivedSessionIds, visible) }
  }

  /**
   * Stream a complete Workspace baseline followed by ordered increments.
   * @param signal - generation cancellation.
   * @returns baseline followed by ordered Workspace increments.
   */
  @Remote({ mode: 'stream' })
  follow(signal: AbortSignal): AsyncIterable<WorkspaceFollowFrame> {
    const visible = this.sessionVisibility?.()
    const source = this.feed.follow(signal)
    if (visible === undefined) return source
    return (async function* () {
      for await (const frame of source) {
        signal.throwIfAborted()
        switch (frame.type) {
          case 'baseline':
            yield { type: 'baseline', value: {
              items: await Promise.all(frame.value.items.map(workspace => visibleWorkspace(workspace, visible))),
              archivedSessionIds: await visibleSessionIds(frame.value.archivedSessionIds, visible),
            } } satisfies WorkspaceFollowFrame
            break
          case 'upsert':
            yield { type: 'upsert', workspace: await visibleWorkspace(frame.workspace, visible) } satisfies WorkspaceFollowFrame
            break
          case 'archived':
            yield { type: 'archived', archivedSessionIds: await visibleSessionIds(frame.archivedSessionIds, visible) } satisfies WorkspaceFollowFrame
            break
          case 'remove':
          case 'order':
            yield frame
        }
      }
    })()
  }
}

async function visibleSessionIds(ids: readonly SessionId[], visible: SessionVisibility | undefined): Promise<readonly SessionId[]> {
  if (visible === undefined) return ids
  const allowed = await Promise.all(ids.map(id => visible(id)))
  return ids.filter((_, index) => allowed[index] === true)
}

async function visibleWorkspace(workspace: WorkspaceView, visible: SessionVisibility | undefined): Promise<WorkspaceView> {
  return { ...workspace, sessionIds: await visibleSessionIds(workspace.sessionIds, visible) }
}

async function requireVisibleSession(id: SessionId, visible: SessionVisibility | undefined): Promise<void> {
  if (visible !== undefined && !await visible(id)) {
    throw new TypertRemoteFailure({ code: 'session-not-found', message: 'Session is unavailable', details: { sessionId: id } })
  }
}

export default WorkspaceController
