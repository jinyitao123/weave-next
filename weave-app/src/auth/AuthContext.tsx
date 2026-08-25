import { useCallback, useEffect, useMemo, useState, type PropsWithChildren } from "react";
import { ApiError, api, type User } from "../api";
import { AuthContext, type AuthContextValue } from "./authContextValue";

export function AuthProvider({ children }: PropsWithChildren) {
  const [status, setStatus] = useState<AuthContextValue["status"]>("restoring");
  const [user, setUser] = useState<User | null>(null);

  const refreshUser = useCallback(async (signal?: AbortSignal) => {
    const refreshed = await api.getMe(signal);
    setUser(refreshed);
    return refreshed;
  }, []);

  const logout = useCallback(async () => {
    await api.clearToken();
    setUser(null);
    setStatus("anonymous");
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    void (async () => {
      const token = await api.restoreToken();
      if (!token) {
        setStatus("anonymous");
        return;
      }

      let retryDelay = 1_000;
      while (!controller.signal.aborted) {
        try {
          await refreshUser(controller.signal);
          setStatus("authenticated");
          return;
        } catch (error) {
          if (controller.signal.aborted) return;
          if (error instanceof ApiError && (error.status === 401 || error.status === 403 || error.status === 404)) {
            await logout();
            return;
          }

          setStatus("authenticated");
          await new Promise<void>((resolve) => {
            const finishWaiting = () => {
              window.clearTimeout(timeout);
              controller.signal.removeEventListener("abort", finishWaiting);
              resolve();
            };
            const timeout = window.setTimeout(finishWaiting, retryDelay);
            controller.signal.addEventListener("abort", finishWaiting, { once: true });
            if (controller.signal.aborted) finishWaiting();
          });
          retryDelay = Math.min(retryDelay * 2, 30_000);
        }
      }
    })();
    return () => controller.abort();
  }, [logout, refreshUser]);

  const value = useMemo<AuthContextValue>(() => ({
    status,
    user,
    async login(input) {
      const response = await api.login(input);
      setUser(response.user);
      setStatus("authenticated");
    },
    async devLogin(tenant) {
      await api.devLogin(tenant);
      const response = await refreshUser();
      setUser(response);
      setStatus("authenticated");
    },
    refreshUser,
    logout,
  }), [logout, refreshUser, status, user]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
