import { ApiError, apiErrorFromResponse, normalizeThrownError } from "./errors";
import type { LoginRequest, LoginResponse, RefreshResponse } from "./types";
import type { TokenRepository } from "../platform";

interface RequestOptions extends Omit<RequestInit, "body"> {
  body?: unknown;
  authenticated?: boolean;
  retryAfterRefresh?: boolean;
}

/** Authenticated JSON requests and the existing login/token-refresh lifecycle. */
export class ApiClient {
  #token: string | null = null;
  #refreshing: Promise<string> | null = null;

  constructor(
    readonly tokens: TokenRepository,
    readonly baseUrl = "",
  ) {}

  setToken(token: string | null): void {
    this.#token = token;
  }

  async request<T>(path: string, options: RequestOptions = {}): Promise<T> {
    const authenticated = options.authenticated !== false;
    const headers = new Headers(options.headers);
    const method = (options.method || "GET").toUpperCase();
    headers.set("Accept", "application/json");
    if (options.body !== undefined) headers.set("Content-Type", "application/json");
    if (authenticated && this.#token) headers.set("Authorization", `Bearer ${this.#token}`);

    try {
      const response = await fetch(`${this.baseUrl}${path}`, {
        ...options,
        headers,
        body: options.body === undefined ? undefined : JSON.stringify(options.body),
      });

      if (
        response.status === 401 &&
        authenticated &&
        this.#token &&
        (method === "GET" || method === "HEAD") &&
        options.retryAfterRefresh !== false &&
        path !== "/v1/auth/refresh"
      ) {
        await this.refreshToken();
        return this.request<T>(path, { ...options, retryAfterRefresh: false });
      }
      if (!response.ok) throw await apiErrorFromResponse(response);
      if (response.status === 204) return undefined as T;
      return (await response.json()) as T;
    } catch (error) {
      throw normalizeThrownError(error);
    }
  }

  async login(input: LoginRequest): Promise<LoginResponse> {
    const response = await this.request<LoginResponse>("/v1/auth/login", {
      method: "POST",
      body: input,
      authenticated: false,
    });
    await this.#acceptToken(response.token);
    return response;
  }

  async refreshToken(): Promise<string> {
    if (!this.#token) throw new ApiError("没有可刷新的会话", { kind: "unauthenticated" });
    if (this.#refreshing) return this.#refreshing;

    this.#refreshing = this.request<RefreshResponse>("/v1/auth/refresh", {
      method: "POST",
      retryAfterRefresh: false,
    })
      .then(async ({ token }) => {
        await this.#acceptToken(token);
        return token;
      })
      .catch(async (error: unknown) => {
        await this.clearToken();
        throw error;
      })
      .finally(() => {
        this.#refreshing = null;
      });

    return this.#refreshing;
  }

  async restoreToken(): Promise<string | null> {
    const token = await this.tokens.read();
    this.#token = token;
    return token;
  }

  async clearToken(): Promise<void> {
    this.#token = null;
    await this.tokens.clear();
  }

  async #acceptToken(token: string): Promise<void> {
    this.#token = token;
    await this.tokens.write(token);
  }
}
