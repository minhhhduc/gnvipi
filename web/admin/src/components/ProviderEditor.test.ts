import { describe, expect, it } from "vitest";
import { formatProviderTestResult, missingCredentials } from "./ProviderEditor";

describe("provider test result diagnostics", () => {
  it("keeps the full 401 outcome visible", () => {
    const summary = formatProviderTestResult({
      ok: false,
      status: "auth_failure",
      status_code: 401,
      latency_ms: 27,
      error: "upstream key was rejected",
    });

    expect(summary).toContain("27ms");
    expect(summary).toContain("auth_failure");
    expect(summary).toContain("HTTP 401");
    expect(summary).toContain("upstream key was rejected");
  });
});

describe("provider credentials", () => {
  it("requires at least one of auth key or api key", () => {
    expect(missingCredentials("", "", false, false)).toBe(true);
    expect(missingCredentials("sk-auth", "", false, false)).toBe(false);
    expect(missingCredentials("", "sk-api", false, false)).toBe(false);
    expect(missingCredentials("", "", true, false)).toBe(false);
  });
});
