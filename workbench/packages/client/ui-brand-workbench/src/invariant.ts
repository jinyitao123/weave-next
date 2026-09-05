/**
 * Package-owned invariant companion for `@deepseek-ai/dsh-client-ui-brand-workbench`.
 * @module @deepseek-ai/dsh-client-ui-brand-workbench/invariant
 */

/* jscpd:ignore-start */
import type { Context } from '@deepseek-ai/cordis'
import type { InvariantInstaller } from '@deepseek-ai/dsh-invariants'

const PACKAGE_NAME = '@deepseek-ai/dsh-client-ui-brand-workbench'

/** Cordis companion plugin name. */
export const name = 'client-ui-brand-workbench-invariant'
/** Service required before the companion can reserve package ownership. */
export const inject = ['invariants']

/**
 * No runtime invariant: the package retains no mutable state, and its three
 * slot occupants install and leave through one transactional effect.
 */
const install: InvariantInstaller = () => {}

/** Register this package's invariant companion. */
export const apply = (ctx: Context): Promise<() => void> =>
  Promise.resolve(ctx.invariants.register(PACKAGE_NAME, install))
/* jscpd:ignore-end */
