/** Launch the Workbench profile with an optional API key from the macOS Keychain. */
import { execFileSync, spawnSync } from 'node:child_process'
import { resolve } from 'node:path'

/** Keychain service reserved for the local Weave Workbench API credential. */
export const WORKBENCH_KEYCHAIN_SERVICE = 'weave-workbench-api-key'

/** Resolve one credential without ever writing it to stdout or stderr. */
export function keychainCredential(platform: NodeJS.Platform = process.platform): string | undefined {
  if (platform !== 'darwin') return undefined
  try {
    const value = execFileSync('security', [
      'find-generic-password',
      '-s', WORKBENCH_KEYCHAIN_SERVICE,
      '-w',
    ], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim()
    return value === '' ? undefined : value
  } catch {
    return undefined
  }
}

/** Prefer an explicit process credential, then add a discovered host credential. */
export function workbenchEnvironment(
  environment: NodeJS.ProcessEnv,
  readCredential: () => string | undefined = keychainCredential,
): NodeJS.ProcessEnv {
  if (environment.WEAVE_API_KEY?.trim()) return { ...environment }
  const credential = readCredential()?.trim()
  return credential === undefined || credential === ''
    ? { ...environment }
    : { ...environment, WEAVE_API_KEY: credential }
}

/** Start the supported `dsh --profile workbench` application path. */
function main(): void {
  const root = resolve(import.meta.dirname, '..')
  const result = spawnSync(process.execPath, [
    '--import', 'tsx/esm',
    'apps/cli/src/bin.ts',
    '--profile', 'workbench',
    ...process.argv.slice(2),
  ], {
    cwd: root,
    env: workbenchEnvironment(process.env),
    stdio: 'inherit',
  })
  if (result.error !== undefined) throw result.error
  process.exitCode = result.status ?? 1
}

if (import.meta.main) main()
