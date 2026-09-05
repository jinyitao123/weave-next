export interface TokenRepository {
  read(): Promise<string | null>;
  write(token: string): Promise<void>;
  clear(): Promise<void>;
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
  localRuntime: boolean;
}

export interface PlatformAdapter {
  readonly kind: "browser" | "desktop";
  readonly capabilities: PlatformCapabilities;
  readonly tokens: TokenRepository;
  startLocalRuntime(options: LocalRuntimeStartOptions): Promise<LocalRuntimeStatus>;
  inspectLocalRuntime(): Promise<LocalRuntimeStatus>;
  stopLocalRuntime(): Promise<LocalRuntimeStatus>;
}

export interface DesktopBridge {
  readToken(): Promise<string | null>;
  writeToken(token: string): Promise<void>;
  clearToken(): Promise<void>;
  startLocalRuntime(options: LocalRuntimeStartOptions): Promise<LocalRuntimeStatus>;
  inspectLocalRuntime(): Promise<LocalRuntimeStatus>;
  stopLocalRuntime(): Promise<LocalRuntimeStatus>;
}
