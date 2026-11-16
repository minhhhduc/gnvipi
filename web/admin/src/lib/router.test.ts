import { describe, expect, it } from "vitest";
import { pageAllowed, pageFromLocation, pageFromPath, pageFromUrl, pathForPage, rawPageFromUrl } from "./router";
import type { Page } from "./context";

describe("pageFromUrl", () => {
  it.each([
    ["?page=stats", "stats"],
    ["?page=usage", "usage"],
    ["?page=playground", "playground"],
    ["?page=api", "api"],
    ["?page=models", "models"],
    ["?page=config", "config"],
    ["?page=chromes", "chromes"],
    ["?page=users", "users"],
    ["?page=tokens", "tokens"],
  ] as const)("reads the direct route from %s", (search, expected) => {
    expect(pageFromUrl(search)).toBe(expected);
  });

  it.each(["", "?page=unknown", "?other=models"])(
    "falls back to stats for %s",
    (search) => {
      expect(pageFromUrl(search)).toBe("stats");
    },
  );

  it("filters direct routes through the signed-in role", () => {
    const forRole = pageFromUrl as (
      search: string,
      role: "superuser" | "admin" | "user",
    ) => Page;

    expect(forRole("?page=config", "superuser")).toBe("config");
    expect(forRole("?page=chromes", "admin")).toBe("chromes");
    expect(forRole("?page=models", "user")).toBe("models");
    expect(forRole("?page=config", "user")).toBe("stats");
    expect(forRole("?page=chromes", "user")).toBe("stats");
  });

  it("opens the usage log to every role", () => {
    const forRole = pageFromUrl as (
      search: string,
      role: "superuser" | "admin" | "user",
    ) => Page;

    expect(forRole("?page=usage", "superuser")).toBe("usage");
    expect(forRole("?page=usage", "admin")).toBe("usage");
    expect(forRole("?page=usage", "user")).toBe("usage");
  });

  it("keeps the users screen superuser-only and the tokens screen open to every role", () => {
    const forRole = pageFromUrl as (
      search: string,
      role: "superuser" | "admin" | "user",
    ) => Page;

    expect(forRole("?page=users", "superuser")).toBe("users");
    expect(forRole("?page=users", "admin")).toBe("stats");
    expect(forRole("?page=users", "user")).toBe("stats");
    expect(forRole("?page=tokens", "superuser")).toBe("tokens");
    expect(forRole("?page=tokens", "admin")).toBe("tokens");
    expect(forRole("?page=tokens", "user")).toBe("tokens");
  });
});

describe("clean page paths", () => {
  it.each([
    ["/", "stats"],
    ["/playground", "playground"],
    ["/api", "api"],
    ["/models", "models"],
    ["/config", "config"],
    ["/chromes", "chromes"],
    ["/tokens", "tokens"],
    ["/users", "users"],
    ["/usage", "usage"],
    ["/audit", "audit"],
    ["/playground/", "playground"],
    ["/unknown", "stats"],
  ] as const)("maps %s to %s", (pathname, expected) => {
    expect(pageFromPath(pathname)).toBe(expected);
    expect(pageFromPath(expected === "stats" ? "/" : `/${expected}`)).toBe(expected);
  });

  it("builds the clean URL for every page", () => {
    expect(pathForPage("stats")).toBe("/");
    expect(pathForPage("models")).toBe("/models");
    expect(pathForPage("audit")).toBe("/audit");
  });

  it("filters a pathname through the role and prefers a legacy ?page=", () => {
    expect(pageFromPath("/users", "admin")).toBe("stats");
    expect(pageFromLocation({ pathname: "/models", search: "" }, "user")).toBe("models");
    expect(pageFromLocation({ pathname: "/models", search: "?page=usage" }, "user")).toBe("usage");
  });
});

describe("rawPageFromUrl", () => {
  it("preserves an unknown page for the fallback notice", () => {
    expect(rawPageFromUrl("?page=unknown")).toBe("unknown");
    expect(rawPageFromUrl("?page=")).toBe("");
    expect(rawPageFromUrl("?other=models")).toBe("");
  });
});

describe("pageAllowed", () => {
  it("gates the superuser-only users screen and the shared tokens screen", () => {
    expect(pageAllowed("users", "superuser")).toBe(true);
    expect(pageAllowed("users", "root")).toBe(true);
    expect(pageAllowed("users", "admin")).toBe(false);
    expect(pageAllowed("users", "user")).toBe(false);
    expect(pageAllowed("tokens", "superuser")).toBe(true);
    expect(pageAllowed("tokens", "admin")).toBe(true);
    expect(pageAllowed("tokens", "user")).toBe(true);
  });

  it("keeps the usage log and dashboard straight", () => {
    expect(pageAllowed("usage", "superuser")).toBe(true);
    expect(pageAllowed("usage", "admin")).toBe(true);
    expect(pageAllowed("usage", "user")).toBe(true);
    expect(pageAllowed("stats", "user")).toBe(true);
  });

  it("opens the playground to every role", () => {
    expect(pageAllowed("playground", "superuser")).toBe(true);
    expect(pageAllowed("playground", "admin")).toBe(true);
    expect(pageAllowed("playground", "user")).toBe(true);
  });

  it("opens the API guide to every role", () => {
    expect(pageAllowed("api", "superuser")).toBe(true);
    expect(pageAllowed("api", "admin")).toBe(true);
    expect(pageAllowed("api", "user")).toBe(true);
  });
});
