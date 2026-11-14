export type TestStatus = "success" | "auth_failure" | "connect_failure" | "error";

export interface TestResult {
  ok: boolean;
  status: TestStatus;
  status_code?: number;
  latency_ms?: number;
  message?: string;
  error?: string;
}

export interface ProviderModel {
  alias: string;
  upstream_model: string;
  hidden?: boolean;
  fallbacks?: string[];
  fallback_statuses?: number[];
  legacy_position?: number;
  allow_shadow?: boolean;
}

export interface ProviderTimeouts {
  connect?: number;
  tls?: number;
  response_header?: number;
  idle?: number;
  total?: number;
}

export interface ProviderAuth {
  scheme: string;
  key?: string;
  key_env?: string;
  key_ref?: string;
  header_name?: string;
}

export interface Provider {
  id: string;
  name: string;
  base_url: string;
  proxy_url?: string;
  protocol: string;
  auth?: ProviderAuth;
  headers?: Record<string, string>;
  models?: ProviderModel[];
  timeouts?: ProviderTimeouts;
  enabled?: boolean;
  tls?: string;
  forward_headers?: string[];
  endpoint_path?: Record<string, string>;
  extra_body?: Record<string, unknown>;
  extensions?: Record<string, unknown>;
  max_concurrency?: number;
  queue_timeout?: number;
}

export interface ModelInfo {
  id: string;
  name: string;
  publisher?: string;
  hidden?: boolean;
  custom?: boolean;
  base_url?: string;
}

export interface ModelsResponse {
  models?: ModelInfo[];
  off?: string[];
  deleted?: string[];
}

export interface MinuteStats {
  minute: string;
  requests: number;
  errors: number;
  input_tokens: number;
  output_tokens: number;
  ttfb_avg_ms: number;
}

export interface ModelStats {
  model: string;
  requests: number;
  errors: number;
  streamed: number;
  input_tokens: number;
  output_tokens: number;
  first_ok?: string;
  last_ok?: string;
  avg_ms: number;
  last_ms: number;
  ttfb_avg_ms: number;
  ttfb_last_ms: number;
  ttfb_min_ms: number;
  ttfb_max_ms: number;
}

export interface RequestEvent {
  time: string;
  model: string;
  input_tokens: number;
  output_tokens: number;
  ms: number;
  ttfb_ms: number;
  streamed: boolean;
  error?: string;
}

export interface Stats {
  models?: ModelStats[];
  series?: MinuteStats[];
  series_by_model?: Record<string, MinuteStats[]>;
  events?: RequestEvent[];
  started_at?: string;
  total_frames?: number;
}

export interface FrameResponse {
  frame: number;
  total_frames: number;
  events: RequestEvent[];
}

export interface CycleConfig {
  name?: string;
  models?: string[];
  available?: string[];
  current?: string;
}

export interface ChromeInfo {
  index: number;
  pid: number;
  busy: boolean;
  killed: boolean;
  warmed: boolean;
  last_ok?: string;
  extracts: number;
}

export interface ChromeStatus {
  enabled: boolean;
  state: string;
  chromes: ChromeInfo[];
  ready: number;
  error?: string;
}

export interface ApiErrorBody {
  error?: string;
  field?: string;
  errors?: string[];
  fields?: Record<string, string>;
  field_errors?: { field: string; message: string }[];
}

/** One account row on the owner-only Users screen. */
export interface AdminUserRecord {
  id: string;
  username: string;
  email: string | null;
  display_name: string | null;
  role: string;
  status: string;
  must_change_password: boolean;
  failed_attempts: number;
  locked_until: string | null;
  last_login_at: string | null;
  created_at: string | null;
  updated_at: string | null;
  created_by: string | null;
}

export interface UserList {
  users: AdminUserRecord[];
  total: number;
  limit: number;
  offset: number;
}

/** One gateway or admin token row. The plaintext and hash never appear here. */
export interface TokenRecord {
  id: string;
  user_id: string;
  kind: string;
  name: string;
  token_prefix: string;
  scope: string | null;
  expires_at: string | null;
  last_used_at: string | null;
  revoked_at: string | null;
  created_at: string | null;
}

export interface TokenList {
  tokens: TokenRecord[];
  limit: number;
  offset: number;
}
