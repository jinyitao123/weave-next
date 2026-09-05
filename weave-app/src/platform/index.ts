import { browserPlatform } from "./browser";
import { createDesktopPlatform } from "./desktop";
import type { DesktopBridge, PlatformAdapter } from "./platform";
import { createTauriDesktopBridge } from "./tauri";

const bridge: DesktopBridge | null = createTauriDesktopBridge(window);

export const platform: PlatformAdapter = bridge
  ? createDesktopPlatform(bridge)
  : browserPlatform;

export type {
  DesktopBridge,
  LocalRuntimeStartOptions,
  LocalRuntimeStatus,
  PlatformAdapter,
  PlatformCapabilities,
  TokenRepository,
} from "./platform";
