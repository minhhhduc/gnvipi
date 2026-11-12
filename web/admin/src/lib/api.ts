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

export async function api<T>(path: string, init: Omit<RequestInit, "body"> & { body?: unknown } = {}): Promise<T> {
  const headers = new Headers(init.headers);
  let body = init.body as BodyInit | undefined;
  if (init.body !== undefined && typeof init.body !== "string") {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(init.body);
  }
  const res = await fetch(path, {
    ...init,
    body,
    headers,
    credentials: "same-origin",
  });

  if (res.status === 401) {
    location.assign("/admin/login");
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
