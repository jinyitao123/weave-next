// Real Workbench profile -> Host proxy -> isolated PostgreSQL -> Loom runner.
// The browser validates application access for an already published capability.
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
const completed = new Promise(resolveExit => run.on('exit', code => resolveExit(code)))
const wait = ms => new Promise(resolveWait => setTimeout(resolveWait, ms))
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
  page.setDefaultTimeout(10_000)
  await page.goto('http://' + authority)
  await page.getByRole('button', { name: /^(设置|Settings)$/ }).click()
  await page.getByRole('button', { name: /^(应用接入|Application access)$/ }).click()
  await page.getByLabel(/^(新应用名称|New application name)$/).fill('Browser client')
  await page.getByRole('button', { name: /^(创建应用|Create application)$/ }).click()
  const applicationSelect = page.getByLabel(/^(选择应用|Select application)$/)
  await page.waitForFunction(() => document.querySelector('select[aria-label="选择应用"],select[aria-label="Select application"]')?.value.startsWith('app_'))
  const appID = await applicationSelect.inputValue()
  await page.getByLabel(/^(选择发布版本|Select published version)$/).selectOption(JSON.stringify(['arithmetic', 1]))
  await page.getByRole('button', { name: /^(授权版本|Grant version)$/ }).click()
  await page.getByRole('button', { name: /^(撤销授权|Revoke grant)$/ }).waitFor()
  await page.getByLabel(/^(密钥名称|Key name)$/).fill('First browser key')
  await page.getByRole('button', { name: /^(创建新密钥|Create new key)$/ }).click()
  const secretView = page.getByLabel(/^(新访问密钥|New access key)$/)
  await secretView.waitFor()
  const firstKey = await secretView.textContent()
  const invoke = async key => {
    const response = await fetch(connection.url + '/v1/capabilities/arithmetic/versions/1/invocations', {
      method: 'POST', headers: { Authorization: 'Bearer ' + key, 'Content-Type': 'application/json' },
      body: JSON.stringify({ request_id: 'browser-app-request', input: { material: '请核对这份资料是否完整。' } }),
    })
    if (response.status !== 202) throw new Error('Application invocation rejected')
    return response.json()
  }
  const accepted = await invoke(firstKey)
  if (accepted.application_id !== appID) throw new Error('Credential identity replaced application identity')
  await page.getByRole('button', { name: /^(隐藏密钥|Hide key)$/ }).click()
  await page.getByLabel(/^(密钥名称|Key name)$/).fill('Replacement browser key')
  await page.getByRole('button', { name: /^(创建新密钥|Create new key)$/ }).click()
  await secretView.waitFor()
  const replacementKey = await secretView.textContent()
  await page.getByRole('button', { name: /^(隐藏密钥|Hide key)$/ }).click()
  const originalRow = page.getByRole('group', { name: 'First browser key', exact: true })
  await originalRow.getByRole('button', { name: /^(撤销密钥|Revoke key)$/ }).click()
  await originalRow.getByText(/^(已撤销|Revoked)$/).waitFor()
  const rotated = await invoke(replacementKey)
  if (rotated.invocation_id !== accepted.invocation_id || !rotated.replayed) throw new Error('Rotation duplicated the invocation')
  const oldResponse = await fetch(connection.url + '/v1/invocations/' + accepted.invocation_id, { headers: { Authorization: 'Bearer ' + firstKey } })
  if (oldResponse.status !== 401) throw new Error('Old key still works')
  let applicationResult
  for (let i = 0; i < 100; i++) {
    const response = await fetch(connection.url + '/v1/invocations/' + accepted.invocation_id, { headers: { Authorization: 'Bearer ' + replacementKey } })
    applicationResult = await response.json()
    if (applicationResult.status === 'completed') break
    if (applicationResult.status === 'failed') throw new Error('Application run failed')
    await wait(100)
  }
  if (applicationResult?.result?.merge?.['材料检查项数'] !== 7 || applicationResult?.result?.merge?.['规则检查项数'] !== 7) {
    throw new Error('Rotated key cannot read the original result')
  }
  const visibleText = await page.locator('body').innerText()
  for (const internal of ['schema_version', 'definition_hash', 'invocation_id', 'caller_kind', 'result_state']) {
    if (visibleText.includes(internal)) throw new Error('Internal field exposed in application settings: ' + internal)
  }
  await page.screenshot({ path: join(output, 'applications.png'), fullPage: true })
  await writeFile(join(output, 'evidence.json'), JSON.stringify({
    provider: 'controlled HTTP fixture', runtime: 'existing Loom runner', capability: { name: '资料核对示例', revision: 1 },
    application: { id: appID, rotationReplayed: rotated.replayed, oldCredentialStatus: oldResponse.status },
    result: { status: applicationResult.status, materialChecks: applicationResult.result.merge['材料检查项数'], ruleChecks: applicationResult.result.merge['规则检查项数'] },
  }, null, 2))
  await writeFile(join(temporary, 'done'), '')
  if (await completed !== 0) throw new Error('Go fixture failed verification')
  console.log('Workbench browser acceptance passed: application creation, version grant, credential rotation and result readback.')
} catch (error) {
  if (page) {
    await page.screenshot({ path: join(output, 'failure.png'), fullPage: true }).catch(() => {})
    await writeFile(join(temporary, 'page.txt'), await page.locator('body').innerText()).catch(() => {})
  }
  console.error(error.message)
  console.error('Private diagnostic directory: ' + temporary)
  process.exitCode = 1
} finally {
  await browser?.close()
  await writeFile(join(temporary, 'done'), '')
  if (host) {
    try { process.kill(-host.pid, 'SIGTERM') } catch {}
    await Promise.race([new Promise(resolveExit => host.once('exit', resolveExit)), wait(3000)])
    if (host.exitCode === null) { try { process.kill(-host.pid, 'SIGKILL') } catch {} }
  }
  await Promise.race([completed, wait(5000)])
  if (run.exitCode === null) { try { process.kill(-run.pid, 'SIGTERM') } catch {} }
  await completed
  log.end()
  if (process.exitCode !== 1) await rm(temporary, { recursive: true, force: true })
}
