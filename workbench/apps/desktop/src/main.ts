/** Native preview shell; the locally packaged Workbench receives no Node or IPC capabilities. */
import { app, BrowserWindow, dialog, ipcMain, Menu, protocol, screen, session, shell, WebContentsView,
  type IpcMainInvokeEvent, type Session, type DownloadItem } from 'electron'
import { readFile } from 'node:fs/promises'
import { basename, join } from 'node:path'
import { createHash, randomUUID } from 'node:crypto'
import { fileURLToPath } from 'node:url'
import { ConnectionSelection, serviceInstanceId, serviceOrigin, type ConnectionScope, type ServiceIdentity } from './connection-selection.js'
import { FilePreferences } from './preferences.js'
import { startPreviewServices, type PreviewServices } from './preview-services.js'
import type { ShellCommand, ShellState } from './shell-protocol.js'

const shellOrigin = 'weave-shell://app'
const root = fileURLToPath(new URL('.', import.meta.url))
const dataArgument = process.argv.find(value => value.startsWith('--desktop-data='))
app.setName('Weave Workbench Preview')
app.setPath('userData', dataArgument?.slice('--desktop-data='.length) || join(app.getPath('appData'), 'Weave Workbench Preview'))
protocol.registerSchemesAsPrivileged(['weave-shell', 'weave-app'].map(scheme => ({ scheme, privileges: { standard: true, secure: true, supportFetchAPI: true } })))

let window: BrowserWindow | null = null
let fixtures: PreviewServices | null = null
let selection: ConnectionSelection | null = null
let view: WebContentsView | null = null
let activeScope: ConnectionScope | null = null
let activeTransport: Session | null = null
let requestId = 0
let quitting = false
let state: ShellState = { selectedOrigin: '', instanceId: null, activeOrigin: null, busy: false,
  settingsOpen: true, notice: 'welcome', detail: '', fatal: false, services: [] }
const candidateSessions = new Set<Session>()
const downloads = new Set<DownloadItem>()
const disposal = new Set<Promise<void>>()
const registeredSessions = new WeakSet<Session>()

function publish(patch: Partial<ShellState> = {}): void {
  state = { ...state, selectedOrigin: selection?.selected.origin ?? state.selectedOrigin,
    instanceId: selection?.selected.expectedInstanceId ?? null, ...patch }
  if (window && !window.isDestroyed()) window.webContents.send('desktop:state', state)
}

function retireSession(value: Session): void {
  candidateSessions.delete(value)
  const work = Promise.all([value.clearStorageData(), value.closeAllConnections()]).then(() => {})
    .catch((error: unknown) => { console.error('Desktop session cleanup failed', error) })
  disposal.add(work)
  void work.finally(() => { disposal.delete(work) })
}

function retireView(): void {
  activeScope = null
  if (activeTransport) { retireSession(activeTransport); activeTransport = null }
  for (const item of downloads) item.cancel()
  downloads.clear()
  if (view) {
    const old = view
    view = null
    if (window && !window.isDestroyed()) window.contentView.removeChildView(old)
    if (!old.webContents.isDestroyed()) old.webContents.close()
  }
}

function sizeView(): void {
  if (!window || !view) return
  const [width = 800, height = 600] = window.getContentSize()
  view.setBounds({ x: 0, y: 36, width, height: Math.max(0, height - 36) })
  view.setVisible(!state.settingsOpen)
}

function isCurrent(scope: ConnectionScope): boolean {
  return activeScope === scope && Boolean(selection?.isCurrent(scope)) && !quitting
}

function sameOrigin(url: string, origin: string): boolean {
  try { return new URL(url).origin === origin && !new URL(url).username && !new URL(url).password } catch { return false }
}

async function offerExternal(url: string, scope: ConnectionScope): Promise<void> {
  if (!window || !isCurrent(scope)) return
  let parsed: URL
  try { parsed = new URL(url) } catch { return }
  if (parsed.protocol !== 'https:' || parsed.username || parsed.password) {
    publish({ notice: 'linkBlocked' })
    return
  }
  const result = await dialog.showMessageBox(window, { type: 'question', title: '打开外部链接',
    message: '在系统浏览器中打开此链接？', detail: parsed.href, buttons: ['取消', '打开浏览器'], defaultId: 0, cancelId: 0 })
  if (result.response === 1 && isCurrent(scope)) await shell.openExternal(parsed.href)
}

function attachPage(scope: ConnectionScope, targetSession: Session): WebContentsView {
  const partition = createHash('sha256').update(`${scope.origin}/${scope.expectedInstanceId}`).digest('hex')
  const localSession = session.fromPartition(`persist:desktop-ui-${partition}`)
  if (!registeredSessions.has(localSession)) {
    registeredSessions.add(localSession)
    localSession.protocol.handle('weave-app', localResource)
  }
  const content = new WebContentsView({ webPreferences: { session: localSession, nodeIntegration: false,
    contextIsolation: true, sandbox: true, webSecurity: true, allowRunningInsecureContent: false, navigateOnDragDrop: false } })
  view = content
  activeScope = scope
  activeTransport = targetSession
  candidateSessions.delete(targetSession)
  localSession.setPermissionRequestHandler((_wc, _permission, callback) => { callback(false) })
  localSession.setPermissionCheckHandler(() => false)
  // The feature view loads only packaged application resources, including redirects.
  localSession.webRequest.onBeforeRequest((details, callback) => {
    callback({ cancel: !isCurrent(scope) || !details.url.startsWith('weave-app://workbench/') })
  })
  content.webContents.setWindowOpenHandler(({ url }) => {
    void offerExternal(url, scope).catch((error: unknown) => { console.error('External link failed', error) })
    return { action: 'deny' }
  })
  content.webContents.on('will-navigate', (event, url) => {
    if (!isCurrent(scope) || !url.startsWith('weave-app://workbench/')) {
      event.preventDefault()
      void offerExternal(url, scope).catch((error: unknown) => { console.error('External link failed', error) })
    }
  })
  content.webContents.on('will-redirect', (event, url) => {
    if (!isCurrent(scope) || !url.startsWith('weave-app://workbench/')) event.preventDefault()
  })
  content.webContents.on('context-menu', (_event, parameters) => {
    if (!window || !isCurrent(scope)) return
    const entries: Electron.MenuItemConstructorOptions[] = parameters.isEditable
      ? [{ role: 'undo' }, { role: 'redo' }, { type: 'separator' }, { role: 'cut' }, { role: 'copy' }, { role: 'paste' }, { role: 'selectAll' }]
      : parameters.selectionText ? [{ role: 'copy' }, { role: 'selectAll' }] : []
    if (parameters.linkURL) entries.push({ label: '在浏览器中打开链接…', click: () => { void offerExternal(parameters.linkURL, scope) } })
    if (entries.length) Menu.buildFromTemplate(entries).popup({ window })
  })
  content.webContents.on('will-attach-webview', (event) => { event.preventDefault() })
  targetSession.on('will-download', (event, item) => {
    if (!isCurrent(scope) || item.getURLChain().some(url => !sameOrigin(url, scope.origin))) {
      event.preventDefault()
      return
    }
    downloads.add(item)
    const filename = basename(item.getFilename()).replace(/[<>:"/\\|?*\u0000-\u001F]/g, '_') || 'download'
    item.setSaveDialogOptions({ defaultPath: join(app.getPath('downloads'), filename) })
    let failed = false
    item.on('updated', (_event, progress) => {
      if (progress === 'interrupted') { failed = true; item.cancel() }
    })
    item.once('done', (_event, outcome) => {
      downloads.delete(item)
      if (isCurrent(scope)) publish({ notice: failed ? 'downloadFailed' : outcome === 'completed' ? 'downloadComplete' : outcome === 'cancelled' ? 'downloadCancelled' : 'downloadFailed' })
    })
  })
  content.webContents.on('did-fail-load', (_event, code, description, _url, mainFrame) => {
    if (mainFrame && code !== -3 && isCurrent(scope)) {
      publish({ notice: 'loadFailed', detail: description, settingsOpen: true })
      sizeView()
    }
  })
  window?.contentView.addChildView(content)
  sizeView()
  return content
}

async function fixtureIdentity(origin: string, path: string, targetSession: Session, signal: AbortSignal): Promise<ServiceIdentity> {
  const response = await targetSession.fetch(`${origin}/fixture/${path}`, { redirect: 'error', signal: AbortSignal.any([signal, AbortSignal.timeout(10000)]) })
  if (!response.ok) throw new Error(`Preview service request failed: ${response.status} ${response.url}`)
  const value: unknown = await response.json()
  if (typeof value !== 'object' || value === null || !('fixture' in value) || value.fixture !== true
    || !('origin' in value) || value.origin !== origin || !('instanceId' in value) || typeof value.instanceId !== 'string'
    || (path === 'authenticate' && (!('authenticated' in value) || value.authenticated !== true))) {
    throw new Error('Invalid preview service identity')
  }
  return { origin: serviceOrigin(origin, true), expectedInstanceId: serviceInstanceId(value.instanceId) }
}

async function connect(origin: string): Promise<void> {
  const owner = selection
  if (!owner || state.fatal) return
  if (!fixtures?.services.some(service => service.origin === origin)) {
    publish({ notice: 'unsupportedOrigin', detail: '' })
    return
  }
  const attempt = ++requestId
  retireView()
  owner.disconnect()
  const targetSession = session.fromPartition(`desktop-candidate-${randomUUID()}`)
  candidateSessions.add(targetSession)
  publish({ busy: true, activeOrigin: null, settingsOpen: true, notice: 'connecting', detail: '' })
  try {
    const scope = await owner.select(origin, {
      describe: (value, signal) => fixtureIdentity(value, 'describe', targetSession, signal),
      authenticate: (value, signal) => fixtureIdentity(value.origin, 'authenticate', targetSession, signal),
    })
    if (attempt !== requestId || quitting) return
    const content = attachPage(scope, targetSession)
    await content.webContents.loadURL('weave-app://workbench/index.html?fixture')
    await content.webContents.executeJavaScript(`new Promise((resolve, reject) => {
      const finish = (ready) => { observer.disconnect(); clearTimeout(timer); ready ? resolve(true) : reject(new Error('Workbench client failed to mount')) }
      const check = () => { if (document.querySelector('[data-workbench]')) finish(true) }
      const observer = new MutationObserver(check)
      const timer = setTimeout(() => finish(false), 15000)
      observer.observe(document.documentElement, { childList: true, subtree: true })
      check()
    })`)
    if (!isCurrent(scope)) return
    publish({ busy: false, activeOrigin: scope.origin, settingsOpen: false, notice: 'connected', detail: '' })
    sizeView()
  } catch (error) {
    if (attempt === requestId && !quitting) {
      owner.disconnect()
      retireView()
      publish({ busy: false, activeOrigin: null, settingsOpen: true, notice: 'connectionFailed', detail: error instanceof Error ? error.message : String(error) })
    }
  } finally {
    if (candidateSessions.has(targetSession)) retireSession(targetSession)
  }
}

function trustedSender(event: IpcMainInvokeEvent): void {
  if (!window || event.sender !== window.webContents || event.senderFrame !== window.webContents.mainFrame
    || event.senderFrame.url !== `${shellOrigin}/index.html`) throw new Error('Untrusted shell sender')
}

function parseCommand(value: unknown): ShellCommand {
  if (typeof value !== 'object' || value === null || !('action' in value)) throw new Error('Invalid shell command')
  if (value.action === 'connect' && 'origin' in value && typeof value.origin === 'string') return { action: 'connect', origin: value.origin }
  if (value.action === 'cancel' || value.action === 'disconnect' || value.action === 'settings' || value.action === 'back' || value.action === 'reload') return { action: value.action }
  throw new Error('Unknown shell command')
}

async function localResource(request: Request): Promise<Response> {
  const url = new URL(request.url)
  const relative = decodeURIComponent(url.pathname).slice(1)
  if (url.host !== 'workbench' || !relative || relative.split('/').some(part => part === '..' || part === '.') || relative.includes('\\') || request.method !== 'GET') return new Response('Not found', { status: 404 })
  const suffix = relative.split('.').pop() ?? ''
  const mime: Record<string, string> = { html: 'text/html', js: 'text/javascript', css: 'text/css', json: 'application/json', svg: 'image/svg+xml', png: 'image/png', woff2: 'font/woff2', webmanifest: 'application/manifest+json' }
  if (!mime[suffix]) return new Response('Not found', { status: 404 })
  try {
    return new Response(await readFile(join(root, 'workbench', relative)), { headers: { 'Content-Type': mime[suffix],
      'Content-Security-Policy': "default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-src 'none'" } })
  } catch { return new Response('Not found', { status: 404 }) }
}

function nativeClick(selector: string): void {
  if (!view || !activeScope || !isCurrent(activeScope)) return
  // Selectors are constants from the application menu, never renderer input.
  void view.webContents.executeJavaScript(`document.querySelector(${JSON.stringify(selector)})?.click()`)
}

function savedWindowBounds(): Partial<Electron.Rectangle> {
  try {
    const saved = new FilePreferences(join(app.getPath('userData'), 'window.json')).read()
    if (saved === null) return {}
    const value: unknown = JSON.parse(saved)
    if (typeof value !== 'object' || value === null) return {}
    const bounds = value as Record<string, unknown>
    if (!['x', 'y', 'width', 'height'].every(key => typeof bounds[key] === 'number' && Number.isInteger(bounds[key]))) return {}
    const rectangle = bounds as unknown as Electron.Rectangle
    if (rectangle.width < 800 || rectangle.width > 5000 || rectangle.height < 600 || rectangle.height > 4000) return {}
    const visible = screen.getAllDisplays().some(({ workArea: area }) =>
      rectangle.x < area.x + area.width - 100 && rectangle.x + rectangle.width > area.x + 100
      && rectangle.y >= area.y && rectangle.y < area.y + area.height - 100)
    return visible ? rectangle : { width: rectangle.width, height: rectangle.height }
  } catch { return {} }
}

async function start(): Promise<void> {
  await app.whenReady()
  protocol.handle('weave-shell', async (request) => {
    const url = new URL(request.url)
    const file = ({ '/index.html': 'index.html', '/renderer.js': 'renderer.js', '/shell.css': 'shell.css' } as Record<string, string>)[url.pathname]
    if (url.host !== 'app' || !file || request.method !== 'GET') return new Response('Not found', { status: 404 })
    const mime = file.endsWith('.html') ? 'text/html' : file.endsWith('.css') ? 'text/css' : 'text/javascript'
    return new Response(await readFile(join(root, file)), { headers: { 'Content-Type': `${mime}; charset=utf-8`,
      'Content-Security-Policy': "default-src 'none'; script-src 'self'; style-src 'self'; base-uri 'none'; frame-src 'none'; form-action 'none'" } })
  })
  window = new BrowserWindow({ width: 1160, height: 800, ...savedWindowBounds(), minWidth: 800, minHeight: 600,
    title: 'Weave Workbench Preview', backgroundColor: '#f5f5f5', show: false,
    titleBarStyle: 'hiddenInset', trafficLightPosition: { x: 16, y: 13 },
    webPreferences: { preload: join(root, 'preload.cjs'), contextIsolation: true, nodeIntegration: false, sandbox: true, navigateOnDragDrop: false } })
  window.webContents.setWindowOpenHandler(() => ({ action: 'deny' }))
  window.webContents.on('will-navigate', (event) => { event.preventDefault() })
  window.on('resize', sizeView)
  Menu.setApplicationMenu(Menu.buildFromTemplate([
    { label: 'Workbench', submenu: [{ label: '连接设置…', accelerator: 'CmdOrCtrl+,', click: () => { publish({ settingsOpen: true }); sizeView() } }, { type: 'separator' }, { role: 'quit' }] },
    { label: '文件', submenu: [{ label: '新建会话', accelerator: 'CmdOrCtrl+N', click: () => { nativeClick('[data-native-new-session]') } }, { label: '下载验证样例…', click: () => { if (activeScope && isCurrent(activeScope)) activeTransport?.downloadURL(`${activeScope.origin}/fixture/download`) } }, { role: 'close' }] },
    { role: 'editMenu' },
    { label: '视图', submenu: [{ label: '切换侧栏', accelerator: 'CmdOrCtrl+B', click: () => { nativeClick('[data-native-toggle-sidebar]') } }, { role: 'togglefullscreen' }, { role: 'resetZoom' }, { role: 'zoomIn' }, { role: 'zoomOut' }] },
    { role: 'windowMenu' },
  ]))
  window.on('close', () => {
    if (window) {
      try { new FilePreferences(join(app.getPath('userData'), 'window.json')).write(JSON.stringify(window.getNormalBounds())) }
      catch (error) { console.error('Window preference could not be saved', error) }
    }
  })
  window.on('closed', () => { window = null; app.quit() })
  try {
    fixtures = await startPreviewServices(join(app.getPath('userData'), 'preview-services.json'))
    selection = new ConnectionSelection(new FilePreferences(join(app.getPath('userData'), 'connection.json')),
      { defaultOrigin: fixtures.services[0]?.origin ?? 'http://127.0.0.1:1', allowLoopbackHttp: true })
    publish({ services: fixtures.services })
  } catch (error) {
    publish({ fatal: true, notice: 'startupFailed', detail: error instanceof Error ? error.message : String(error) })
  }
  ipcMain.handle('desktop:snapshot', (event) => { trustedSender(event); return state })
  ipcMain.handle('desktop:command', async (event, value: unknown) => {
    trustedSender(event)
    const command = parseCommand(value)
    if (command.action === 'connect') { await connect(command.origin); return }
    if (command.action === 'cancel' || command.action === 'disconnect') {
      requestId++
      selection?.disconnect()
      retireView()
      publish({ busy: false, activeOrigin: null, settingsOpen: true, notice: command.action === 'cancel' ? 'cancelled' : 'disconnected', detail: '' })
    } else if (command.action === 'settings') publish({ settingsOpen: true })
    else if (command.action === 'back' && activeScope && isCurrent(activeScope)) publish({ settingsOpen: false })
    else if (command.action === 'reload') await connect(state.selectedOrigin)
    sizeView()
  })
  await window.loadURL(`${shellOrigin}/index.html`)
  window.show()
  if (selection && !state.fatal) void connect(selection.selected.origin)
}

if (!app.requestSingleInstanceLock()) app.quit()
else {
  app.on('second-instance', () => { window?.show(); window?.focus() })
  app.on('before-quit', (event) => {
    if (quitting) return
    event.preventDefault()
    quitting = true
    requestId++
    selection?.close()
    retireView()
    for (const candidate of candidateSessions) retireSession(candidate)
    void Promise.all([fixtures?.close(), ...disposal]).catch((error: unknown) => { console.error('Desktop shutdown failed', error) }).finally(() => { app.quit() })
  })
  void start().catch((error: unknown) => {
    console.error('Desktop startup failed', error)
    dialog.showErrorBox('Workbench Preview', String(error))
    app.quit()
  })
}
