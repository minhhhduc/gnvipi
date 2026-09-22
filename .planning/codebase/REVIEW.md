---
reviewed: 2026-09-23T00:00:00Z
depth: standard
files_reviewed: 17
files_reviewed_list:
  - cmd/example/main.go
  - cmd/serve/admin.go
  - cmd/serve/main.go
  - cmd/serve/passthrough.go
  - cmd/serve/passthrough_test.go
  - cmd/serve/serve_test.go
  - cmd/serve/responses_anthropic.go
  - cmd/serve/responses_anthropic_test.go
  - internal/captcha/group.go
  - internal/captcha/group_test.go
  - internal/captcha/url_provider.go
  - internal/gnvipi/client.go
  - internal/gnvipi/client_test.go
  - internal/models/playground_models.default.json
  - internal/models/registry.go
  - internal/models/registry_test.go
  - internal/provider/nvidia/executor.go
findings:
  critical: 1
  warning: 2
  info: 0
  total: 3
status: issues_found
---

# Code Review Report

**Reviewed:** 2026-09-23
**Depth:** standard
**Files Reviewed:** 17
**Status:** issues_found

## Summary

The implementation compiles and the focused Go tests pass, but the provider-routing changes contain a wrong-format dispatch path and an unauthenticated SSRF surface. There are also regressions around restoring deleted models, loading local model metadata, and malformed CAPTCHA tokens.

## Critical Issues

### CR-01: Responses-only providers receive Chat Completions requests

**Severity:** BLOCKER
**File:** `cmd/serve/passthrough.go:83-100`

**Issue:** `customOpenAICompatPassthrough` forwards every custom provider except `MessagesOnly` to `/v1/chat/completions`. That includes providers explicitly marked `ResponsesOnly`, so an OpenAI Chat Completions request for such a model is sent to an endpoint the provider does not support. The new Responses-only flag therefore does not isolate the provider to Responses traffic and causes deterministic 4xx failures (or, for a permissive upstream, sends the wrong payload contract).

**Fix:** Exclude `ResponsesOnly` in this middleware and either return an explicit unsupported-format error or translate Chat Completions to Responses before forwarding:

```go
if prov == nil || prov.MessagesOnly || prov.ResponsesOnly {

	if prov != nil && prov.ResponsesOnly {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider accepts Responses API only"})
		c.Abort()
		return
	}
	c.Next()
	return
}
```

### CR-02: Unauthenticated admin endpoint enables arbitrary SSRF (ACCEPTED RISK)

**Severity:** BLOCKER
**File:** `cmd/serve/admin.go:480-500`; `cmd/serve/passthrough.go:100-117`

**Issue:** `/admin/providers` accepts any non-empty `base_url` and persists it. The server defaults to `:8080` (all interfaces), and subsequent model requests cause the gateway to make a server-side request to that URL with the configured bearer key. A remote caller can add a provider pointing at localhost, cloud metadata (`169.254.169.254`), or another internal service and then use the advertised model to probe it. There is no admin authentication, URL scheme/host validation, or private-network policy.

**Fix:** Protect all `/admin/*` routes with authentication and validate provider URLs before saving. Permit only `https` by default, reject loopback/private/link-local/metadata destinations after DNS resolution, and avoid following redirects to a different host (or apply the same policy to every redirect).

**Disposition:** Out of scope for the current deployment: the gateway is local-only and the host firewall does not expose the listener publicly. Revisit if the bind address or network exposure changes.

## Warnings

### WR-01: Restoring a deleted model leaves it hidden

**Severity:** WARNING
**File:** `cmd/serve/admin.go:191-198`

**Issue:** `addDeleted` adds the model to both `deleted` and `hidden`, but `removeDeleted` removes only `deleted`. The UI reports the model as restored while it remains absent from the served/catalog model set until the user separately clears its mask.

**Fix:** Delete the ID from `p.hidden` in `removeDeleted` when deletion is the source of the mask (or track user-hidden and deleted-hidden sets separately so an intentional user mask is preserved).

### WR-02: Auto-extracted CAPTCHA token can crash the example

**Severity:** WARNING
**File:** `cmd/example/main.go:48`

**Issue:** The diagnostic output slices `token[:30]` without checking length. A malformed/short token returned by a changed CAPTCHA page causes a panic before the request is attempted.

**Fix:** Bound the preview:

```go
	preview := token
	if len(preview) > 30 {
		preview = preview[:30]
	}
	fmt.Printf("✓ Extracted captcha token: %s...\\n", preview)
```

### WR-03: `-models-file` group metadata is silently ignored (RESOLVED)

**Severity:** WARNING
**File:** `cmd/serve/admin.go:105-126`

**Issue:** The previous loader populated `models.Models` from `prefsFile.Groups`; that logic was removed, while `load` still accepts and logs the file. A user-supplied models file containing refreshed/custom playground entries is now treated as masks/providers only, so those entries cannot be selected and the file's model catalog is misleading. This is a behavior regression for existing model files.

**Fix:** Either keep the documented override behavior by validating and loading `Groups` into the registry, or remove `Groups` from the persisted format and clearly migrate/reject files that depend on it. Do not silently discard configured model entries.

**Resolution:** `cmd/serve/admin.go` now merges valid local `groups` entries into the embedded registry while preserving embedded models and capabilities. Covered by `TestModelPrefsLoadKeepsEmbeddedModelCatalog`.

---

_Reviewer: the agent (gsd-code-reviewer)_
_Depth: standard_
