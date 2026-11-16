package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
)

// routingNotConfigured is the typed "nothing to route to" error: the caller has
// no saved setting and the global default is empty.
type routingNotConfigured struct {
	Setting string
}

func (e *routingNotConfigured) Error() string {
	return e.Setting + " is not configured for this account"
}

// userRoutingPrefs holds per-account automode/cycle preferences (issue #240).
// Database mode constructs one; file mode leaves p.userRouting nil and keeps
// the single global setting.
type userRoutingPrefs struct {
	pool   *pgxpool.Pool
	router *providerstore.Router
	prefs  *modelPrefs

	mu      sync.Mutex
	cursors map[string]int
	// ponytail: cursors live in this process; a multi-instance deployment
	// would advance independently. Move the cursor into the table when the
	// gateway runs more than one serving replica.
}

func newUserRoutingPrefs(pool *pgxpool.Pool, router *providerstore.Router, prefs *modelPrefs) *userRoutingPrefs {
	return &userRoutingPrefs{pool: pool, router: router, prefs: prefs, cursors: map[string]int{}}
}

// load returns the account's own row; a missing row is empty, so resolution
// falls through to the global default.
func (u *userRoutingPrefs) load(ctx context.Context, userID string) (auto string, models []string, err error) {
	var raw []byte
	err = u.pool.QueryRow(ctx,
		`SELECT auto_model, cycle_models FROM user_routing_prefs WHERE user_id = $1::uuid`,
		userID).Scan(&auto, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &models); err != nil {
			return "", nil, fmt.Errorf("routing preferences: %w", err)
		}
	}
	return auto, models, nil
}

// automodeTarget resolves the caller's own target, then the global default,
// then a typed not-configured error.
func (u *userRoutingPrefs) automodeTarget(ctx context.Context, userID string) (string, error) {
	auto, _, err := u.load(ctx, userID)
	if err != nil {
		return "", err
	}
	if auto != "" {
		return auto, nil
	}
	if global := u.prefs.autoModelID(); global != "" {
		return global, nil
	}
	return "", &routingNotConfigured{Setting: "automode"}
}

// cycleTargets resolves the caller's own list, then the global default, then a
// typed not-configured error.
func (u *userRoutingPrefs) cycleTargets(ctx context.Context, userID string) ([]string, error) {
	_, models, err := u.load(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(models) > 0 {
		return models, nil
	}
	if cfg, _ := u.prefs.cycleSnapshot(); len(cfg.Models) > 0 {
		return cfg.Models, nil
	}
	return nil, &routingNotConfigured{Setting: "cycle"}
}

// validateTarget rejects aliases this account cannot reach. The key-aware
// router selects personal candidates for the key owner and shared `none` keys only,
// so an alias that exists solely in another account's key is ErrKeyRequired.
func (u *userRoutingPrefs) validateTarget(ctx context.Context, userID, target string) error {
	if u.router == nil {
		return errors.New("routing is not available")
	}
	_, err := u.router.Resolve(ctx, userID, target, "")
	if err == nil {
		return nil
	}
	// Protected NVIDIA models use the shared captcha pool rather than a
	// per-account key, so they are reachable without a key row.
	if protectedModel(target) {
		return nil
	}
	return err
}

// cycleStart returns the caller's own rotation position.
func (u *userRoutingPrefs) cycleStart(userID string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cursors[userID]
}

// advanceCycle moves only the failed caller's cursor.
func (u *userRoutingPrefs) advanceCycle(userID string, models []string, failed string) {
	if len(models) == 0 {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if models[u.cursors[userID]%len(models)] == failed {
		u.cursors[userID] = (u.cursors[userID] + 1) % len(models)
	}
}

func (u *userRoutingPrefs) resetCycle(userID string) {
	u.mu.Lock()
	u.cursors[userID] = 0
	u.mu.Unlock()
}

// setAutomode persists one account's target and audits the change in the same
// transaction.
func (u *userRoutingPrefs) setAutomode(ctx context.Context, identity *adminIdentity, subject, model string) error {
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_routing_prefs (user_id, auto_model)
		VALUES ($1::uuid, $2)
		ON CONFLICT (user_id) DO UPDATE SET auto_model = EXCLUDED.auto_model`,
		subject, model); err != nil {
		return err
	}
	if err := writeAdminAudit(ctx, tx, identity, "routing.automode.update", "user", subject, subject,
		map[string]any{"model": model}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// setCycle persists one account's rotation list and audits it in the same
// transaction. Details carry the list length and the joined ids, never a key.
func (u *userRoutingPrefs) setCycle(ctx context.Context, identity *adminIdentity, subject string, models []string) error {
	raw, err := json.Marshal(models)
	if err != nil {
		return err
	}
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_routing_prefs (user_id, cycle_models)
		VALUES ($1::uuid, $2::jsonb)
		ON CONFLICT (user_id) DO UPDATE SET cycle_models = EXCLUDED.cycle_models`,
		subject, string(raw)); err != nil {
		return err
	}
	if err := writeAdminAudit(ctx, tx, identity, "routing.cycle.update", "user", subject, subject,
		map[string]any{"count": len(models), "models": strings.Join(models, ",")}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// routingTargetUser resolves the account whose settings a request addresses.
// Only a superuser may name another account; everyone else gets the shared 404
// for a foreign id, exactly as cross-subject admin access does.
func routingTargetUser(c *gin.Context, identity *adminIdentity, requested string) (string, bool) {
	if identity == nil {
		adminAPIError(c, http.StatusUnauthorized, "authentication_required", "sign in to the admin console")
		return "", false
	}
	requested = strings.TrimSpace(requested)
	if !identity.Role.AtLeast(auth.RoleSuperuser) {
		if requested != "" && !sameUserID(requested, identity.UserID) {
			adminNotFoundResponse(c)
			return "", false
		}
		return identity.UserID, true
	}
	if requested == "" || sameUserID(requested, identity.UserID) {
		return identity.UserID, true
	}
	return requested, true
}

func userRoutingAvailable(c *gin.Context, p *modelPrefs) bool {
	if p.userRouting != nil {
		return true
	}
	adminAPIError(c, http.StatusServiceUnavailable, "routing_unavailable", "per-account routing is not available")
	return false
}

// handleAdminAPIGetAutomodeDB reports the effective target for the addressed
// account: its own setting first, else the global default.
func handleAdminAPIGetAutomodeDB(p *modelPrefs) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !userRoutingAvailable(c, p) {
			return
		}
		subject, ok := routingTargetUser(c, adminIdentityOf(c), c.Query("user_id"))
		if !ok {
			return
		}
		auto, _, err := p.userRouting.load(c.Request.Context(), subject)
		if err != nil {
			if isInvalidUUID(err) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusServiceUnavailable, "routing_unavailable", "could not read routing settings")
			return
		}
		model := auto
		if model == "" {
			model = p.autoModelID()
		}
		c.JSON(http.StatusOK, gin.H{
			"model":          model,
			"chat_responses": p.autoModelChatResponsesEnabled(),
			"configured":     model != "",
		})
	}
}

// handleAdminAPIPutAutomodeDB saves the addressed account's target after
// checking that account's keys can reach it.
func handleAdminAPIPutAutomodeDB(p *modelPrefs) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !userRoutingAvailable(c, p) {
			return
		}
		identity := adminIdentityOf(c)
		var payload struct {
			Model         string `json:"model"`
			ChatResponses *bool  `json:"chat_responses,omitempty"`
			UserID        string `json:"user_id,omitempty"`
		}
		if err := c.ShouldBindJSON(&payload); err != nil {
			adminAPIError(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with model")
			return
		}
		subject, ok := routingTargetUser(c, identity, payload.UserID)
		if !ok {
			return
		}
		model := strings.TrimSpace(payload.Model)
		if model == "" || model == routing.VirtualAutoMode {
			adminFieldError(c, http.StatusBadRequest, "model", "choose a model")
			return
		}
		ctx := c.Request.Context()
		if model == routing.VirtualCycle {
			if _, err := p.userRouting.cycleTargets(ctx, subject); err != nil {
				adminFieldError(c, http.StatusBadRequest, "model", "cycle has no models for this account")
				return
			}
		} else if err := p.userRouting.validateTarget(ctx, subject, model); err != nil {
			adminFieldError(c, http.StatusBadRequest, "model", routingTargetProblem(err))
			return
		}
		if err := p.userRouting.setAutomode(ctx, identity, subject, model); err != nil {
			if isInvalidUUID(err) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "routing_unavailable", "could not save the auto model")
			return
		}
		c.JSON(http.StatusOK, gin.H{"model": model, "user_id": subject})
	}
}

// handleAdminAPIGetCycleDB reports the addressed account's effective rotation
// list and its own cursor.
func handleAdminAPIGetCycleDB(p *modelPrefs, catalog []*cliproxy.ModelInfo) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !userRoutingAvailable(c, p) {
			return
		}
		subject, ok := routingTargetUser(c, adminIdentityOf(c), c.Query("user_id"))
		if !ok {
			return
		}
		_, models, err := p.userRouting.load(c.Request.Context(), subject)
		if err != nil {
			if isInvalidUUID(err) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusServiceUnavailable, "routing_unavailable", "could not read routing settings")
			return
		}
		if len(models) == 0 {
			if global, _ := p.cycleSnapshot(); len(global.Models) > 0 {
				models = global.Models
			}
		}
		writeAdminCycleResponse(c, p, catalog, cycleConfig{Name: "cycle", Models: models}, p.userRouting.cycleStart(subject))
	}
}

// handleAdminAPIPutCycleDB validates and saves the addressed account's rotation
// list. Unknown or unreachable aliases are a field error on "models".
func handleAdminAPIPutCycleDB(p *modelPrefs) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !userRoutingAvailable(c, p) {
			return
		}
		identity := adminIdentityOf(c)
		var payload struct {
			Models []string `json:"models"`
			UserID string   `json:"user_id,omitempty"`
		}
		if err := c.ShouldBindJSON(&payload); err != nil {
			adminAPIError(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with models")
			return
		}
		subject, ok := routingTargetUser(c, identity, payload.UserID)
		if !ok {
			return
		}
		models := make([]string, 0, len(payload.Models))
		seen := make(map[string]bool, len(payload.Models))
		for _, raw := range payload.Models {
			model := strings.TrimSpace(raw)
			switch {
			case model == "":
				adminFieldError(c, http.StatusBadRequest, "models", "models must not contain empty entries")
				return
			case model == routing.VirtualCycle || model == routing.VirtualAutoMode:
				adminFieldError(c, http.StatusBadRequest, "models", "models must not contain virtual aliases")
				return
			case seen[model]:
				adminFieldError(c, http.StatusBadRequest, "models", "models must be unique")
				return
			}
			seen[model] = true
			if err := p.userRouting.validateTarget(c.Request.Context(), subject, model); err != nil {
				adminFieldError(c, http.StatusBadRequest, "models", routingTargetProblem(err))
				return
			}
			models = append(models, model)
		}
		if err := p.userRouting.setCycle(c.Request.Context(), identity, subject, models); err != nil {
			if isInvalidUUID(err) {
				adminNotFoundResponse(c)
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "routing_unavailable", "could not save the cycle")
			return
		}
		p.userRouting.resetCycle(subject)
		writeAdminCycleResponse(c, p, nil, cycleConfig{Name: "cycle", Models: models}, 0)
	}
}

// routingTargetProblem keeps the key-aware error text (alias and provider ids
// only) in the field error, without credential material.
func routingTargetProblem(err error) string {
	return strings.TrimPrefix(err.Error(), "providerstore: ")
}
