package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/db"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
	"github.com/minhhhduc/gnvipi/migrations"
)

const defaultV2File = "internal/models/playground_models.json"

func runImportV2Command(ctx context.Context, args []string, lookup func(string) (string, bool)) error {
	flags := flag.NewFlagSet("admin import-v2", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	owner := flags.String("owner", "", "owner username for credentialed provider keys")
	file := flags.String("file", defaultV2File, "v2 models file")
	force := flags.Bool("force", false, "replace a previous import from a different source")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("serve admin import-v2: invalid arguments")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve admin import-v2 accepts no positional arguments (got %d)", flags.NArg())
	}
	*owner = strings.TrimSpace(*owner)
	if *owner == "" {
		return errors.New("serve admin import-v2 requires --owner <username>")
	}
	source, err := readV2Source(*file)
	if err != nil {
		return fmt.Errorf("v2 source: %w", err)
	}
	// v2 providers may reference credentials in the adjacent secrets map; the
	// import resolves and encrypts them so database keys stay self-contained.
	secrets, err := gatewayprovider.LoadSecrets(gatewayprovider.DefaultSecretsPath(*file))
	if err != nil {
		return fmt.Errorf("v2 secrets: %w", err)
	}
	cfg, err := db.LoadConfigFrom(lookup)
	if err != nil {
		return fmt.Errorf("database configuration: %w", err)
	}
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("database connect: %w", err)
	}
	defer pool.Close()
	latest, err := db.LatestVersion(migrations.FS)
	if err != nil {
		return err
	}
	if err := db.CheckSchema(ctx, pool, latest); err != nil {
		return databaseSchemaError(err)
	}
	getenv := func(key string) string {
		value, _ := lookup(key)
		return value
	}
	keyring, err := providerstore.KeyringFromEnv(getenv)
	if err != nil {
		return fmt.Errorf("provider key encryption: %w", err)
	}
	imported, err := providerstore.ImportV2(ctx, pool, source, *owner, *force, keyring, providerstore.MapRefResolver(secrets))
	if err != nil {
		return fmt.Errorf("v2 import: %w", err)
	}
	if imported {
		log.Print("database: v2 providers imported")
	} else {
		log.Print("database: v2 provider source already imported; no changes")
	}
	return nil
}

// readV2Source creates the one-time backup before decoding or importing bytes.
// An existing backup is never opened for writing.
func readV2Source(path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("--file path is required")
	}
	backup := path + ".v2.bak"
	created, err := createV2Backup(path, backup)
	if err != nil {
		return nil, err
	}
	if created {
		return os.ReadFile(backup)
	}
	return os.ReadFile(path)
}

func createV2Backup(sourcePath, backupPath string) (created bool, err error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return false, fmt.Errorf("open source: %w", err)
	}
	defer source.Close() //nolint:errcheck // read-only handle

	backup, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create backup: %w", err)
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		if closeErr := backup.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			err = errors.Join(err, fmt.Errorf("close incomplete backup: %w", closeErr))
		}
		if removeErr := os.Remove(backupPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove incomplete backup: %w", removeErr))
		}
	}()
	if _, err := io.Copy(backup, source); err != nil {
		return false, fmt.Errorf("copy backup: %w", err)
	}
	if err := backup.Sync(); err != nil {
		return false, fmt.Errorf("sync backup: %w", err)
	}
	if err := backup.Close(); err != nil {
		return false, fmt.Errorf("close backup: %w", err)
	}
	if err := os.Chmod(backupPath, 0o400); err != nil {
		return false, fmt.Errorf("make backup read-only: %w", err)
	}
	complete = true
	return true, nil
}
