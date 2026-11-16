package main

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/minhhhduc/gnvipi/internal/models"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

// dbSettingsStore is the database-mode owner of the settings the models file
// holds in file mode: the model mask, the deletion set and the search
// template. Nothing here reads or writes a JSON file.
type dbSettingsStore struct {
	pool *pgxpool.Pool
	// runtime republishes the routing snapshot after a committed write, so the
	// next request observes the new mask without waiting for the poll.
	runtime *providerRuntime
}

// loadStateDB replaces the in-memory masks, deletions and search template with
// the rows committed in PostgreSQL.
func (p *modelPrefs) loadStateDB(ctx context.Context) error {
	if p.dbSettings == nil {
		return nil
	}
	hidden, deleted, err := providerstore.LoadModelState(ctx, p.dbSettings.pool)
	if err != nil {
		return err
	}
	var setting struct {
		URL string `json:"url"`
	}
	if _, err := providerstore.LoadSettingObject(ctx, p.dbSettings.pool, "search", &setting); err != nil {
		return err
	}
	p.mu.Lock()
	p.hidden, p.deleted = map[string]struct{}{}, map[string]struct{}{}
	for _, id := range hidden {
		p.hidden[id] = struct{}{}
	}
	for _, id := range deleted {
		p.deleted[id] = struct{}{}
	}
	p.searchURL = setting.URL
	if p.cycle == nil {
		p.cycle = &cycleConfig{Name: "cycle"}
	}
	p.mu.Unlock()
	log.Printf("database settings: %d model(s) off, %d deleted, search %s",
		len(hidden), len(deleted), searchState(setting.URL))
	return nil
}

func searchState(url string) string {
	if url == "" {
		return "unset"
	}
	return "configured"
}

// save commits one mask/deleted/search snapshot and audits it in the same
// transaction. The config-version triggers publish the change to every process.
func (s *dbSettingsStore) save(ctx context.Context, identity *adminIdentity, hidden, deleted []string, searchURL string) error {
	actor := ""
	target := ""
	if identity != nil {
		actor, target = identity.UserID, identity.UserID
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("model state: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write
	if err := providerstore.ReplaceModelState(ctx, tx, hidden, deleted, actor); err != nil {
		return err
	}
	if err := providerstore.PutSetting(ctx, tx, "search", map[string]string{"url": searchURL}, actor); err != nil {
		return err
	}
	if identity != nil {
		if err := writeAdminAudit(ctx, tx, identity, "model.state.update", "models", target, target,
			map[string]any{"hidden": len(hidden), "deleted": len(deleted), "search": searchURL != ""}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("model state: %w", err)
	}
	s.reload(ctx)
	return nil
}

// reload publishes a fresh snapshot now; the watcher would publish it on the
// next notification, and a failed reload here leaves the last good snapshot in
// place rather than failing the committed write.
func (s *dbSettingsStore) reload(ctx context.Context) {
	if s.runtime == nil || s.runtime.store == nil {
		return
	}
	reloadCtx, cancel := context.WithTimeout(ctx, providerstore.ReloadTimeout)
	defer cancel()
	if err := s.runtime.store.Reload(reloadCtx); err != nil {
		log.Printf("provider snapshot: reload after settings write: %v", err)
	}
}

// commitPrefsDB applies a mask/deleted/search edit to PostgreSQL and then to
// the in-memory copies. It requires changeMu, exactly like commitPrefs.
func (p *modelPrefs) commitPrefsDB(identity *adminIdentity, edit func(*prefsFile) error, providersChanged bool) error {
	if providersChanged {
		return fmt.Errorf("provider configuration is not file-backed in database mode")
	}
	p.mu.Lock()
	file := p.prefsFileLocked()
	if err := edit(&file); err != nil {
		p.mu.Unlock()
		return err
	}
	if err := p.dbSettings.save(context.Background(), identity, file.MasksID, file.DeletedID, file.SearchURL); err != nil {
		p.mu.Unlock()
		return err
	}
	p.applySettingsLocked(&file)
	maskNotify := p.onMaskChange
	p.mu.Unlock()
	if maskNotify != nil {
		maskNotify()
	}
	return nil
}

// prefsFileLocked snapshots the database-owned settings as a prefsFile so file
// and database commits share one edit closure shape. Providers, automode and
// cycle are absent: PostgreSQL and the per-account rows own those.
func (p *modelPrefs) prefsFileLocked() prefsFile {
	file := prefsFile{
		SearchURL: p.searchURL,
		MasksID:   make([]string, 0, len(p.hidden)),
		DeletedID: make([]string, 0, len(p.deleted)),
	}
	for id := range p.hidden {
		file.MasksID = append(file.MasksID, id)
	}
	for id := range p.deleted {
		file.DeletedID = append(file.DeletedID, id)
	}
	return file
}

// applySettingsLocked publishes mask, deletions and search template. It leaves
// providers, automode and cycle untouched because this write did not change
// them, and a mask edit must not reset an unrelated cycle position.
func (p *modelPrefs) applySettingsLocked(file *prefsFile) {
	p.hidden, p.deleted = map[string]struct{}{}, map[string]struct{}{}
	for _, id := range file.MasksID {
		p.hidden[id] = struct{}{}
	}
	for _, id := range file.DeletedID {
		p.deleted[id] = struct{}{}
	}
	p.searchURL = file.SearchURL
}

// applySnapshotDB publishes the NVIDIA catalog from a committed configuration
// snapshot so every running instance stays in sync with remote rescans.
func (p *modelPrefs) applySnapshotDB(snapshot *providerstore.Snapshot) {
	if p == nil || snapshot == nil {
		return
	}
	if p.catalogRead != nil {
		models.Replace(snapshot.NvidiaCatalog())
	}
}
