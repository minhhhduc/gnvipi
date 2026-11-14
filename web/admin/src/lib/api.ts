import type { ApiErrorBody } from "./types";

export class ApiError extends Error {
  status: number;
  body?: ApiErrorBody;
  retryAfter?: number;

  constructor(message: string, status: number, body?: ApiErrorBody, retryAfter?: number) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
    this.retryAfter = retryAfter;
  }
}

export function modelPath(id: string): string {
  return String(id)
    .split("/")
    .map(encodeURIComponent)
    .join("/");
}

export type ApiRequestInit = Omit<RequestInit, "body"> & {
  body?: unknown;
  redirectOn401?: boolean;
};

const SAFE_METHODS: Record<string, true> = { GET: true, HEAD: true, OPTIONS: true };

function cookieValue(name: string): string {
  if (typeof document === "undefined") return "";
  const prefix = `${name}=`;
  const pair = document.cookie.split(";").map((part) => part.trim()).find((part) => part.startsWith(prefix));
  if (!pair) return "";
  const value = pair.slice(prefix.length);
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}

export async function api<T>(path: string, init: ApiRequestInit = {}): Promise<T> {
  const { body: inputBody, redirectOn401 = true, ...requestInit } = init;
  const headers = new Headers(requestInit.headers);
  let body = inputBody as BodyInit | undefined;
  if (inputBody !== undefined && typeof inputBody !== "string") {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(inputBody);
  }
  const method = (requestInit.method || "GET").toUpperCase();
  const bearerRequest = /^Bearer\s+\S+$/i.test(headers.get("Authorization")?.trim() || "");
  if (!SAFE_METHODS[method] && !bearerRequest && !headers.has("X-CSRF-Token")) {
    const csrf = cookieValue("gw_csrf");
    if (csrf) headers.set("X-CSRF-Token", csrf);
  }
  const res = await fetch(path, {
    ...requestInit,
    body,
    headers,
    credentials: "same-origin",
  });

  if (res.status === 401) {
    if (redirectOn401) location.assign("/admin/login");
    throw new ApiError("Unauthorized", 401);
  }

  const text = await res.text();
  let parsed: unknown = undefined;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = undefined;
    }
  }

  if (!res.ok) {
    const errBody = (parsed ?? {}) as ApiErrorBody;
    const retryAfter = Number(res.headers.get("Retry-After")) || undefined;
    throw new ApiError(
      errBody.error || `Request failed (${res.status})`,
      res.status,
      errBody,
      retryAfter,
    );
  }

  if (parsed === undefined && text) return text as T;
  return parsed as T;
}
