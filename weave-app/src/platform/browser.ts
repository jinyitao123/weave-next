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
    localRuntime: false,
  },
  tokens: new SessionTokenRepository(),
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
