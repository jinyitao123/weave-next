export interface TokenRepository {
  read(): Promise<string | null>;
  write(token: string): Promise<void>;
  clear(): Promise<void>;
}

export interface LocalWorkspaceSelection {
  handle: string;
  displayName: string;
}

export function isOpaqueUUIDHandle(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
}

export interface LocalRuntimeStartOptions {
  serverUrl: string;
  runtimeToken: string;
  concurrency: number;
  binaryPath?: string;
}

export interface LocalRuntimeStatus {
  running: boolean;
  pid?: number;
  startedAt?: string;
  lastError?: string;
}

export interface PlatformCapabilities {
  localWorkspace: boolean;
  folderOpener: boolean;
  diffViewer: boolean;
  localRuntime: boolean;
}

export interface PlatformAdapter {
  readonly kind: "browser" | "desktop";
  readonly capabilities: PlatformCapabilities;
  readonly tokens: TokenRepository;
  chooseLocalWorkspace(): Promise<LocalWorkspaceSelection | null>;
  openLocalWorkspace(workspaceHandle: string): Promise<void>;
  openDiff(workspaceHandle: string): Promise<void>;
  startLocalRuntime(options: LocalRuntimeStartOptions): Promise<LocalRuntimeStatus>;
  inspectLocalRuntime(): Promise<LocalRuntimeStatus>;
  stopLocalRuntime(): Promise<LocalRuntimeStatus>;
}

export interface DesktopBridge {
  readToken(): Promise<string | null>;
  writeToken(token: string): Promise<void>;
  clearToken(): Promise<void>;
  chooseLocalWorkspace(): Promise<LocalWorkspaceSelection | null>;
  openLocalWorkspace(workspaceHandle: string): Promise<void>;
  openDiff(workspaceHandle: string): Promise<void>;
  startLocalRuntime(options: LocalRuntimeStartOptions): Promise<LocalRuntimeStatus>;
  inspectLocalRuntime(): Promise<LocalRuntimeStatus>;
  stopLocalRuntime(): Promise<LocalRuntimeStatus>;
}
