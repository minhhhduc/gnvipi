package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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

// providerIDPattern mirrors the providers.id CHECK so a bad slug is a field
// error instead of a raw check-constraint 500.
var providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// registerAdminProviderDBRoutes installs the database-backed v2 provider CRUD.
// Every write runs in one transaction that also carries its audit row; the
// snapshot refreshes from the committed config version bump, never mid-write.
func registerAdminProviderDBRoutes(engine *gin.Engine, state *authState) {
	engine.GET("/admin/api/providers", func(c *gin.Context) { handleDBAdminAPIListProviders(c, state) })
	engine.POST("/admin/api/providers", func(c *gin.Context) { handleDBAdminAPICreateProvider(c, state) })
	engine.GET("/admin/api/providers/:id", func(c *gin.Context) { handleDBAdminAPIGetProvider(c, state) })
	engine.PUT("/admin/api/providers/:id", func(c *gin.Context) { handleDBAdminAPIUpdateProvider(c, state) })
	engine.DELETE("/admin/api/providers/:id", func(c *gin.Context) { handleDBAdminAPIDeleteProvider(c, state) })
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
	records, err := providerstore.ListProviderKeys(ctx, q, row.ID, "")
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
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "create provider failed")
		return
	}
	if len(pv.Models) > 0 {
		if !replaceProviderModels(c, ctx, tx, row.ID, pv.Models) {
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
		adminAPIError(c, http.StatusInternalServerError, "internal_error", "update provider failed")
		return
	}
	// A nil slice means the client omitted models; an explicit list (including
	// []) is a provider-level write and needs exactly one key.
	if pv.Models != nil {
		if !replaceProviderModels(c, ctx, tx, id, pv.Models) {
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

// replaceProviderModels writes a v2 provider-level models list onto the single
// visible key. An empty list on a keyless provider is a no-op; any other
// provider without exactly one key gets the documented 409.
func replaceProviderModels(c *gin.Context, ctx context.Context, tx pgx.Tx, providerID string, models []gatewayprovider.ModelMapping) bool {
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
