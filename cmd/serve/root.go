package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/minhhhduc/gnvipi/internal/auth"
)

const (
	// rootPasswordEnv overrides the seeded root password. A password set here is
	// taken as final: the account is not forced to change it.
	rootPasswordEnv = "GNVIPI_ROOT_PASSWORD"
	// rootEmailEnv optionally sets the built-in root account email on first seed
	// (or backfills it when the root account has no email yet).
	rootEmailEnv = "GNVIPI_ROOT_EMAIL"
	// defaultRootPassword is the documented first-sign-in credential for the
	// built-in root account. Seeding it sets must_change_password, so the
	// console answers nothing else until a real password is chosen.
	defaultRootPassword = "gnvipi"
)

// ensureRootAccount seeds the built-in root account so every database-mode
// install keeps an identity that user management cannot delete, rename, demote
// or disable. An account that already exists is left alone apart from a role
// promotion, so a password the operator set survives restarts.
func ensureRootAccount(ctx context.Context, state *authState) error {
	password := os.Getenv(rootPasswordEnv)
	mustChange := strings.TrimSpace(password) == ""
	if mustChange {
		password = defaultRootPassword
	}
	user, created, err := auth.EnsureRoot(ctx, state.pool, auth.RootInput{
		Username:           auth.RootUsername,
		Email:              os.Getenv(rootEmailEnv),
		Password:           password,
		MustChangePassword: mustChange,
	}, state.password)
	if err != nil {
		return fmt.Errorf("root account: %w", err)
	}
	if created {
		log.Printf("auth: seeded built-in root account %q", user.Username)
	}
	// The warning follows the stored hash, not the seed path, so a root reset
	// back to the default is reported too.
	if state.password.Params.Memory != 0 {
		ok, _, err := auth.VerifyPassword(defaultRootPassword, user.PasswordHash, state.password.Params, state.password.Pepper)
		if err == nil && ok {
			log.Printf("auth: built-in root account %q still uses the default password; sign in and change it", user.Username)
		}
	}
	return nil
}
