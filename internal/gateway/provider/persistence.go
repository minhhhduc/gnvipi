package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// SchemaVersion is the models-file format written by this package. Writes are
// v2-only: downgrade to a legacy-only gateway requires restoring a backup.
const SchemaVersion = 2

// Document holds providers and the untouched remainder of a models file.
// Fields retains raw JSON, including catalog metadata and unknown extensions.
// Providers is authoritative; providers and schema_version in Fields are ignored.
type Document struct {
	Providers []Provider
	Fields    map[string]json.RawMessage
}

func cloneDocument(doc Document) Document {
	out := Document{Providers: make([]Provider, len(doc.Providers)), Fields: make(map[string]json.RawMessage, len(doc.Fields))}
	for i := range doc.Providers {
		out.Providers[i] = Clone(&doc.Providers[i])
	}
	for key, value := range doc.Fields {
		out.Fields[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

// MarshalJSON writes the v2-only format without losing unrelated JSON fields.
func (d Document) MarshalJSON() ([]byte, error) {
	fields := make(map[string]json.RawMessage, len(d.Fields)+2)
	for key, value := range d.Fields {
		fields[key] = value
	}
	delete(fields, "providers")
	fields["schema_version"] = json.RawMessage("2")
	list := d.Providers
	if list == nil {
		list = []Provider{}
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	fields["providers_v2"] = raw
	return json.Marshal(fields)
}

// DecodeDocument validates a v2 file or migrates a legacy file via FromLegacy.
// Migration is in-memory until the next successful write. An unsupported schema
// or malformed document is an error, never a reason to replace the original file.
func DecodeDocument(raw []byte) (Document, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Document{}, err
	}
	if fields == nil {
		return Document{}, errors.New("models document must be a JSON object")
	}
	version := 0
	if v, present := fields["schema_version"]; present {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return Document{}, errors.New("schema_version must be an integer")
		}
		if err := json.Unmarshal(v, &version); err != nil {
			return Document{}, fmt.Errorf("schema_version: %w", err)
		}
		if version != 1 && version != SchemaVersion {
			return Document{}, fmt.Errorf("unsupported models schema version %d", version)
		}
	}
	doc := Document{Fields: fields}
	if list, present := fields["providers_v2"]; present {
		if version != SchemaVersion {
			return Document{}, errors.New("providers_v2 requires schema_version 2")
		}
		if bytes.Equal(bytes.TrimSpace(list), []byte("null")) {
			return Document{}, errors.New("providers_v2 must be an array")
		}
		if err := json.Unmarshal(list, &doc.Providers); err != nil {
			return Document{}, fmt.Errorf("providers_v2: %w", err)
		}
	} else {
		if version == SchemaVersion {
			return Document{}, errors.New("schema_version 2 requires providers_v2")
		}
		var legacy []LegacyProvider
		if list, present := fields["providers"]; present {
			if err := json.Unmarshal(list, &legacy); err != nil {
				return Document{}, fmt.Errorf("providers: %w", err)
			}
		}
		var err error
		doc.Providers, err = FromLegacy(legacy)
		if err != nil {
			return Document{}, err
		}
	}
	delete(fields, "providers")
	delete(fields, "providers_v2")
	delete(fields, "schema_version")
	if err := validateDocument(&doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func validateDocument(doc *Document) error {
	ids := make(map[string]bool, len(doc.Providers))
	for i := range doc.Providers {
		p := &doc.Providers[i]
		if err := p.Validate(); err != nil {
			return err
		}
		if ids[p.ID] {
			return &FieldError{Provider: p.ID, Field: "id", Problem: "duplicate provider ID"}
		}
		ids[p.ID] = true
	}
	return nil
}

// LoadDocument reads a models file; a missing file starts an empty document.
// Credentials from the adjacent secrets file (if present) are resolved.
func LoadDocument(path string) (Document, error) {
	return LoadDocumentWithSecrets(path, DefaultSecretsPath(path))
}

// WriteDocument replaces the file atomically using a synced, private temporary
// file beside the destination. Secrets are stored in the adjacent secrets store.
// Any error leaves the published store unchanged.
func WriteDocument(path string, doc Document) error {
	return WriteDocumentWithSecrets(path, DefaultSecretsPath(path), doc)
}

// Store serializes candidate validation, disk replacement, publication, and
// notifications. Callbacks may read snapshots, but must not mutate the store.
// Every exported snapshot is defensive, including callback arguments.
type Store struct {
	changeMu    sync.Mutex
	mu          sync.RWMutex
	path        string
	secretsPath string
	doc         Document
	onChange    func([]Provider)
}

// OpenStore loads without modifying the file, rejecting unsafe documents.
func OpenStore(path string, onChange func([]Provider)) (*Store, error) {
	return OpenStoreWithSecrets(path, DefaultSecretsPath(path), onChange)
}

// OpenStoreWithSecrets loads using an explicit secrets file path.
func OpenStoreWithSecrets(path, secretsPath string, onChange func([]Provider)) (*Store, error) {
	doc, err := LoadDocumentWithSecrets(path, secretsPath)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, secretsPath: secretsPath, doc: doc, onChange: onChange}, nil
}

// Snapshot returns the full document, owned by the caller.
func (s *Store) Snapshot() Document {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneDocument(s.doc)
}

// List returns providers in configuration order.
func (s *Store) List() []Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Provider, len(s.doc.Providers))
	for i := range s.doc.Providers {
		out[i] = Clone(&s.doc.Providers[i])
	}
	return out
}

// Get returns a defensive provider copy and whether its ID exists.
func (s *Store) Get(id string) (Provider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.doc.Providers {
		if s.doc.Providers[i].ID == id {
			return Clone(&s.doc.Providers[i]), true
		}
	}
	return Provider{}, false
}

// Update atomically edits providers and/or unrelated document fields. Neither
// a validation error nor an I/O error publishes any part of the candidate.
// The editor must not mutate this store; it may retain its candidate safely.
func (s *Store) Update(edit func(*Document) error) error {
	s.changeMu.Lock()
	defer s.changeMu.Unlock()
	candidate := s.Snapshot()
	if err := edit(&candidate); err != nil {
		return err
	}
	candidate = cloneDocument(candidate)
	if err := validateDocument(&candidate); err != nil {
		return err
	}
	if err := WriteDocumentWithSecrets(s.path, s.secretsPath, candidate); err != nil {
		return err
	}
	s.mu.Lock()
	s.doc = candidate
	s.mu.Unlock()
	if s.onChange != nil {
		s.onChange(s.List())
	}
	return nil
}

// Upsert inserts a provider or replaces the provider with its ID in-place.
// Redacted keys or sentinels will preserve existing credentials.
func (s *Store) Upsert(p *Provider) error {
	if p == nil {
		return &FieldError{Field: "id", Problem: "provider is nil"}
	}
	owned := Clone(p)
	return s.Update(func(doc *Document) error {
		for i := range doc.Providers {
			if doc.Providers[i].ID == owned.ID {
				if owned.Auth.Scheme != "" && owned.Auth.Scheme != AuthNone && owned.Auth.KeyEnv == "" && IsRedactedKey(owned.Auth.Key) && doc.Providers[i].Auth.Key != "" {
					owned.Auth.Key = doc.Providers[i].Auth.Key
					if doc.Providers[i].Auth.KeyRef != "" {
						owned.Auth.KeyRef = doc.Providers[i].Auth.KeyRef
					}
				}
				doc.Providers[i] = owned
				return nil
			}
		}
		doc.Providers = append(doc.Providers, owned)
		return nil
	})
}

// Delete removes an existing provider. Missing IDs are errors and do not notify.
func (s *Store) Delete(id string) error {
	return s.Update(func(doc *Document) error {
		for i := range doc.Providers {
			if doc.Providers[i].ID == id {
				doc.Providers = append(doc.Providers[:i], doc.Providers[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("provider %q not found", id)
	})
}
