package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/minhhhduc/gnvipi/internal/auth"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

const (
	keyNameMaxLength = 100
	keyProbeTimeout  = 10 * time.Second
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

func keyReachFor(identity *adminIdentity, key providerstore.KeyRow) keyReach {
	if identity == nil {
		return keyReachNone
	}
	if identity.Role == auth.RoleOwner {
		return keyReachFull
	}
	if key.OwnerID != "" && key.OwnerID == identity.UserID {
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
func (r keyWriteRequest) ownerScope() (present, shared bool, id string, err error) {
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
	return true, false, id, nil
}

func (r keyWriteRequest) secretValue() []byte {
	if r.Secret == nil || *r.Secret == "" {
		return nil
	}
	return []byte(*r.Secret)
}

func (r keyWriteRequest) carriesCredential() bool {
	return len(r.secretValue()) > 0 ||
		(r.KeyEnv != nil && *r.KeyEnv != "") ||
		(r.KeyRef != nil && *r.KeyRef != "") ||
		(r.AuthHeaderName != nil && *r.AuthHeaderName != "")
}

// keyPatch builds the providerstore patch. AllowKeyRef is only ever set for
// owner and admin callers, which is what keeps key_ref out of user writes.
func (r keyWriteRequest) keyPatch(identity *adminIdentity) providerstore.KeyPatch {
	return providerstore.KeyPatch{
		Enabled:        r.Enabled,
		AuthScheme:     r.AuthScheme,
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
func (r keyWriteRequest) adminDisableOnly() bool {
	if r.Enabled == nil || *r.Enabled {
		return false
	}
	return r.Name == nil && r.AuthScheme == nil && r.AuthHeaderName == nil &&
		r.Secret == nil && r.KeyEnv == nil && r.KeyRef == nil && !r.ClearSecret
}

// adminKeyFieldError answers with the shared validation envelope and lists the
// offending field once in every shape the console and tests read.
func adminKeyFieldError(c *gin.Context, status int, field, problem string) {
	fields := []gin.H{{"field": field, "problem": problem}}
	c.JSON(status, gin.H{
		"error":        problem,
		"field":        field,
		"fields":       map[string]string{field: problem},
		"errors":       fields,
		"field_errors": fields,
	})
}

func adminKeyCredentialError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, providerstore.ErrInvalidCredentialSource),
		errors.Is(err, providerstore.ErrClearSecretNotAllowed):
		adminKeyFieldError(c, http.StatusUnprocessableEntity, "credential", strings.TrimPrefix(err.Error(), "providerstore: "))
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
		adminKeyFieldError(c, http.StatusConflict, "name", "this name is already used in the same owner scope")
	case errors.Is(err, providerstore.ErrDuplicateKeyAlias):
		adminKeyFieldError(c, http.StatusConflict, "alias", "this alias is already used in the same owner scope")
	case errors.Is(err, providerstore.ErrKeyNotFound), errors.Is(err, providerstore.ErrProviderNotFound):
		adminNotFoundResponse(c)
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		adminKeyFieldError(c, http.StatusUnprocessableEntity, "owner_id", "owner does not exist")
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
func keyJSON(record providerstore.KeyRecord) gin.H {
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
	for _, record := range records {
		keys = append(keys, keyJSON(record))
	}
	return gin.H{"keys": keys}
}

func writeKeyAudit(ctx context.Context, q auth.Querier, identity *adminIdentity, action string, key providerstore.KeyRow, ip string, details map[string]any) error {
	entry := adminAuditActor(identity)
	entry.Action, entry.TargetType, entry.TargetID = action, "provider_key", key.ID
	entry.UserID, entry.KeyID, entry.ProviderID = key.OwnerID, key.ID, key.ProviderID
	entry.IP = ip
	entry.Details = details
	return auth.WriteAudit(ctx, q, entry)
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
	reach := keyReachFor(identity, record.KeyRow)
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
		if _, err := providerstore.GetProviderRow(ctx, state.pool, providerID); err != nil {
			if errors.Is(err, providerstore.ErrProviderNotFound) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
			return
		}
		records, err := providerstore.ListProviderKeys(ctx, state.pool, providerID)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "list provider keys failed")
			return
		}
		visible := make([]providerstore.KeyRecord, 0, len(records))
		for _, record := range records {
			if keyReachFor(identity, record.KeyRow) != keyReachNone {
				visible = append(visible, record)
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
		c.JSON(http.StatusOK, keyJSON(record))
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
		if _, err := providerstore.GetProviderRow(ctx, state.pool, providerID); err != nil {
			if errors.Is(err, providerstore.ErrProviderNotFound) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "read provider failed")
			return
		}
		name := ""
		if body.Name != nil {
			name = strings.TrimSpace(*body.Name)
		}
		if name == "" || len(name) > keyNameMaxLength {
			adminKeyFieldError(c, http.StatusBadRequest, "name", "name must be 1-100 characters")
			return
		}
		ownerID, ok := createKeyOwner(c, identity, body)
		if !ok {
			return
		}
		scheme := providerstore.SchemeNone
		switch {
		case body.AuthScheme != nil:
			scheme = strings.TrimSpace(*body.AuthScheme)
		case body.carriesCredential():
			scheme = providerstore.SchemeBearer
		}
		if scheme == providerstore.SchemeNone && body.carriesCredential() {
			adminKeyFieldError(c, http.StatusUnprocessableEntity, "credential", "auth_scheme none does not accept a credential")
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
		defer func() { _ = tx.Rollback(context.Background()) }()
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
		if err := writeKeyAudit(ctx, tx, identity, "key.create", record.KeyRow, c.ClientIP(), details); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "write key audit failed")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "commit key transaction failed")
			return
		}
		c.JSON(http.StatusCreated, keyJSON(record))
	}
}

// createKeyOwner resolves owner_id without widening a user's reach: a user may
// only create keys for themselves, and only the owner creates shared keys.
func createKeyOwner(c *gin.Context, identity *adminIdentity, body keyWriteRequest) (string, bool) {
	present, shared, id, err := body.ownerScope()
	if err != nil {
		adminKeyFieldError(c, http.StatusBadRequest, "owner_id", err.Error())
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
	if identity.Role == auth.RoleOwner || id == identity.UserID {
		return id, true
	}
	adminAPIError(c, http.StatusForbidden, "forbidden", "a key may only be created for yourself")
	return "", false
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
			adminKeyFieldError(c, http.StatusBadRequest, "owner_id", "ownership cannot be changed")
			return
		}
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "begin key transaction failed")
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		record, reach, ok := loadKey(c, tx, identity, true)
		if !ok {
			return
		}
		if reach != keyReachFull && !body.adminDisableOnly() {
			adminAPIError(c, http.StatusForbidden, "forbidden", "this key belongs to another user")
			return
		}
		next, err := providerstore.ApplyKeyPatch(record.KeyRow, body.keyPatch(identity), state.keyring)
		if err != nil {
			adminKeyCredentialError(c, err)
			return
		}
		if body.Name != nil {
			name := strings.TrimSpace(*body.Name)
			if name == "" || len(name) > keyNameMaxLength {
				adminKeyFieldError(c, http.StatusBadRequest, "name", "name must be 1-100 characters")
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
		if err := writeKeyAudit(ctx, tx, identity, "key.update", updated.KeyRow, c.ClientIP(), details); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "write key audit failed")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "commit key transaction failed")
			return
		}
		c.JSON(http.StatusOK, keyJSON(updated))
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
		defer func() { _ = tx.Rollback(context.Background()) }()
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
		if err := writeKeyAudit(ctx, tx, identity, "key.delete", record.KeyRow, c.ClientIP(), details); err != nil {
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
			adminKeyFieldError(c, http.StatusUnprocessableEntity, "credential", "a key without a credential cannot be tested")
			return
		}
		credential, err := providerstore.ResolveKeyCredential(ctx, record.KeyRow, state.keyring, nil)
		if err != nil {
			adminKeyFieldError(c, http.StatusUnprocessableEntity, "credential", strings.TrimPrefix(err.Error(), "providerstore: "))
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
			adminKeyFieldError(c, http.StatusUnprocessableEntity, "provider", "provider configuration is invalid")
			return
		}
		c.JSON(http.StatusOK, probeKeyCredential(ctx, definition, record.KeyRow, credential))
	}
}

// probeKeyCredential sends the resolved credential upstream. Bearer and header
// keys reuse the provider connection tester; basic auth needs its own request
// because the tester expresses credentials only as provider-level bearer or
// api-key headers.
func probeKeyCredential(ctx context.Context, definition gatewayprovider.Provider, key providerstore.KeyRow, credential providerstore.ResolvedCredential) ConnectionTestResult {
	switch key.AuthScheme {
	case providerstore.SchemeBearer:
		definition.Auth = gatewayprovider.Auth{Scheme: gatewayprovider.AuthBearer, Key: credential.Raw}
		return testProviderConnection(ctx, &definition)
	case providerstore.SchemeHeader:
		definition.Auth = gatewayprovider.Auth{
			Scheme:     gatewayprovider.AuthAPIKey,
			Key:        credential.Raw,
			HeaderName: key.AuthHeaderName,
		}
		return testProviderConnection(ctx, &definition)
	case providerstore.SchemeBasic:
		return probeBasicCredential(ctx, definition.BaseURL, credential.Basic)
	default:
		return ConnectionTestResult{OK: false, Status: "error", Message: "unsupported auth scheme"}
	}
}

func probeBasicCredential(ctx context.Context, baseURL string, basic providerstore.BasicSecret) ConnectionTestResult {
	token := base64.StdEncoding.EncodeToString([]byte(basic.Username + ":" + basic.Password))
	base := strings.TrimRight(baseURL, "/")
	probeURL := base + "/models"
	if !strings.HasSuffix(base, "/v1") {
		probeURL = base + "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, http.NoBody)
	if err != nil {
		return ConnectionTestResult{OK: false, Status: "error", Error: "build probe request failed"}
	}
	req.Header.Set("Authorization", "Basic "+token)
	client := &http.Client{Timeout: keyProbeTimeout}
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return ConnectionTestResult{
			OK:        false,
			Status:    "connect_failure",
			LatencyMs: latency,
			Error:     redact.Default.TextWithSecrets(err.Error(), basic.Password, token),
		}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	result := ConnectionTestResult{StatusCode: resp.StatusCode, LatencyMs: latency}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.OK, result.Status, result.Message = true, "success", "Connection successful"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		result.Status, result.Message = "auth_failure", fmt.Sprintf("upstream rejected the credential (HTTP %d)", resp.StatusCode)
	default:
		result.Status, result.Message = "error", fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
	}
	if snippet := strings.TrimSpace(string(body)); snippet != "" {
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		result.Error = redact.Default.TextWithSecrets(snippet, basic.Password, token)
	}
	return result
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

func (r keyModelsRequest) validate() ([]providerstore.KeyModel, string, string) {
	models := make([]providerstore.KeyModel, 0, len(r.Models))
	seen := make(map[string]bool, len(r.Models))
	for _, model := range r.Models {
		alias := strings.TrimSpace(model.Alias)
		if alias == "" || len(alias) > 200 {
			return nil, "alias", "alias must be 1-200 characters"
		}
		if strings.Contains(alias, "*") && (alias != "/*" && !strings.HasSuffix(alias, "/*")) {
			return nil, "alias", "a wildcard alias must end in /*"
		}
		if alias == "/*" {
			return nil, "alias", "/* is not a valid alias"
		}
		if seen[alias] {
			return nil, "alias", "this alias is used twice in the request"
		}
		seen[alias] = true
		upstream := strings.TrimSpace(model.UpstreamModel)
		if len(upstream) > 200 {
			return nil, "upstream_model", "upstream_model must be at most 200 characters"
		}
		if upstream == "" && !strings.HasSuffix(alias, "/*") {
			return nil, "upstream_model", "upstream_model is required unless the alias ends in /*"
		}
		if len(model.Fallbacks) > 16 {
			return nil, "fallbacks", "at most 16 fallbacks are allowed"
		}
		for _, fallback := range model.Fallbacks {
			if fallback == alias {
				return nil, "fallbacks", "a fallback cannot name the alias itself"
			}
		}
		if len(model.FallbackStatuses) > 32 {
			return nil, "fallback_statuses", "at most 32 fallback statuses are allowed"
		}
		if model.LegacyPosition < 0 {
			return nil, "legacy_position", "legacy_position must not be negative"
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
			adminKeyFieldError(c, http.StatusBadRequest, field, problem)
			return
		}
		tx, err := state.pool.Begin(ctx)
		if err != nil {
			adminAPIError(c, http.StatusInternalServerError, "internal_error", "begin key transaction failed")
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		record, reach, ok := loadKey(c, tx, adminIdentityOf(c), true)
		if !ok {
			return
		}
		if reach != keyReachFull {
			adminAPIError(c, http.StatusForbidden, "forbidden", "this key belongs to another user")
			return
		}
		if err := providerstore.ReplaceKeyModels(ctx, tx, record.KeyRow, models); err != nil {
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
