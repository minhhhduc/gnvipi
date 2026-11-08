package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// MaskSecret returns a redacted hint of a secret, e.g. "sk-...abcd" or "[REDACTED]".
func MaskSecret(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if key == "[REDACTED]" || strings.EqualFold(key, "unchanged") {
		return "[REDACTED]"
	}
	if strings.HasPrefix(key, "sk-...") {
		return key
	}
	if strings.HasPrefix(key, "sk-") && len(key) >= 8 {
		return "sk-..." + key[len(key)-4:]
	}
	if len(key) >= 8 {
		return key[:3] + "..." + key[len(key)-4:]
	}
	return "[REDACTED]"
}

// IsRedactedKey reports whether key is a placeholder or redacted hint.
func IsRedactedKey(key string) bool {
	key = strings.TrimSpace(key)
	return key == "" || key == "[REDACTED]" || strings.EqualFold(key, "unchanged") || strings.Contains(key, "...")
}

// DefaultSecretsPath returns the default secrets file path located in the same directory as modelsPath.
func DefaultSecretsPath(modelsPath string) string {
	if modelsPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(modelsPath), "secrets.json")
}

func checkPOSIXPermissions(perm fs.FileMode) error {
	if perm.Perm()&0o077 != 0 {
		return fmt.Errorf("permissions are too open (%04o); should be 0600", perm.Perm())
	}
	return nil
}

// CheckSecretsFilePermissions returns an error warning if path permissions on POSIX are too open (>0600).
// On Windows or if the file does not exist, it returns nil.
func CheckSecretsFilePermissions(path string) error {
	if path == "" || runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := checkPOSIXPermissions(info.Mode()); err != nil {
		return fmt.Errorf("secrets file %s %w", path, err)
	}
	return nil
}

// LoadSecrets reads the secrets map from path. If the file is missing or path is empty,
// an empty map is returned without error. If permissions on POSIX are too open, a warning is logged.
func LoadSecrets(path string) (map[string]string, error) {
	if path == "" {
		return make(map[string]string), nil
	}
	if warn := CheckSecretsFilePermissions(path); warn != nil {
		log.Printf("warning: %v", warn)
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, err
	}
	var flat map[string]string
	if err := json.Unmarshal(raw, &flat); err == nil && flat != nil {
		return flat, nil
	}
	var nested struct {
		Secrets   map[string]string         `json:"secrets"`
		Providers map[string]map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil {
		res := make(map[string]string)
		for k, v := range nested.Secrets {
			res[k] = v
		}
		for id, fields := range nested.Providers {
			if k, ok := fields["key"].(string); ok {
				res[id] = k
			} else if k, ok := fields["api_key"].(string); ok {
				res[id] = k
			}
		}
		if len(res) > 0 {
			return res, nil
		}
	}
	return make(map[string]string), nil
}

// WriteSecrets atomically writes the secrets map to path with restricted permissions (0600).
func WriteSecrets(path string, secrets map[string]string) (err error) {
	if path == "" {
		return nil
	}
	if secrets == nil {
		secrets = map[string]string{}
	}
	raw, err := json.MarshalIndent(secrets, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".gnvipi-secrets-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		if removeErr := os.Remove(name); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	err = file.Close()
	file = nil
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

// WriteDocumentWithSecrets saves provider secrets to secretsPath (if set) and writes the catalog
// document to modelsPath without literal secrets. Existing secrets in secretsPath are updated,
// and legacy inline keys in doc.Providers are migrated to secrets.
func WriteDocumentWithSecrets(modelsPath, secretsPath string, doc Document) error {
	if modelsPath == "" {
		return nil
	}
	if secretsPath == "" {
		secretsPath = DefaultSecretsPath(modelsPath)
	}

	existingSecrets, err := LoadSecrets(secretsPath)
	if err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}
	secretsMap := make(map[string]string, len(existingSecrets)+len(doc.Providers))
	for k, v := range existingSecrets {
		secretsMap[k] = v
	}

	activeRefs := make(map[string]bool, len(doc.Providers))
	for i := range doc.Providers {
		p := &doc.Providers[i]
		if p.Auth.Scheme == "" || p.Auth.Scheme == AuthNone || p.Auth.KeyEnv != "" {
			continue
		}
		if p.Auth.KeyRef == "" {
			p.Auth.KeyRef = p.ID
		}
		ref := p.Auth.KeyRef
		activeRefs[ref] = true
		if p.Auth.Key != "" && !IsRedactedKey(p.Auth.Key) {
			secretsMap[ref] = p.Auth.Key
		}
	}

	for k := range secretsMap {
		if !activeRefs[k] {
			delete(secretsMap, k)
		}
	}

	if secretsPath != "" && (len(secretsMap) > 0 || len(existingSecrets) > 0) {
		if err := WriteSecrets(secretsPath, secretsMap); err != nil {
			return fmt.Errorf("write secrets: %w", err)
		}
	}

	catalogDoc := cloneDocument(doc)
	for i := range catalogDoc.Providers {
		p := &catalogDoc.Providers[i]
		if p.Auth.Scheme != "" && p.Auth.Scheme != AuthNone && p.Auth.KeyEnv == "" {
			p.Auth.Key = ""
		}
	}

	return writeCatalogFile(modelsPath, catalogDoc)
}

func writeCatalogFile(path string, doc Document) (err error) {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".gnvipi-providers-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		if removeErr := os.Remove(name); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	err = file.Close()
	file = nil
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

// LoadDocumentWithSecrets loads the catalog document and populates credentials from secretsPath.
func LoadDocumentWithSecrets(modelsPath, secretsPath string) (Document, error) {
	if modelsPath == "" {
		return Document{Providers: []Provider{}, Fields: map[string]json.RawMessage{}}, nil
	}
	if secretsPath == "" {
		secretsPath = DefaultSecretsPath(modelsPath)
	}
	raw, err := os.ReadFile(modelsPath)
	if errors.Is(err, os.ErrNotExist) {
		return Document{Providers: []Provider{}, Fields: map[string]json.RawMessage{}}, nil
	}
	if err != nil {
		return Document{}, err
	}
	doc, err := DecodeDocument(raw)
	if err != nil {
		return Document{}, err
	}
	if secretsPath != "" {
		secrets, err := LoadSecrets(secretsPath)
		if err == nil && len(secrets) > 0 {
			for i := range doc.Providers {
				p := &doc.Providers[i]
				if p.Auth.Scheme == "" || p.Auth.Scheme == AuthNone || p.Auth.KeyEnv != "" {
					continue
				}
				ref := p.Auth.KeyRef
				if ref == "" {
					ref = p.ID
				}
				if secret, found := secrets[ref]; found && secret != "" {
					p.Auth.Key = secret
				}
				if p.Auth.KeyRef == "" {
					p.Auth.KeyRef = ref
				}
			}
		}
	}
	return doc, nil
}
