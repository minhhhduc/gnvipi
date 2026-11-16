package main

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/models"
)

// playgroundModel is one selectable entry. It never carries credentials.
type playgroundModel struct {
	Provider      string `json:"provider"`
	Alias         string `json:"alias"`
	UpstreamModel string `json:"upstream_model"`
	Source        string `json:"source"`
}

// registerAdminPlaygroundRoutes mounts the console playground. The chat route
// re-enters the engine as an in-process gateway call, so the request uses the
// exact same resolver, dispatcher, limits and provider chain as /v1/*.
func registerAdminPlaygroundRoutes(engine *gin.Engine, state *authState, runtime *providerRuntime, prefs *modelPrefs) {
	if state == nil {
		return
	}
	engine.GET("/admin/api/playground/models", handlePlaygroundModels(runtime, prefs))
	engine.POST("/admin/api/playground/chat", handlePlaygroundChat(engine))
}

// playgroundTargetUser resolves the account the console acts as. The session
// identity is authoritative; an owner may select another account, and anyone
// else naming a foreign id gets the same 404 as any other cross-subject access.
func playgroundTargetUser(c *gin.Context, identity *adminIdentity) (string, bool) {
	if identity == nil {
		adminAPIError(c, http.StatusUnauthorized, "authentication_required", "sign in to the admin console")
		return "", false
	}
	requested := strings.TrimSpace(c.Query("user_id"))
	if !identity.Role.AtLeast(auth.RoleOwner) {
		if requested != "" && requested != identity.UserID {
			adminNotFoundResponse(c)
			return "", false
		}
		return identity.UserID, true
	}
	if requested == "" {
		return identity.UserID, true
	}
	return requested, true
}

func handlePlaygroundModels(runtime *providerRuntime, prefs *modelPrefs) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		target, ok := playgroundTargetUser(c, identity)
		if !ok {
			return
		}
		union := identity != nil && identity.Role.AtLeast(auth.RoleOwner) && strings.TrimSpace(c.Query("user_id")) == ""
		out := make([]playgroundModel, 0, 32)
		keyAliases := make(map[string]struct{})
		if runtime != nil {
			if snapshot := runtime.store.Current(); snapshot != nil {
				for _, model := range snapshot.AvailableModels(target, union) {
					entry := playgroundModel{
						Provider:      model.ProviderID,
						Alias:         model.Alias,
						UpstreamModel: model.UpstreamModel,
						Source:        "key",
					}
					// A shared mapping is the operator's configuration: the
					// account selects it by the id the gateway derives, and the
					// alias and upstream model stay out of the response.
					if model.PublicID != "" {
						entry.Alias, entry.UpstreamModel = model.PublicID, ""
					}
					out = append(out, entry)
					keyAliases[entry.Alias] = struct{}{}
				}
			}
		}
		for id := range models.Snapshot() {
			if prefs != nil && prefs.isHidden(id) {
				continue
			}
			if _, exists := keyAliases[id]; exists {
				continue
			}
			out = append(out, playgroundModel{Provider: "nvidia", Alias: id, UpstreamModel: id, Source: "nvidia"})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Provider != out[j].Provider {
				return out[i].Provider < out[j].Provider
			}
			if out[i].Alias != out[j].Alias {
				return out[i].Alias < out[j].Alias
			}
			return out[i].UpstreamModel < out[j].UpstreamModel
		})
		c.JSON(http.StatusOK, gin.H{"models": out, "user_id": target, "union": union})
	}
}

// handlePlaygroundChat forwards the console body through the gateway pipeline.
// The inner request is marked with the resolved user id; gatewayAuth trusts
// that Go context value because an external request cannot set it.
func handlePlaygroundChat(engine *gin.Engine) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := adminIdentityOf(c)
		target, ok := playgroundTargetUser(c, identity)
		if !ok {
			return
		}
		ctx := withInternalGatewayUser(c.Request.Context(), target)
		req := c.Request.Clone(ctx)
		req.URL.Path = "/v1/chat/completions"
		req.RequestURI = ""
		engine.ServeHTTP(c.Writer, req)
	}
}
