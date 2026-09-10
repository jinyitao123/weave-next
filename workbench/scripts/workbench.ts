/** Launch the Workbench profile with an optional API key from the macOS Keychain. */
import { execFileSync, spawnSync } from 'node:child_process'
import { accessSync, constants } from 'node:fs'
import { isAbsolute, resolve } from 'node:path'

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

/** Normalize an explicit MCP binary path and fail before opening a partially connected Workbench. */
export function validateWeaveCommand(environment: NodeJS.ProcessEnv, cwd: string): NodeJS.ProcessEnv {
  const command = environment.WEAVE_COMMAND?.trim()
  if (command === undefined || command === '' || (!command.includes('/') && !command.includes('\\'))) return { ...environment }
  const absolute = isAbsolute(command) ? command : resolve(cwd, command)
  try { accessSync(absolute, constants.X_OK) } catch {
    throw new Error(`WEAVE_COMMAND is not an executable file: ${absolute}`)
  }
  return { ...environment, WEAVE_COMMAND: absolute }
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
    env: validateWeaveCommand(workbenchEnvironment(process.env), root),
    stdio: 'inherit',
  })
  if (result.error !== undefined) throw result.error
  process.exitCode = result.status ?? 1
}

if (import.meta.main) main()
