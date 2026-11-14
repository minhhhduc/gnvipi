import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { api } from "../lib/api";
import type { AdminUser } from "../lib/auth";
import type { AdminUserRecord } from "../lib/types";
import { message } from "../lib/admin";
import { useGateway } from "../lib/context";
import { USER_LIST_MAX, parseUserList } from "../lib/users";
import {
  groupModels,
  isStoppedError,
  parseModels,
  streamPlaygroundChat,
  type ChatMessage,
  type PlaygroundModel,
} from "../lib/playground";
import { Card, CardHead, PageHeader } from "../components/ui";

/** One rendered turn. `model` and `provider` label assistant turns. */
interface Turn extends ChatMessage {
  provider?: string;
  upstream?: string;
}

export function PlaygroundView({ user }: { user: AdminUser }) {
  const owner = user.role === "owner";
  const { navigate } = useGateway();
  const [accounts, setAccounts] = useState<AdminUserRecord[]>([]);
  const [accountID, setAccountID] = useState("");
  const [models, setModels] = useState<PlaygroundModel[]>([]);
  const [union, setUnion] = useState(false);
  const [model, setModel] = useState("");
  const [filter, setFilter] = useState("");
  const [modelsLoading, setModelsLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [input, setInput] = useState("");
  const [turns, setTurns] = useState<Turn[]>([]);
  const [error, setError] = useState("");
  const [streaming, setStreaming] = useState(false);
  const abort = useRef<AbortController | null>(null);
  const transcript = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!owner) return;
    api<unknown>(`/admin/api/users?limit=${USER_LIST_MAX}`)
      .then((body) => setAccounts(parseUserList(body).users))
      // The playground still works for the caller's own account; the selector
      // just stays on "My account".
      .catch(() => setAccounts([]));
  }, [owner]);

  useEffect(() => {
    let cancelled = false;
    setModelsLoading(true);
    const query = owner && accountID ? `?user_id=${encodeURIComponent(accountID)}` : "";
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
  }, [owner, accountID]);

  // Switching the account being tested changes whose keys resolve, so the old
  // conversation no longer describes anything real.
  useEffect(() => {
    setTurns([]);
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

  const visible = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    if (!needle) return models;
    return models.filter((entry) =>
      [entry.alias, entry.upstream_model, entry.provider].some((value) => value.toLowerCase().includes(needle)),
    );
  }, [models, filter]);

  const groups = groupModels(visible);

  const send = async () => {
    const prompt = input.trim();
    if (!prompt || !model || streaming) return;
    const controller = new AbortController();
    abort.current = controller;
    const history: Turn[] = [...turns, { role: "user", content: prompt }, { role: "assistant", content: "" }];
    setTurns(history);
    setInput("");
    setStreaming(true);
    setError("");
    try {
      await streamPlaygroundChat({
        model,
        messages: history
          .filter((turn) => turn.role !== "assistant" || turn.content !== "")
          .map((turn) => ({ role: turn.role, content: turn.content })),
        userId: owner && accountID ? accountID : undefined,
        signal: controller.signal,
        onDelta: (text) =>
          setTurns((current) => {
            const next = [...current];
            const last = next[next.length - 1];
            next[next.length - 1] = { ...last, content: last.content + text };
            return next;
          }),
      });
    } catch (err) {
      if (!isStoppedError(err)) {
        setError(message(err));
        // Drop the empty assistant placeholder so the transcript stays honest.
        setTurns((current) => (current[current.length - 1]?.content === "" ? current.slice(0, -1) : current));
      }
    } finally {
      abort.current = null;
      setStreaming(false);
    }
  };

  const stop = () => abort.current?.abort();

  const composerDisabled = !model || streaming;

  return (
    <>
      <PageHeader title="Playground" sub="Send a test message through this gateway as the signed-in account">
        {owner && (
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
      </PageHeader>

      <div class="grid items-start gap-3 md:grid-cols-[280px_minmax(0,1fr)]">
        <Card class="flex max-h-[46vh] flex-col overflow-hidden md:max-h-[calc(100vh-190px)]">
          <CardHead title="Models" sub={models.length > 0 ? `${models.length} reachable` : undefined} />
          <div class="border-b border-line px-[15px] py-2.5">
            <input
              class="input"
              type="search"
              placeholder="Filter by model or provider"
              aria-label="Filter models"
              value={filter}
              onInput={(e) => setFilter((e.currentTarget as HTMLInputElement).value)}
            />
          </div>

          <div class="min-h-0 flex-1 overflow-auto p-1.5">
            {modelsLoading ? (
              <p class="px-2 py-3 text-xs text-dim">Loading models…</p>
            ) : loadError ? (
              <p class="px-2 py-3 text-xs text-bad">Models unavailable: {loadError}</p>
            ) : models.length === 0 ? (
              <div class="px-2 py-3 text-xs text-dim">
                <p>No model this account can reach.</p>
                <p class="mt-1.5">Add a provider key on Models, or rescan the NVIDIA catalog.</p>
                <button class="btn btn-sm mt-2.5" type="button" onClick={() => navigate("models")}>Open Models</button>
              </div>
            ) : visible.length === 0 ? (
              <p class="px-2 py-3 text-xs text-dim">Nothing matches “{filter.trim()}”.</p>
            ) : (
              groups.map((group) => (
                <div key={group.provider} class="mb-1.5 last:mb-0">
                  <div class="px-2 py-1 text-[11px] font-semibold text-dim">{group.provider}</div>
                  {group.models.map((entry) => {
                    const active = entry.alias === model;
                    return (
                      <button
                        key={`${entry.provider}:${entry.alias}:${entry.upstream_model}`}
                        type="button"
                        aria-pressed={active}
                        class={`flex w-full flex-col items-start gap-0.5 rounded-[6px] border-l-2 px-2 py-1.5 text-left transition-colors ${
                          active
                            ? "border-accent bg-panel2"
                            : "border-transparent hover:bg-panel2 focus-visible:bg-panel2"
                        }`}
                        onClick={() => setModel(entry.alias)}
                      >
                        <span class="mono w-full truncate text-[12px] text-fg" title={entry.alias}>{entry.alias}</span>
                        {entry.upstream_model !== entry.alias && (
                          <span class="mono w-full truncate text-[10px] text-dim" title={entry.upstream_model}>
                            {entry.upstream_model}
                          </span>
                        )}
                      </button>
                    );
                  })}
                </div>
              ))
            )}
          </div>
          {union && (
            <p class="border-t border-line px-3 py-2 text-[11px] text-dim">
              Union of every account’s models. Pick an account above to test as that account.
            </p>
          )}
        </Card>

        <Card class="flex min-h-[420px] flex-col md:min-h-[calc(100vh-190px)]">
          <CardHead title="Transcript" sub={selected ? selected.upstream_model : "No model selected"}>
            {selected && <span class="chip mono">{selected.provider}</span>}
            {turns.length > 0 && !streaming && (
              <button class="btn btn-sm" type="button" onClick={() => setTurns([])}>Clear</button>
            )}
          </CardHead>

          <div
            ref={transcript}
            class="min-h-0 flex-1 overflow-auto px-[15px] py-3"
            aria-live="polite"
            aria-label="Conversation transcript"
          >
            {turns.length === 0 ? (
              <div class="flex h-full min-h-[240px] flex-col items-start justify-center gap-1 text-dim">
                <span class="text-[15px] font-semibold text-fg">Nothing sent yet</span>
                <span class="max-w-[52ch] text-xs">
                  {model
                    ? `Messages resolve as ${
                        accountID ? accounts.find((entry) => entry.id === accountID)?.username ?? "the selected account" : user.username
                      } using ${model}.`
                    : "Pick a model on the left to start."}
                </span>
              </div>
            ) : (
              <ol class="mx-auto max-w-[78ch]">
                {turns.map((turn, index) => (
                  <li key={index} class="border-b border-line py-3 first:pt-0 last:border-b-0">
                    <div class="mb-1 flex items-baseline gap-2 text-[11px] text-dim">
                      <span class="font-semibold text-fg">{turn.role === "user" ? "You" : "Assistant"}</span>
                      {turn.role === "assistant" && selected && (
                        <span class="mono truncate">
                          {selected.provider} · {selected.upstream_model}
                        </span>
                      )}
                    </div>
                    <div class="whitespace-pre-wrap break-words text-[13px] leading-6">
                      {turn.content}
                      {streaming && index === turns.length - 1 && (
                        <span class="ml-0.5 inline-block h-4 w-[6px] translate-y-0.5 bg-accent motion-safe:animate-pulse" aria-hidden="true" />
                      )}
                    </div>
                  </li>
                ))}
              </ol>
            )}
          </div>

          <div class="border-t border-line px-[15px] py-3">
            {error && (
              <div role="alert" class="mb-2 rounded-[8px] border border-bad bg-bad-soft px-3 py-2 text-xs text-bad">
                {error}
              </div>
            )}
            <label class="sr-only" for="playground-message">Message</label>
            <textarea
              id="playground-message"
              class="input min-h-[76px] resize-y text-[13px]"
              rows={3}
              placeholder={model ? `Message ${model}` : "Select a model first"}
              value={input}
              disabled={composerDisabled}
              onInput={(e) => setInput((e.currentTarget as HTMLTextAreaElement).value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  void send();
                }
              }}
            />
            <div class="mt-2 flex flex-wrap items-center gap-2">
              <button type="button" class="btn btn-primary" disabled={composerDisabled || !input.trim()} onClick={() => void send()}>
                Send
              </button>
              <button type="button" class="btn" disabled={!streaming} onClick={stop}>Stop</button>
              <span class="text-[11px] text-dim">
                Enter to send · Shift+Enter for a new line
              </span>
              {streaming && <span class="ml-auto text-[11px] text-dim" role="status">Streaming…</span>}
            </div>
          </div>
        </Card>
      </div>
    </>
  );
}
