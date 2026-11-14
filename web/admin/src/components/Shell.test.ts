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

function renderShell(role: TestRole, legacy = false): string {
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
    legacy,
    onLogout: legacy ? undefined : vi.fn(),
  } as Parameters<typeof Shell>[0] & { user: typeof user; onLogout: () => void };
  return textContent(Shell(props));
}

describe("Shell role navigation", () => {
  it.each(["owner", "admin"] as const)("shows operator screens to %s", (role) => {
    const text = renderShell(role);
    expect(text).toContain("Stats");
    expect(text).toContain("Models");
    expect(text).toContain("Config");
    expect(text).toContain("Chrome");
  });

  it("hides operator-only screens from users", () => {
    const text = renderShell("user");
    expect(text).toContain("Stats");
    expect(text).not.toContain("Models");
    expect(text).not.toContain("Config");
    expect(text).not.toContain("Chrome");
  });

  it("shows the signed-in identity and logout control", () => {
    const text = renderShell("admin");
    expect(text).toContain("admin-name");
    expect(text).toContain("admin");
    expect(text).toContain("Sign out");
  });

  it("hides the database-backed screens in legacy mode", () => {
    const text = renderShell("owner", true);
    expect(text).toContain("Stats");
    expect(text).toContain("Models");
    expect(text).not.toContain("Tokens");
    expect(text).not.toContain("Users");
  });
});
