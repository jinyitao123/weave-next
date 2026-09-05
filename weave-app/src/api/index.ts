import { platform } from "../platform";
import { ApiClient } from "./client";
import { authMethods } from "./auth";
import { runtimeMethods } from "./runtimes";

export const apiBaseUrl = import.meta.env.VITE_WEAVE_API_URL?.replace(/\/+$/, "") ?? "";

/** The maintenance UI only consumes identity and runtime APIs. */
export const api = Object.assign(new ApiClient(platform.tokens, apiBaseUrl), authMethods, runtimeMethods);

export { ApiError, apiErrorMessage, normalizeThrownError } from "./errors";
export type * from "./types";
