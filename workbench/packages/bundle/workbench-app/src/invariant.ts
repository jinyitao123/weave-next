/**
 * Package-owned invariant companion for `@deepseek-ai/dsh-workbench-app`.
 * @module @deepseek-ai/dsh-workbench-app/invariant
 */

/* jscpd:ignore-start */
import type { Context } from '@deepseek-ai/cordis'
import type { InvariantInstaller } from '@deepseek-ai/dsh-invariants'

const PACKAGE_NAME = '@deepseek-ai/dsh-workbench-app'

/** Cordis companion plugin name. */
export const name = 'workbench-app-invariant'
/** Service required before the companion can register. */
export const inject = ['invariants']

/** No runtime invariant: the bundle is a static profile patch. */
const install: InvariantInstaller = () => {}

/** Register this package's invariant companion. */
export const apply = (ctx: Context): Promise<() => void> =>
  Promise.resolve(ctx.invariants.register(PACKAGE_NAME, install))
/* jscpd:ignore-end */
