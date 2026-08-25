import type { PlatformAdapter, TokenRepository } from "./platform";

const TOKEN_KEY = "weave.session.token";

class SessionTokenRepository implements TokenRepository {
  async read(): Promise<string | null> {
    return sessionStorage.getItem(TOKEN_KEY);
  }

  async write(token: string): Promise<void> {
    sessionStorage.setItem(TOKEN_KEY, token);
  }

  async clear(): Promise<void> {
    sessionStorage.removeItem(TOKEN_KEY);
  }
}

export const browserPlatform: PlatformAdapter = {
  kind: "browser",
  capabilities: {
    localWorkspace: false,
    folderOpener: false,
    diffViewer: false,
    localRuntime: false,
  },
  tokens: new SessionTokenRepository(),
  async chooseLocalWorkspace() {
    return null;
  },
  async openLocalWorkspace() {
    throw new Error("浏览器无法打开本机工作目录");
  },
  async openDiff() {
    throw new Error("浏览器无法打开本机差异视图");
  },
  async startLocalRuntime() {
    throw new Error("浏览器无法启动本机 Runtime");
  },
  async inspectLocalRuntime() {
    return { running: false };
  },
  async stopLocalRuntime() {
    throw new Error("浏览器无法停止本机 Runtime");
  },
};
