/** Package-owned durable dispatch-input invariants. @module @deepseek-ai/dsh-workbench-app/invariant */

import type { Context } from '@deepseek-ai/cordis'
import type { InvariantFailure, InvariantInstaller } from '@deepseek-ai/dsh-invariants'
import type { Session, SessionEvent } from '@deepseek-ai/dsh-session'
import { decodeDispatchInput } from './dispatch-input.ts'

const PACKAGE_NAME = '@deepseek-ai/dsh-workbench-app'

/** Cordis companion plugin name. */
export const name = 'workbench-app-invariant'
/** Service required before the companion can register. */
export const inject = ['invariants']

/** Check each dispatch against the source events that already existed when it was recorded. */
function validateEvent(session: Session, event: SessionEvent, fail: InvariantFailure): void {
  if (event.type !== 'weave/dispatch-input') return
  try {
    decodeDispatchInput(event.data, session.events.filter(source => source.seq < event.seq))
  } catch (error) {
    /* v8 ignore next -- the dispatch decoder throws Error instances */
    const message = error instanceof Error ? error.message : String(error)
    fail(`session "${session.id}" event ${event.seq} violates the durable dispatch input: ${message}`)
  }
}

/** Validate restored history and reject invalid live dispatch records before they commit. */
const install: InvariantInstaller = Object.assign((ctx: Context, fail: InvariantFailure) => {
  const staged = new WeakMap<SessionEvent, Session>()
  const seed = (session: Session): void => {
    for (const event of session.events) validateEvent(session, event, fail)
  }
  for (const session of ctx.sessions.list()) seed(session)
  ctx.on('session/created', seed, { global: true })
  ctx.on('internal/dispatch', (_mode, eventName, args) => {
    if (eventName !== 'session/event') return
    const [session, event] = args as [Session, SessionEvent]
    if (event.type !== 'weave/dispatch-input') return
    validateEvent(session, event, fail)
    staged.set(event, session)
  }, { global: true })
  ctx.on('session/event', (session, event) => {
    if (event.type !== 'weave/dispatch-input') return
    /* v8 ignore next 3 -- internal/dispatch validates the exact callback arguments before publication */
    if (staged.get(event) !== session) {
      return fail(`session "${session.id}" event ${event.seq} reached publication without dispatch-input validation`)
    }
    staged.delete(event)
  }, { global: true })
}, { inject: ['sessions'] })

/**
 * Register this package's invariant companion.
 * @param ctx - Cordis context carrying the invariant service.
 * @returns the registration disposer after its session listeners are installed.
 */
export const apply = (ctx: Context): Promise<() => void> =>
  Promise.resolve(ctx.invariants.register(PACKAGE_NAME, install))
