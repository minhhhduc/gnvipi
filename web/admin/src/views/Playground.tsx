import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { api } from "../lib/api";
import { isSuperuserLevelRole, type AdminUser } from "../lib/auth";
import type { AdminUserRecord } from "../lib/types";
import { message } from "../lib/admin";
import { useGateway } from "../lib/context";
import { USER_LIST_MAX, parseUserList } from "../lib/users";
import {
  groupModels,
  isStoppedError,
  parseModels,
  streamPlaygroundChat,
  userMessage,
  type PlaygroundModel,
} from "../lib/playground";
import { Card, CardHead, PageHeader } from "../components/ui";

interface Turn {
  role: "user" | "assistant";
  content: string;
  /** Data URLs or public URLs attached to a user turn. */
  images?: string[];
  /** Provider and upstream model recorded when the turn was sent. */
  provider?: string;
  upstream?: string;
  /** Milliseconds to the first streamed token, when measured. */
  firstTokenMs?: number;
  /** Milliseconds for the whole reply, when measured. */
  totalMs?: number;
}

const MAX_IMAGE_BYTES = 700 * 1024;

const DEFAULT_TEMPERATURE = 1;

export function PlaygroundView({ user }: { user: AdminUser }) {
  const superuser = isSuperuserLevelRole(user.role);
  const { navigate } = useGateway();
  const [accounts, setAccounts] = useState<AdminUserRecord[]>([]);
  const [accountID, setAccountID] = useState("");
  const [models, setModels] = useState<PlaygroundModel[]>([]);
  const [union, setUnion] = useState(false);
  const [model, setModel] = useState("");
  const [modelsLoading, setModelsLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [system, setSystem] = useState("");
  const [temperature, setTemperature] = useState(DEFAULT_TEMPERATURE);
  const [maxTokens, setMaxTokens] = useState("");
  const [input, setInput] = useState("");
  const [images, setImages] = useState<string[]>([]);
  // Each model keeps its own conversation; switching models must not relabel
  // or mix replies from another model.
  const [conversations, setConversations] = useState<Record<string, Turn[]>>({});
  const turns = conversations[model] ?? [];
  const setTurns = (update: Turn[] | ((current: Turn[]) => Turn[])) =>
    setConversations((prev) => {
      const current = prev[model] ?? [];
      const next = typeof update === "function" ? (update as (current: Turn[]) => Turn[])(current) : update;
      return { ...prev, [model]: next };
    });
  const [error, setError] = useState("");
  const [streaming, setStreaming] = useState(false);
  const [copied, setCopied] = useState(false);
  const abort = useRef<AbortController | null>(null);
  const transcript = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!superuser) return;
    api<unknown>(`/admin/api/users?limit=${USER_LIST_MAX}`)
      .then((body) => setAccounts(parseUserList(body).users))
      // The playground still works for the caller's own account; the selector
      // just stays on "My account".
      .catch(() => setAccounts([]));
  }, [superuser]);

  useEffect(() => {
    let cancelled = false;
    setModelsLoading(true);
    const query = superuser && accountID ? `?user_id=${encodeURIComponent(accountID)}` : "";
    api<unknown>(`/admin/api/playground/models${query}`)
      .then((body) => {
        if (cancelled) return;
        const parsed = parseModels(body);
        setModels(parsed.models);
        setUnion(parsed.union);
        setLoadError("");
      })
      .catch((err) => {
        if (cancelled) return;
        setModels([]);
        setLoadError(message(err));
      })
      .finally(() => {
        if (!cancelled) setModelsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [superuser, accountID]);

  // Switching the account being tested changes whose keys resolve, so every
  // saved conversation no longer describes anything real.
  useEffect(() => {
    setConversations({});
    setError("");
  }, [accountID]);

  useEffect(() => {
    setModel((current) => (models.some((entry) => entry.alias === current) ? current : models[0]?.alias ?? ""));
  }, [models]);

  // A sign-out or account switch must not leave an old stream rendering.
  useEffect(() => () => abort.current?.abort(), []);

  useEffect(() => {
    const node = transcript.current;
    if (node) node.scrollTop = node.scrollHeight;
  }, [turns]);

  const selected = models.find((entry) => entry.alias === model);
  const groups = useMemo(() => groupModels(models), [models]);
  const asUser = accountID ? accounts.find((entry) => entry.id === accountID)?.username ?? "the selected account" : user.username;
  const maxTokensValue = Number.parseInt(maxTokens, 10);

  /** Streams a reply for an existing turn list; the last turn is the empty assistant. */
  const run = async (history: Turn[]) => {
    if (!model || streaming) return;
    const controller = new AbortController();
    abort.current = controller;
    // Show the turn (and the empty assistant placeholder) before any network
    // wait, so a failed request still leaves the sent message visible.
    setTurns(history);
    setStreaming(true);
    setError("");
    const started = performance.now();
    let firstToken: number | null = null;
    try {
      await streamPlaygroundChat({
        model,
        messages: [
          ...(system.trim() ? [{ role: "system" as const, content: system.trim() }] : []),
          ...history
            .filter((turn) => turn.role !== "assistant" || turn.content !== "")
            .map((turn) =>
              turn.role === "user"
                ? userMessage(turn.content, turn.images)
                : { role: "assistant" as const, content: turn.content },
            ),
        ],
        userId: superuser && accountID ? accountID : undefined,
        temperature,
        maxTokens: Number.isFinite(maxTokensValue) && maxTokensValue > 0 ? maxTokensValue : undefined,
        signal: controller.signal,
        onDelta: (text) => {
          if (firstToken === null) firstToken = performance.now() - started;
          setTurns((current) => {
            const next = [...current];
            const last = next[next.length - 1];
            if (!last) return next;
            next[next.length - 1] = { ...last, content: last.content + text };
            return next;
          });
        },
      });
      setTurns((current) => {
        const next = [...current];
        const last = next[next.length - 1];
        if (last) {
          next[next.length - 1] = {
            ...last,
            firstTokenMs: firstToken ?? undefined,
            totalMs: performance.now() - started,
          };
        }
        return next;
      });
    } catch (err) {
      if (!isStoppedError(err)) {
        setError(message(err));
        // Drop an empty assistant placeholder so the transcript stays honest.
        setTurns((current) => (current[current.length - 1]?.content === "" ? current.slice(0, -1) : current));
      }
    } finally {
      abort.current = null;
      setStreaming(false);
    }
  };

  const send = () => {
    const prompt = input.trim();
    if ((!prompt && images.length === 0) || !model || streaming) return;
    const attached = images;
    setInput("");
    setImages([]);
    void run([
      ...turns,
      { role: "user", content: prompt, images: attached },
      { role: "assistant", content: "", provider: selected?.provider, upstream: selected?.upstream_model },
    ]);
  };

  const addImageFile = (file: File) => {
    if (file.size > MAX_IMAGE_BYTES) {
      setError(`Images must be ${Math.round(MAX_IMAGE_BYTES / 1024)} KB or smaller (the admin request limit).`);
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      if (typeof reader.result === "string") setImages((current) => [...current, reader.result as string]);
    };
    reader.readAsDataURL(file);
  };

  const regenerate = () => {
    if (streaming) return;
    const lastUser = turns.map((turn) => turn.role).lastIndexOf("user");
    if (lastUser < 0) return;
    const history = turns.slice(0, lastUser + 1);
    void run([...history, { role: "assistant", content: "", provider: selected?.provider, upstream: selected?.upstream_model }]);
  };

  const copyReply = async () => {
    const reply = [...turns].reverse().find((turn) => turn.role === "assistant" && turn.content);
    if (!reply) return;
    try {
      await navigator.clipboard.writeText(reply.content);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      setError("Could not copy the reply");
    }
  };

  const stop = () => abort.current?.abort();
  const hasReply = turns.some((turn) => turn.role === "assistant" && turn.content);

  return (
    <>
      <PageHeader title="Playground" sub="Send a test message through this gateway as the signed-in account">
        {superuser && (
          <label class="flex items-center gap-2 text-xs text-dim">
            Test as
            <select class="input" value={accountID} onChange={(e) => setAccountID((e.target as HTMLSelectElement).value)}>
              <option value="">My account ({user.username})</option>
              {accounts
                .filter((account) => account.id !== user.id)
                .map((account) => (
                  <option key={account.id} value={account.id}>
                    {account.display_name || account.username} ({account.role})
                  </option>
                ))}
            </select>
          </label>
        )}
        <a
          href="/api"
          class="text-xs font-medium text-dim hover:text-fg"
          onClick={(e) => {
            e.preventDefault();
            navigate("api");
          }}
        >
          API usage
        </a>
      </PageHeader>

      <div class="grid items-start gap-3 lg:grid-cols-[300px_minmax(0,1fr)]">
        <Card class="lg:sticky lg:top-3">
          <CardHead title="Settings" />
          <div class="space-y-3 px-[15px] py-3">
            {loadError ? (
              <p class="text-xs text-bad">Models unavailable: {loadError}</p>
            ) : modelsLoading ? (
              <p class="text-xs text-dim">Loading models…</p>
            ) : models.length === 0 ? (
              <div class="text-xs text-dim">
                <p>No model this account can reach.</p>
                <p class="mt-1.5">Add a provider key, or rescan the NVIDIA catalog.</p>
                <button class="btn btn-sm mt-2.5" type="button" onClick={() => navigate("models")}>Open Models</button>
              </div>
            ) : (
              <>
                <label class="block text-xs font-semibold">
                  Model
                  <select class="input mt-1" value={model} onChange={(e) => setModel((e.target as HTMLSelectElement).value)}>
                    {groups.map((group) => (
                      <optgroup key={group.provider} label={group.provider}>
                        {group.models.map((entry) => (
                          <option key={`${entry.provider}:${entry.alias}:${entry.upstream_model}`} value={entry.alias}>
                            {entry.alias}
                          </option>
                        ))}
                      </optgroup>
                    ))}
                  </select>
                </label>
                {selected && (
                  <p class="mono break-all text-[11px] text-dim">
                    {selected.upstream_model ? `${selected.provider} · ${selected.upstream_model}` : `${selected.provider} · shared mapping`}
                  </p>
                )}
                {union && (
                  <p class="text-[11px] text-dim">Union of every account’s models; pick an account above to test as that account.</p>
                )}

                <label class="block text-xs font-semibold">
                  System prompt
                  <textarea
                    class="input mt-1 min-h-[64px] resize-y text-[12px]"
                    rows={3}
                    placeholder="Optional instructions sent before the conversation"
                    value={system}
                    onInput={(e) => setSystem((e.currentTarget as HTMLTextAreaElement).value)}
                  />
                </label>

                <label class="block text-xs font-semibold">
                  <span class="flex items-center justify-between">
                    Temperature
                    <span class="mono text-[11px] font-normal text-dim">{temperature.toFixed(1)}</span>
                  </span>
                  <input
                    class="mt-2 w-full accent-[var(--accent)]"
                    type="range"
                    min="0"
                    max="2"
                    step="0.1"
                    value={temperature}
                    onInput={(e) => setTemperature(Number.parseFloat((e.currentTarget as HTMLInputElement).value))}
                  />
                </label>

                <label class="block text-xs font-semibold">
                  Max tokens
                  <input
                    class="input mt-1"
                    type="number"
                    min="1"
                    inputMode="numeric"
                    placeholder="Provider default"
                    value={maxTokens}
                    onInput={(e) => setMaxTokens((e.currentTarget as HTMLInputElement).value)}
                  />
                </label>
              </>
            )}
          </div>
        </Card>

        <Card class="flex min-h-[520px] flex-col">
          <CardHead title="Conversation" sub={`as ${asUser}`}>
            {hasReply && !streaming && (
              <>
                <button class="btn btn-sm" type="button" onClick={() => void copyReply()}>
                  {copied ? "Copied" : "Copy reply"}
                </button>
                <button class="btn btn-sm" type="button" onClick={regenerate}>Regenerate</button>
              </>
            )}
            {turns.length > 0 && !streaming && (
              <button class="btn btn-sm" type="button" onClick={() => setTurns([])}>Clear</button>
            )}
          </CardHead>

          <div
            ref={transcript}
            class="min-h-0 flex-1 overflow-auto px-[15px] py-4"
            aria-live="polite"
            aria-label="Conversation transcript"
          >
            {turns.length === 0 ? (
              <div class="flex h-full min-h-[260px] flex-col items-center justify-center gap-1 text-center text-dim">
                <span class="text-[15px] font-semibold text-fg">Send a message to start</span>
                <span class="max-w-[46ch] text-xs">
                  {model ? `Replies come from ${model}, resolved as ${asUser}.` : "Choose a model on the left first."}
                </span>
              </div>
            ) : (
              <ol class="mx-auto flex max-w-[760px] flex-col gap-3">
                {turns.map((turn, index) =>
                  turn.role === "user" ? (
                    <li key={index} class="flex justify-end">
                      <div class="max-w-[85%] rounded-[10px] bg-panel2 px-3 py-2">
                        {turn.images && turn.images.length > 0 && (
                          <div class="mb-1.5 flex flex-wrap gap-1.5">
                            {turn.images.map((src, imageIndex) => (
                              <img
                                key={imageIndex}
                                src={src}
                                alt={`Attachment ${imageIndex + 1}`}
                                class="h-24 w-24 rounded-[6px] border border-line object-cover"
                              />
                            ))}
                          </div>
                        )}
                        {turn.content && (
                          <div class="whitespace-pre-wrap break-words text-[13px] leading-6">{turn.content}</div>
                        )}
                      </div>
                    </li>
                  ) : (
                    <li key={index} class="max-w-[92%]">
                      <div class="mb-1 mono text-[10px] text-dim">{turn.upstream || selected?.upstream_model || "assistant"}</div>
                      <div class="whitespace-pre-wrap break-words text-[13px] leading-6">
                        {turn.content}
                        {streaming && index === turns.length - 1 && (
                          <span
                            class="ml-0.5 inline-block h-4 w-[6px] translate-y-0.5 bg-accent motion-safe:animate-pulse"
                            aria-hidden="true"
                          />
                        )}
                      </div>
                      {turn.totalMs !== undefined && !streaming && (
                        <div class="mt-1 text-[10px] text-dim">
                          {turn.firstTokenMs !== undefined ? `first token ${Math.round(turn.firstTokenMs)} ms · ` : ""}
                          {Math.round(turn.totalMs)} ms
                        </div>
                      )}
                    </li>
                  ),
                )}
              </ol>
            )}
          </div>

          <div class="border-t border-line px-[15px] py-3">
            {error && (
              <div role="alert" class="mb-2 rounded-[8px] border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">
                {error}
              </div>
            )}
            {images.length > 0 && (
              <div class="mb-2 flex flex-wrap gap-2">
                {images.map((src, imageIndex) => (
                  <div key={imageIndex} class="relative">
                    <img src={src} alt={`Pending attachment ${imageIndex + 1}`} class="h-16 w-16 rounded-[6px] border border-line object-cover" />
                    <button
                      type="button"
                      class="absolute -right-1.5 -top-1.5 grid size-5 place-items-center rounded-full border border-line bg-panel text-[11px] text-dim"
                      aria-label={`Remove attachment ${imageIndex + 1}`}
                      onClick={() => setImages((current) => current.filter((_, index) => index !== imageIndex))}
                    >
                      ×
                    </button>
                  </div>
                ))}
              </div>
            )}
            <label class="sr-only" for="playground-message">Message</label>
            <textarea
              id="playground-message"
              class="input min-h-[72px] resize-y text-[13px]"
              rows={3}
              placeholder={model ? `Message ${model}` : "Select a model first"}
              value={input}
              disabled={!model || streaming}
              onInput={(e) => setInput((e.currentTarget as HTMLTextAreaElement).value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  send();
                }
              }}
            />
            <div class="mt-2 flex flex-wrap items-center gap-2">
              <button
                type="button"
                class="btn btn-primary"
                disabled={!model || streaming || (!input.trim() && images.length === 0)}
                onClick={send}
              >
                Send
              </button>
              <button type="button" class="btn" disabled={!streaming} onClick={stop}>Stop</button>
              <label class="btn btn-sm cursor-pointer">
                Attach image
                <input
                  type="file"
                  accept="image/*"
                  multiple
                  class="hidden"
                  disabled={!model || streaming}
                  onChange={(e) => {
                    const files = (e.currentTarget as HTMLInputElement).files;
                    if (files) for (const file of Array.from(files)) addImageFile(file);
                    (e.currentTarget as HTMLInputElement).value = "";
                  }}
                />
              </label>
              <span class="text-[11px] text-dim">Enter to send · Shift+Enter for a new line</span>
              {streaming && <span class="ml-auto text-[11px] text-dim" role="status">Streaming…</span>}
            </div>
          </div>
        </Card>
      </div>
    </>
  );
}
