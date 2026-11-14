import { afterEach, describe, expect, it, vi } from "vitest";
import {
  groupModels,
  isStoppedError,
  parseSSEBlock,
  PlaygroundError,
  streamPlaygroundChat,
  type PlaygroundModel,
} from "./playground";

afterEach(() => {
  vi.unstubAllGlobals();
});

function sseResponse(chunks: string[]): Response {
  const encoder = new TextEncoder();
  let index = 0;
  const body = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (index >= chunks.length) {
        controller.close();
        return;
      }
      controller.enqueue(encoder.encode(chunks[index++]));
    },
  });
  return new Response(body, {
    status: 200,
    headers: { "Content-Type": "text/event-stream" },
  });
}

describe("parseSSEBlock", () => {
  it("extracts streamed chat delta content", () => {
    expect(parseSSEBlock('data: {"choices":[{"delta":{"content":"hel"}}]}')).toEqual({ text: "hel" });
  });

  it("ignores non-data lines and empty deltas", () => {
    expect(parseSSEBlock(": keep-alive\ndata: {\"choices\":[{\"delta\":{}}]}")).toEqual({});
  });

  it("recognises the terminal marker", () => {
    expect(parseSSEBlock("data: [DONE]")).toEqual({ done: true });
  });

  it("surfaces an upstream error event", () => {
    expect(parseSSEBlock('data: {"error":{"message":"upstream exploded"}}')).toEqual({
      error: "upstream exploded",
    });
  });
});

describe("groupModels", () => {
  it("groups by provider and sorts aliases", () => {
    const models: PlaygroundModel[] = [
      { provider: "zeta", alias: "b", upstream_model: "b-up", source: "key" },
      { provider: "alpha", alias: "a", upstream_model: "a-up", source: "key" },
      { provider: "zeta", alias: "a", upstream_model: "a-up2", source: "key" },
    ];
    expect(groupModels(models).map((group) => [group.provider, group.models.map((m) => m.alias)])).toEqual([
      ["alpha", ["a"]],
      ["zeta", ["a", "b"]],
    ]);
  });
});

describe("streamPlaygroundChat", () => {
  it("posts with CSRF and streams deltas to the caller", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      sseResponse([
        'data: {"choices":[{"delta":{"content":"hel"}}]}\n\n',
        'data: {"choices":[{"delta":{"content":"lo"}}]}\n\n',
        "data: [DONE]\n\n",
      ]),
    );
    vi.stubGlobal("document", { cookie: "gw_csrf=csrf%2Dtoken" });
    vi.stubGlobal("fetch", fetchMock);
    const deltas: string[] = [];

    await streamPlaygroundChat({
      model: "shared-alias",
      messages: [{ role: "user", content: "hi" }],
      onDelta: (text) => deltas.push(text),
    });

    expect(deltas.join("")).toBe("hello");
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/admin/api/playground/chat");
    expect(new Headers(init.headers).get("X-CSRF-Token")).toBe("csrf-token");
    expect(init.credentials).toBe("same-origin");
    expect(JSON.parse(String(init.body))).toMatchObject({
      model: "shared-alias",
      stream: true,
      messages: [{ role: "user", content: "hi" }],
    });
  });

  it("adds the owner account selector to the query", async () => {
    const fetchMock = vi.fn().mockResolvedValue(sseResponse(["data: [DONE]\n\n"]));
    vi.stubGlobal("document", { cookie: "" });
    vi.stubGlobal("fetch", fetchMock);

    await streamPlaygroundChat({
      model: "m",
      messages: [],
      userId: "user-42",
      onDelta: () => {},
    });

    expect(fetchMock.mock.calls[0][0]).toBe("/admin/api/playground/chat?user_id=user-42");
  });

  it("forwards sampling parameters only when they are set", async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(sseResponse(["data: [DONE]\n\n"])));
    vi.stubGlobal("document", { cookie: "" });
    vi.stubGlobal("fetch", fetchMock);

    await streamPlaygroundChat({ model: "m", messages: [], temperature: 0.3, maxTokens: 128, onDelta: () => {} });
    expect(JSON.parse(String(fetchMock.mock.calls[0][1].body))).toMatchObject({ temperature: 0.3, max_tokens: 128 });

    await streamPlaygroundChat({ model: "m", messages: [], onDelta: () => {} });
    expect(JSON.parse(String(fetchMock.mock.calls[1][1].body))).not.toHaveProperty("temperature");
  });

  it("maps an OpenAI-style error body to a typed console error", async () => {
    vi.stubGlobal("document", { cookie: "" });
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(() =>
        Promise.resolve(
          new Response(JSON.stringify({ error: { message: 'provider "p1" requires a key for alias "m"' } }), {
            status: 403,
            headers: { "Content-Type": "application/json" },
          }),
        ),
      ),
    );

    await expect(
      streamPlaygroundChat({ model: "m", messages: [], onDelta: () => {} }),
    ).rejects.toMatchObject({ status: 403, message: 'provider "p1" requires a key for alias "m"' });
    await expect(
      streamPlaygroundChat({ model: "m", messages: [], onDelta: () => {} }),
    ).rejects.toBeInstanceOf(PlaygroundError);
  });

  it("surfaces an in-stream error and stops reading", async () => {
    vi.stubGlobal("document", { cookie: "" });
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        sseResponse([
          'data: {"choices":[{"delta":{"content":"par"}}]}\n\n',
          'data: {"error":{"message":"upstream failed"}}\n\n',
        ]),
      ),
    );
    const deltas: string[] = [];

    await expect(
      streamPlaygroundChat({ model: "m", messages: [], onDelta: (text) => deltas.push(text) }),
    ).rejects.toMatchObject({ message: "upstream failed" });
    expect(deltas).toEqual(["par"]);
  });

  it("stops when the caller aborts", async () => {
    const controller = new AbortController();
    const body = new ReadableStream<Uint8Array>({
      pull() {
        // Hang until the abort tears the stream down.
      },
    });
    vi.stubGlobal("document", { cookie: "" });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(body, { status: 200 })));

    const pending = streamPlaygroundChat({
      model: "m",
      messages: [],
      signal: controller.signal,
      onDelta: () => {
        throw new Error("no delta expected before the abort");
      },
    });
    controller.abort();

    const err = await pending.catch((reason) => reason);
    expect(isStoppedError(err)).toBe(true);
  });
});
