import { platform } from "../platform";
import { ApiClient } from "./client";
import { agentsMethods } from "./agents";
import { authMethods } from "./auth";
import { buildRunsMethods } from "./buildruns";
import { conversationsMethods } from "./conversations";
import { deliverablesMethods } from "./deliverables";
import { projectsMethods } from "./projects";
import { runsMethods } from "./runs";
import { schedulesMethods } from "./schedules";
import { settingsMethods } from "./settings";
import { teamsMethods } from "./teams";
import { workflowsMethods } from "./workflows";

export const apiBaseUrl = import.meta.env.VITE_WEAVE_API_URL?.replace(/\/+$/, "") ?? "";

/**
 * 按域拆分后的 API 单例：核心（client.ts）+ 各域方法模块合并。
 * 调用方 import 路径与既有 `api.*` 用法保持不变。
 */
export const api = Object.assign(
  Object.assign(
    Object.assign(
      Object.assign(
        Object.assign(
          new ApiClient(platform.tokens, apiBaseUrl),
          authMethods,
          agentsMethods,
        ),
        teamsMethods,
        workflowsMethods,
      ),
      projectsMethods,
      conversationsMethods,
    ),
    runsMethods,
    deliverablesMethods,
  ),
  buildRunsMethods,
  schedulesMethods,
  settingsMethods,
);

export { ApiError, apiErrorMessage, normalizeThrownError } from "./errors";
export { EventStreamClient } from "./sse";
export type * from "./types";
