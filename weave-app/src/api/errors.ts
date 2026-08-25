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
    };
    const nested = typeof payload.error === "object" ? payload.error : undefined;
    code = payload.code || nested?.code;
    message = payload.message || nested?.message || (typeof payload.error === "string" ? payload.error : "") || message;
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
  invalid_project_resource: "资料引用不符合后端合同。",
  project_resource_operation_failed: "项目资料操作失败。",
  invalid_client_request_id: "请求标识不是有效 UUID。",
  client_request_conflict: "同一请求标识对应了不同内容。",
  chat_request_idempotency_unavailable: "服务端持久幂等暂不可用。",
  chat_request_admission_failed: "对话请求未能进入服务端。",
  client_request_in_progress: "该请求仍在服务端执行。",
  final_deliverables_unavailable: "最终交付物服务当前不可用。",
  final_deliverable_not_found: "最终交付物尚未生成或已不可用。",
  final_deliverable_read_failed: "最终交付物读取失败，请重试。",
  conversation_not_found: "Conversation 不存在或当前用户不可访问。",
  invalid_conversation_title: "会话名称必须包含 1 到 80 个字符。",
  conversation_thread_immutable: "讨论线程不能重命名。",
  workflow_run_project_unavailable: "Workflow Run 的 Project 服务当前不可用，请稍后重试或配置 Project 服务。",
  workflow_run_project_not_found: "请选择一个存在且可用的活跃 Project。",
  workflow_run_project_archived: "所选 Project 已归档，请选择一个活跃 Project。",
  workflow_run_project_avatar_mismatch: "请选择所属分身与工作流团队主分身一致的项目。",
  invalid_team_selector: "Run 筛选参数无效。",
  runtime_unavailable: "所选 Runtime 当前不可用。",
  no_eligible_runtime: "没有满足当前引擎要求的在线 Runtime。",
  runtime_override_not_supported: "当前 Agent 引擎不支持指定 Runtime。",
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
  workflow_mcp_unsupported_transport: "MCP transport 不受支持。",
  workflow_mcp_invalid_request: "MCP Server 字段不符合后端合同。",
  workflow_mcp_server_not_found: "MCP Server 不存在或已删除。",
  workflow_mcp_server_conflict: "MCP Server slug 已存在或当前状态冲突。",
  workflow_mcp_server_closed: "MCP Server 已禁用、撤销或删除，不能执行该操作。",
  workflow_mcp_credential_unavailable: "MCP registry 加密密钥不可用。请配置服务端密钥后重试。",
  workflow_provider_revision_required: "Provider 配置不完整、ID 保留或字段不符合 revision 合同。",
  workflow_credential_unavailable: "凭据加密材料不可用或无法解密。",
  workflow_frozen_manifest_mismatch: "冻结配置清单与当前 revision 不一致。",
  workflow_delivery_invalid_request: "Delivery Target 请求字段不符合后端合同。",
  workflow_delivery_target_not_found: "Delivery Target 不存在或已不可用。",
  workflow_delivery_target_closed: "Delivery Target 已关闭，后端不允许继续修改。",
  workflow_delivery_store_unavailable: "Delivery Target store 未配置。",
  workflow_delivery_store_failed: "Delivery Target store 操作失败。",
  team_roster_write_required: "Team 成员关系只能通过 Teams 的完整 Roster command 修改。",
  provider_required: "当前工作区没有可用的模型供应商。请先在控制中心 → 模型供应商中镜像系统供应商，再创建新团队。",
};

export function apiErrorMessage(error: unknown): string {
  const normalized = normalizeThrownError(error);
  return (normalized.code && knownMessages[normalized.code]) || normalized.message;
}
