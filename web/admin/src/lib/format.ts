// Display helpers. Internal codes never reach the page as-is: every code maps
// to readable copy, and unknown codes fall back to a neutral label.

const engineNames: Record<string, string> = { claude: 'Claude', codex: 'Codex', opencode: 'OpenCode', loom: 'Loom' }

export function engineName(engine: string): string {
  return engineNames[engine] ?? '其他引擎'
}

const reasonCopy: Record<string, string> = {
  runtime_disabled: '已停用',
  runtime_revoked: '已吊销',
  runtime_paused: '已暂停',
  runtime_offline: '离线',
  runtime_quarantined: '连续执行环境故障，已隔离',
  engine_unavailable: '未安装或不可用',
  provider_credentials_missing: '缺少模型凭据',
  codex_login_required: '需要登录 Codex',
}

export function reasonText(reason?: string): string {
  if (!reason) return ''
  return reasonCopy[reason] ?? '暂不可用'
}

const authModeCopy: Record<string, string> = {
  chatgpt: 'ChatGPT 订阅登录',
  oauth: 'Claude 订阅登录',
  provider: '模型供应方密钥',
  subject_provider: '平台下发凭据',
}

export function authModeText(mode?: string): string {
  if (!mode || mode === 'unknown') return '未识别'
  return authModeCopy[mode] ?? '其他方式'
}

const relative = new Intl.RelativeTimeFormat('zh-CN', { numeric: 'auto' })

export function relativeTime(value?: string | null, now = Date.now()): string {
  if (!value) return '从未'
  const seconds = Math.round((new Date(value).getTime() - now) / 1000)
  if (!Number.isFinite(seconds)) return '未知'
  const abs = Math.abs(seconds)
  if (abs < 45) return '刚刚'
  if (abs < 3600) return relative.format(Math.round(seconds / 60), 'minute')
  if (abs < 86400) return relative.format(Math.round(seconds / 3600), 'hour')
  return relative.format(Math.round(seconds / 86400), 'day')
}

const checkTitles: Record<string, string> = {
  _coverage: '验证要求覆盖',
  _output: '输出格式',
  _external_effects: '外部影响',
}

export function checkTitle(checkId: string, title?: string): string {
  if (title) return title
  if (checkId.startsWith('_limitation:')) return '无法自动核验的要求'
  return checkTitles[checkId] ?? '检查'
}

const checkReasons: Record<string, string> = {
  requirements_not_explicit: '团队没有声明可计算的验证要求',
  unsupported_requirement: '存在无法自动核验的要求',
  output_type_valid: '输出格式符合要求',
  external_effects_scope_unverified: '未核实对外部系统的影响',
}

export function checkReason(reason?: string): string {
  if (!reason) return ''
  return checkReasons[reason] ?? ''
}
