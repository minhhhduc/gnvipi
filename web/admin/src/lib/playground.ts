import { cookieValue } from "./api";

/** One model the caller can actually resolve through the gateway. */
export interface PlaygroundModel {
  provider: string;
  alias: string;
  upstream_model: string;
  source: "key" | "nvidia";
}

export interface PlaygroundModelsResponse {
  models: PlaygroundModel[];
  user_id: string;
  union: boolean;
}

export interface ChatMessage {
  role: "system" | "user" | "assistant";
  content: string;
}

export class PlaygroundError extends Error {
  status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "PlaygroundError";
    this.status = status;
  }
}

/** A user-initiated stop is not an error state in the console. */
export function isStoppedError(err: unknown): boolean {
  return err instanceof Error && err.name === "AbortError";
}

export function parseModels(value: unknown): PlaygroundModelsResponse {
  if (!value || typeof value !== "object") throw new Error("Invalid playground models response");
  const body = value as Record<string, unknown>;
  if (!Array.isArray(body.models)) throw new Error("Invalid playground models response");
  return {
    models: body.models.filter((model): model is PlaygroundModel => {
      if (!model || typeof model !== "object") return false;
      const entry = model as Record<string, unknown>;
      return typeof entry.provider === "string" && typeof entry.alias === "string" && typeof entry.upstream_model === "string";
    }),
    user_id: typeof body.user_id === "string" ? body.user_id : "",
    union: body.union === true,
  };
}

/** Group the flat model list into one optgroup per provider. */
export function groupModels(models: PlaygroundModel[]): { provider: string; models: PlaygroundModel[] }[] {
  const byProvider = new Map<string, PlaygroundModel[]>();
  for (const model of models) {
    const list = byProvider.get(model.provider);
    if (list) list.push(model);
    else byProvider.set(model.provider, [model]);
  }
  return [...byProvider.entries()]
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([provider, list]) => ({
      provider,
      models: [...list].sort((left, right) => left.alias.localeCompare(right.alias)),
    }));
}

export interface SSEBlockResult {
  text?: string;
  done?: boolean;
  error?: string;
}

/** Parse one SSE event, keeping only assistant text and typed failures. */
export function parseSSEBlock(block: string): SSEBlockResult {
  const out: SSEBlockResult = {};
  for (const raw of block.split("\n")) {
    const line = raw.trimEnd();
    if (!line.startsWith("data:")) continue;
    const data = line.slice(5).trim();
    if (!data) continue;
    if (data === "[DONE]") {
      out.done = true;
      continue;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(data);
    } catch {
      continue;
    }
    const entry = parsed as Record<string, unknown> | null;
    const failure = entry?.error;
    if (failure) {
      out.error =
        typeof failure === "string"
          ? failure
          : typeof (failure as Record<string, unknown>).message === "string"
            ? String((failure as Record<string, unknown>).message)
            : "Upstream request failed";
      continue;
    }
    // OpenAI chat chunks carry delta.content; a non-streaming body carries
    // message.content. Both are rendered as assistant text.
    const choices = entry?.choices as Array<Record<string, unknown>> | undefined;
    const first = choices?.[0];
    const delta = (first?.delta as Record<string, unknown> | undefined)?.content;
    const message = (first?.message as Record<string, unknown> | undefined)?.content;
    const text = typeof delta === "string" ? delta : typeof message === "string" ? message : "";
    if (text) out.text = (out.text ?? "") + text;
  }
  return out;
}

function abortError(): Error {
  const err = new Error("Stopped");
  err.name = "AbortError";
  return err;
}

function bodyMessage(body: unknown): string | undefined {
  if (!body || typeof body !== "object") return undefined;
  const entry = body as Record<string, unknown>;
  if (typeof entry.error === "string") return entry.error;
  const failure = entry.error as Record<string, unknown> | undefined;
  if (failure && typeof failure.message === "string") return failure.message;
  if (typeof entry.message === "string") return entry.message;
  return undefined;
}

/**
 * Send one playground completion and stream the reply to onDelta. The browser
 * never talks to the upstream directly: the console endpoint resolves the
 * caller and the gateway's provider chain does the work.
 */
export async function streamPlaygroundChat(options: {
  model: string;
  messages: ChatMessage[];
  userId?: string;
  signal?: AbortSignal;
  onDelta: (text: string) => void;
}): Promise<void> {
  const url = `/admin/api/playground/chat${options.userId ? `?user_id=${encodeURIComponent(options.userId)}` : ""}`;
  const headers = new Headers({ "Content-Type": "application/json" });
  const csrf = cookieValue("gw_csrf");
  if (csrf) headers.set("X-CSRF-Token", csrf);

  const response = await fetch(url, {
    method: "POST",
    headers,
    credentials: "same-origin",
    signal: options.signal,
    body: JSON.stringify({
      model: options.model,
      messages: options.messages,
      stream: true,
    }),
  });

  if (!response.ok) {
    const text = await response.text();
    let message = `Request failed (${response.status})`;
    try {
      message = bodyMessage(JSON.parse(text)) ?? message;
    } catch {
      // A non-JSON upstream error keeps the status line.
    }
    throw new PlaygroundError(message, response.status);
  }

  const reader = response.body?.getReader();
  if (!reader) throw new PlaygroundError("This browser cannot stream the reply", 0);
  const cancel = () => {
    if (options.signal?.aborted) void reader.cancel().catch(() => {});
  };
  options.signal?.addEventListener("abort", cancel, { once: true });
  // The abort may have landed between fetch resolving and this listener.
  cancel();
  const decoder = new TextDecoder();
  let buffer = "";
  let sawEvent = false;
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let boundary = buffer.indexOf("\n\n");
      while (boundary >= 0) {
        const block = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        const parsed = parseSSEBlock(block);
        sawEvent = true;
        if (parsed.error) throw new PlaygroundError(parsed.error, 502);
        if (parsed.text) options.onDelta(parsed.text);
        if (parsed.done) return;
        boundary = buffer.indexOf("\n\n");
      }
      if (options.signal?.aborted) break;
    }
    const trailing = parseSSEBlock(buffer);
    if (trailing.error) throw new PlaygroundError(trailing.error, 502);
    if (trailing.text) options.onDelta(trailing.text);
    // A provider that ignored stream:true answers with one JSON body. Render
    // it instead of leaving the console empty.
    if (!sawEvent && buffer.trim()) {
      const fallback = parseSSEBlock(`data: ${buffer.trim()}`);
      if (fallback.error) throw new PlaygroundError(fallback.error, 502);
      if (fallback.text) options.onDelta(fallback.text);
    }
  } finally {
    options.signal?.removeEventListener("abort", cancel);
  }
  if (options.signal?.aborted) throw abortError();
}
