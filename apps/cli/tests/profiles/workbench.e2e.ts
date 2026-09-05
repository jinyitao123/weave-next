/** Real CLI composition contract for the shipped Weave Workbench profile. */
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { spawnSync } from 'node:child_process'
import { afterEach, describe, expect, it } from 'vitest'

const root = resolve(import.meta.dirname, '../../../..')
const homes: string[] = []

afterEach(() => {
  for (const home of homes.splice(0)) rmSync(home, { recursive: true, force: true })
})

function dump(apiKey?: string) {
  const home = mkdtempSync(join(tmpdir(), 'dsh-workbench-profile-'))
  homes.push(home)
  const env: NodeJS.ProcessEnv = { ...process.env, DSH_HOME: home }
  delete env.WEAVE_SECRET_KEY
  delete env.WEAVE_SECRET_KEY_FILE
  if (apiKey === undefined) delete env.WEAVE_API_KEY
  else env.WEAVE_API_KEY = apiKey
  const result = spawnSync(process.execPath, [
    '--import', 'tsx/esm',
    'apps/cli/src/bin.ts',
    '--profile', 'workbench',
    '--dump-config',
  ], { cwd: root, env, encoding: 'utf8' })
  expect(result.status, result.stderr).toBe(0)
  return { home, output: result.stdout }
}

describe('shipped Workbench profile', () => {
  it('composes the Web runtime, Workbench product layer, and key-gated Weave MCP', () => {
    const disconnected = dump()
    expect(JSON.parse(readFileSync(join(disconnected.home, 'profiles/workbench/package.json'), 'utf8')))
      .toMatchObject({
        dsh: { profile: { bundles: [
          '@deepseek-ai/dsh-base',
          '@deepseek-ai/dsh-web-app',
          '@deepseek-ai/dsh-workbench-app',
        ] } },
      })
    expect(disconnected.output).toContain('id: weave-mcp')
    expect(disconnected.output).toContain("disabled: !!js '!process.env.WEAVE_API_KEY'")
    expect(disconnected.output).toContain("name: '@deepseek-ai/dsh-client-ui-brand-workbench'")
    expect(disconnected.output).toContain("name: '@deepseek-ai/dsh-client-ui-weave'")
    expect(disconnected.output).toContain('You are Weave Workbench')
    const personaText = disconnected.output.replace(/\s+/gu, ' ')
    expect(personaText).toContain('confirmed the selected team, task scope, and expected deliverables')
    expect(personaText).toContain('without asking again')
    expect(personaText).toContain('Internal construction and evaluation do not add user approval steps')
    expect(personaText).toContain('do not claim delivery is complete')
    expect(personaText).toContain('do not repeat team matching or dispatch a new task')
    expect(personaText).toContain('never describe an accepted request as an applied change')
    expect(disconnected.output).toMatch(
      /- id: agent-default-model\n(?:  .*\n){0,3}  config:\n    provider: unconfigured\n    model: unconfigured/,
    )
    expect(disconnected.output).not.toContain('provider: deepseek-official')
    expect(disconnected.output).toMatch(/- id: llm-deepseek\n(?:  .*\n){1,3}  disabled: true/)
    expect(disconnected.output).toMatch(/- id: web-search-deepseek\n(?:  .*\n){1,3}  disabled: true/)
    expect(disconnected.output).toMatch(
      /- id: tool-web\n(?:  .*\n){0,3}  config:\n    search: false\n    fetch: false/,
    )
    expect(disconnected.output).not.toContain('defaultWorkspacePath')
    expect(disconnected.output).not.toContain('WEAVE_WORKBENCH_WORKSPACE')
    expect(disconnected.output).toMatch(/- id: ui-agent-preset\n(?:  .*\n){0,3}  disabled: true/)
    expect(disconnected.output).toMatch(/- id: agent-presets\n(?:  .*\n){1,5}    default: standard/)
    for (const id of ['ui-brand-official', 'ui-subagent', 'ui-message-feedback', 'ui-trajectory']) {
      expect(disconnected.output).toMatch(new RegExp(`- id: ${id}\\n(?:  .*\\n){1,3}  disabled: true`))
    }

    const connected = dump('wv_sk_profile_test')
    expect(connected.output).toContain("disabled: !!js '!process.env.WEAVE_API_KEY'")
    expect(connected.output).not.toContain('wv_sk_profile_test')
    expect(connected.output).not.toContain('WEAVE_SECRET_KEY')
  })
})
