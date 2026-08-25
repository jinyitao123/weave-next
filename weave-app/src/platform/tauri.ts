import type {
  DesktopBridge,
  LocalRuntimeStartOptions,
  LocalRuntimeStatus,
  LocalWorkspaceSelection,
} from "./platform";

type TauriInvoke = <T>(command: string, args?: Record<string, unknown>) => Promise<T>;

interface TauriWindow extends Window {
  __TAURI__?: {
    core?: {
      invoke?: unknown;
    };
  };
}

function globalInvoke(target: Window): TauriInvoke | null {
  const candidate = (target as TauriWindow).__TAURI__?.core?.invoke;
  if (typeof candidate !== "function") return null;
  return candidate as TauriInvoke;
}

export function createTauriDesktopBridge(target: Window): DesktopBridge | null {
  const invoke = globalInvoke(target);
  if (!invoke) return null;

  return {
    readToken: () => invoke<string | null>("read_auth_token"),
    writeToken: (token) => invoke<void>("write_auth_token", { token }),
    clearToken: () => invoke<void>("clear_auth_token"),
    chooseLocalWorkspace: () => invoke<LocalWorkspaceSelection | null>("choose_local_workspace"),
    openLocalWorkspace: (handle) => invoke<void>("open_local_workspace", { handle }),
    openDiff: (handle) => invoke<void>("open_diff", { handle }),
    startLocalRuntime: (request: LocalRuntimeStartOptions) =>
      invoke<LocalRuntimeStatus>("start_local_runtime", { request }),
    inspectLocalRuntime: () => invoke<LocalRuntimeStatus>("inspect_local_runtime"),
    stopLocalRuntime: () => invoke<LocalRuntimeStatus>("stop_local_runtime"),
  };
}
