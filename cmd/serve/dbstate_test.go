package main

import (
	"strings"
	"testing"
)

func TestOpenAppDBRequiresDSNInProduction(t *testing.T) {
	t.Setenv(databaseEnv, "")
	t.Setenv("GNVIPI_ENV", "production")

	state, err := openAppDB(t.Context(), false)
	if state != nil {
		state.Close()
		t.Fatal("production without a DSN returned database state")
	}
	if err == nil || !strings.Contains(err.Error(), databaseEnv) {
		t.Fatalf("production without a DSN error = %v, want actionable %s error", err, databaseEnv)
	}
}

func TestOpenAppDBKeepsDevelopmentFileModeWithoutDSN(t *testing.T) {
	t.Setenv(databaseEnv, "")
	t.Setenv("GNVIPI_ENV", "development")

	state, err := openAppDB(t.Context(), false)
	if err != nil || state != nil {
		t.Fatalf("development without a DSN = state %v err %v, want nil nil", state, err)
	}
}
