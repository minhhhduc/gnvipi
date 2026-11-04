package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/models"
)

func toLegacyProviders(list []customProvider) []gatewayprovider.LegacyProvider {
	out := make([]gatewayprovider.LegacyProvider, len(list))
	for i, p := range list {
		out[i] = gatewayprovider.LegacyProvider{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model, Alias: p.Alias, MessagesOnly: p.MessagesOnly, ResponsesOnly: p.ResponsesOnly}
	}
	return out
}

func fromLegacyProviders(list []gatewayprovider.LegacyProvider) []customProvider {
	out := make([]customProvider, len(list))
	for i, p := range list {
		out[i] = customProvider{Name: p.Name, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model, Alias: p.Alias, MessagesOnly: p.MessagesOnly, ResponsesOnly: p.ResponsesOnly}
	}
	return out
}

func decodePrefsDocument(doc gatewayprovider.Document) (prefsFile, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return prefsFile{}, err
	}
	var file prefsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return prefsFile{}, err
	}
	return file, nil
}

// applyPrefsLocked publishes a document only after persistence succeeds.
func (p *modelPrefs) applyPrefsLocked(file *prefsFile) {
	p.hidden, p.deleted = map[string]struct{}{}, map[string]struct{}{}
	for _, id := range file.MasksID {
		p.hidden[id] = struct{}{}
	}
	for _, id := range file.DeletedID {
		p.deleted[id] = struct{}{}
	}
	p.providers = fromLegacyProviders(gatewayprovider.LegacyView(file.ProvidersV2))
	p.autoModel, p.searchURL = file.AutoModel, file.SearchURL
	var cycleModels []string
	if file.Cycle != nil {
		cycleModels = file.Cycle.Models
	}
	if p.cycle == nil || !slices.Equal(p.cycle.Models, cycleModels) {
		p.cycleIndex = 0
	}
	p.cycle = &cycleConfig{Name: "cycle", Models: append([]string(nil), cycleModels...)}
}

// initialCatalog is used only when no catalog was supplied. Loaded groups and
// _meta remain byte-for-byte JSON data so scraper and extension metadata survive.
func initialCatalog() (groups []ModelGroup, meta map[string]any) {
	byPublisher := map[string][]ModelItem{}
	for id, m := range models.Models {
		publisher, _, _ := strings.Cut(id, "/")
		if publisher == "" {
			publisher = "unknown"
		}
		byPublisher[publisher] = append(byPublisher[publisher], ModelItem{Model: id, Slug: m.Slug, Namespace: m.Namespace, FunctionID: m.FunctionID})
	}
	for publisher, entries := range byPublisher {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Model < entries[j].Model })
		groups = append(groups, ModelGroup{Publisher: publisher, Models: entries})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Publisher < groups[j].Publisher })
	return groups, map[string]any{"count": len(models.Models), "namespaces": []string{models.Namespace}}
}

// commitPrefs requires changeMu. The runtime read lock covers candidate writes
// and publication so failed candidates are never visible to request handlers.
// Store callbacks are nil here; existing gateway callbacks run once, in order,
// after both the full v2 store and the derived legacy view have been published.
func (p *modelPrefs) commitPrefs(edit func(*prefsFile) error, providersChanged bool) error {
	p.mu.Lock()
	if p.loadErr != nil {
		err := p.loadErr
		p.mu.Unlock()
		return err
	}
	if p.providerStore == nil {
		store, err := gatewayprovider.OpenStore(p.path, nil)
		if err != nil {
			p.loadErr = err
			p.mu.Unlock()
			return err
		}
		if _, err := decodePrefsDocument(store.Snapshot()); err != nil {
			p.loadErr = err
			p.mu.Unlock()
			return err
		}
		p.providerStore = store
	}
	var published prefsFile
	err := p.providerStore.Update(func(doc *gatewayprovider.Document) error {
		file, err := decodePrefsDocument(*doc)
		if err != nil {
			return err
		}
		// Runtime masks also support callers constructing an in-memory modelPrefs.
		file.MasksID, file.DeletedID = nil, nil
		for id := range p.hidden {
			file.MasksID = append(file.MasksID, id)
		}
		for id := range p.deleted {
			file.DeletedID = append(file.DeletedID, id)
		}
		file.AutoModel, file.SearchURL = p.autoModel, p.searchURL
		file.Cycle = &cycleConfig{Name: "cycle"}
		if p.cycle != nil {
			file.Cycle.Models = append([]string(nil), p.cycle.Models...)
		}
		if len(file.ProvidersV2) == 0 && len(p.providers) > 0 {
			file.ProvidersV2, err = gatewayprovider.FromLegacy(toLegacyProviders(p.providers))
			if err != nil {
				return err
			}
		}
		if err := edit(&file); err != nil {
			return err
		}
		if err := validateLegacyProviderURLs(fromLegacyProviders(gatewayprovider.LegacyView(file.ProvidersV2))); err != nil {
			return err
		}
		sort.Strings(file.MasksID)
		sort.Strings(file.DeletedID)
		doc.Providers = file.ProvidersV2
		values := map[string]any{"masks_id": file.MasksID, "deleted_id": file.DeletedID, "auto_model": file.AutoModel, "search_url": file.SearchURL, "cycle": file.Cycle}
		if _, exists := doc.Fields["groups"]; !exists {
			groups, meta := initialCatalog()
			values["groups"] = groups
			if _, exists := doc.Fields["_meta"]; !exists {
				values["_meta"] = meta
			}
		}
		for key, value := range values {
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			doc.Fields[key] = raw
		}
		published = file
		return nil
	})
	if err != nil {
		p.mu.Unlock()
		return err
	}
	// Validation may canonicalize headers; use the actual committed providers.
	published.ProvidersV2 = p.providerStore.List()
	p.applyPrefsLocked(&published)
	if err := p.refreshRoutingLocked(); err != nil {
		p.mu.Unlock()
		return err
	}
	notify, maskNotify := p.onChange, p.onMaskChange
	list := append([]customProvider(nil), p.providers...)
	p.mu.Unlock()
	if providersChanged && notify != nil {
		notify(list)
	}
	if maskNotify != nil {
		maskNotify()
	}
	return nil
}

func (p *modelPrefs) listProvidersV2() ([]gatewayprovider.Provider, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.providerStore != nil {
		return p.providerStore.List(), nil
	}
	return gatewayprovider.FromLegacy(toLegacyProviders(p.providers))
}

func (p *modelPrefs) getProviderV2(id string) (gatewayprovider.Provider, bool, error) {
	list, err := p.listProvidersV2()
	if err != nil {
		return gatewayprovider.Provider{}, false, err
	}
	for i := range list {
		if list[i].ID == id {
			return list[i], true, nil
		}
	}
	return gatewayprovider.Provider{}, false, nil
}

func (p *modelPrefs) upsertProviderV2(pv *gatewayprovider.Provider) error {
	if pv == nil {
		return &gatewayprovider.FieldError{Field: "id", Problem: "provider is nil"}
	}
	owned := gatewayprovider.Clone(pv)
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	return p.commitPrefs(func(file *prefsFile) error {
		for i := range file.ProvidersV2 {
			if file.ProvidersV2[i].ID == owned.ID {
				file.ProvidersV2[i] = owned
				return nil
			}
		}
		file.ProvidersV2 = append(file.ProvidersV2, owned)
		return nil
	}, true)
}

func (p *modelPrefs) deleteProviderV2(id string) error {
	p.changeMu.Lock()
	defer p.changeMu.Unlock()
	return p.commitPrefs(func(file *prefsFile) error {
		for i := range file.ProvidersV2 {
			if file.ProvidersV2[i].ID == id {
				file.ProvidersV2 = append(file.ProvidersV2[:i], file.ProvidersV2[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("provider %q not found", id)
	}, true)
}
