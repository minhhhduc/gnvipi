package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/minhhhduc/gnvipi/internal/auth"
	"github.com/minhhhduc/gnvipi/internal/db"
	"github.com/minhhhduc/gnvipi/migrations"
)

type createOwnerOptions struct {
	Username         string
	Email            string
	DisplayName      string
	WithGatewayToken bool
}

// parseCreateOwnerArgs keeps flag handling separate from database work so
// argument errors are testable without PostgreSQL. The password is never a
// flag; it is read from stdin by runCreateOwnerCommand.
func parseCreateOwnerArgs(args []string) (createOwnerOptions, error) {
	var opts createOwnerOptions
	flags := flag.NewFlagSet("admin create-owner", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.Username, "username", "", "owner username")
	flags.StringVar(&opts.Email, "email", "", "owner email")
	flags.StringVar(&opts.DisplayName, "display-name", "", "owner display name")
	flags.BoolVar(&opts.WithGatewayToken, "with-gateway-token", false, "print a one-time gateway token for the owner")
	if err := flags.Parse(args); err != nil {
		return opts, errors.New("serve admin create-owner: invalid arguments")
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("serve admin create-owner accepts no positional arguments (got %d)", flags.NArg())
	}
	opts.Username = strings.TrimSpace(opts.Username)
	opts.Email = strings.TrimSpace(opts.Email)
	opts.DisplayName = strings.TrimSpace(opts.DisplayName)
	if opts.Username == "" {
		return opts, errors.New("serve admin create-owner requires --username <name>")
	}
	if utf8.RuneCountInString(opts.DisplayName) > 200 {
		return opts, errors.New("serve admin create-owner: --display-name must be at most 200 characters")
	}
	return opts, nil
}

// parseResetPasswordArgs accepts exactly one target username.
func parseResetPasswordArgs(args []string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("serve admin reset-password requires exactly one <username> argument (got %d)", len(args))
	}
	username := strings.TrimSpace(args[0])
	if username == "" {
		return "", errors.New("serve admin reset-password requires a username")
	}
	return username, nil
}

// readAdminPassword reads one line from stdin. It is never printed or logged.
// The run commands refuse a terminal so the password is not echoed; pipe it in
// (for example: printf '%s\n' "$GNVIPI_OWNER_PASSWORD" | serve admin create-owner ...).
func readAdminPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 8192)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", errors.New("a password is required on stdin")
	}
	return password, nil
}

// adminPasswordInput refuses a terminal stdin: without an external dependency
// there is no portable way to disable terminal echo, so an interactive password
// would be visible on screen.
//
// MSYS2/mintty (Git Bash) exposes its pseudo-terminal as a named pipe rather
// than a character device, so the terminal check never fires there. A pty pipe
// cannot be told apart from a real pipeline without a terminal API, so on
// Windows any non-console stdin from an MSYS2 shell is refused: typing is safe
// but piping must go through cmd.exe or PowerShell, where MSYSTEM is unset.
func adminPasswordInput(stdin io.Reader) (io.Reader, error) {
	file, ok := stdin.(*os.File)
	if !ok {
		return stdin, nil
	}
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect password input: %w", err)
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return nil, errors.New("refusing to read the password from a terminal (it would be echoed); pipe it on stdin instead")
	}
	if runtime.GOOS == "windows" && info.Mode()&os.ModeNamedPipe != 0 && os.Getenv("MSYSTEM") != "" {
		return nil, errors.New("refusing to read the password from an MSYS2/mintty pipe (pty input is indistinguishable from a pipeline there and would be echoed); run the command from cmd.exe or PowerShell and pipe the password in")
	}
	return stdin, nil
}

func runCreateOwnerCommand(ctx context.Context, args []string, lookup func(string) (string, bool), stdin io.Reader, stdout io.Writer) error {
	opts, err := parseCreateOwnerArgs(args)
	if err != nil {
		return err
	}
	stdin, err = adminPasswordInput(stdin)
	if err != nil {
		return err
	}
	password, err := readAdminPassword(stdin)
	if err != nil {
		return err
	}
	cfg, err := loadPasswordHashConfig(lookup)
	if err != nil {
		return fmt.Errorf("password hashing configuration: %w", err)
	}
	pool, err := openAdminPool(ctx, lookup)
	if err != nil {
		return err
	}
	defer pool.Close()
	return createOwner(ctx, pool, opts, password, &cfg, stdout)
}

func runResetPasswordCommand(ctx context.Context, args []string, lookup func(string) (string, bool), stdin io.Reader) error {
	username, err := parseResetPasswordArgs(args)
	if err != nil {
		return err
	}
	stdin, err = adminPasswordInput(stdin)
	if err != nil {
		return err
	}
	password, err := readAdminPassword(stdin)
	if err != nil {
		return err
	}
	cfg, err := loadPasswordHashConfig(lookup)
	if err != nil {
		return fmt.Errorf("password hashing configuration: %w", err)
	}
	pool, err := openAdminPool(ctx, lookup)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := resetPasswordByUsername(ctx, pool, username, password, &cfg); err != nil {
		return err
	}
	log.Printf("password reset for user %q; sessions and tokens revoked", username)
	return nil
}

// openAdminPool connects with the runtime application role and refuses an
// unmigrated or half-migrated schema, matching the server's startup guard.
func openAdminPool(ctx context.Context, lookup func(string) (string, bool)) (*pgxpool.Pool, error) {
	cfg, err := db.LoadConfigFrom(lookup)
	if err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database connect: %w", err)
	}
	latest, err := db.LatestVersion(migrations.FS)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if err := db.CheckSchema(ctx, pool, latest); err != nil {
		pool.Close()
		return nil, databaseSchemaError(err)
	}
	return pool, nil
}

// createOwner creates the first owner and, when requested, a gateway token in
// the same transaction. Nothing is printed until the transaction commits, so a
// failed token write leaves no half-bootstrapped database.
func createOwner(ctx context.Context, pool *pgxpool.Pool, opts createOwnerOptions, password string, cfg *auth.BootstrapConfig, stdout io.Writer) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // best-effort rollback after an aborted write

	owner, err := auth.CreateFirstOwnerTx(ctx, tx, auth.CreateOwnerInput{
		Username:    opts.Username,
		Email:       opts.Email,
		DisplayName: opts.DisplayName,
		Password:    password,
	}, *cfg)
	if err != nil {
		return err
	}
	var raw string
	if opts.WithGatewayToken {
		raw, err = createGatewayToken(ctx, tx, owner.ID)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "owner %q created (id %s)\n", owner.Username, owner.ID); err != nil {
		return stdoutWriteError(owner, opts.WithGatewayToken, err)
	}
	if opts.WithGatewayToken {
		if _, err := fmt.Fprintf(stdout, "gateway token (shown once): %s\n", raw); err != nil {
			return stdoutWriteError(owner, true, err)
		}
	}
	return nil
}

// stdoutWriteError reports a post-commit output failure. The owner and any
// gateway token are already committed, and the plaintext token exists nowhere
// else, so the operator must be told exactly what survived and how to recover
// instead of seeing a generic write error.
func stdoutWriteError(owner *auth.User, withGatewayToken bool, err error) error {
	recovery := fmt.Sprintf("recover access with `serve admin reset-password %s`", owner.Username)
	if withGatewayToken {
		return fmt.Errorf("owner %q was created (id %s) but the one-time gateway token could not be written to stdout and is unrecoverable; %s or sign in to the admin console and mint a new token: %w", owner.Username, owner.ID, recovery, err)
	}
	return fmt.Errorf("owner %q was created (id %s) but the confirmation could not be written to stdout; %s: %w", owner.Username, owner.ID, recovery, err)
}

// createGatewayToken writes the token row and its audit entry in the caller's
// transaction, so a token never exists without a matching audit row.
func createGatewayToken(ctx context.Context, q auth.Querier, userID string) (string, error) {
	raw, token, err := auth.CreateAPIToken(ctx, q, auth.CreateAPITokenInput{
		UserID: userID,
		Kind:   auth.TokenKindGateway,
		Name:   "bootstrap",
		Expiry: auth.TokenExpiry{},
		Now:    time.Now(),
	})
	if err != nil {
		return "", err
	}
	if err := auth.WriteAudit(ctx, q, auth.AuditEntry{
		ActorType:  "system",
		Action:     "token.create",
		TargetType: "token",
		TargetID:   token.ID,
		UserID:     userID,
		Details: map[string]any{
			"user_id": userID,
			"kind":    string(token.Kind),
			"name":    token.Name,
		},
	}); err != nil {
		return "", err
	}
	return raw, nil
}

func resetPasswordByUsername(ctx context.Context, pool *pgxpool.Pool, username, password string, cfg *auth.BootstrapConfig) error {
	user, err := auth.GetUserByUsername(ctx, pool, username)
	if err != nil {
		return err
	}
	return auth.ResetPassword(ctx, pool, user.ID, password, *cfg)
}
