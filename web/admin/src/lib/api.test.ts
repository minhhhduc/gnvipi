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
});
