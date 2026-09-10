/** Native acceptance for the locally packaged Workbench and isolated connection controls. */
import { _electron as electron } from 'playwright'
import executablePath from 'electron'
import assert from 'node:assert/strict'
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createHash } from 'node:crypto'

const data = await mkdtemp(join(tmpdir(), 'weave-desktop-smoke-'))
const output = resolve(process.env.WEAVE_DESKTOP_ARTIFACTS || '.dsh-build/desktop-smoke')
await mkdir(output, { recursive: true })
const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) => !/KEY|SECRET|TOKEN|PASSWORD|ELECTRON_RUN_AS_NODE/i.test(key)))
const entry = fileURLToPath(new URL('../dist', import.meta.url))
const packaged = process.env.WEAVE_DESKTOP_EXECUTABLE
const results = []
const errors = []
const launched = []
let application
let page
let services
let lastBounds

async function start() {
  application = await electron.launch({ executablePath: packaged || executablePath,
    args: [...(packaged ? [] : [entry]), `--desktop-data=${data}`], env: environment, timeout: 30000 })
  launched.push(application.process().pid)
  application.on('window', window => window.on('pageerror', error => errors.push(String(error))))
  page = await application.firstWindow()
  await page.waitForFunction(() => Boolean(window.workbenchDesktop))
  await until(async () => { const value = await snapshot(); return value.fatal || value.notice === 'connected' })
  return snapshot()
}
async function stop() {
  if (!application) return
  const owned = application
  application = undefined
  await owned.close()
}
async function step(name, work) {
  const startTime = Date.now()
  await work()
  results.push({ name, status: 'passed', durationMs: Date.now() - startTime })
  console.log(`PASS ${name}`)
}
async function until(predicate) {
  const deadline = Date.now() + 20000
  while (Date.now() < deadline) {
    if (await predicate()) return
    await page.waitForTimeout(50)
  }
  throw new Error(`Native condition timed out: ${JSON.stringify(await snapshot())}`)
}
const snapshot = () => page.evaluate(() => window.workbenchDesktop.snapshot())
const waitNotice = notice => until(async () => (await snapshot()).notice === notice)
function workbench() {
  const result = application.windows().find(window => window.url().startsWith('weave-app://workbench/'))
  assert.ok(result, 'the actual local Workbench must be mounted')
  return result
}
async function select(name) {
  await page.locator('#settings').click()
  await page.getByRole('button', { name: `服务 ${name}`, exact: true }).click()
  await page.locator('#connect').click()
  await waitNotice('connected')
  await workbench().locator('[data-workbench]').waitFor()
}
async function menu(label) {
  await application.evaluate(({ Menu }, target) => {
    const find = items => {
      for (const item of items) {
        if (item.label === target) return item
        const child = item.submenu && find(item.submenu.items)
        if (child) return child
      }
    }
    const item = find(Menu.getApplicationMenu().items)
    if (!item) throw new Error(`Missing native menu item: ${target}`)
    item.click()
  }, label)
}
async function prepareDownload(mode) {
  const path = join(data, `download-${mode}.txt`)
  await application.evaluate((_electron, options) => {
    globalThis.desktopLatestTransport.once('will-download', (_event, item) => {
      item.setSavePath(options.path)
      if (options.cancel) item.cancel()
    })
  }, { path, cancel: mode === 'cancel' })
}

try {
  await step('native window mounts the real shared Workbench from packaged local assets', async () => {
    const value = await start()
    assert.equal(value.fatal, false)
    services = value.services
    assert.equal(value.selectedOrigin, services[0].origin)
    assert.equal(value.activeOrigin, services[0].origin)
    await workbench().locator('[data-native-new-session]').waitFor()
    await workbench().screenshot({ path: join(output, 'workbench-home.png') })
  })
  await step('local Workbench has no Node, preload bridge, or remote navigation privilege', async () => {
    assert.deepEqual(await workbench().evaluate(() => ({ node: typeof process, require: typeof require, bridge: typeof window.workbenchDesktop })),
      { node: 'undefined', require: 'undefined', bridge: 'undefined' })
    const policies = await application.evaluate(({ webContents }) => {
      const prefs = webContents.getAllWebContents().find(wc => wc.getURL().startsWith('weave-app:')).getLastWebPreferences()
      return { sandbox: prefs.sandbox, node: prefs.nodeIntegration, isolated: prefs.contextIsolation }
    })
    assert.deepEqual(policies, { sandbox: true, node: false, isolated: true })
    const denied = await workbench().evaluate(async () => { try { await fetch('https://example.com/'); return false } catch { return true } })
    assert.equal(denied, true)
  })
  await step('native sidebar menu controls the existing Workbench layout', async () => {
    const frame = workbench().locator('[data-workbench]')
    await menu('切换侧栏')
    await until(async () => await frame.getAttribute('data-sidebar-collapsed') !== null)
    await menu('切换侧栏')
    await until(async () => await frame.getAttribute('data-sidebar-collapsed') === null)
  })
  await step('the actual composer stays editable and retains its draft while settings open', async () => {
    const input = workbench().locator('[data-composer-input][contenteditable="true"]')
    await input.fill('桌面交互验证草稿，不提交业务任务。')
    await page.locator('#settings').click()
    await page.locator('#back').click()
    assert.match(await input.innerText(), /桌面交互验证草稿/)
  })
  await step('right-clicking the actual composer opens its native editing menu', async () => {
    await application.evaluate(({ Menu }) => {
      globalThis.desktopOriginalPopup = Menu.prototype.popup
      Menu.prototype.popup = function () { globalThis.desktopContextRoles = this.items.map(item => item.role) }
    })
    await workbench().locator('[data-composer-input][contenteditable="true"]').click({ button: 'right' })
    await until(async () => await application.evaluate(() => globalThis.desktopContextRoles?.includes('paste')))
    await application.evaluate(({ Menu }) => { Menu.prototype.popup = globalThis.desktopOriginalPopup })
  })
  await step('shared settings and a real history conversation open inside Workbench', async () => {
    await workbench().getByRole('button', { name: '设置', exact: true }).click()
    await workbench().getByRole('dialog', { name: '设置' }).waitFor()
    await workbench().screenshot({ path: join(output, 'workbench-settings.png') })
    await workbench().keyboard.press('Escape')
    await application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setContentSize(1440, 900))
    await workbench().getByText('Fixture 历史会话', { exact: true }).click()
    await workbench().locator('[data-work-scene-entry]').waitFor()
    assert.match(await workbench().locator('body').innerText(), /Fixture 历史会话/)
    await workbench().screenshot({ path: join(output, 'workbench-conversation.png') })
    await menu('新建会话')
  })
  await step('switch closes the old frontend and clears its in-memory authentication', async () => {
    await application.evaluate(({ session }) => {
      const original = session.fromPartition.bind(session)
      session.fromPartition = (partition, options) => {
        const result = original(partition, options)
        if (partition.startsWith('desktop-candidate-')) globalThis.desktopLatestTransport = result
        return result
      }
    })
    await select('A')
    await application.evaluate(() => { globalThis.desktopOldTransport = globalThis.desktopLatestTransport })
    const old = workbench()
    await select('B')
    await until(async () => await application.evaluate(async () => (await globalThis.desktopOldTransport.cookies.get({})).length === 0))
    assert.equal(old.isClosed(), true)
    assert.equal((await snapshot()).selectedOrigin, services[1].origin)
    await workbench().evaluate(() => localStorage.setItem('dsh.conversation.contentWidth', '720'))
  })
  await step('native download saves exact bytes and distinguishes cancellation and failure', async () => {
    await prepareDownload('save')
    await menu('下载验证样例…')
    await waitNotice('downloadComplete')
    const bytes = await readFile(join(data, 'download-save.txt'))
    assert.equal(bytes.toString(), 'Workbench desktop preview\nService B\nFixture only; no business task was created.\n')
    results.push({ name: 'download evidence', bytes: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex') })
    await prepareDownload('cancel')
    await menu('下载验证样例…')
    await waitNotice('downloadCancelled')
    await fetch(`${services[1].origin}/fixture/control?downloadFailure=1`, { method: 'POST' })
    await prepareDownload('fail')
    await menu('下载验证样例…')
    await waitNotice('downloadFailed')
    await fetch(`${services[1].origin}/fixture/control`, { method: 'POST' })
  })
  await step('external protocols are blocked and HTTPS opening requires native confirmation', async () => {
    const blocked = await workbench().evaluate(() => window.open('file:///not-an-allowed-target') === null)
    assert.equal(blocked, true)
    assert.equal(application.windows().length, 2)
    await application.evaluate(({ dialog }) => {
      globalThis.desktopOriginalDialog = dialog.showMessageBox
      globalThis.desktopDialogSeen = false
      dialog.showMessageBox = async () => { globalThis.desktopDialogSeen = true; return { response: 0, checkboxChecked: false } }
    })
    await workbench().evaluate(() => window.open('https://www.electronjs.org/'))
    await until(async () => await application.evaluate(() => globalThis.desktopDialogSeen))
    await application.evaluate(({ dialog }) => { dialog.showMessageBox = globalThis.desktopOriginalDialog })
    assert.equal(application.windows().length, 2)
  })
  await step('disconnect retains the selected origin and closes the local client', async () => {
    await page.locator('#disconnect').click()
    await waitNotice('disconnected')
    assert.equal((await snapshot()).selectedOrigin, services[1].origin)
    assert.equal((await snapshot()).activeOrigin, null)
    assert.equal(application.windows().length, 1)
  })
  await step('restart restores window bounds, selected B, pinned instance, and scoped UI preferences', async () => {
    lastBounds = await application.evaluate(({ BrowserWindow, screen }) => {
      const area = screen.getPrimaryDisplay().workArea
      const target = BrowserWindow.getAllWindows()[0]
      target.setBounds({ x: area.x + 32, y: area.y + 32, width: 1120, height: 760 })
      return target.getNormalBounds()
    })
    await stop()
    const value = await start()
    assert.deepEqual(await application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].getNormalBounds()), lastBounds)
    assert.equal(value.selectedOrigin, services[1].origin)
    assert.deepEqual(value.services, services)
    assert.equal(await workbench().evaluate(() => localStorage.getItem('dsh.conversation.contentWidth')), '720')
  })
  await step('cancelled slow candidate cannot replace B after a late response', async () => {
    await fetch(`${services[0].origin}/fixture/control?delay=1`, { method: 'POST' })
    await page.locator('#settings').click()
    await page.getByRole('button', { name: '服务 A', exact: true }).click()
    await page.locator('#connect').click()
    await waitNotice('connecting')
    await page.locator('#cancel').click()
    await waitNotice('cancelled')
    await page.waitForTimeout(1100)
    assert.equal((await snapshot()).selectedOrigin, services[1].origin)
    assert.equal((await snapshot()).activeOrigin, null)
    await fetch(`${services[0].origin}/fixture/control`, { method: 'POST' })
  })
  await step('connection loss preserves B and reports a retryable error', async () => {
    await fetch(`${services[0].origin}/fixture/control?unavailable=1`, { method: 'POST' })
    await page.locator('#connect').click()
    await waitNotice('connectionFailed')
    assert.equal((await snapshot()).selectedOrigin, services[1].origin)
    await fetch(`${services[0].origin}/fixture/control`, { method: 'POST' })
  })
  await step('replacement at selected origin rejects the new instance', async () => {
    await fetch(`${services[1].origin}/fixture/control?rotate=1`, { method: 'POST' })
    await page.getByRole('button', { name: '服务 B', exact: true }).click()
    await page.locator('#connect').click()
    await waitNotice('connectionFailed')
    assert.match((await snapshot()).detail, /instance changed/)
    assert.equal((await snapshot()).activeOrigin, null)
  })
  await step('corrupt preference fails visibly without overwriting or silently falling back', async () => {
    await stop()
    await writeFile(join(data, 'connection.json'), '{invalid-json')
    const value = await start()
    assert.equal(value.fatal, true)
    assert.equal(value.activeOrigin, null)
    assert.equal(await readFile(join(data, 'connection.json'), 'utf8'), '{invalid-json')
    await page.screenshot({ path: join(output, 'corrupt-preference.png') })
  })
  assert.deepEqual(errors, [])
} finally {
  await stop()
  await writeFile(join(output, 'native-smoke.json'), `${JSON.stringify({ results, launched, errors }, null, 2)}\n`)
  await rm(data, { recursive: true, force: true })
}
