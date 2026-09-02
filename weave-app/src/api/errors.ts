export type ApiErrorKind =
  | "bad_request"
  | "unauthenticated"
  | "forbidden"
  | "not_found"
  | "conflict"
  | "unprocessable"
  | "server"
  | "network"
  | "aborted"
  | "unknown";

const statusKinds: Record<number, ApiErrorKind> = {
  400: "bad_request",
  401: "unauthenticated",
  403: "forbidden",
  404: "not_found",
  409: "conflict",
  422: "unprocessable",
};

export class ApiError extends Error {
  readonly kind: ApiErrorKind;
  readonly status: number | null;
  readonly code: string | null;

  constructor(message: string, options: { kind: ApiErrorKind; status?: number; code?: string }) {
    super(message);
    this.name = "ApiError";
    this.kind = options.kind;
    this.status = options.status ?? null;
    this.code = options.code ?? null;
  }
}

export async function apiErrorFromResponse(response: Response): Promise<ApiError> {
  let code: string | undefined;
  let message = `请求失败（${response.status}）`;

  try {
    const payload = (await response.json()) as {
      code?: string;
      error?: string | { code?: string; message?: string };
      message?: string;
      problems?: Array<{ path?: string; message?: string }>;
    };
    const nested = typeof payload.error === "object" ? payload.error : undefined;
    code = payload.code || nested?.code;
    const problemMessage = payload.problems
      ?.filter((problem) => problem.message)
      .map((problem) => `${problem.path || "/"}：${problem.message}`)
      .join("；");
    message = payload.message || nested?.message || (typeof payload.error === "string" ? payload.error : "") || problemMessage || message;
  } catch {
    // A non-JSON error body still keeps its HTTP classification.
  }

  return new ApiError(message, {
    kind: statusKinds[response.status] ?? (response.status >= 500 ? "server" : "unknown"),
    status: response.status,
    code,
  });
}

export function normalizeThrownError(error: unknown): ApiError {
  if (error instanceof ApiError) return error;
  if (error instanceof DOMException && error.name === "AbortError") {
    return new ApiError("请求已取消", { kind: "aborted" });
  }
  if (error instanceof TypeError) {
    return new ApiError("无法连接 Weave 服务", { kind: "network" });
  }
  return new ApiError(error instanceof Error ? error.message : "发生未知错误", { kind: "unknown" });
}

const knownMessages: Record<string, string> = {
  invalid_include_archived: "归档筛选参数无效。",
  project_name_required: "请输入项目名称。",
  project_not_found: "项目不存在或已不可用。",
  invalid_project_avatar: "请选择当前工作区中可用的分身。",
  project_name_conflict: "这个分身下已有同名项目。",
  projects_unavailable: "项目服务尚未配置。",
  project_operation_failed: "项目操作失败，请稍后重试。",
  project_resource_not_found: "项目资料不存在或已被移除。",
  project_archived: "已归档项目不能新增资料。",
  project_resource_conflict: "该资料已绑定到项目。",
  invalid_project_resource: "这项资料无法添加，请重新选择。",
  project_resource_operation_failed: "项目资料操作失败。",
  invalid_client_request_id: "这次操作已失效，请重新发起。",
  client_request_conflict: "这次操作的内容已经变化，请重新发起。",
  chat_request_idempotency_unavailable: "请求保护暂不可用，请稍后重试。",
  chat_request_admission_failed: "任务暂时未能开始，请稍后重试。",
  client_request_in_progress: "该任务仍在执行，请稍候。",
  final_deliverables_unavailable: "最终交付物服务当前不可用。",
  final_deliverable_not_found: "最终交付物尚未生成或已不可用。",
  final_deliverable_read_failed: "最终交付物读取失败，请重试。",
  conversation_not_found: "会话不存在或当前账号无法访问。",
  invalid_conversation_title: "会话名称必须包含 1 到 80 个字符。",
  conversation_thread_immutable: "讨论线程不能重命名。",
  workflow_run_project_unavailable: "项目服务当前不可用，请稍后重试。",
  workflow_run_project_not_found: "请选择一个可用的项目。",
  workflow_run_project_archived: "所选项目已归档，请选择一个活跃项目。",
  workflow_run_project_avatar_mismatch: "请选择所属分身与工作流团队主分身一致的项目。",
  invalid_team_selector: "运行记录的筛选条件无效。",
  runtime_unavailable: "所选运行环境当前不可用。",
  no_eligible_runtime: "没有满足当前任务要求的在线运行环境。",
  runtime_override_not_supported: "当前智能体不支持指定运行环境。",
  project_usage_unavailable: "项目用量读取当前不可用。",
  project_memory_unavailable: "项目记忆服务当前不可用。",
  project_memory_list_failed: "项目记忆列表读取失败。",
  project_memory_create_failed: "项目记忆添加失败。",
  project_memory_delete_failed: "项目记忆删除失败。",
  workflow_skill_import_legacy_not_found: "没有找到该 ID 对应的 legacy Skill。",
  workflow_skill_import_invalid: "导入请求或 legacy Skill 内容无效。",
  workflow_skill_import_source_changed: "legacy Skill 源内容已变化，请重读 source hash 后再导入。",
  workflow_skill_import_idempotency_conflict: "该幂等键已用于不同的导入请求，请更换幂等键。",
  workflow_skill_import_unavailable: "legacy Skill 导入服务当前不可用。",
  workflow_skill_import_failed: "legacy Skill 导入失败，请稍后重试。",
  memory_disabled: "记忆未启用 · 嵌入服务未配置。",
  project_memory_search_failed: "项目记忆搜索失败。",
  content_required: "请输入记忆内容。",
  query_required: "请输入搜索内容。",
  project_run_filter_unavailable: "服务端项目 Run 筛选当前不可用。",
  client_request_replay: "服务端已复用同一请求。", 
  workflow_mcp_unsupported_transport: "当前不支持这种连接方式。",
  workflow_mcp_invalid_request: "连接配置不完整，请检查后重试。",
  workflow_mcp_server_not_found: "外部工具服务不存在或已删除。",
  workflow_mcp_server_conflict: "已有同名外部工具服务，或当前状态不允许修改。",
  workflow_mcp_server_closed: "外部工具服务已停用，不能继续操作。",
  workflow_mcp_credential_unavailable: "安全凭据服务不可用，请联系管理员。",
  workflow_provider_revision_required: "模型供应商配置不完整，请检查后重试。",
  workflow_credential_unavailable: "凭据加密材料不可用或无法解密。",
  workflow_frozen_manifest_mismatch: "配置已发生变化，请刷新后重试。",
  workflow_delivery_invalid_request: "交付目标配置不完整，请检查后重试。",
  workflow_delivery_target_not_found: "交付目标不存在或已不可用。",
  workflow_delivery_target_closed: "交付目标已关闭，不能继续修改。",
  workflow_delivery_store_unavailable: "交付服务尚未配置。",
  workflow_delivery_store_failed: "交付服务操作失败，请稍后重试。",
  team_roster_write_required: "请从团队成员页面修改成员关系。",
  provider_required: "当前工作区没有可用的模型供应商。请先在控制中心 → 模型供应商中镜像系统供应商，再创建新团队。",
  template_idempotency_conflict: "同一请求标识已经用于另一份模板，请关闭后重新发起创建。",
  template_build_failed: "模板构建未能完成，请查看构建步骤后重试。",
  team_template_unavailable: "团队模板服务当前不可用。",
};

export function apiErrorMessage(error: unknown): string {
  const normalized = normalizeThrownError(error);
  if (normalized.code && knownMessages[normalized.code]) return knownMessages[normalized.code];
  switch (normalized.kind) {
    case "bad_request":
    case "unprocessable":
      return "请求内容有误，请检查后重试。";
    case "unauthenticated":
      return "登录状态已失效，请重新登录。";
    case "forbidden":
      return "当前账号没有执行此操作的权限。";
    case "not_found":
      return "目标内容不存在或已不可用。";
    case "conflict":
      return "内容已发生变化，请刷新后重试。";
    case "server":
      return "服务暂时不可用，请稍后重试。";
    case "network":
      return "暂时无法连接 Weave，请检查网络后重试。";
    case "aborted":
      return "操作已停止。";
    default:
      return "操作未完成，请稍后重试。";
  }
}
