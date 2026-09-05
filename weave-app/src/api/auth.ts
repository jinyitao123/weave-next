import type { ApiClient } from "./client";
import type { User } from "./types";

/** Identity for the signed-in runtime administrator. */
export const authMethods = {
  getMe(this: ApiClient, signal?: AbortSignal): Promise<User> {
    return this.request<User>("/v1/auth/me", { signal });
  },
};
