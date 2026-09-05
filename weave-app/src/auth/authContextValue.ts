import { createContext } from "react";
import type { LoginRequest, User } from "../api";

export interface AuthContextValue {
  status: "restoring" | "authenticated" | "anonymous";
  user: User | null;
  login(input: LoginRequest): Promise<void>;
  refreshUser(signal?: AbortSignal): Promise<User>;
  logout(): Promise<void>;
}

export const AuthContext = createContext<AuthContextValue | null>(null);
