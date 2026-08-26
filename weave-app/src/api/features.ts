import type { ApiClient } from "./client";
import type { PlatformFeatures } from "./types";

export const featuresMethods = {
  getFeatures(this: ApiClient, signal?: AbortSignal): Promise<PlatformFeatures> {
    return this.request<PlatformFeatures>("/v1/features", { signal });
  },
};
