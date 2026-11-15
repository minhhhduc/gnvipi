import { toChildArray, type ComponentChildren, type VNode } from "preact";
import { describe, expect, it, vi } from "vitest";
import { Shell } from "./Shell";

vi.mock("../lib/context", () => ({
  totals: () => ({ requests: 0, errors: 0 }),
  useGateway: () => ({ navigate: vi.fn() }),
}));

type TestRole = "owner" | "admin" | "user";

function textContent(value: ComponentChildren): string {
  return toChildArray(value)
    .map((child) => {
      if (typeof child === "string" || typeof child === "number") return String(child);
      if (!child || typeof child === "boolean") return "";
      return textContent((child as VNode<{ children?: ComponentChildren }>).props.children);
    })
    .join(" ");
}

function renderShell(role: TestRole): string {
  const user = {
    id: `${role}-id`,
    username: `${role}-name`,
    display_name: "",
    email: "",
    role,
    status: "active" as const,
    must_change_password: false,
  };
  const props = {
    page: "stats" as const,
    children: null,
    user,
    onLogout: vi.fn(),
  } as Parameters<typeof Shell>[0] & { user: typeof user; onLogout: () => void };
  return textContent(Shell(props));
}

describe("Shell role navigation", () => {
  it.each(["owner", "admin"] as const)("shows operator screens to %s", (role) => {
    const text = renderShell(role);
    expect(text).toContain("Dashboard");
    expect(text).toContain("Usage log");
    expect(text).toContain("Models");
    expect(text).toContain("Config");
    expect(text).toContain("Chrome");
  });

  it("shows Models to users and hides the other operator-only screens", () => {
    const text = renderShell("user");
    expect(text).toContain("Dashboard");
    expect(text).toContain("Models");
    expect(text).toContain("Usage log");
    expect(text).not.toContain("Config");
    expect(text).not.toContain("Chrome");
  });

  it("shows the owner-only Audit screen to owners only", () => {
    expect(renderShell("owner")).toContain("Audit");
    expect(renderShell("admin")).not.toContain("Audit");
    expect(renderShell("user")).not.toContain("Audit");
  });

  it("shows the signed-in identity and logout control", () => {
    const text = renderShell("admin");
    expect(text).toContain("admin-name");
    expect(text).toContain("admin");
    expect(text).toContain("Sign out");
  });
});

it("keeps the logs together at the end of the nav", () => {
  const nav = renderShell("owner");
  const order = ["Dashboard", "Playground", "API", "Models", "Config", "Chrome", "API Keys", "Users", "Usage log", "Audit"];
  const positions = order.map((label) => nav.indexOf(label));
  expect(positions.every((position) => position >= 0)).toBe(true);
  expect([...positions].sort((a, b) => a - b)).toEqual(positions);
});
