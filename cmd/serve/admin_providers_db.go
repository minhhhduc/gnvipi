package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/minhhhduc/gnvipi/internal/auth"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// providerModelsGuidance is the 409 body for a provider-level models write on a
// provider that does not have exactly one key.
const providerModelsGuidance = "provider-level models require exactly one key; use PUT /admin/api/providers/{id}/keys/{keyID}/models"

// v2ProviderKeyName names the key a v2 auth block creates. A provider has one
// v2 auth object, so its target key needs no user-visible name.
const v2ProviderKeyName = "default"

// providerIDPattern mirrors the providers.id CHECK so a bad slug is a field
// error instead of a raw check-constraint 500.
var providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// providerCheckFields names the request field behind each providers CHECK so a
// rejected value is a field error instead of a raw 23514 500. PostgreSQL names
// inline checks <table>_<column>_check, with a numeric suffix on repeats, so
// the match is by prefix.
var providerCheckFields = []struct{ prefix, field, problem string }{
	{"providers_id_check", "id", "must start with a lowercase letter or digit and contain only a-z, 0-9, '.', '_' or '-'"},
	{"providers_name_check", "name", "must be 1-200 characters"},
	{"providers_protocol_check", "protocol", "must be 1-64 characters"},
	{"providers_base_url_check", "base_url", "must be at most 2048 characters and start with http:// or https://"},
	{"providers_proxy_url_check", "proxy_url", "must be at most 2048 characters and use an http(s) or socks proxy URL"},
	{"providers_tls_check", "tls", `must be "default" or "chrome"`},
	{"providers_endpoint_paths_check", "endpoint_paths", "must be an object of at most 8192 JSON characters"},
	{"providers_forward_headers_check", "forward_headers", "must be an array of at most 64 entries"},
	{"providers_headers_check", "headers", "must be an object of at most 16384 JSON characters"},
	{"providers_extra_body_check", "extra_body", "must be a JSON object without model or stream, at most 16384 characters"},
	{"providers_extensions_check", "extensions", "must be an object of at most 65536 JSON characters"},
	{"providers_max_concurrency_check", "max_concurrency", "must be non-negative"},
	{"providers_queue_timeout_check", "queue_timeout", "must be non-negative"},
	{"providers_timeouts_check", "timeouts", "must be an object of at most 2048 JSON characters"},
}

// dbCheckFieldError answers a providers CHECK violation as a field error. It
// reports false for every other failure so the caller keeps its 500.
func dbCheckFieldError(c *gin.Context, err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		return false
	}
	for _, candidate := range providerCheckFields {
		if strings.HasPrefix(pgErr.ConstraintName, candidate.prefix) {
			adminFieldError(c, http.StatusBadRequest, candidate.field, candidate.problem)
			return true
		}
	}
	return false
}

// registerAdminProviderDBRoutes installs the database-backed v2 provider CRUD.
// Every write runs in one transaction that also carries its audit row; the
// snapshot refreshes from the committed config version bump, never mid-write.
func registerAdminProviderDBRoutes(engine *gin.Engine, state *authState) {
	engine.GET("/admin/api/providers", func(c *gin.Context) { handleDBAdminAPIListProviders(c, state) })
	engine.POST("/admin/api/providers", func(c *gin.Context) { handleDBAdminAPICreateProvider(c, state) })
	engine.GET("/admin/api/providers/:id", func(c *gin.Context) { handleDBAdminAPIGetProvider(c, state) })
	engine.PUT("/admin/api/providers/:id", func(c *gin.Context) { handleDBAdminAPIUpdateProvider(c, state) })
	engine.DELETE("/admin/api/providers/:id", func(c *gin.Context) { handleDBAdminAPIDeleteProvider(c, state) })
	engine.POST("/admin/api/providers/:id/test", func(c *gin.Context) { handleDBAdminAPITestProvider(c, state) })
}

func userProviderReader(identity *adminIdentity) bool {
	return identity != nil && identity.Role == auth.RoleUser
}

func providerNotFound(c *gin.Context, id string, user bool) {
	if user {
		adminNotFoundResponse(c)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("provider %q not found", id)})
}

// providerView projects committed rows through LegacyView. A regular user sees
// only their own and shared keys and a metadata-only provider shape.
func providerView(ctx context.Context, q providerstore.Querier, row *providerstore.ProviderRow, identity *adminIdentity) (gatewayprovider.Provider, error) {
	var records []providerstore.KeyRecord
	var err error
	if userProviderReader(identity) {
		// A regular user only needs shared keys plus their own; loading every
		// owner's ciphertext would defeat the #203 visibility invariant.
		records, err = providerstore.ListProviderKeysVisibleTo(ctx, q, row.ID, identity.UserID)
	} else {
		records, err = providerstore.ListProviderKeys(ctx, q, row.ID, "")
	}
	if err != nil {
		return gatewayprovider.Provider{}, err
	}
	models, err := providerstore.ListProviderKeyModels(ctx, q, row.ID)
	if err != nil {
		return gatewayprovider.Provider{}, err
	}
	keys := make([]providerstore.KeyRow, len(records))
	for i := range records {
		keys[i] = records[i].KeyRow
	}
	opts := providerstore.LegacyViewOptions{IncludeKeyEnv: true, IncludeKeyRef: true}
	if userProviderReader(identity) {
		visible := make([]string, 0, len(keys))
		for i := range keys {
			if keys[i].OwnerID == "" || sameUserID(keys[i].OwnerID, identity.UserID) {
				visible = append(visible, keys[i].ID)
			}
		}
		opts.VisibleKeyIDs = visible
	}
	view, err := providerstore.LegacyView(*row, keys, models, opts)
	if err != nil {
		return gatewayprovider.Provider{}, err
	}
	if userProviderReader(identity) {
		view = providerMetadata(&view)
	}
	return view, nil
}

func handleDBAdminAPIListProviders(c *gin.Context, state *authState) {
	ctx := c.Request.Context()
	rows, err := providerstore.ListProviders(ctx, state.pool)
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "list providers failed")
		return
	}
	identity := adminIdentityOf(c)
	out := make([]gatewayprovider.Provider, 0, len(rows))
	for i := range rows {
		if userProviderReader(identity) && !rows[i].Enabled {
			continue
		}
		view, err := providerView(ctx, state.pool, &rows[i], identity)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
			return
		}
		out = append(out, view)
	}
	c.JSON(http.StatusOK, out)
}

func handleDBAdminAPIGetProvider(c *gin.Context, state *authState) {
	id := getProviderID(c)
	ctx := c.Request.Context()
	row, err := providerstore.GetProviderRow(ctx, state.pool, id)
	if err != nil {
		if errors.Is(err, providerstore.ErrProviderNotFound) {
			providerNotFound(c, id, userProviderReader(adminIdentityOf(c)))
			return
		}
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
		return
	}
	identity := adminIdentityOf(c)
	if userProviderReader(identity) && !row.Enabled {
		adminNotFoundResponse(c)
		return
	}
	view, err := providerView(ctx, state.pool, &row, identity)
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
		return
	}
	c.JSON(http.StatusOK, view)
}

func handleDBAdminAPICreateProvider(c *gin.Context, state *authState) {
	var pv gatewayprovider.Provider
	if err := c.ShouldBindJSON(&pv); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pv.ID = strings.TrimSpace(pv.ID)
	if pv.ID == "" {
		adminFieldError(c, http.StatusBadRequest, "id", "must not be empty")
		return
	}
	if !providerIDPattern.MatchString(pv.ID) {
		adminFieldError(c, http.StatusBadRequest, "id", "must start with a lowercase letter or digit and contain only a-z, 0-9, '.', '_' or '-'")
		return
	}
	if pv.Auth.Scheme == "api-key" {
		pv.Auth.Scheme = gatewayprovider.AuthAPIKey
	}
	row, err := providerstore.ProviderRowFromV2(pv)
	if err != nil {
		validationErrorResponse(c, err)
		return
	}

	ctx := c.Request.Context()
	tx, err := state.pool.Begin(ctx)
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "begin provider transaction failed")
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort rollback after an aborted write

	if err := providerstore.InsertProvider(ctx, tx, row); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			adminFieldError(c, http.StatusConflict, "id", "already exists")
			return
		}
		if dbCheckFieldError(c, err) {
			return
		}
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "create provider failed")
		return
	}
	// A v2 auth block is a provider_keys row; dropping it would return 201 for a
	// provider that cannot route.
	if !applyProviderAuth(c, ctx, tx, state, row.ID, &pv.Auth, adminIdentityOf(c), nil) {
		return
	}
	if len(pv.Models) > 0 {
		if !replaceProviderModels(c, ctx, tx, row.ID, pv.Models, adminIdentityOf(c)) {
			return
		}
	}
	if err := writeProviderAudit(ctx, tx, adminIdentityOf(c), "provider.create", row.ID, c.ClientIP(), map[string]any{
		"name":    row.Name,
		"enabled": row.Enabled,
	}); err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "write provider audit failed")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "commit provider transaction failed")
		return
	}
	view, err := providerView(ctx, state.pool, &row, adminIdentityOf(c))
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
		return
	}
	c.JSON(http.StatusCreated, view)
}

func handleDBAdminAPIUpdateProvider(c *gin.Context, state *authState) {
	id := getProviderID(c)
	var pv gatewayprovider.Provider
	if err := c.ShouldBindJSON(&pv); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if pv.ID == "" {
		pv.ID = id
	} else if pv.ID != id {
		adminFieldError(c, http.StatusBadRequest, "id", "id in path does not match id in body")
		return
	}
	if pv.Auth.Scheme == "api-key" {
		pv.Auth.Scheme = gatewayprovider.AuthAPIKey
	}
	row, err := providerstore.ProviderRowFromV2(pv)
	if err != nil {
		validationErrorResponse(c, err)
		return
	}

	ctx := c.Request.Context()
	tx, err := state.pool.Begin(ctx)
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "begin provider transaction failed")
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort rollback after an aborted write

	if _, err := providerstore.GetProviderRowForUpdate(ctx, tx, id); err != nil {
		if errors.Is(err, providerstore.ErrProviderNotFound) {
			providerNotFound(c, id, false)
			return
		}
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
		return
	}
	if err := providerstore.UpdateProvider(ctx, tx, row); err != nil {
		if errors.Is(err, providerstore.ErrProviderNotFound) {
			providerNotFound(c, id, false)
			return
		}
		if dbCheckFieldError(c, err) {
			return
		}
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "update provider failed")
		return
	}
	records, err := providerstore.ListProviderKeysForUpdate(ctx, tx, id)
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider keys failed")
		return
	}
	if !applyProviderAuth(c, ctx, tx, state, id, &pv.Auth, adminIdentityOf(c), records) {
		return
	}
	// A nil slice means the client omitted models; an explicit list (including
	// []) is a provider-level write. The console echoes the current aggregate,
	// so an unchanged list is not a write and must not trip the multi-key 409.
	if pv.Models != nil {
		unchanged, err := providerModelsUnchanged(ctx, tx, id, pv.Models)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider models failed")
			return
		}
		if !unchanged && !replaceProviderModels(c, ctx, tx, id, pv.Models, adminIdentityOf(c)) {
			return
		}
	}
	if err := writeProviderAudit(ctx, tx, adminIdentityOf(c), "provider.update", id, c.ClientIP(), map[string]any{
		"name":    row.Name,
		"enabled": row.Enabled,
	}); err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "write provider audit failed")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "commit provider transaction failed")
		return
	}
	view, err := providerView(ctx, state.pool, &row, adminIdentityOf(c))
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
		return
	}
	c.JSON(http.StatusOK, view)
}

func handleDBAdminAPIDeleteProvider(c *gin.Context, state *authState) {
	id := getProviderID(c)
	ctx := c.Request.Context()
	tx, err := state.pool.Begin(ctx)
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "begin provider transaction failed")
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort rollback after an aborted write
	if err := providerstore.DeleteProvider(ctx, tx, id); err != nil {
		if errors.Is(err, providerstore.ErrProviderNotFound) {
			providerNotFound(c, id, false)
			return
		}
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "delete provider failed")
		return
	}
	if err := writeProviderAudit(ctx, tx, adminIdentityOf(c), "provider.delete", id, c.ClientIP(), nil); err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "write provider audit failed")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "commit provider transaction failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "deleted": id})
}

// handleDBAdminAPITestProvider probes the stored provider with the credential
// of the same first key LegacyView reports, so Test Connection never falls back
// to the file store in database mode.
func handleDBAdminAPITestProvider(c *gin.Context, state *authState) {
	id := getProviderID(c)
	ctx := c.Request.Context()
	row, err := providerstore.GetProviderRow(ctx, state.pool, id)
	if err != nil {
		if errors.Is(err, providerstore.ErrProviderNotFound) {
			providerNotFound(c, id, false)
			return
		}
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
		return
	}
	definition, err := providerstore.BuildProviderDefinition(row)
	if err != nil {
		adminFieldError(c, http.StatusUnprocessableEntity, "provider", "provider configuration is invalid")
		return
	}
	records, err := providerstore.ListProviderKeys(ctx, state.pool, id, "")
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider keys failed")
		return
	}
	if len(records) > 0 {
		keys := make([]providerstore.KeyRow, len(records))
		for i := range records {
			keys[i] = records[i].KeyRow
		}
		key := providerstore.SortedKeys(keys)[0]
		if key.AuthScheme != providerstore.SchemeNone {
			credential, err := providerstore.ResolveKeyCredential(ctx, key, state.keyring, nil)
			if err != nil {
				if errors.Is(err, providerstore.ErrMissingMasterKey) {
					adminAPIError(c, http.StatusInternalServerError, "credential_unavailable", "stored key material cannot be decrypted")
					return
				}
				adminFieldError(c, http.StatusUnprocessableEntity, "credential", strings.TrimPrefix(err.Error(), "providerstore: "))
				return
			}
			c.JSON(http.StatusOK, probeKeyCredential(ctx, &definition, &key, credential))
			return
		}
	}
	c.JSON(http.StatusOK, testProviderConnection(ctx, &definition))
}

// authBlockAbsent reports whether a request carried no auth object at all. The
// v2 wire shape cannot distinguish an omitted auth from an empty one, so an
// empty scheme with no credential fields leaves stored keys alone.
func authBlockAbsent(a *gatewayprovider.Auth) bool {
	return a.Scheme == "" && a.Key == "" && a.KeyEnv == "" && a.KeyRef == "" && a.HeaderName == ""
}

// providerAuthScheme maps the v2 auth.scheme onto the database key scheme.
func providerAuthScheme(c *gin.Context, scheme gatewayprovider.AuthScheme) (dbScheme, headerName string, ok bool) {
	switch scheme {
	case gatewayprovider.AuthNone:
		return providerstore.SchemeNone, "", true
	case gatewayprovider.AuthBearer:
		return providerstore.SchemeBearer, "", true
	case gatewayprovider.AuthAPIKey:
		return providerstore.SchemeHeader, "x-api-key", true
	default:
		adminFieldError(c, http.StatusUnprocessableEntity, "auth.scheme", `must be "none", "bearer" or "x-api-key"`)
		return "", "", false
	}
}

// applyProviderAuth persists a v2 auth block through the key CRUD path. A v2
// provider has one auth object, so an update targets the same key LegacyView
// reports (shared first, then id); a provider without keys gets one created.
// An absent block, or a credentialless echo of the target key, changes nothing.
func applyProviderAuth(c *gin.Context, ctx context.Context, tx pgx.Tx, state *authState, providerID string, block *gatewayprovider.Auth, identity *adminIdentity, existing []providerstore.KeyRecord) bool {
	if authBlockAbsent(block) {
		return true
	}
	scheme, headerName, ok := providerAuthScheme(c, block.Scheme)
	if !ok {
		return false
	}
	if scheme == providerstore.SchemeHeader && strings.TrimSpace(block.HeaderName) != "" {
		headerName = strings.TrimSpace(block.HeaderName)
	}
	var secret []byte
	if block.Key != "" && !isRedactedKey(block.Key) {
		secret = []byte(block.Key)
	}
	keyEnv := strings.TrimSpace(block.KeyEnv)
	keyRef := strings.TrimSpace(block.KeyRef)
	hasCredential := len(secret) > 0 || keyEnv != "" || keyRef != ""
	if scheme == providerstore.SchemeNone && (hasCredential || block.HeaderName != "") {
		adminFieldError(c, http.StatusUnprocessableEntity, "auth.key", "auth.scheme none does not accept a credential")
		return false
	}
	allowKeyRef := identity != nil && (identity.Role == auth.RoleOwner || identity.Role == auth.RoleAdmin)

	var target *providerstore.KeyRecord
	if len(existing) > 0 {
		keys := make([]providerstore.KeyRow, len(existing))
		for i := range existing {
			keys[i] = existing[i].KeyRow
		}
		first := providerstore.SortedKeys(keys)[0]
		for i := range existing {
			if existing[i].ID == first.ID {
				target = &existing[i]
				break
			}
		}
	}
	if target != nil {
		// An echo of the stored auth (GET never returns auth.key) is not a write,
		// so it must not be refused for ownership either.
		sameScheme := scheme == target.AuthScheme
		headerEcho := scheme != providerstore.SchemeHeader ||
			strings.TrimSpace(block.HeaderName) == "" ||
			strings.EqualFold(strings.TrimSpace(block.HeaderName), target.AuthHeaderName)
		envChanged := keyEnv != "" && keyEnv != target.KeyEnv
		refChanged := keyRef != "" && keyRef != target.KeyRef
		if sameScheme && headerEcho && len(secret) == 0 && !envChanged && !refChanged {
			return true
		}
		if keyReachFor(identity, &target.KeyRow) != keyReachFull {
			adminAPIError(c, http.StatusForbidden, "forbidden", "this key belongs to another user")
			return false
		}
		patch := providerstore.KeyPatch{AuthScheme: &scheme, Secret: secret, AllowKeyRef: allowKeyRef}
		if scheme == providerstore.SchemeHeader {
			patch.AuthHeaderName = &headerName
		}
		if envChanged {
			patch.KeyEnv = &keyEnv
		}
		if refChanged {
			patch.KeyRef = &keyRef
		}
		next, err := providerstore.ApplyKeyPatch(target.KeyRow, patch, state.keyring)
		if err != nil {
			adminKeyCredentialError(c, err)
			return false
		}
		updated, err := providerstore.UpdateProviderKey(ctx, tx, next)
		if err != nil {
			adminKeyStoreError(c, err)
			return false
		}
		if err := writeKeyAudit(ctx, tx, identity, "key.update", updated.ID, updated.OwnerID, updated.ID, updated.ProviderID, c.ClientIP(), keyAuditDetails(&updated)); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "write key audit failed")
			return false
		}
		return true
	}

	// No key exists yet: the v2 auth block becomes the provider's first key.
	ownerID := ""
	if scheme != providerstore.SchemeNone {
		if identity == nil || strings.TrimSpace(identity.UserID) == "" {
			adminAPIError(c, http.StatusForbidden, "forbidden", "a credential key requires an owning user")
			return false
		}
		ownerID = identity.UserID
	}
	keyID, err := providerstore.NewKeyID(ctx, tx)
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "allocate provider key id failed")
		return false
	}
	patch := providerstore.KeyPatch{AuthScheme: &scheme, Secret: secret, AllowKeyRef: allowKeyRef}
	if scheme == providerstore.SchemeHeader {
		patch.AuthHeaderName = &headerName
	}
	if keyEnv != "" {
		patch.KeyEnv = &keyEnv
	}
	if keyRef != "" {
		patch.KeyRef = &keyRef
	}
	row, err := providerstore.ApplyKeyPatch(providerstore.KeyRow{
		ID:         keyID,
		ProviderID: providerID,
		OwnerID:    ownerID,
		Name:       v2ProviderKeyName,
		Enabled:    true,
	}, patch, state.keyring)
	if err != nil {
		adminKeyCredentialError(c, err)
		return false
	}
	record, err := providerstore.InsertProviderKey(ctx, tx, row)
	if err != nil {
		adminKeyStoreError(c, err)
		return false
	}
	if err := writeKeyAudit(ctx, tx, identity, "key.create", record.ID, record.OwnerID, record.ID, record.ProviderID, c.ClientIP(), keyAuditDetails(&record)); err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "write key audit failed")
		return false
	}
	return true
}

func keyAuditDetails(record *providerstore.KeyRecord) map[string]any {
	return map[string]any{
		"name":        record.Name,
		"auth_scheme": record.AuthScheme,
		"enabled":     record.Enabled,
		"has_secret":  record.HasSecret(),
		"has_key_env": record.KeyEnv != "",
		"has_key_ref": record.KeyRef != "",
	}
}

// providerModelsWritable applies the per-key reach rules to a provider-level
// models write: the owner may write any key, an admin only its own or a shared
// key, and a user only its own.
func providerModelsWritable(identity *adminIdentity, key *providerstore.KeyRow) bool {
	if identity == nil {
		return false
	}
	switch identity.Role {
	case auth.RoleOwner:
		return true
	case auth.RoleAdmin:
		return key.OwnerID == "" || sameUserID(key.OwnerID, identity.UserID)
	case auth.RoleUser:
		return key.OwnerID != "" && sameUserID(key.OwnerID, identity.UserID)
	default:
		return false
	}
}

// providerModelsUnchanged reports whether a provider-level models list is the
// console echoing the current aggregate. Such an echo is not a write and must
// not trip the multi-key 409.
func providerModelsUnchanged(ctx context.Context, q providerstore.Querier, providerID string, models []gatewayprovider.ModelMapping) (bool, error) {
	records, err := providerstore.ListProviderKeys(ctx, q, providerID, "")
	if err != nil {
		return false, err
	}
	rows, err := providerstore.ListProviderKeyModels(ctx, q, providerID)
	if err != nil {
		return false, err
	}
	keys := make([]providerstore.KeyRow, len(records))
	for i := range records {
		keys[i] = records[i].KeyRow
	}
	current := providerstore.AggregateModels(keys, rows, providerstore.LegacyViewOptions{})
	return sameModelMappings(current, models), nil
}

// sameModelMappings compares two v2 model lists. Empty and nil fallback slices
// are equivalent because the JSON wire form omits both.
func sameModelMappings(a, b []gatewayprovider.ModelMapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Alias != b[i].Alias || a[i].UpstreamModel != b[i].UpstreamModel ||
			a[i].Hidden != b[i].Hidden || a[i].AllowShadow != b[i].AllowShadow ||
			a[i].LegacyPosition != b[i].LegacyPosition ||
			!slices.Equal(a[i].Fallbacks, b[i].Fallbacks) ||
			!slices.Equal(a[i].FallbackStatuses, b[i].FallbackStatuses) {
			return false
		}
	}
	return true
}

// replaceProviderModels writes a v2 provider-level models list onto the single
// visible key. An empty list on a keyless provider is a no-op; any other
// provider without exactly one key gets the documented 409.
func replaceProviderModels(c *gin.Context, ctx context.Context, tx pgx.Tx, providerID string, models []gatewayprovider.ModelMapping, identity *adminIdentity) bool {
	records, err := providerstore.ListProviderKeysForUpdate(ctx, tx, providerID)
	if err != nil {
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider keys failed")
		return false
	}
	if len(models) == 0 && len(records) == 0 {
		return true
	}
	keys := make([]providerstore.KeyRow, len(records))
	for i := range records {
		keys[i] = records[i].KeyRow
	}
	target, err := providerstore.LegacyModelsTarget(keys, providerstore.LegacyViewOptions{})
	if err != nil {
		if errors.Is(err, providerstore.ErrModelsAmbiguous) {
			adminFieldError(c, http.StatusConflict, "models", providerModelsGuidance)
			return false
		}
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "resolve provider models target failed")
		return false
	}
	var key providerstore.KeyRow
	for i := range keys {
		if keys[i].ID == target {
			key = keys[i]
			break
		}
	}
	if !providerModelsWritable(identity, &key) {
		adminAPIError(c, http.StatusForbidden, "forbidden", "this key belongs to another user")
		return false
	}
	rows := make([]providerstore.KeyModel, 0, len(models))
	for _, model := range models {
		rows = append(rows, providerstore.KeyModel{
			Alias:            model.Alias,
			UpstreamModel:    model.UpstreamModel,
			Hidden:           model.Hidden,
			Fallbacks:        model.Fallbacks,
			FallbackStatuses: model.FallbackStatuses,
			AllowShadow:      model.AllowShadow,
			LegacyPosition:   model.LegacyPosition,
		})
	}
	if err := providerstore.ReplaceKeyModels(ctx, tx, key, rows); err != nil {
		switch {
		case errors.Is(err, providerstore.ErrDuplicateKeyAlias):
			adminFieldError(c, http.StatusConflict, "alias", "this alias is already used in the same owner scope")
		case errors.Is(err, providerstore.ErrInvalidConfig):
			adminFieldError(c, http.StatusUnprocessableEntity, "models", strings.TrimPrefix(err.Error(), "providerstore: "))
		default:
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "write provider models failed")
		}
		return false
	}
	return true
}

// writeProviderAudit records a provider change in the caller's transaction, so
// the audit row commits with the write it describes.
func writeProviderAudit(ctx context.Context, q auth.Querier, identity *adminIdentity, action, providerID, ip string, details map[string]any) error {
	entry := auth.AuditEntry{ActorType: "system"}
	if identity != nil {
		entry = adminAuditActor(identity)
	}
	entry.Action, entry.TargetType, entry.TargetID = action, "provider", providerID
	entry.ProviderID, entry.IP, entry.Details = providerID, ip, details
	return auth.WriteAudit(ctx, q, entry)
}
