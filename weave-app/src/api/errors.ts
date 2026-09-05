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

export function apiErrorMessage(error: unknown): string {
  const normalized = normalizeThrownError(error);
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
