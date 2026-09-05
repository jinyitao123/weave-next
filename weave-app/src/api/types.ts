export interface User {
  id: string;
  tenant_id: string;
  username?: string;
  display_name?: string;
  role: string;
  disabled?: boolean;
  source?: "apikey";
  created_at?: string;
  updated_at?: string;
}

export interface LoginRequest {
  username: string;
  password: string;
  tenant: string;
}

export interface LoginResponse {
  token: string;
  user: User;
}

export interface RefreshResponse {
  token: string;
}

export interface RuntimeCreateResponse {
  id: string;
  name: string;
  token: string;
}

export interface Runtime {
  id: string;
  name: string;
  engines: string[];
  engine_capabilities: Record<string, {
    engine: string;
    binary_path: string;
    binary_version: string;
    auth_mode: "chatgpt" | "oauth" | "provider" | "unknown" | string;
    protocol_version: string;
    endpoint_class: string;
  }>;
  functional_revision: number;
  health_status: "healthy" | "busy" | "offline" | string;
  total_slots: number;
  active_slots: number;
  pool_id?: string;
  consecutive_infra_failures: number;
  quarantine_until?: string | null;
  last_failure_reason?: string;
  enabled: boolean;
  revoked_at: string | null;
  deleted_at: string | null;
  online: boolean;
  last_heartbeat_at: string | null;
  created_at: string;
}
