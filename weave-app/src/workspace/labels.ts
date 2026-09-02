/**
 * 全站统一的「后端枚举 / 标识 → 用户可读中文」映射层。
 *
 * 规则（与《对话为主、证据随行》简报一致）：
 * - 后端枚举值、内部标识符、错误码绝不直接渲染给用户；
 * - 映射不到时回退为通用中文文案，不回显原始值；
 * - 新增枚举时在这里加映射，而不是在使用处各建一张表。
 */

/** Agent 角色：avatar=分身（面向人），worker=数字员工（面向任务）。 */
export function agentRoleLabel(role?: string): string {
  switch ((role || "").toLowerCase()) {
    case "avatar":
      return "分身";
    case "worker":
      return "数字员工";
    case "lead":
      return "负责人";
    default:
      return "成员";
  }
}

/**
 * 执行引擎。引擎名是供应商产品名，保留品牌写法；loom 是内置执行器。
 * 未知引擎不回显原始字符串（避免配置错误直接把脏数据摆上界面）。
 */
export function engineLabel(engine?: string): string {
  switch ((engine || "").toLowerCase()) {
    case "":
    case "loom":
      return "内置";
    case "claude":
      return "Claude Code";
    case "codex":
      return "Codex";
    case "opencode":
      return "OpenCode";
    default:
      return "外部引擎";
  }
}

/** 团队成员调用方式（TeamWorker allowed_kinds / default_kind）。 */
export function collabKindLabel(kind?: string): string {
  switch (kind) {
    case "consult":
      return "同步咨询";
    case "dispatch":
      return "异步派工";
    case "handoff":
      return "交接处理";
    default:
      return "协作";
  }
}

/** 定时任务类型。 */
export function scheduleKindLabel(kind?: string): string {
  switch (kind) {
    case "daily":
      return "每天";
    case "once":
      return "单次";
    default:
      return "定时";
  }
}

/** 数据源同步方式。 */
export function syncModeLabel(mode?: string): string {
  switch (mode) {
    case "manual":
      return "手动同步";
    case "batch":
      return "定时批量";
    case "event":
      return "事件触发";
    default:
      return "同步";
  }
}

export function syncFrequencyLabel(frequency?: string): string {
  switch (frequency) {
    case "hourly":
      return "每小时";
    case "daily":
      return "每天";
    case "weekly":
      return "每周";
    default:
      return "定期";
  }
}

export function syncStrategyLabel(strategy?: string): string {
  switch (strategy) {
    case "full":
      return "全量";
    case "incremental":
      return "增量";
    default:
      return "同步";
  }
}

export function syncConflictLabel(conflict?: string): string {
  switch (conflict) {
    case "source":
      return "以来源为准";
    case "local":
      return "以本地为准";
    case "mark":
      return "仅标记冲突";
    default:
      return "按默认规则";
  }
}

/**
 * 执行过程步骤名。已知的内部步骤映射为动作描述；未映射的返回 mapped=false，
 * 调用方应显示通用文案（如「处理中」）而不是原始步骤标识符。
 */
const EXECUTION_STEP_LABELS: Record<string, string> = {
  team_context: "读取团队上下文",
  chat: "模型推理",
  prompt_assemble: "组装上下文",
  memory_retrieve: "检索记忆",
  guard: "安全检查",
};

export function executionStepLabel(step?: string): { label: string; mapped: boolean } {
  if (!step) return { label: "处理中", mapped: true };
  const label = EXECUTION_STEP_LABELS[step];
  return label ? { label, mapped: true } : { label: "处理中", mapped: false };
}

export const UUID_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * 技术标识符判定：UUID、内部平台资产名（__ 前缀）、服务端拼接 ID
 * （frt1_/flt1_/task- 前缀、default: 组合键）。命中即不得直接渲染。
 */
export function isTechnicalIdentifier(value?: string): boolean {
  if (!value) return false;
  return UUID_PATTERN.test(value)
    || value.startsWith("__")
    || /^(?:frt1_|flt1_|task-|default:|[0-9a-f]{8}-[0-9a-f-]{27,})/i.test(value);
}

/**
 * 名称展示闸门：名称缺失或本质是技术标识符时，回退为通用中文称呼，
 * 绝不把原始 ID/内部名回显到界面。
 */
export function displayName(name: string | undefined | null, fallback: string): string {
  const trimmed = (name || "").trim();
  return trimmed && !isTechnicalIdentifier(trimmed) ? trimmed : fallback;
}

/**
 * 枚举展示闸门。调用方只提供受控映射；未收录的新值永远不会穿透到界面。
 */
export function enumLabel(labels: Readonly<Record<string, string>>, value: string | undefined | null, fallback: string): string {
  return value ? labels[value] || fallback : fallback;
}

const BUSINESS_FIELD_LABELS: Record<string, string> = {
  action: "处理方式",
  approve: "审批决定",
  comment: "补充说明",
  decision: "处理决定",
  feedback: "修改意见",
  message: "继续说明",
  note: "备注",
  reason: "原因",
  reject: "驳回原因",
};

/** 结构化工作流字段 → 业务称呼；陌生的代码式字段名不会直接暴露。 */
export function businessFieldLabel(name?: string): string {
  const trimmed = (name || "").trim();
  const known = BUSINESS_FIELD_LABELS[trimmed.toLowerCase()];
  if (known) return known;
  if (!trimmed || isTechnicalIdentifier(trimmed) || /^[a-z][a-z0-9_-]*$/i.test(trimmed)) return "补充信息";
  return trimmed;
}

/** 用户角色 → 中文。 */
export function userRoleLabel(role?: string): string {
  switch (role) {
    case "owner":
      return "所有者";
    case "admin":
      return "管理员";
    default:
      return "成员";
  }
}

/** 运行停止原因 → 中文；未映射时回退通用文案，不回显原始枚举。 */
export function stopReasonLabel(reason?: string): string {
  switch ((reason || "").trim()) {
    case "":
      return "未记录";
    case "end_turn":
    case "stop":
      return "自然结束";
    case "max_tokens":
    case "length":
      return "达到长度上限";
    case "cancelled":
      return "已取消";
    case "timed_out":
    case "timeout":
      return "超时";
    case "error":
    case "failed":
      return "异常结束";
    default:
      return "已结束";
  }
}

/** 相对时间：1 分钟内「刚刚」，之后逐级到具体日期。无效输入回退空串。 */
export function relativeTimeLabel(value?: string, now = Date.now()): string {
  if (!value) return "";
  const time = Date.parse(value);
  if (!Number.isFinite(time)) return "";
  const seconds = Math.max(0, Math.floor((now - time) / 1000));
  if (seconds < 60) return "刚刚";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "昨天";
  if (days < 7) return `${days} 天前`;
  return new Intl.DateTimeFormat("zh-CN", { month: "short", day: "numeric" }).format(new Date(time));
}

/**
 * 执行过程里成员标识的兜底显示名。仅在拿不到真实名称时使用：
 * UUID、内部平台资产名（__ 前缀）一律不展示碎片，回退为通用称呼。
 */
export function memberFallbackLabel(identifier: string): string {
  if (!identifier || isTechnicalIdentifier(identifier)) {
    return "一位成员";
  }
  return identifier;
}

/** 模型用量展示：输入/输出 token 的人性化文案。 */
export function usageLabel(tokensIn?: number | null, tokensOut?: number | null): string {
  const input = tokensIn ?? 0;
  const output = tokensOut ?? 0;
  if (!input && !output) return "无模型用量记录";
  return `输入 ${formatTokenCount(input)} · 输出 ${formatTokenCount(output)}`;
}

export function formatTokenCount(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`;
  if (value >= 10_000) return `${(value / 1_000).toFixed(1)}k`;
  return String(value);
}

/** 成本展示：不满 1 美分显示「<¥0.01」级别的人性化金额，避免 6 位小数美元。 */
export function costLabel(usd?: number | null): string {
  if (usd === null || usd === undefined || Number.isNaN(usd)) return "费用未统计";
  if (usd === 0) return "¥0";
  if (usd < 0.01) return "<¥0.01";
  return `¥${usd.toFixed(2)}`;
}

/** API 密钥权限范围。 */
export const API_SCOPE_OPTIONS: Array<{ value: string; label: string; description: string }> = [
  { value: "chat", label: "对话", description: "使用对话、会话与交付物" },
  { value: "runs", label: "运行与用量", description: "查看运行、任务与用量" },
  { value: "agents", label: "智能体与技能", description: "管理智能体、技能和附件，查看 MCP 服务器" },
  { value: "memory", label: "记忆", description: "管理智能体和项目记忆" },
  { value: "org", label: "工作区与团队", description: "管理工作区、项目、团队、工作流与运行时" },
  { value: "admin", label: "管理功能", description: "使用用户、API 密钥、模型、MCP 和交付等管理功能" },
];

export function apiScopeLabel(scope: string): string {
  return API_SCOPE_OPTIONS.find((option) => option.value === scope)?.label || "未知权限";
}

/** 交付物内容类型 → 中文。MIME 串不出现在用户可见处。 */
export function deliverableTypeLabel(contentType?: string): string {
  const type = (contentType || "").split(";", 1)[0].trim().toLowerCase();
  switch (type) {
    case "text/markdown":
      return "文档";
    case "text/html":
      return "网页";
    case "image/svg+xml":
      return "SVG 图";
    case "text/plain":
      return "文本";
    case "application/json":
      return "JSON";
    case "application/pdf":
      return "PDF";
    default:
      return "文件";
  }
}
