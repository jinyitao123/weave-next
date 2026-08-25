import { apiErrorFromResponse, normalizeThrownError } from "./errors";
import type { RealtimeEvent } from "./types";
import type { TokenRepository } from "../platform";

interface EventStreamOptions {
  baseUrl: string;
  tokens: TokenRepository;
  onEvent(event: RealtimeEvent): void;
  onConnectionChange(connected: boolean): void;
  refreshToken(): Promise<string>;
  onAuthError(): void;
}

interface ParsedFrame {
  event: string;
  data: string;
}

const reconnectDelays = [1_000, 2_000, 4_000, 8_000, 15_000, 30_000];

function parseFrame(frame: string): ParsedFrame | null {
  let event = "message";
  const data: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith(":")) continue;
    if (line.startsWith("event:")) event = line.slice(6).trim();
    if (line.startsWith("data:")) data.push(line.slice(5).trimStart());
  }
  return data.length ? { event, data: data.join("\n") } : null;
}

export class EventStreamClient {
  #controller: AbortController | null = null;
  #stopped = true;

  constructor(private readonly options: EventStreamOptions) {}

  start(): void {
    if (!this.#stopped) return;
    this.#stopped = false;
    void this.#connect();
  }

  stop(): void {
    this.#stopped = true;
    this.#controller?.abort();
    this.#controller = null;
    this.options.onConnectionChange(false);
  }

  async #connect(): Promise<void> {
    let attempt = 0;
    while (!this.#stopped) {
      const token = await this.options.tokens.read();
      if (!token) {
        this.options.onAuthError();
        return;
      }

      this.#controller = new AbortController();
      try {
        const response = await fetch(`${this.options.baseUrl}/v1/events`, {
          headers: {
            Accept: "text/event-stream",
            Authorization: `Bearer ${token}`,
          },
          signal: this.#controller.signal,
        });
        if (response.status === 401) {
          await this.options.refreshToken();
          continue;
        }
        if (!response.ok) throw await apiErrorFromResponse(response);
        if (!response.body) throw new Error("服务未返回事件流");

        this.options.onConnectionChange(true);
        attempt = 0;
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";

        while (!this.#stopped) {
          const { done, value } = await reader.read();
          if (done) break;
          buffer += decoder.decode(value, { stream: true }).replaceAll("\r\n", "\n");
          let boundary = buffer.indexOf("\n\n");
          while (boundary >= 0) {
            const frame = parseFrame(buffer.slice(0, boundary));
            buffer = buffer.slice(boundary + 2);
            if (frame) {
              const payload = JSON.parse(frame.data) as RealtimeEvent;
              this.options.onEvent({ ...payload, type: payload.type || frame.event });
            }
            boundary = buffer.indexOf("\n\n");
          }
        }
      } catch (error) {
        const normalized = normalizeThrownError(error);
        if (normalized.kind !== "aborted") console.warn("Weave event stream disconnected", normalized);
      } finally {
        // 被 stop()/替代实例中止时不回写离线状态——stop() 已负责置 false；
        // 否则旧实例的 finally 会把新实例已建立的 true 覆盖掉（StrictMode 双挂载竞态）。
        if (!this.#stopped) this.options.onConnectionChange(false);
        this.#controller = null;
      }

      if (this.#stopped) return;
      const delay = reconnectDelays[Math.min(attempt, reconnectDelays.length - 1)];
      attempt += 1;
      await new Promise<void>((resolve) => window.setTimeout(resolve, delay));
    }
  }
}
