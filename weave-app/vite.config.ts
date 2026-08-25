import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// 最小 process 声明：前端 tsconfig 不含 @types/node，vite 配置在 Node 侧运行。
declare const process: { env: Record<string, string | undefined> };
declare const setImmediate: (callback: () => void) => void;

type FlushableServerResponse = {
  readonly headersSent: boolean;
  flushHeaders?: () => void;
};

type ProxyResponseHook = {
  on: (
    event: "proxyRes",
    listener: (proxyRes: unknown, req: unknown, res: FlushableServerResponse) => void,
  ) => void;
};

export default defineConfig({
  plugins: [react()],
  clearScreen: false,
  envPrefix: ["VITE_", "TAURI_ENV_"],
  server: {
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    proxy: {
      "/v1": {
        target: process.env.VITE_WEAVE_PROXY_TARGET ?? "http://127.0.0.1:8081",
        changeOrigin: true,
        configure: (proxy) => {
          const responseHook = proxy as unknown as ProxyResponseHook;
          responseHook.on("proxyRes", (_proxyRes, _req, res) => {
            setImmediate(() => {
              if (!res.headersSent && typeof res.flushHeaders === "function") {
                res.flushHeaders();
              }
            });
          });
        },
      },
    },
  },
});
