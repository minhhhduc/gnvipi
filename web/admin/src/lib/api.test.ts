import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("api", () => {
  it("redirects unauthorized responses to the admin login", async () => {
    const assign = vi.fn();
    vi.stubGlobal("location", { assign });
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response("", { status: 401 })),
    );

    await expect(api("/admin/api/stats")).rejects.toMatchObject({
      message: "Unauthorized",
      status: 401,
    });
    expect(assign).toHaveBeenCalledOnce();
    expect(assign).toHaveBeenCalledWith("/admin/login");
  });

  it("surfaces a clear API error for malformed error JSON", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(new Response("not JSON", { status: 502 })),
    );

    await expect(api("/admin/api/stats")).rejects.toMatchObject({
      message: "Request failed (502)",
      status: 502,
    });
  });

  it("carries numeric Retry-After seconds on API errors", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ error: "Slow down" }), {
          status: 429,
          headers: {
            "Content-Type": "application/json",
            "Retry-After": "7",
          },
        }),
      ),
    );

    await expect(api("/admin/api/stats?frames=1")).rejects.toMatchObject({
      message: "Slow down",
      retryAfter: 7,
      status: 429,
    });
  });

  it("sends the double-submit CSRF cookie on cookie-authenticated writes", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("document", { cookie: "other=value; gw_csrf=csrf%2Dtoken" });
    vi.stubGlobal("fetch", fetchMock);

    await api("/admin/api/config/search", {
      method: "POST",
      body: { url: "https://example.test/?q={query}" },
    });

    const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
    expect(new Headers(init.headers).get("X-CSRF-Token")).toBe("csrf-token");
    expect(init.credentials).toBe("same-origin");
  });

  it("does not send a CSRF header on safe reads or bearer writes", async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(new Response("{}", { status: 200 })));
    vi.stubGlobal("document", { cookie: "gw_csrf=session-token" });
    vi.stubGlobal("fetch", fetchMock);

    await api("/admin/api/stats");
    await api("/admin/api/tokens", {
      method: "POST",
      headers: { Authorization: "Bearer adm_test" },
      body: {},
    });

    for (const [, init] of fetchMock.mock.calls) {
      expect(new Headers((init as RequestInit).headers).has("X-CSRF-Token")).toBe(false);
    }
  });

  it.each(["", "Basic dXNlcjpwYXNz", "Token opaque", "Bearer", "bearer   "])(
    "keeps CSRF protection for a non-bearer Authorization value %j",
    async (authorization) => {
      const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
      vi.stubGlobal("document", { cookie: "gw_csrf=session-token" });
      vi.stubGlobal("fetch", fetchMock);

      await api("/admin/api/config/search", {
        method: "POST",
        headers: { Authorization: authorization },
        body: {},
      });

      const init = fetchMock.mock.calls[0]?.[1] as RequestInit;
      expect(new Headers(init.headers).get("X-CSRF-Token")).toBe("session-token");
    },
  );

  it("lets the initial anonymous session probe show login without a redirect loop", async () => {
    const assign = vi.fn();
    vi.stubGlobal("location", { assign });
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 401 })));

    await expect(
      api("/admin/api/auth/session", { redirectOn401: false } as Parameters<typeof api>[1]),
    ).rejects.toMatchObject({ status: 401 });
    expect(assign).not.toHaveBeenCalled();
  });
});
