package main

import (
	"testing"
	"time"
)

func TestKeyRevealOTPStoreLifecycle(t *testing.T) {
	store := newKeyRevealOTPStore()
	now := time.Now()

	code, err := store.Issue("u1", "k1", now)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("code length = %d, want 6", len(code))
	}
	if store.Verify("u1", "k1", "000000", now) && code != "000000" {
		t.Fatal("wrong code should not verify")
	}
	if !store.Verify("u1", "k1", code, now.Add(time.Minute)) {
		t.Fatal("valid code should verify")
	}
	if store.Verify("u1", "k1", code, now.Add(time.Minute)) {
		t.Fatal("code must be single-use")
	}

	code2, err := store.Issue("u1", "k2", now)
	if err != nil {
		t.Fatal(err)
	}
	if store.Verify("u1", "k2", code2, now.Add(revealOTPTTL+time.Second)) {
		t.Fatal("expired code must fail")
	}
}

func TestMaskEmail(t *testing.T) {
	if got := maskEmail("alice@example.com"); got != "al***@example.com" {
		t.Fatalf("maskEmail = %q", got)
	}
	if got := maskEmail("a@example.com"); got != "a***@example.com" {
		t.Fatalf("maskEmail = %q", got)
	}
}
