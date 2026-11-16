import { toChildArray, type ComponentChildren, type VNode } from "preact";
import { describe, expect, it } from "vitest";
import { NvidiaRescanButton, rescanSummary } from "./Models";

function textContent(value: ComponentChildren): string {
  return toChildArray(value)
    .map((child) => {
      if (typeof child === "string" || typeof child === "number") return String(child);
      if (!child || typeof child === "boolean") return "";
      return textContent((child as VNode<{ children?: ComponentChildren }>).props.children);
    })
    .join(" ");
}

function renderButton(role: string, busy = false): string {
  const node = NvidiaRescanButton({ role, busy, onClick: () => undefined });
  return node ? textContent(node) : "";
}

describe("NVIDIA rescan button role rendering", () => {
  it.each(["superuser", "admin"])("shows the rescan action to %s", (role) => {
    expect(renderButton(role)).toContain("Rescan NVIDIA models");
  });

  it("hides the rescan action from users", () => {
    expect(renderButton("user")).not.toContain("Rescan NVIDIA models");
    expect(renderButton("user")).toBe("");
  });

  it("reports progress while the scan runs", () => {
    expect(renderButton("superuser", true)).toContain("Rescanning");
  });

  it("summarizes reconcile counts", () => {
    expect(rescanSummary({ added: 2, updated: 1, removed: 3, scanned_at: "2026-01-01T00:00:00Z" })).toBe(
      "2 added, 1 updated, 3 removed",
    );
  });
});
