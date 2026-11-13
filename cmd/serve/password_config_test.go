package main

import (
	"bytes"
	"os"
	"testing"
)

func TestNewAuthStateLoadsArgon2AndPepperConfiguration(t *testing.T) {
	t.Setenv("GNVIPI_PASSWORD_ARGON2_MEMORY_KIB", "16384")
	t.Setenv("GNVIPI_PASSWORD_ARGON2_ITERATIONS", "2")
	t.Setenv("GNVIPI_PASSWORD_ARGON2_PARALLELISM", "1")
	const pepper = "  deployment pepper with spaces  "
	t.Setenv(pepperEnv, pepper)
	password, err := loadPasswordHashConfig(os.LookupEnv)
	if err != nil {
		t.Fatalf("load password hash configuration: %v", err)
	}
	state := newAuthState(nil, &password)
	wantMemory, wantIterations, wantParallelism := uint32(16384), uint32(2), uint8(1)
	if state.login.Params.Memory != wantMemory ||
		state.login.Params.Iterations != wantIterations ||
		state.login.Params.Parallelism != wantParallelism {
		t.Fatalf("login Argon2 params = %+v, want m=%d t=%d p=%d",
			state.login.Params, wantMemory, wantIterations, wantParallelism)
	}
	if state.password.Params != state.login.Params {
		t.Fatalf("password writer params = %+v, want login params %+v", state.password.Params, state.login.Params)
	}
	if !bytes.Equal(state.login.Pepper, []byte(pepper)) || !bytes.Equal(state.password.Pepper, []byte(pepper)) {
		t.Fatal("password pepper was trimmed or differs between verification and hashing")
	}
}

func TestLoadPasswordHashConfigRejectsExcessiveArgon2(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{argon2MemoryEnv, "262145"},
		{argon2IterationsEnv, "11"},
		{argon2ParallelismEnv, "17"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := loadPasswordHashConfig(os.LookupEnv); err == nil {
				t.Fatalf("%s=%s accepted", tc.name, tc.value)
			}
		})
	}
}
