package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/minhhhduc/gnvipi/internal/auth"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

const (
	keyNameMaxLength = 100
)

func registerAdminKeyRoutes(engine *gin.Engine, state *authState) {
	engine.GET("/admin/api/providers/:id/keys", handleAdminKeyList(state))
	engine.POST("/admin/api/providers/:id/keys", handleAdminKeyCreate(state))
	engine.GET("/admin/api/providers/:id/keys/:keyID", handleAdminKeyGet(state))
	engine.PUT("/admin/api/providers/:id/keys/:keyID", handleAdminKeyUpdate(state))
	engine.DELETE("/admin/api/providers/:id/keys/:keyID", handleAdminKeyDelete(state))
	engine.POST("/admin/api/providers/:id/keys/:keyID/test", handleAdminKeyTest(state))
	engine.GET("/admin/api/providers/:id/keys/:keyID/models", handleAdminKeyModelList(state))
	engine.PUT("/admin/api/providers/:id/keys/:keyID/models", handleAdminKeyModelReplace(state))
}

// keyReach is how far one caller reaches into one key. The permission table
// admits roles; the ownership rules below are per row and cannot live there.
type keyReach uint8

const (
	keyReachNone keyReach = iota
	// keyReachDisable is an admin on a foreign key: metadata read and disabling
	// only. Secrets, names, schemes, credential clearing, tests and deletion
	// stay with the owner and the key's own user.
	keyReachDisable
	keyReachFull
)

func keyReachFor(identity *adminIdentity, key *providerstore.KeyRow) keyReach {
	if identity == nil {
		return keyReachNone
	}
	if identity.Role == auth.RoleOwner {
		return keyReachFull
	}
	if key.OwnerID != "" && sameUserID(key.OwnerID, identity.UserID) {
		return keyReachFull
	}
	if identity.Role == auth.RoleAdmin {
		return keyReachDisable
	}
	return keyReachNone
}

// keyWriteRequest is the create and update body. Every mutation field is a
// pointer so an omitted field means "unchanged" and an explicit empty secret
// keeps the stored one.
type keyWriteRequest struct {
	Name           *string         `json:"name"`
	Enabled        *bool           `json:"enabled"`
	AuthScheme     *string         `json:"auth_scheme"`
	AuthHeaderName *string         `json:"auth_header_name"`
	Secret         *string         `json:"secret"`
	KeyEnv         *string         `json:"key_env"`
	KeyRef         *string         `json:"key_ref"`
	ClearSecret    bool            `json:"clear_secret"`
	OwnerID        json.RawMessage `json:"owner_id"`
}

var errInvalidOwnerID = errors.New("owner_id must be a user id or null")

// ownerScope distinguishes an absent owner_id from an explicit null (a shared
// key) and from a named user.
func (r *keyWriteRequest) ownerScope() (present, shared bool, id string, err error) {
	raw := strings.TrimSpace(string(r.OwnerID))
	if raw == "" {
		return false, false, "", nil
	}
	if raw == "null" {
		return true, true, "", nil
	}
	if err := json.Unmarshal(r.OwnerID, &id); err != nil || strings.TrimSpace(id) == "" {
		return true, false, "", errInvalidOwnerID
	}
	var parsed pgtype.UUID
	if err := parsed.Scan(id); err != nil || !parsed.Valid {
		return true, false, "", errInvalidOwnerID
	}
	return true, false, id, nil
}

func (r *keyWriteRequest) secretValue() []byte {
	if r.Secret == nil || *r.Secret == "" {
		return nil
	}
	return []byte(*r.Secret)
}

func (r *keyWriteRequest) carriesCredential() bool {
	return len(r.secretValue()) > 0 ||
		(r.KeyEnv != nil && *r.KeyEnv != "") ||
		(r.KeyRef != nil && *r.KeyRef != "")
}

// noneSchemeRejectsCredential keeps a caller from handing over a secret that a
// "none" key would silently drop.
func noneSchemeRejectsCredential(scheme string, r *keyWriteRequest) bool {
	if scheme != providerstore.SchemeNone {
		return false
	}
	return r.carriesCredential() || (r.AuthHeaderName != nil && strings.TrimSpace(*r.AuthHeaderName) != "")
}

// patchScheme is the scheme the row will carry after the patch: an explicit
// auth_scheme wins, otherwise the stored one applies.
func patchScheme(current string, patch providerstore.KeyPatch) string {
	if patch.AuthScheme != nil {
		return strings.TrimSpace(*patch.AuthScheme)
	}
	return current
}

// keyPatch builds the providerstore patch. AllowKeyRef is only ever set for
// owner and admin callers, which is what keeps key_ref out of user writes.
func (r *keyWriteRequest) keyPatch(identity *adminIdentity) providerstore.KeyPatch {
	scheme := r.AuthScheme
	if scheme != nil {
		trimmed := strings.TrimSpace(*scheme)
		scheme = &trimmed
	}
	return providerstore.KeyPatch{
		Enabled:        r.Enabled,
		AuthScheme:     scheme,
		AuthHeaderName: r.AuthHeaderName,
		Secret:         r.secretValue(),
		KeyEnv:         r.KeyEnv,
		KeyRef:         r.KeyRef,
		ClearSecret:    r.ClearSecret,
		AllowKeyRef:    identity.Role == auth.RoleOwner || identity.Role == auth.RoleAdmin,
	}
}

// adminDisableOnly reports whether the body is exactly the one mutation an
// admin may apply to someone else's key.
func (r *keyWriteRequest) adminDisableOnly() bool {
	if r.Enabled == nil || *r.Enabled {
		return false
	}
	return r.Name == nil && r.AuthScheme == nil && r.AuthHeaderName == nil &&
		r.Secret == nil && r.KeyEnv == nil && r.KeyRef == nil && !r.ClearSecret
}

func adminKeyCredentialError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, providerstore.ErrInvalidCredentialSource),
		errors.Is(err, providerstore.ErrClearSecretNotAllowed):
		adminFieldError(c, http.StatusUnprocessableEntity, "credential", strings.TrimPrefix(err.Error(), "providerstore: "))
	case errors.Is(err, providerstore.ErrMissingMasterKey):
		adminAPIError(c, http.StatusInternalServerError, "master_key_missing", "provider key encryption is not configured")
	default:
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "provider key credential write failed")
	}
}

func adminKeyStoreError(c *gin.Context, err error) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, providerstore.ErrDuplicateKeyName):
		adminFieldError(c, http.StatusConflict, "name", "this name is already used in the same owner scope")
	case errors.Is(err, providerstore.ErrDuplicateKeyAlias):
		adminFieldError(c, http.StatusConflict, "alias", "this alias is already used in the same owner scope")
	case errors.Is(err, providerstore.ErrKeyNotFound), errors.Is(err, providerstore.ErrProviderNotFound):
		adminNotFoundResponse(c)
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		adminFieldError(c, http.StatusUnprocessableEntity, "owner_id", "owner does not exist")
	default:
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "provider key write failed")
	}
}

func decodeKeyRequest(c *gin.Context, dst any) bool {
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20))
	if err := decoder.Decode(dst); err != nil {
		adminAPIError(c, http.StatusBadRequest, "invalid_request", "request body must be a JSON object")
		return false
	}
	return true
}

// keyJSON is the only key representation an API returns: metadata plus secret
// presence, never ciphertext or plaintext.
func keyJSON(record *providerstore.KeyRecord) gin.H {
	var ownerID any
	if record.OwnerID != "" {
		ownerID = record.OwnerID
	}
	return gin.H{
		"id":               record.ID,
		"provider_id":      record.ProviderID,
		"owner_id":         ownerID,
		"name":             record.Name,
		"enabled":          record.Enabled,
		"auth_scheme":      record.AuthScheme,
		"auth_header_name": record.AuthHeaderName,
		"has_secret":       record.HasSecret(),
		"secret_last4":     record.SecretLast4,
		"has_key_env":      record.KeyEnv != "",
		"has_key_ref":      record.KeyRef != "",
		"key_env":          record.KeyEnv,
		"key_ref":          record.KeyRef,
		"created_at":       record.CreatedAt,
		"updated_at":       record.UpdatedAt,
	}
}

func keyListJSON(records []providerstore.KeyRecord) gin.H {
	keys := make([]gin.H, 0, len(records))
	for i := range records {
		keys = append(keys, keyJSON(&records[i]))
	}
	return gin.H{"keys": keys}
}

// loadKey resolves the :id/:keyID path pair and enforces row-level reach. A
// caller with no reach gets the same 404 a missing key gets, so key ids cannot
// be probed across subjects.
func loadKey(c *gin.Context, q providerstore.Querier, identity *adminIdentity, forUpdate bool) (providerstore.KeyRecord, keyReach, bool) {
	providerID := strings.TrimSpace(c.Param("id"))
	keyID := strings.TrimSpace(c.Param("keyID"))
	record, err := providerstore.GetProviderKey(c.Request.Context(), q, providerID, keyID, forUpdate)
	if err != nil {
		if errors.Is(err, providerstore.ErrKeyNotFound) || isInvalidUUID(err) {
			adminNotFoundResponse(c)
			return providerstore.KeyRecord{}, keyReachNone, false
		}
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider key failed")
		return providerstore.KeyRecord{}, keyReachNone, false
	}
	reach := keyReachFor(identity, &record.KeyRow)
	if reach == keyReachNone {
		adminNotFoundResponse(c)
		return providerstore.KeyRecord{}, keyReachNone, false
	}
	return record, reach, true
}

func handleAdminKeyList(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		providerID := strings.TrimSpace(c.Param("id"))
		ctx := c.Request.Context()
		if exists, err := providerstore.ProviderExists(ctx, state.pool, providerID); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
			return
		} else if !exists {
			adminNotFoundResponse(c)
			return
		}
		ownerScope := ""
		if identity != nil && identity.Role == auth.RoleUser {
			ownerScope = identity.UserID
		}
		records, err := providerstore.ListProviderKeys(ctx, state.pool, providerID, ownerScope)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "list provider keys failed")
			return
		}
		visible := make([]providerstore.KeyRecord, 0, len(records))
		for i := range records {
			if keyReachFor(identity, &records[i].KeyRow) != keyReachNone {
				visible = append(visible, records[i])
			}
		}
		c.JSON(http.StatusOK, keyListJSON(visible))
	}
}

func handleAdminKeyGet(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		record, _, ok := loadKey(c, state.pool, adminIdentityOf(c), false)
		if !ok {
			return
		}
		c.JSON(http.StatusOK, keyJSON(&record))
	}
}

func handleAdminKeyCreate(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		ctx := c.Request.Context()
		providerID := strings.TrimSpace(c.Param("id"))
		var body keyWriteRequest
		if !decodeKeyRequest(c, &body) {
			return
		}
		if exists, err := providerstore.ProviderExists(ctx, state.pool, providerID); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
			return
		} else if !exists {
			adminNotFoundResponse(c)
			return
		}
		name := ""
		if body.Name != nil {
			name = strings.TrimSpace(*body.Name)
		}
		if name == "" || len(name) > keyNameMaxLength {
			adminFieldError(c, http.StatusBadRequest, "name", "name must be 1-100 characters")
			return
		}
		ownerID, ok := createKeyOwner(c, identity, &body)
		if !ok {
			return
		}
		scheme := providerstore.SchemeNone
		switch {
		case body.AuthScheme != nil:
			scheme = strings.TrimSpace(*body.AuthScheme)
		case body.AuthHeaderName != nil && strings.TrimSpace(*body.AuthHeaderName) != "":
			adminFieldError(c, http.StatusBadRequest, "auth_scheme", "auth_scheme is required with auth_header_name")
			return
		case body.carriesCredential():
			// A credential without a scheme is the common bearer case.
			scheme = providerstore.SchemeBearer
		}
		if noneSchemeRejectsCredential(scheme, &body) {
			adminFieldError(c, http.StatusUnprocessableEntity, "credential", "auth_scheme none does not accept a credential")
			return
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		keyID, err := providerstore.NewKeyID(ctx, state.pool)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "allocate provider key id failed")
			return
		}
		patch := body.keyPatch(identity)
		patch.Enabled, patch.AuthScheme = &enabled, &scheme
		row, err := providerstore.ApplyKeyPatch(providerstore.KeyRow{
			ID:         keyID,
			ProviderID: providerID,
			OwnerID:    ownerID,
			Name:       name,
			Enabled:    enabled,
			AuthScheme: scheme,
		}, patch, state.keyring)
		if err != nil {
			adminKeyCredentialError(c, err)
			return
		}

		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "begin key transaction failed")
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort rollback after an aborted write
		record, err := providerstore.InsertProviderKey(ctx, tx, row)
		if err != nil {
			adminKeyStoreError(c, err)
			return
		}
		details := map[string]any{
			"name":        record.Name,
			"auth_scheme": record.AuthScheme,
			"enabled":     record.Enabled,
			"has_secret":  record.HasSecret(),
			"has_key_env": record.KeyEnv != "",
			"has_key_ref": record.KeyRef != "",
		}
		if err := writeKeyAudit(ctx, tx, identity, "key.create", record.ID, record.OwnerID, record.ID, record.ProviderID, c.ClientIP(), details); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "write key audit failed")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "commit key transaction failed")
			return
		}
		c.JSON(http.StatusCreated, keyJSON(&record))
	}
}

// createKeyOwner resolves owner_id without widening a user's reach: a user may
// only create keys for themselves, and only the owner creates shared keys.
func createKeyOwner(c *gin.Context, identity *adminIdentity, body *keyWriteRequest) (string, bool) {
	present, shared, id, err := body.ownerScope()
	if err != nil {
		adminFieldError(c, http.StatusBadRequest, "owner_id", err.Error())
		return "", false
	}
	if !present {
		return identity.UserID, true
	}
	if shared {
		if identity.Role != auth.RoleOwner {
			adminAPIError(c, http.StatusForbidden, "forbidden", "only the owner may create shared keys")
			return "", false
		}
		return "", true
	}
	if identity.Role == auth.RoleOwner || sameUserID(id, identity.UserID) {
		return id, true
	}
	adminAPIError(c, http.StatusForbidden, "forbidden", "a key may only be created for yourself")
	return "", false
}

// sameUserID compares two UUID spellings. Postgres and pgtype both accept
// upper-case and dashless forms, so a raw string compare would reject a caller
// passing their own id in a different (but equivalent) notation.
func sameUserID(a, b string) bool {
	var left, right pgtype.UUID
	if left.Scan(a) != nil || right.Scan(b) != nil {
		return a == b
	}
	return left.Bytes == right.Bytes
}

func handleAdminKeyUpdate(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		ctx := c.Request.Context()
		var body keyWriteRequest
		if !decodeKeyRequest(c, &body) {
			return
		}
		if len(body.OwnerID) > 0 {
			adminFieldError(c, http.StatusBadRequest, "owner_id", "ownership cannot be changed")
			return
		}
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "begin key transaction failed")
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort rollback after an aborted write
		record, reach, ok := loadKey(c, tx, identity, true)
		if !ok {
			return
		}
		if reach != keyReachFull && !body.adminDisableOnly() {
			adminAPIError(c, http.StatusForbidden, "forbidden", "this key belongs to another user")
			return
		}
		patch := body.keyPatch(identity)
		if noneSchemeRejectsCredential(patchScheme(record.AuthScheme, patch), &body) {
			adminFieldError(c, http.StatusUnprocessableEntity, "credential", "auth_scheme none does not accept a credential")
			return
		}
		next, err := providerstore.ApplyKeyPatch(record.KeyRow, patch, state.keyring)
		if err != nil {
			adminKeyCredentialError(c, err)
			return
		}
		if body.Name != nil {
			name := strings.TrimSpace(*body.Name)
			if name == "" || len(name) > keyNameMaxLength {
				adminFieldError(c, http.StatusBadRequest, "name", "name must be 1-100 characters")
				return
			}
			next.Name = name
		}
		updated, err := providerstore.UpdateProviderKey(ctx, tx, next)
		if err != nil {
			adminKeyStoreError(c, err)
			return
		}
		details := map[string]any{
			"name":        updated.Name,
			"auth_scheme": updated.AuthScheme,
			"enabled":     updated.Enabled,
			"has_secret":  updated.HasSecret(),
			"has_key_env": updated.KeyEnv != "",
			"has_key_ref": updated.KeyRef != "",
		}
		if err := writeKeyAudit(ctx, tx, identity, "key.update", updated.ID, updated.OwnerID, updated.ID, updated.ProviderID, c.ClientIP(), details); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "write key audit failed")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "commit key transaction failed")
			return
		}
		c.JSON(http.StatusOK, keyJSON(&updated))
	}
}

func handleAdminKeyDelete(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		ctx := c.Request.Context()
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "begin key transaction failed")
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort rollback after an aborted write
		record, reach, ok := loadKey(c, tx, identity, true)
		if !ok {
			return
		}
		if reach != keyReachFull {
			adminAPIError(c, http.StatusForbidden, "forbidden", "this key belongs to another user")
			return
		}
		details := map[string]any{
			"name":        record.Name,
			"auth_scheme": record.AuthScheme,
		}
		if err := writeKeyAudit(ctx, tx, identity, "key.delete", record.ID, record.OwnerID, record.ID, record.ProviderID, c.ClientIP(), details); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "write key audit failed")
			return
		}
		if err := providerstore.DeleteProviderKey(ctx, tx, record.ProviderID, record.ID); err != nil {
			adminKeyStoreError(c, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "commit key transaction failed")
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// handleAdminKeyTest probes the upstream with the key's own credential. The
// answer never carries the secret: only a status, the upstream status code and
// a redacted diagnostic.
func handleAdminKeyTest(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		record, reach, ok := loadKey(c, state.pool, adminIdentityOf(c), false)
		if !ok {
			return
		}
		if reach != keyReachFull {
			adminAPIError(c, http.StatusForbidden, "forbidden", "this key belongs to another user")
			return
		}
		if record.AuthScheme == providerstore.SchemeNone {
			adminFieldError(c, http.StatusUnprocessableEntity, "credential", "a key without a credential cannot be tested")
			return
		}
		credential, err := providerstore.ResolveKeyCredential(ctx, record.KeyRow, state.keyring, nil)
		if err != nil {
			// A missing or rotated master key is a server-side failure, not a
			// problem with the caller's key.
			if errors.Is(err, providerstore.ErrMissingMasterKey) {
				adminAPIError(c, http.StatusInternalServerError, "credential_unavailable", "stored key material cannot be decrypted")
				return
			}
			adminFieldError(c, http.StatusUnprocessableEntity, "credential", strings.TrimPrefix(err.Error(), "providerstore: "))
			return
		}
		providerRow, err := providerstore.GetProviderRow(ctx, state.pool, record.ProviderID)
		if err != nil {
			if errors.Is(err, providerstore.ErrProviderNotFound) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
			return
		}
		definition, err := providerstore.BuildProviderDefinition(providerRow)
		if err != nil {
			adminFieldError(c, http.StatusUnprocessableEntity, "provider", "provider configuration is invalid")
			return
		}
		c.JSON(http.StatusOK, probeKeyCredential(ctx, &definition, &record.KeyRow, credential))
	}
}

// probeKeyCredential sends the resolved credential upstream. Every scheme goes
// through the provider connection tester so URL policy, proxying, timeouts and
// redaction stay identical to the provider-level test; basic auth travels as
// an Authorization header because the tester only speaks bearer and api-key.
func probeKeyCredential(ctx context.Context, definition *gatewayprovider.Provider, key *providerstore.KeyRow, credential providerstore.ResolvedCredential) ConnectionTestResult {
	switch key.AuthScheme {
	case providerstore.SchemeBearer:
		definition.Auth = gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: credential.Raw}
	case providerstore.SchemeHeader:
		definition.Auth = gatewayprovider.Auth{
			Scheme:     gatewayprovider.AuthAPIKey,
			Key:        credential.Raw,
			HeaderName: key.AuthHeaderName,
		}
	case providerstore.SchemeBasic:
		token := base64.StdEncoding.EncodeToString([]byte(credential.Basic.Username + ":" + credential.Basic.Password))
		definition.Auth = gatewayprovider.Auth{
			Scheme:     gatewayprovider.AuthAPIKey,
			Key:        "Basic " + token,
			HeaderName: "Authorization",
		}
	default:
		return ConnectionTestResult{OK: false, Status: "error", Message: "unsupported auth scheme"}
	}
	return testProviderConnection(ctx, definition)
}

func handleAdminKeyModelList(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		record, _, ok := loadKey(c, state.pool, adminIdentityOf(c), false)
		if !ok {
			return
		}
		models, err := providerstore.ListKeyModels(c.Request.Context(), state.pool, record.ID)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "list key models failed")
			return
		}
		c.JSON(http.StatusOK, gin.H{"models": keyModelsJSON(models)})
	}
}

type keyModelRequest struct {
	Alias            string   `json:"alias"`
	UpstreamModel    string   `json:"upstream_model"`
	Hidden           bool     `json:"hidden"`
	Fallbacks        []string `json:"fallbacks"`
	FallbackStatuses []int    `json:"fallback_statuses"`
	AllowShadow      bool     `json:"allow_shadow"`
	LegacyPosition   int      `json:"legacy_position"`
}

type keyModelsRequest struct {
	Models []keyModelRequest `json:"models"`
}

func keyModelsJSON(models []providerstore.KeyModel) []gin.H {
	out := make([]gin.H, 0, len(models))
	for _, model := range models {
		fallbacks := model.Fallbacks
		if fallbacks == nil {
			fallbacks = []string{}
		}
		statuses := model.FallbackStatuses
		if statuses == nil {
			statuses = []int{}
		}
		out = append(out, gin.H{
			"alias":             model.Alias,
			"upstream_model":    model.UpstreamModel,
			"hidden":            model.Hidden,
			"fallbacks":         fallbacks,
			"fallback_statuses": statuses,
			"allow_shadow":      model.AllowShadow,
			"legacy_position":   model.LegacyPosition,
		})
	}
	return out
}

// validate only shapes the request (trim, column lengths and the one
// cross-row rule the canonical validator cannot see). Every semantic rule
// lives in providerstore.ValidateKeyModelRow, which ReplaceKeyModels runs
// before writing.
func (r keyModelsRequest) validate() (models []providerstore.KeyModel, field, problem string) {
	models = make([]providerstore.KeyModel, 0, len(r.Models))
	seen := make(map[string]bool, len(r.Models))
	for _, model := range r.Models {
		alias := strings.TrimSpace(model.Alias)
		if alias == "" || len(alias) > 200 {
			return nil, "alias", "alias must be 1-200 characters"
		}
		if seen[alias] {
			return nil, "alias", "this alias is used twice in the request"
		}
		seen[alias] = true
		upstream := strings.TrimSpace(model.UpstreamModel)
		if len(upstream) > 200 {
			return nil, "upstream_model", "upstream_model must be at most 200 characters"
		}
		models = append(models, providerstore.KeyModel{
			Alias:            alias,
			UpstreamModel:    upstream,
			Hidden:           model.Hidden,
			Fallbacks:        model.Fallbacks,
			FallbackStatuses: model.FallbackStatuses,
			AllowShadow:      model.AllowShadow,
			LegacyPosition:   model.LegacyPosition,
		})
	}
	return models, "", ""
}

func handleAdminKeyModelReplace(state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		var body keyModelsRequest
		if !decodeKeyRequest(c, &body) {
			return
		}
		models, field, problem := body.validate()
		if field != "" {
			adminFieldError(c, http.StatusBadRequest, field, problem)
			return
		}
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "begin key transaction failed")
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort rollback after an aborted write
		record, reach, ok := loadKey(c, tx, adminIdentityOf(c), true)
		if !ok {
			return
		}
		if reach != keyReachFull {
			adminAPIError(c, http.StatusForbidden, "forbidden", "this key belongs to another user")
			return
		}
		if err := providerstore.ReplaceKeyModels(ctx, tx, record.KeyRow, models); err != nil {
			if errors.Is(err, providerstore.ErrInvalidConfig) {
				adminFieldError(c, http.StatusUnprocessableEntity, "models", strings.TrimPrefix(err.Error(), "providerstore: "))
				return
			}
			adminKeyStoreError(c, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "commit key transaction failed")
			return
		}
		c.JSON(http.StatusOK, gin.H{"models": keyModelsJSON(models)})
	}
}
