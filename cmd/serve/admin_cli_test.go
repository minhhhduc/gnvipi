package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/minhhhduc/gnvipi/internal/auth"
)

const dbBootstrapToken = "bootstrap-token-for-tests"

// cliTestPasswordConfig keeps Argon2 cheap so DB-backed CLI tests stay fast.
func cliTestPasswordConfig() auth.BootstrapConfig {
	return auth.BootstrapConfig{Params: auth.Params{
		Memory: 8192, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	}}
}

// requireCLITestPool installs the production schema into a throwaway schema.
// Locally there is no PostgreSQL, so the test skips; CI's Test (postgres) job
// sets GNVIPI_TEST_DATABASE_URL and runs it for real.
func requireCLITestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("GNVIPI_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("GNVIPI_REQUIRE_DB_TESTS") == "true" {
			t.Fatal("GNVIPI_TEST_DATABASE_URL is required when GNVIPI_REQUIRE_DB_TESTS=true")
		}
		t.Skip("GNVIPI_TEST_DATABASE_URL is not set; skipping DB-backed CLI tests")
	}
	return newAdminTestPool(t, dsn)
}

func TestParseCreateOwnerArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    createOwnerOptions
		wantErr bool
	}{
		{
			name: "all flags",
			args: []string{"--username", " owner ", "--email", "owner@example.com", "--display-name", "Owner", "--with-gateway-token"},
			want: createOwnerOptions{Username: "owner", Email: "owner@example.com", DisplayName: "Owner", WithGatewayToken: true},
		},
		{
			name: "username only",
			args: []string{"--username=owner"},
			want: createOwnerOptions{Username: "owner"},
		},
		{name: "missing username", args: nil, wantErr: true},
		{name: "blank username", args: []string{"--username", "  "}, wantErr: true},
		{name: "unknown flag", args: []string{"--username", "owner", "--password", "nope"}, wantErr: true},
		{name: "positional argument", args: []string{"--username", "owner", "extra"}, wantErr: true},
		{
			name: "display name at limit",
			args: []string{"--username", "owner", "--display-name", strings.Repeat("x", 200)},
			want: createOwnerOptions{Username: "owner", DisplayName: strings.Repeat("x", 200)},
		},
		{
			name:    "display name too long",
			args:    []string{"--username", "owner", "--display-name", strings.Repeat("x", 201)},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCreateOwnerArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCreateOwnerArgs(%v) = %+v, want error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCreateOwnerArgs(%v): %v", tc.args, err)
			}
			if got != tc.want {
				t.Fatalf("parseCreateOwnerArgs(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseResetPasswordArgs(t *testing.T) {
	if got, err := parseResetPasswordArgs([]string{" owner "}); err != nil || got != "owner" {
		t.Fatalf("parseResetPasswordArgs = %q %v, want owner", got, err)
	}
	for _, args := range [][]string{nil, {"one", "two"}, {"  "}} {
		if _, err := parseResetPasswordArgs(args); err == nil {
			t.Fatalf("parseResetPasswordArgs(%v) succeeded, want error", args)
		}
	}
}

func TestReadAdminPassword(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "line", in: "correct horse battery staple\n", want: "correct horse battery staple"},
		{name: "crlf", in: "correct horse battery staple\r\n", want: "correct horse battery staple"},
		{name: "eof without newline", in: "correct horse battery staple", want: "correct horse battery staple"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readAdminPassword(strings.NewReader(tc.in))
			if err != nil || got != tc.want {
				t.Fatalf("readAdminPassword = %q %v, want %q", got, err, tc.want)
			}
		})
	}
	for _, in := range []string{"", "\n", "\r\n"} {
		if _, err := readAdminPassword(strings.NewReader(in)); err == nil {
			t.Fatalf("readAdminPassword(%q) succeeded, want error", in)
		}
	}
}

// MSYS2/mintty reports its pty as a named pipe, not a character device. The
// guard must refuse that input; on other platforms a pipe stays the documented
// safe path.
func TestAdminPasswordInputMSYS2Pipe(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()  //nolint:errcheck // test cleanup
	defer write.Close() //nolint:errcheck // test cleanup

	if runtime.GOOS != "windows" {
		t.Setenv("MSYSTEM", "MINGW64")
		if _, err := adminPasswordInput(read); err != nil {
			t.Fatalf("pipe input refused on %s: %v", runtime.GOOS, err)
		}
		return
	}
	t.Setenv("MSYSTEM", "MINGW64")
	if _, err := adminPasswordInput(read); err == nil {
		t.Fatal("MSYS2 pty-like pipe was accepted; an interactive password would be echoed")
	}
	// cmd.exe/PowerShell piping has no MSYSTEM and remains supported.
	t.Setenv("MSYSTEM", "")
	if _, err := adminPasswordInput(read); err != nil {
		t.Fatalf("console-shell pipe input refused: %v", err)
	}
}

// A mistyped admin subcommand must fail with usage instead of falling through
// to runServer and silently starting the gateway.
func TestRunCLICommandRejectsUnknownAdminSubcommand(t *testing.T) {
	for _, args := range [][]string{{"admin"}, {"admin", "create-ownr"}, {"admin", "--help"}} {
		handled, err := runCLICommand(args, os.LookupEnv, nil, nil)
		if !handled {
			t.Fatalf("runCLICommand(%v) was not handled", args)
		}
		if err == nil || !strings.Contains(err.Error(), "unknown or missing subcommand") {
			t.Fatalf("runCLICommand(%v) error = %v, want unknown subcommand usage", args, err)
		}
	}
	if handled, err := runCLICommand([]string{"--addr", ":8080"}, os.LookupEnv, nil, nil); handled || err != nil {
		t.Fatalf("server flags = handled %v err %v, want unhandled nil", handled, err)
	}
}

func TestCreateOwnerDB(t *testing.T) {
	pool := requireCLITestPool(t)
	ctx := t.Context()
	cfg := cliTestPasswordConfig()
	var out bytes.Buffer

	opts := createOwnerOptions{
		Username:         "owner",
		Email:            "owner@example.com",
		DisplayName:      "Owner",
		WithGatewayToken: true,
	}
	if err := createOwner(ctx, pool, opts, dbOwnerPassword, &cfg, &out); err != nil {
		t.Fatalf("createOwner: %v", err)
	}
	if n, err := auth.CountUsers(ctx, pool); err != nil || n != 1 {
		t.Fatalf("user count = %d, %v; want 1", n, err)
	}
	if strings.Contains(out.String(), dbOwnerPassword) {
		t.Fatal("createOwner wrote the password to stdout")
	}

	// Bootstrap runs exactly once: the second attempt sees the committed owner.
	if err := createOwner(ctx, pool, createOwnerOptions{Username: "second"}, dbOwnerPassword, &cfg, &out); !errors.Is(err, auth.ErrUsersExist) {
		t.Fatalf("second createOwner = %v, want ErrUsersExist", err)
	}

	// The gateway token is printed exactly once and is usable.
	const marker = "gateway token (shown once): "
	tokenLines := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, marker) {
			tokenLines++
		}
	}
	if tokenLines != 1 {
		t.Fatalf("gateway token not printed exactly once: %q", out.String())
	}
	start := strings.Index(out.String(), marker)
	if start < 0 {
		t.Fatalf("gateway token marker missing: %q", out.String())
	}
	raw := strings.Fields(out.String()[start+len(marker):])[0]
	identity, err := auth.ValidateAPIToken(ctx, pool, raw, auth.TokenKindGateway, time.Now())
	if err != nil || identity.Kind != auth.TokenKindGateway {
		t.Fatalf("validate gateway token: %v %+v", err, identity)
	}

	var bootstraps, tokens int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'user.bootstrap'`).Scan(&bootstraps); err != nil {
		t.Fatalf("count bootstrap audit: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'token.create'`).Scan(&tokens); err != nil {
		t.Fatalf("count token audit: %v", err)
	}
	if bootstraps != 1 || tokens != 1 {
		t.Fatalf("audit rows = bootstrap %d token %d, want 1/1", bootstraps, tokens)
	}
}

// A stdout failure after commit must say the owner and token already exist and
// where to recover, rather than looking like a failed bootstrap.
func TestCreateOwnerStdoutFailureDB(t *testing.T) {
	pool := requireCLITestPool(t)
	ctx := t.Context()
	cfg := cliTestPasswordConfig()

	err := createOwner(ctx, pool, createOwnerOptions{
		Username:         "owner",
		Email:            "owner@example.com",
		WithGatewayToken: true,
	}, dbOwnerPassword, &cfg, errorWriter{})
	if err == nil {
		t.Fatal("createOwner succeeded with a failing stdout")
	}
	for _, want := range []string{`owner "owner" was created`, "unrecoverable", "reset-password", "broken pipe"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("stdout failure error = %q, want %q", err, want)
		}
	}
	if n, countErr := auth.CountUsers(ctx, pool); countErr != nil || n != 1 {
		t.Fatalf("user count after committed bootstrap = %d, %v; want 1", n, countErr)
	}
	var tokens int
	if queryErr := pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens`).Scan(&tokens); queryErr != nil || tokens != 1 {
		t.Fatalf("token count after committed bootstrap = %d, %v; want 1", tokens, queryErr)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// The bootstrap token must stop working on the legacy surface too, not just on
// /admin/api, once an owner exists.
func TestAdminBootstrapTokenLegacySurfaceDB(t *testing.T) {
	pool := requireCLITestPool(t)
	ctx := t.Context()
	cfg := cliTestPasswordConfig()
	state := newAuthState(pool, &cfg)
	state.setBootstrapToken(dbBootstrapToken)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(adminAccess(":8080", dbBootstrapToken, state))
	engine.GET("/admin/providers", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	// While users is empty the one-time token still opens the legacy surface.
	res := makeRequest(t, engine, http.MethodGet, "/admin/providers", nil, bearer(dbBootstrapToken))
	if res.Code != http.StatusNoContent {
		t.Fatalf("bootstrap token on empty install = %d, want 204 (body %s)", res.Code, res.Body.String())
	}

	if _, err := auth.CreateFirstOwner(ctx, pool, auth.CreateOwnerInput{
		Username: "owner", Email: "owner@example.com", Password: dbOwnerPassword,
	}, cfg); err != nil {
		t.Fatalf("create owner: %v", err)
	}

	res = makeRequest(t, engine, http.MethodGet, "/admin/providers", nil, bearer(dbBootstrapToken))
	if res.Code != http.StatusForbidden {
		t.Fatalf("bootstrap token after owner = %d, want 403 (body %s)", res.Code, res.Body.String())
	}

	// The legacy shared-token login is disabled with it: loopback would otherwise
	// redirect to the SPA. No cookie may be issued.
	loopback := gin.New()
	loopback.Use(adminAccess("127.0.0.1:8080", dbBootstrapToken, state))
	res = makeRequest(t, loopback, http.MethodPost, "/admin/login", "token="+dbBootstrapToken, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if res.Code != http.StatusSeeOther || res.Header().Get("Location") != "/admin" || len(res.Result().Cookies()) != 0 {
		t.Fatalf("legacy token login after owner = %d %q cookies=%d, want a redirect without a session", res.Code, res.Header().Get("Location"), len(res.Result().Cookies()))
	}
}

func TestResetPasswordDB(t *testing.T) {
	pool := requireCLITestPool(t)
	ctx := t.Context()
	cfg := cliTestPasswordConfig()
	owner, err := auth.CreateFirstOwner(ctx, pool, auth.CreateOwnerInput{
		Username: "owner", Email: "owner@example.com", Password: dbOwnerPassword,
	}, cfg)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	session, _, err := auth.CreateSession(ctx, pool, auth.CreateSessionInput{UserID: owner.ID}, auth.DefaultSessionConfig())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	gatewayRaw, _, err := auth.CreateAPIToken(ctx, pool, auth.CreateAPITokenInput{
		UserID: owner.ID, Kind: auth.TokenKindGateway, Name: "laptop", Expiry: auth.TokenExpiry{NoExpiry: true},
	})
	if err != nil {
		t.Fatalf("create gateway token: %v", err)
	}
	adminRaw, _, err := auth.CreateAPIToken(ctx, pool, auth.CreateAPITokenInput{
		UserID: owner.ID, Kind: auth.TokenKindAdmin, Name: "ci", Scope: auth.ScopeWrite,
	})
	if err != nil {
		t.Fatalf("create admin token: %v", err)
	}

	const newPassword = "Quartz-Meadow8-Signal!"
	if err := resetPasswordByUsername(ctx, pool, "owner", newPassword, &cfg); err != nil {
		t.Fatalf("resetPasswordByUsername: %v", err)
	}

	if _, err := auth.ValidateSession(ctx, pool, session, time.Now(), auth.DefaultSessionConfig()); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("reset left session valid: %v", err)
	}
	if _, err := auth.ValidateAPIToken(ctx, pool, gatewayRaw, auth.TokenKindGateway, time.Now()); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("reset left gateway token valid: %v", err)
	}
	if _, err := auth.ValidateAPIToken(ctx, pool, adminRaw, auth.TokenKindAdmin, time.Now()); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("reset left admin token valid: %v", err)
	}
	after, err := auth.GetUserByID(ctx, pool, owner.ID)
	if err != nil || !after.MustChangePassword {
		t.Fatalf("reset did not force a password change: %+v %v", after, err)
	}
	var resets int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'user.password_reset'`).Scan(&resets); err != nil {
		t.Fatalf("count reset audit: %v", err)
	}
	if resets != 1 {
		t.Fatalf("reset audit rows = %d, want 1", resets)
	}
}

// TestAdminBootstrapTokenDB pins the one-time GNVIPI_ADMIN_TOKEN bootstrap: it
// can create the first owner while users is empty, then it is ignored.
func TestAdminBootstrapTokenDB(t *testing.T) {
	pool := requireCLITestPool(t)
	cfg := cliTestPasswordConfig()
	state := newAuthState(pool, &cfg)
	state.loopbackBind = true
	state.setBootstrapToken(dbBootstrapToken)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(adminAPIAuthorization(state))
	registerAdminUserRoutes(engine, state)

	// While empty, the bootstrap token may only create the first owner.
	res := makeRequest(t, engine, http.MethodGet, "/admin/api/users", nil, bearer(dbBootstrapToken))
	if res.Code != http.StatusForbidden || decodeBody(t, res)["code"] != "forbidden" {
		t.Fatalf("bootstrap GET users = %d %s, want 403 forbidden", res.Code, res.Body.String())
	}

	res = makeRequest(t, engine, http.MethodPost, "/admin/api/users", gin.H{
		"username": "owner",
		"email":    "owner@example.com",
		"role":     string(auth.RoleOwner),
		"password": dbOwnerPassword,
	}, bearer(dbBootstrapToken))
	if res.Code != http.StatusCreated {
		t.Fatalf("bootstrap create owner = %d, want 201 (body %s)", res.Code, res.Body.String())
	}
	created := decodeBody(t, res)
	if created["role"] != string(auth.RoleOwner) {
		t.Fatalf("bootstrap created role = %v, want owner", created["role"])
	}
	if n, err := auth.CountUsers(t.Context(), pool); err != nil || n != 1 {
		t.Fatalf("user count = %d, %v; want 1", n, err)
	}

	// After the first owner exists the bootstrap token is ignored.
	res = makeRequest(t, engine, http.MethodPost, "/admin/api/users", gin.H{
		"username": "second",
		"role":     string(auth.RoleOwner),
		"password": dbOwnerPassword,
	}, bearer(dbBootstrapToken))
	if res.Code != http.StatusUnauthorized || decodeBody(t, res)["code"] != "invalid_credentials" {
		t.Fatalf("bootstrap token after owner = %d %s, want 401 invalid_credentials", res.Code, res.Body.String())
	}
	if n, err := auth.CountUsers(t.Context(), pool); err != nil || n != 1 {
		t.Fatalf("user count after ignored token = %d, %v; want 1", n, err)
	}
}
