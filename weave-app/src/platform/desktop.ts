import type { DesktopBridge, PlatformAdapter } from "./platform";

export function createDesktopPlatform(bridge: DesktopBridge): PlatformAdapter {
  return {
    kind: "desktop",
    capabilities: {
      localWorkspace: true,
      folderOpener: true,
      diffViewer: true,
      localRuntime: true,
    },
    tokens: {
      read: () => bridge.readToken(),
      write: (token) => bridge.writeToken(token),
      clear: () => bridge.clearToken(),
    },
    chooseLocalWorkspace: () => bridge.chooseLocalWorkspace(),
    openLocalWorkspace: (workspaceHandle) => bridge.openLocalWorkspace(workspaceHandle),
    openDiff: (workspaceHandle) => bridge.openDiff(workspaceHandle),
    startLocalRuntime: (options) => bridge.startLocalRuntime(options),
    inspectLocalRuntime: () => bridge.inspectLocalRuntime(),
    stopLocalRuntime: () => bridge.stopLocalRuntime(),
  };
}
