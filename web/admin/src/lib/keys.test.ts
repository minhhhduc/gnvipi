import { toChildArray, type ComponentChildren, type VNode } from "preact";
import { describe, expect, it, vi } from "vitest";
import type { AdminUser } from "./auth";
import type { KeyModel, ProviderKey } from "./types";
import { BaseUrlLine } from "../components/ui";
import { formatProviderTestResult } from "../components/ProviderEditor";
import {
  canDeleteKey,
  canEditKey,
  canEditKeyModels,
  canManageProviders,
  canTestKey,
  canToggleKey,
  copyBaseUrl,
  keyCreatePayload,
  keyCredentialLabel,
  keyDisablePayload,
  keyModelsPayload,
  keyNameError,
  keyOwnerLabel,
  keyReach,
  keyUpdatePayload,
  modelChoices,
  parseKeyList,
  parseKeyModels,
  toggleKeyModel,
} from "./keys";

function user(id: string, role: AdminUser["role"]): AdminUser {
  return {
    id,
    username: id,
    email: "",
    display_name: "",
    role,
    status: "active",
    must_change_password: false,
  };
}

const superuser = user("superuser-1", "superuser");
const admin = user("admin-1", "admin");
const regular = user("user-1", "user");

function key(overrides: Partial<ProviderKey> = {}): ProviderKey {
  return {
    id: "key-1",
    provider_id: "p1",
    owner_id: "user-1",
    name: "primary",
    enabled: true,
    auth_scheme: "bearer",
    auth_header_name: "",
    has_secret: true,
    secret_last4: "abcd",
    has_key_env: false,
    has_key_ref: false,
    ...overrides,
  };
}

function model(overrides: Partial<KeyModel> = {}): KeyModel {
  return {
    alias: "vendor/model",
    upstream_model: "vendor/model",
    hidden: false,
    fallbacks: [],
    fallback_statuses: [],
    allow_shadow: false,
    legacy_position: 0,
    ...overrides,
  };
}

function classes(value: ComponentChildren): string[] {
  return toChildArray(value).flatMap((child) => {
    if (!child || typeof child !== "object") return [];
    const node = child as VNode<{ children?: ComponentChildren; class?: string }>;
    return [
      ...(typeof node.props?.class === "string" ? [node.props.class] : []),
      ...classes(node.props?.children),
    ];
  });
}

function text(value: ComponentChildren): string {
  return toChildArray(value)
    .map((child) => {
      if (typeof child === "string" || typeof child === "number") return String(child);
      if (!child || typeof child !== "object") return "";
      return text((child as VNode<{ children?: ComponentChildren }>).props?.children);
    })
    .join(" ");
}

describe("key list parsing", () => {
  it("keeps only metadata and never carries a plaintext secret", () => {
    const parsed = parseKeyList({
      keys: [
        {
          id: "key-1",
          provider_id: "p1",
          owner_id: "user-1",
          name: "primary",
          enabled: true,
          auth_scheme: "bearer",
          auth_header_name: "",
          has_secret: true,
          secret_last4: "abcd",
          has_key_env: false,
          has_key_ref: false,
          key_env: "",
          key_ref: "",
          created_at: "2026-01-01T00:00:00Z",
          updated_at: "2026-01-02T00:00:00Z",
          secret: "sk-live-plaintext",
          ciphertext: "deadbeef",
        },
      ],
    });
    expect(parsed).toEqual([key({ key_env: "", key_ref: "", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-02T00:00:00Z" })]);
    expect(JSON.stringify(parsed)).not.toContain("sk-live-plaintext");
    expect(parsed[0]).not.toHaveProperty("secret");
    expect(Object.keys(parsed[0]).sort()).toEqual([
      "auth_header_name",
      "auth_scheme",
      "created_at",
      "enabled",
      "has_key_env",
      "has_key_ref",
      "has_secret",
      "id",
      "key_env",
      "key_ref",
      "name",
      "owner_id",
      "provider_id",
      "secret_last4",
      "updated_at",
    ]);
  });

  it("rejects a malformed list body", () => {
    expect(() => parseKeyList({ keys: "nope" })).toThrow("Invalid keys response");
    expect(() => parseKeyList(null)).toThrow("Invalid keys response");
  });
});

describe("secret masking", () => {
  it("renders metadata about the secret, never the secret", () => {
    expect(keyCredentialLabel(key())).toBe("•••• abcd");
    expect(keyCredentialLabel(key({ secret_last4: null }))).toBe("Key set");
    expect(keyCredentialLabel(key({ has_secret: false, has_key_env: true, key_env: "OPENAI_KEY" }))).toBe("No key");
    expect(keyCredentialLabel(key({ has_secret: false }))).toBe("No key");
  });
});

describe("role gates", () => {
  it("gives a superuser full reach and confines everyone else to their own keys", () => {
    expect(keyReach(superuser, key({ owner_id: "someone-else" }))).toBe("full");
    expect(keyReach(admin, key({ owner_id: admin.id }))).toBe("full");
    expect(keyReach(admin, key({ owner_id: regular.id }))).toBe("disable");
    expect(keyReach(regular, key({ owner_id: regular.id }))).toBe("full");
    expect(keyReach(regular, key({ owner_id: "someone-else" }))).toBe("none");
    expect(keyReach(regular, key({ owner_id: null }))).toBe("none");
  });

  it("gates provider management and foreign-key actions", () => {
    expect(canManageProviders("superuser")).toBe(true);
    expect(canManageProviders("admin")).toBe(true);
    expect(canManageProviders("user")).toBe(false);

    expect(canEditKey("full")).toBe(true);
    expect(canEditKey("disable")).toBe(false);
    expect(canTestKey("disable")).toBe(false);
    expect(canDeleteKey("disable")).toBe(false);
    expect(canEditKeyModels("disable")).toBe(false);
    expect(canToggleKey("disable")).toBe(true);
    expect(canToggleKey("none")).toBe(false);
  });

  it("labels ownership without leaking anything else", () => {
    expect(keyOwnerLabel(regular, key({ owner_id: null }))).toBe("shared");
    expect(keyOwnerLabel(regular, key({ owner_id: regular.id }))).toBe("you");
    expect(keyOwnerLabel(admin, key({ owner_id: "other-id" }))).toBe("other-id");
  });
});

describe("key payload building", () => {
  it("trims the name and keeps the secret byte-for-byte", () => {
    expect(
      keyCreatePayload({
        name: "  primary ",
        enabled: true,
        authScheme: "bearer",
        secret: "  sk keep me  ",
      }),
    ).toEqual({ name: "primary", enabled: true, auth_scheme: "bearer", secret: "  sk keep me  " });
  });

  it("rejects a blank or over-long name", () => {
    expect(keyNameError("  ")).toBe("Key name is required");
    expect(keyNameError("😀".repeat(101))).toBe("Key name must be at most 100 characters");
    expect(keyNameError("😀".repeat(100))).toBeNull();
    expect(() => keyCreatePayload({ name: " ", enabled: true, authScheme: "bearer" })).toThrow("Key name is required");
  });

  it("never sends a credential with the none scheme", () => {
    expect(
      keyCreatePayload({
        name: "shared",
        enabled: true,
        authScheme: "none",
        secret: "ignored",
        keyEnv: "IGNORED",
        keyRef: "ignored/ref",
        allowKeyRef: true,
      }),
    ).toEqual({ name: "shared", enabled: true, auth_scheme: "none" });
  });

  it("requires a header name for the header scheme and only sends key_ref when allowed", () => {
    expect(() => keyCreatePayload({ name: "h", enabled: true, authScheme: "header" })).toThrow(
      "Header name is required for header auth",
    );
    expect(
      keyCreatePayload({
        name: "h",
        enabled: true,
        authScheme: "header",
        authHeaderName: " x-api-key ",
        keyEnv: " KEY ",
      }),
    ).toEqual({
      name: "h",
      enabled: true,
      auth_scheme: "header",
      auth_header_name: "x-api-key",
      key_env: "KEY",
    });
    expect(
      keyCreatePayload({ name: "h", enabled: true, authScheme: "header", authHeaderName: "x-api-key", keyRef: "vault/key", allowKeyRef: true }),
    ).toMatchObject({ key_ref: "vault/key" });
    expect(
      keyCreatePayload({ name: "h", enabled: true, authScheme: "header", authHeaderName: "x-api-key", keyRef: "vault/key" }),
    ).not.toHaveProperty("key_ref");
  });

  it("marks a superuser-created shared key with owner_id null", () => {
    expect(keyCreatePayload({ name: "shared", enabled: true, authScheme: "none" }, { shared: true })).toEqual({
      name: "shared",
      enabled: true,
      auth_scheme: "none",
      owner_id: null,
    });
    expect(keyCreatePayload({ name: "own", enabled: true, authScheme: "none" })).not.toHaveProperty("owner_id");
  });

  it("omits a blank secret on update and never sends owner_id", () => {
    const payload = keyUpdatePayload({ name: "renamed", enabled: false, authScheme: "bearer", secret: "" });
    expect(payload).toEqual({ name: "renamed", enabled: false, auth_scheme: "bearer" });
    expect(payload).not.toHaveProperty("owner_id");
    expect(keyUpdatePayload({ name: "renamed", enabled: true, authScheme: "bearer", secret: "new" })).toEqual({
      name: "renamed",
      enabled: true,
      auth_scheme: "bearer",
      secret: "new",
    });
  });

  it("rejects key_env and key_ref together", () => {
    expect(() =>
      keyCreatePayload({ name: "k", enabled: true, authScheme: "bearer", keyEnv: "ENV", keyRef: "ref", allowKeyRef: true }),
    ).toThrow("Use either key_env or key_ref, not both");
  });

  it("keeps the admin disable patch minimal", () => {
    expect(keyDisablePayload()).toEqual({ enabled: false });
  });
});

describe("per-key model selection", () => {
  it("adds and removes a selected model without duplicating it", () => {
    const added = toggleKeyModel([], { alias: "vendor/model", upstream_model: "" }, true);
    expect(added).toEqual([model()]);
    expect(toggleKeyModel(added, { alias: "vendor/model", upstream_model: "other" }, true)).toEqual(added);
    expect(toggleKeyModel(added, { alias: "vendor/model", upstream_model: "other" }, false)).toEqual([]);
  });

  it("keeps a wildcard upstream empty and defaults an exact alias upstream", () => {
    expect(toggleKeyModel([], { alias: "vendor/*", upstream_model: "" }, true)[0].upstream_model).toBe("");
    expect(toggleKeyModel([], { alias: "vendor/model", upstream_model: "" }, true)[0].upstream_model).toBe("vendor/model");
  });

  it("offers only provider aliases that are not already on the key", () => {
    expect(
      modelChoices(
        [
          { alias: "a", upstream_model: "a-up" },
          { alias: "a", upstream_model: "duplicate" },
          { alias: "b", upstream_model: "" },
          { alias: "", upstream_model: "blank" },
        ],
        [model({ alias: "b" })],
      ),
    ).toEqual([{ alias: "a", upstream_model: "a-up" }]);
  });

  it("parses key models with safe defaults", () => {
    expect(
      parseKeyModels({
        models: [
          {
            alias: "vendor/*",
            upstream_model: "",
            hidden: true,
            fallbacks: ["a"],
            fallback_statuses: [429],
            allow_shadow: true,
            legacy_position: 2,
            secret: "ignored",
          },
          { alias: "b" },
        ],
      }),
    ).toEqual([
      {
        alias: "vendor/*",
        upstream_model: "",
        hidden: true,
        fallbacks: ["a"],
        fallback_statuses: [429],
        allow_shadow: true,
        legacy_position: 2,
      },
      {
        alias: "b",
        upstream_model: "",
        hidden: false,
        fallbacks: [],
        fallback_statuses: [],
        allow_shadow: false,
        legacy_position: 0,
      },
    ]);
  });

  it("builds the replace payload with trimmed aliases and explicit arrays", () => {
    expect(keyModelsPayload([model({ alias: " vendor/model ", fallbacks: undefined as unknown as string[] })])).toEqual({
      models: [
        {
          alias: "vendor/model",
          upstream_model: "vendor/model",
          hidden: false,
          fallbacks: [],
          fallback_statuses: [],
          allow_shadow: false,
          legacy_position: 0,
        },
      ],
    });
  });
});

describe("key test diagnostics", () => {
  it("keeps status, status_code, latency and the upstream message visible", () => {
    const summary = formatProviderTestResult({
      ok: false,
      status: "auth_failure",
      status_code: 401,
      latency_ms: 123,
      message: "upstream rejected the credential",
    });
    expect(summary).toContain("auth_failure");
    expect(summary).toContain("HTTP 401");
    expect(summary).toContain("123ms");
    expect(summary).toContain("upstream rejected the credential");
  });
});

describe("base URL copy", () => {
  it("copies the exact URL through an injectable writer", async () => {
    const write = vi.fn(async () => {});
    await expect(copyBaseUrl("http://example.test/v1", write)).resolves.toBe(true);
    expect(write).toHaveBeenCalledWith("http://example.test/v1");
  });

  it("reports a clipboard failure and skips an empty URL", async () => {
    await expect(copyBaseUrl("http://example.test", async () => { throw new Error("nope"); })).resolves.toBe(false);
    const write = vi.fn(async () => {});
    await expect(copyBaseUrl("", write)).resolves.toBe(false);
    expect(write).not.toHaveBeenCalled();
  });

  it("renders the URL with wrapping and a copy control", () => {
    const vnode = BaseUrlLine({ url: "http://example.test/v1", label: "Base URL", onCopy: () => {} });
    expect(classes(vnode).some((cls) => cls.includes("break-all"))).toBe(true);
    expect(text(vnode)).toContain("http://example.test/v1");
    expect(text(vnode)).toContain("Copy");
  });
});
