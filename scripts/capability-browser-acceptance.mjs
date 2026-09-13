// Real Workbench profile -> Host proxy -> isolated PostgreSQL -> Loom runner.
// Model responses are deterministic HTTP fixtures owned by the Go test.
import { spawn } from 'node:child_process'
import { createHash, createHmac } from 'node:crypto'
import { mkdtemp, readFile, writeFile, mkdir, rm } from 'node:fs/promises'
import { createWriteStream } from 'node:fs'
import { tmpdir } from 'node:os'
import { resolve, join } from 'node:path'
import { createRequire } from 'node:module'

const root = resolve(import.meta.dirname, '..')
const require = createRequire(join(root, 'workbench/package.json'))
const { chromium } = require('playwright')
const { load } = require('js-yaml')
const temporary = await mkdtemp(join(tmpdir(), 'weave-capability-browser-'))
const output = join(root, 'docs/验收/2026-09-13-能力服务Workbench')
await mkdir(output, { recursive: true })
const home = join(temporary, 'home')
const patch = join(temporary, 'patch.yml')
await writeFile(patch, '- id: weave-mcp\n  disabled: true\n')
const log = createWriteStream(join(temporary, 'process.log'))
let host, browser, page
const run = spawn('go', ['test', './internal/app/api', '-run', '^TestCapabilityHTTPToLoomRuntimeRealPG$', '-count=1', '-timeout', '150s'], {
  cwd: root, env: { ...process.env, WEAVE_CAPABILITY_BROWSER_DIR: temporary }, stdio: ['ignore', 'pipe', 'pipe'], detached: true,
})
run.stdout.pipe(log, { end: false }); run.stderr.pipe(log, { end: false })
const completed = new Promise(resolve => run.on('exit', code => resolve(code)))
const wait = ms => new Promise(resolve => setTimeout(resolve, ms))
try {
  let connection
  for (let i = 0; i < 150; i++) {
    try { connection = JSON.parse(await readFile(join(temporary, 'connection.json'), 'utf8')); break } catch {}
    if (run.exitCode !== null) throw new Error('Go fixture failed before browser connection')
    await wait(100)
  }
  if (!connection) throw new Error('Go fixture unavailable')
  host = spawn(process.execPath, ['--import', 'tsx/esm', 'apps/cli/src/bin.ts', '--profile', 'workbench', '--patch', patch, '--host', '127.0.0.1', '--port', '3109', '--no-open'], {
    cwd: join(root, 'workbench'), env: { ...process.env, DSH_HOME: home, WEAVE_API_URL: connection.url, WEAVE_API_KEY: connection.token },
    stdio: ['ignore', 'pipe', 'pipe'], detached: true,
  })
  host.stdout.pipe(log, { end: false }); host.stderr.pipe(log, { end: false })
  const authority = '127.0.0.1:3109'
  let credentials
  for (let i = 0; i < 200; i++) {
    try {
      credentials = load(await readFile(join(home, '.credentials.yaml'), 'utf8'))
      if (credentials.records?.['client-connection/browser-session']) break
    } catch {}
    await wait(100)
  }
  const secret = Buffer.from(credentials.records['client-connection/browser-session'].payload.secret, 'base64url')
  const issuedAt = Date.now(), expiresAt = issuedAt + 3600_000
  const encode = value => Buffer.from(value).toString('base64url')
  const body = encode(JSON.stringify({ version: 1, authority, issuedAt, expiresAt }))
  browser = await chromium.launch({ headless: true })
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale: 'zh-CN' })
  await context.addCookies([{
    name: 'dsh-auth-' + encode(createHash('sha256').update(authority).digest()),
    value: 'v1.' + body + '.' + encode(createHmac('sha256', secret).update(body).digest()),
    domain: '127.0.0.1', path: '/', httpOnly: true, sameSite: 'Strict', expires: expiresAt / 1000,
  }])
  page = await context.newPage()
  page.setDefaultTimeout(10000)
  await page.goto('http://' + authority)
  await page.getByRole('button', { name: /^(设置|Settings)$/ }).click()
  await page.getByRole('button', { name: /^(能力开发|Capability development)$/ }).click()
  await page.getByLabel(/已有草稿|Saved drafts/).selectOption('arithmetic')
  const editor = page.getByLabel(/^(能力定义 JSON|Capability definition JSON)$/)
  const definition = JSON.parse(await editor.inputValue())
  definition.capability_id = 'browser-capability'
  definition.name = '浏览器角色核对'
  await editor.fill(JSON.stringify(definition, null, 2))
  await page.getByRole('button', { name: /^(调试当前草稿|Debug current draft)$/ }).click()
  await page.locator('pre').filter({ hasText: '"completed"' }).waitFor({ timeout: 30_000 })
  const debugResult = JSON.parse(await page.locator('pre').filter({ hasText: '"invocation_id"' }).textContent())
  if (debugResult.run_kind !== 'debug' || debugResult.revision !== undefined || debugResult.result?.merge?.left !== 7) throw new Error('Invalid draft debug result')
  await page.locator('pre').filter({ hasText: '"invocation_id"' }).scrollIntoViewIfNeeded()
  await page.screenshot({ path: join(output, 'debug.png'), fullPage: true })
  definition.name = '浏览器角色核对 · 发布'
  await editor.fill(JSON.stringify(definition, null, 2))
  await page.getByRole('button', { name: /^(保存并发布|Save and publish)$/ }).click()
  await page.getByText(/^(版本已发布|Version published)$/).waitFor()
  await page.getByRole('button', { name: /^(调用已发布版本|Invoke published version)$/ }).click()
  await page.locator('pre').filter({ hasText: '"published"' }).filter({ hasText: '"completed"' }).waitFor({ timeout: 30_000 })
  const result = JSON.parse(await page.locator('pre').filter({ hasText: '"invocation_id"' }).textContent())
  if (result.run_kind !== 'published' || result.invocation_id === debugResult.invocation_id || result.definition_hash === debugResult.definition_hash) throw new Error('Debug and publication identities were mixed')
  if (result.result?.merge?.left !== 7 || result.result?.merge?.right !== 7) throw new Error('Wrong browser result')
  await page.locator('pre').filter({ hasText: '"invocation_id"' }).scrollIntoViewIfNeeded()
  await page.screenshot({ path: join(output, 'desktop.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.waitForTimeout(500)
  await page.locator('pre').filter({ hasText: '"invocation_id"' }).scrollIntoViewIfNeeded()
  await page.screenshot({ path: join(output, 'mobile.png'), fullPage: true })
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)
  if (overflow) throw new Error('Mobile document overflows')
  const editorWidth = await editor.evaluate(element => element.getBoundingClientRect().width)
  if (editorWidth < 280) throw new Error('Mobile editor is too narrow')
  const dialogVisible = await page.evaluate(() => document.elementFromPoint(350, 400)?.closest('[role="dialog"]') !== null)
  if (!dialogVisible) {
    await writeFile(join(temporary, 'layers.json'), JSON.stringify(await page.evaluate(() => document.elementsFromPoint(350,400).map(e => ({tag:e.tagName, class:e.className,position:getComputedStyle(e).position,z:getComputedStyle(e).zIndex}))),null,2))
    throw new Error('Another surface overlaps the mobile settings dialog')
  }
  await writeFile(join(output, 'evidence.json'), JSON.stringify({ provider: 'controlled HTTP fixture', runtime: 'existing Loom runner', debugResult, result, mobileOverflow: overflow }, null, 2))
  await writeFile(join(temporary, 'done'), '')
  if (await completed !== 0) throw new Error('Go fixture failed verification')
  console.log('Workbench browser acceptance passed: debug unsaved draft, publish changed definition, invoke, read both results, desktop and mobile.')
} catch (error) {
  if (page) {
    await page.screenshot({path:join(output,'failure.png'),fullPage:true}).catch(()=>{})
    await writeFile(join(temporary,'page.txt'),await page.locator('body').innerText()).catch(()=>{})
  }
  // Logs can contain local bootstrap credentials, so do not print them.
  console.error(error.message)
  console.error('Private diagnostic directory: ' + temporary)
  process.exitCode = 1
} finally {
  await browser?.close()
  await writeFile(join(temporary, 'done'), '')
  if (host) {
    try { process.kill(-host.pid, 'SIGTERM') } catch {}
    await Promise.race([new Promise(resolve => host.once('exit', resolve)), wait(3000)])
    if (host.exitCode === null) { try { process.kill(-host.pid, 'SIGKILL') } catch {} }
  }
  await Promise.race([completed, wait(5000)])
  if (run.exitCode === null) { try { process.kill(-run.pid, 'SIGTERM') } catch {} }
  await completed
  log.end()
  if (process.exitCode !== 1) await rm(temporary, { recursive: true, force: true })
}
