// Package migrations embeds the PostgreSQL schema migrations so the binary can
// apply and verify them without shipping SQL files alongside it.
package migrations

import "embed"

// FS holds the versioned *.up.sql / *.down.sql migration files.
//
//go:embed *.sql
var FS embed.FS
