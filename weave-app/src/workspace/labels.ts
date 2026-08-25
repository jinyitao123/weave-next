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
 * 执行过程里成员标识的兜底显示名。仅在拿不到真实名称时使用：
 * UUID、内部平台资产名（__ 前缀）一律不展示碎片，回退为通用称呼。
 */
export function memberFallbackLabel(identifier: string): string {
  if (!identifier || UUID_PATTERN.test(identifier) || identifier.startsWith("__")) {
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
