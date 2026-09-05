import type { DesktopBridge, PlatformAdapter } from "./platform";

export function createDesktopPlatform(bridge: DesktopBridge): PlatformAdapter {
  return {
    kind: "desktop",
    capabilities: {
      localRuntime: true,
    },
    tokens: {
      read: () => bridge.readToken(),
      write: (token) => bridge.writeToken(token),
      clear: () => bridge.clearToken(),
    },
    startLocalRuntime: (options) => bridge.startLocalRuntime(options),
    inspectLocalRuntime: () => bridge.inspectLocalRuntime(),
    stopLocalRuntime: () => bridge.stopLocalRuntime(),
  };
}
