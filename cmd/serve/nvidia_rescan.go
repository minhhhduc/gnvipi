package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// discoverNvidiaCatalog is a seam for tests; production reads the live
// Playground catalog.
var discoverNvidiaCatalog = models.DiscoverCatalog

type nvidiaRescanResult struct {
	Added     int       `json:"added"`
	Updated   int       `json:"updated"`
	Removed   int       `json:"removed"`
	ScannedAt time.Time `json:"scanned_at"`
}

// loadStoredNvidiaCatalog publishes the catalog the last rescan committed to
// PostgreSQL. An empty table is an empty catalog: the admin console then
// offers a rescan. File mode keeps the catalog prefs.load read from the saved
// models file.
func (p *modelPrefs) loadStoredNvidiaCatalog(ctx context.Context) error {
	if p.catalogRead == nil {
		return nil
	}
	rows, err := providerstore.LoadNvidiaCatalog(ctx, p.catalogRead)
	if err != nil {
		return fmt.Errorf("load nvidia catalog: %w", err)
	}
	next := make(map[string]models.ModelInfo, len(rows))
	for _, row := range rows {
		next[row.Model] = models.ModelInfo{
			Slug:       row.Slug,
			Namespace:  row.Namespace,
			FunctionID: row.FunctionID,
		}
	}
	models.Replace(next)
	return nil
}

// rescanNvidiaCatalog reconciles the stored catalog with the live upstream
// list. A discovery or validation failure, or a failed transaction, leaves
// both the stored catalog and the in-memory registry untouched.
func (p *modelPrefs) rescanNvidiaCatalog(ctx context.Context) (nvidiaRescanResult, error) {
	groups, err := discoverNvidiaCatalog(ctx)
	if err != nil {
		return nvidiaRescanResult{}, err
	}
	upstream, err := upstreamCatalog(groups)
	if err != nil {
		return nvidiaRescanResult{}, &models.DiscoveryError{Op: "validation", Err: err}
	}

	current := models.Snapshot()
	next := make(map[string]models.ModelInfo, len(upstream))
	result := nvidiaRescanResult{ScannedAt: time.Now().UTC()}
	for id, info := range upstream {
		if old, exists := current[id]; exists {
			if old != info {
				result.Updated++
			}
		} else {
			result.Added++
		}
		next[id] = info
	}
	for id := range current {
		if _, exists := upstream[id]; !exists {
			result.Removed++
		}
	}
	if result.Added == 0 && result.Updated == 0 && result.Removed == 0 {
		return result, nil
	}

	if p.catalogWrite != nil {
		if err := p.storeNvidiaCatalog(ctx, upstream); err != nil {
			return nvidiaRescanResult{}, err
		}
		models.Replace(next)
		p.notifyCatalogChanged()
		return result, nil
	}

	fileGroups := make([]ModelGroup, 0, len(groups))
	for _, group := range groups {
		items := make([]ModelItem, 0, len(group.Models))
		for _, row := range group.Models {
			items = append(items, ModelItem{
				Model:      row.Model,
				Slug:       row.Slug,
				Namespace:  row.Namespace,
				FunctionID: row.FunctionID,
			})
		}
		fileGroups = append(fileGroups, ModelGroup{Publisher: group.Publisher, Models: items})
	}

	p.changeMu.Lock()
	err = p.commitPrefs(func(file *prefsFile) error {
		file.Groups = fileGroups
		file.MasksID = retainKnown(file.MasksID, upstream)
		file.DeletedID = retainKnown(file.DeletedID, upstream)
		return nil
	}, false)
	p.changeMu.Unlock()
	if err != nil {
		return nvidiaRescanResult{}, fmt.Errorf("save nvidia catalog: %w", err)
	}
	models.Replace(next)
	p.notifyCatalogChanged()
	return result, nil
}

// storeNvidiaCatalog replaces the table in one transaction. It returns only
// after commit, so a failure leaves PostgreSQL unchanged.
func (p *modelPrefs) storeNvidiaCatalog(ctx context.Context, upstream map[string]models.ModelInfo) error {
	rows := make([]models.PlaygroundRow, 0, len(upstream))
	for id, info := range upstream {
		rows = append(rows, models.PlaygroundRow{
			Model:      id,
			Slug:       info.Slug,
			Namespace:  info.Namespace,
			FunctionID: info.FunctionID,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })

	tx, err := p.catalogWrite.Begin(ctx)
	if err != nil {
		return fmt.Errorf("save nvidia catalog: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best-effort cleanup after commit or failure

	if err := providerstore.ReplaceNvidiaCatalog(ctx, tx, rows); err != nil {
		return fmt.Errorf("save nvidia catalog: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("save nvidia catalog: %w", err)
	}
	return nil
}

func upstreamCatalog(groups []models.PlaygroundGroup) (map[string]models.ModelInfo, error) {
	out := make(map[string]models.ModelInfo)
	for _, group := range groups {
		for _, row := range group.Models {
			if row.Model == "" || row.Slug == "" || row.Namespace == "" || row.FunctionID == "" {
				return nil, fmt.Errorf("incomplete catalog entry %q", row.Model)
			}
			if _, duplicate := out[row.Model]; duplicate {
				return nil, fmt.Errorf("duplicate catalog entry %q", row.Model)
			}
			out[row.Model] = models.ModelInfo{
				Slug:       row.Slug,
				Namespace:  row.Namespace,
				FunctionID: row.FunctionID,
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("empty catalog")
	}
	return out, nil
}

func retainKnown(ids []string, upstream map[string]models.ModelInfo) []string {
	kept := ids[:0]
	for _, id := range ids {
		if _, exists := upstream[id]; exists {
			kept = append(kept, id)
		}
	}
	return kept
}

func (p *modelPrefs) notifyCatalogChanged() {
	p.mu.RLock()
	notify := p.onMaskChange
	p.mu.RUnlock()
	if notify != nil {
		notify()
	}
}

func handleAdminNvidiaRescan(p *modelPrefs, state *authState) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := p.rescanNvidiaCatalog(c.Request.Context())
		if err != nil {
			var discoveryErr *models.DiscoveryError
			if errors.As(err, &discoveryErr) {
				adminAPIError(c, http.StatusBadGateway, "nvidia_catalog_unavailable",
					"could not read the NVIDIA Playground catalog; the previous catalog is unchanged")
				return
			}
			adminAPIError(c, http.StatusInternalServerError, "nvidia_catalog_write_failed",
				"could not save the NVIDIA catalog")
			return
		}
		if state != nil && state.pool != nil {
			if identity := adminIdentityOf(c); identity != nil {
				if err := writeAdminAudit(c.Request.Context(), state.pool, identity,
					"nvidia.catalog.rescan", "nvidia_catalog", "", identity.UserID, map[string]any{
						"added":   result.Added,
						"updated": result.Updated,
						"removed": result.Removed,
					}); err != nil {
					log.Printf("audit nvidia.catalog.rescan: %v", err)
				}
			}
		}
		c.JSON(http.StatusOK, result)
	}
}
