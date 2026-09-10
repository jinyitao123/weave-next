/** Verify shared Workbench UI through the supported profile in a private, keyless Host. */
import { spawn } from 'node:child_process'
import { mkdtemp, mkdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { once } from 'node:events'
import assert from 'node:assert/strict'
import { chromium } from 'playwright'

const workspace = fileURLToPath(new URL('../../..', import.meta.url))
const directory = await mkdtemp(join(tmpdir(), 'weave-desktop-web-'))
const output = resolve('.dsh-build/desktop-smoke')
await mkdir(output, { recursive: true })
const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) => !/KEY|SECRET|TOKEN|PASSWORD|^WEAVE_|^DSH_/i.test(key)))
let browser
let transcript = ''
const errors = []
const host = spawn(process.execPath, [join(workspace, 'apps/cli/lib/bin.js'), '--profile', 'workbench', '--host', '127.0.0.1', '--port', '0', '--no-open'], {
  cwd: directory, env: { ...environment, DSH_HOME: join(directory, 'host') }, stdio: ['ignore', 'pipe', 'pipe'],
})
host.stdout.on('data', value => { transcript += value })
host.stderr.on('data', value => { transcript += value })
try {
  const deadline = Date.now() + 45000
  let target
  while (Date.now() < deadline) {
    target = transcript.match(/dsh web: (http:\/\/127\.0\.0\.1:\d+\S*)/)?.[1]
    if (target) break
    if (host.exitCode !== null) throw new Error(`Private Workbench exited before readiness (${host.exitCode})`)
    await new Promise(resolve => setTimeout(resolve, 100))
  }
  assert.ok(target, 'the supported Workbench profile must report readiness')
  browser = await chromium.launch({ headless: true })
  const page = await browser.newPage({ viewport: { width: 1280, height: 840 } })
  page.on('pageerror', error => errors.push(String(error)))
  await page.goto(target)
  await page.locator('[data-workbench]').waitFor({ timeout: 20000 })
  await page.locator('[data-native-new-session]').waitFor()
  await page.getByText('开始一项工作', { exact: true }).waitFor()
  await page.screenshot({ path: join(output, 'web-home.png') })
  await page.getByRole('button', { name: '设置', exact: true }).click()
  await page.getByRole('dialog', { name: '设置' }).waitFor()
  await page.screenshot({ path: join(output, 'web-settings.png') })
  await page.keyboard.press('Escape')
  const fixture = new URL(page.url())
  fixture.searchParams.set('fixture', '')
  await page.goto(fixture.href)
  await page.getByText('Fixture 历史会话', { exact: true }).click()
  const scene = page.getByRole('button', { name: '打开工作现场', exact: true })
  await scene.waitFor()
  assert.equal(await scene.locator('svg').count(), 1)
  await scene.hover()
  await page.getByRole('tooltip').waitFor()
  await page.screenshot({ path: join(output, 'web-scene-icon.png') })
  await scene.click()
  await page.getByText('工作现场', { exact: true }).last().waitFor()
  assert.deepEqual(errors, [])
  await writeFile(join(output, 'web-smoke.json'), `${JSON.stringify({ profile: 'workbench', host: 'isolated loopback',
    home: 'private temporary directory', credentials: 'none', checks: ['real profile homepage', 'settings dialog', 'shared fixture scene icon and tooltip', 'scene navigation'], errors }, null, 2)}\n`)
  console.log('PASS isolated supported Workbench profile, shared homepage, settings and scene icon')
} catch (error) {
  // A private process token in the readiness URL is never written to evidence.
  console.error(transcript.replace(/https?:\/\/[^\s)]+/g, '[PRIVATE URL]').slice(-8000))
  throw error
} finally {
  await browser?.close()
  if (host.exitCode === null) {
    const closed = once(host, 'exit')
    host.kill('SIGTERM')
    const timer = setTimeout(() => { host.kill('SIGKILL') }, 5000)
    await closed
    clearTimeout(timer)
  }
  await rm(directory, { recursive: true, force: true })
}
