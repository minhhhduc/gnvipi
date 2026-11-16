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

// update reads the latest committed mask/deleted/search state inside a
// serialized transaction, applies edit, commits, and audits in the same
// transaction. Reading from PostgreSQL rather than process-local memory ensures
// sequential or concurrent writes on different instances never clobber
// unrelated settings.
func (s *dbSettingsStore) update(ctx context.Context, identity *adminIdentity, edit func(*prefsFile) error) (prefsFile, error) {
	actor := ""
	target := ""
	if identity != nil {
		actor, target = identity.UserID, identity.UserID
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return prefsFile{}, fmt.Errorf("model state: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('gnvipi:settings'))`); err != nil {
		return prefsFile{}, fmt.Errorf("model state: %w", err)
	}
	hidden, deleted, err := providerstore.LoadModelState(ctx, tx)
	if err != nil {
		return prefsFile{}, err
	}
	var setting struct {
		URL string `json:"url"`
	}
	if _, err := providerstore.LoadSettingObject(ctx, tx, "search", &setting); err != nil {
		return prefsFile{}, err
	}
	file := prefsFile{
		MasksID:   hidden,
		DeletedID: deleted,
		SearchURL: setting.URL,
	}
	if err := edit(&file); err != nil {
		return prefsFile{}, err
	}
	if err := providerstore.ReplaceModelState(ctx, tx, file.MasksID, file.DeletedID, actor); err != nil {
		return prefsFile{}, err
	}
	if err := providerstore.PutSetting(ctx, tx, "search", map[string]string{"url": file.SearchURL}, actor); err != nil {
		return prefsFile{}, err
	}
	if identity != nil {
		if err := writeAdminAudit(ctx, tx, identity, "model.state.update", "models", target, target,
			map[string]any{"hidden": len(file.MasksID), "deleted": len(file.DeletedID), "search": file.SearchURL != ""}); err != nil {
			return prefsFile{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return prefsFile{}, fmt.Errorf("model state: %w", err)
	}
	s.reload(ctx)
	return file, nil
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
	file, err := p.dbSettings.update(context.Background(), identity, edit)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.applySettingsLocked(&file)
	maskNotify := p.onMaskChange
	p.mu.Unlock()
	if maskNotify != nil {
		maskNotify()
	}
	return nil
}

// applySnapshotDB publishes the mask, deletion, search settings, and NVIDIA
// catalog from a committed configuration snapshot so every running instance
// stays in sync with remote writes and rescans.
func (p *modelPrefs) applySnapshotDB(snapshot *providerstore.Snapshot) {
	if p == nil || snapshot == nil {
		return
	}
	p.mu.Lock()
	if p.dbSettings != nil {
		hidden, deleted := snapshot.ModelState()
		file := prefsFile{
			MasksID:   hidden,
			DeletedID: deleted,
			SearchURL: snapshot.Settings().Search.URL,
		}
		p.applySettingsLocked(&file)
	}
	catalogRead := p.catalogRead
	maskNotify := p.onMaskChange
	p.mu.Unlock()
	if catalogRead != nil {
		models.Replace(snapshot.NvidiaCatalog())
	}
	if maskNotify != nil {
		maskNotify()
	}
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
