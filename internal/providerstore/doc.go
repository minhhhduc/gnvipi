// Package providerstore implements the DB-backed provider, key and model
// configuration layer described by the 2026-11-12 design: the AES-256-GCM
// secret envelope, credential resolution, configuration snapshot loading and
// publication, sensitive-configuration validation, key-aware routing, and the
// v2 LegacyView bridge.
//
// The package deliberately does not own the database schema or the admin HTTP
// routes. RowSource is the only database dependency; callers wire a pooled
// pgx source and the watcher into the serve command.
package providerstore
