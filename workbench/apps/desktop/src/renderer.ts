/** Trusted shell renderer; every user-originated value is sent through a narrow bridge. */
import type { ShellBridge, ShellState } from './shell-protocol.js'

declare global { interface Window { workbenchDesktop: ShellBridge } }

const notices: Record<string, string> = {
  welcome: '选择服务，打开你的工作空间。', connecting: '正在验证连接…', connected: '已连接 · 验证环境',
  cancelled: '已取消连接，保留上次选择。', disconnected: '已断开连接，保留上次选择。',
  connectionFailed: '连接未成功。请检查服务后重试。', unsupportedOrigin: '此预览仅支持下方两个验证服务。',
  startupFailed: '无法读取本地配置或启动验证服务。', downloadComplete: '样例文件已保存',
  downloadCancelled: '已取消下载', downloadFailed: '下载未完成，请重试', linkBlocked: '无法打开此类型的链接',
  loadFailed: '页面加载失败，请重新连接。',
}
const element = (id: string): HTMLElement => {
  const value = document.getElementById(id)
  if (!value) throw new Error(`Missing shell element: ${id}`)
  return value
}
const input = element('origin') as HTMLInputElement
const button = (id: string): HTMLButtonElement => {
  const value = element(id)
  if (!(value instanceof HTMLButtonElement)) throw new Error(`Missing shell button: ${id}`)
  return value
}
const presets = element('presets')
let previousOrigin: string | null = null
let currentState: ShellState | null = null

function render(state: ShellState): void {
  currentState = state
  element('panel').hidden = !state.settingsOpen
  element('notice').textContent = notices[state.notice] ?? state.notice
  element('detail').textContent = state.detail
  element('detail').hidden = !state.detail
  element('service-chip').textContent = state.activeOrigin ? `已连接 · 服务 ${state.services.find(service => service.origin === state.activeOrigin)?.name ?? ''}` : '尚未连接'
  element('download-status').textContent = state.notice.startsWith('download') || state.notice === 'linkBlocked' ? notices[state.notice] ?? '' : ''
  button('connect').disabled = state.busy || state.fatal
  element('connect').textContent = state.busy ? '正在连接…' : '连接工作空间'
  button('cancel').hidden = !state.busy
  button('back').hidden = !state.activeOrigin || state.busy
  button('disconnect').disabled = !state.activeOrigin && !state.busy
  button('reload').disabled = !state.activeOrigin || state.busy
  input.disabled = state.busy || state.fatal
  element('instance').textContent = state.instanceId ? `实例 ${state.instanceId.slice(0, 8)}` : '连接成功后会记住此服务'
  if (previousOrigin !== state.selectedOrigin) {
    input.value = state.selectedOrigin
    previousOrigin = state.selectedOrigin
  }
  presets.replaceChildren(...state.services.map((service) => {
    const button = document.createElement('button')
    button.type = 'button'
    button.className = input.value === service.origin ? 'preset selected' : 'preset'
    button.textContent = `服务 ${service.name}`
    button.disabled = state.busy || state.fatal
    button.addEventListener('click', () => { input.value = service.origin; render(state) })
    return button
  }))
}

async function command(action: 'connect' | 'cancel' | 'disconnect' | 'settings' | 'back' | 'reload'): Promise<void> {
  try { await window.workbenchDesktop.command(action === 'connect' ? { action, origin: input.value.trim() } : { action }) } catch {
    element('notice').textContent = '桌面操作未完成，请重试。'
  }
}
for (const action of ['cancel', 'disconnect', 'settings', 'back', 'reload'] as const) {
  element(action).addEventListener('click', () => { void command(action) })
}
element('connection-form').addEventListener('submit', (event) => { event.preventDefault(); void command('connect') })
input.addEventListener('input', () => { if (currentState) render(currentState) })
window.workbenchDesktop.subscribe(render)
void window.workbenchDesktop.snapshot().then(render)
