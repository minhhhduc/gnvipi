package auth

import (
	"errors"
	"strings"
	"testing"
)

func fastParams() Params {
	return Params{Memory: 8192, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

func TestHashPasswordVerify(t *testing.T) {
	params := fastParams()
	encoded, err := HashPassword("correct horse battery staple", params, nil)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("unexpected PHC string: %s", encoded)
	}
	ok, rehash, err := VerifyPassword("correct horse battery staple", encoded, params, nil)
	if err != nil || !ok {
		t.Fatalf("verify: ok=%v err=%v", ok, err)
	}
	if rehash {
		t.Fatal("fresh hash must not need rehash")
	}
	ok, _, err = VerifyPassword("wrong password", encoded, params, nil)
	if err != nil {
		t.Fatalf("verify wrong: %v", err)
	}
	if ok {
		t.Fatal("wrong password accepted")
	}
	if !NeedsRehash(encoded, DefaultParams()) {
		t.Fatal("different params must need rehash")
	}
}

func TestPasswordPepper(t *testing.T) {
	params := fastParams()
	pepper := []byte("pepper-value")
	encoded, err := HashPassword("correct horse battery staple", params, pepper)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, _, err := VerifyPassword("correct horse battery staple", encoded, params, pepper)
	if err != nil || !ok {
		t.Fatalf("peppered verify: ok=%v err=%v", ok, err)
	}
	ok, _, err = VerifyPassword("correct horse battery staple", encoded, params, nil)
	if err != nil {
		t.Fatalf("unpeppered verify: %v", err)
	}
	if ok {
		t.Fatal("hash without the pepper must not verify")
	}
}

func TestVerifyRejectsMalformedPHC(t *testing.T) {
	for _, encoded := range []string{
		"", "plaintext", "$argon2i$v=19$m=8192,t=1,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=bad,t=1,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=8192,t=1,p=1$!!!$aGFzaA",
	} {
		if _, _, err := VerifyPassword("x", encoded, fastParams(), nil); !errors.Is(err, ErrInvalidPHC) {
			t.Errorf("encoded %q: got %v, want ErrInvalidPHC", encoded, err)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("correct horse battery staple", "alice", "alice@example.com"); err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
	if err := ValidatePassword("short", "", ""); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("x", 129), "", ""); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("long: %v", err)
	}
	if err := ValidatePassword("alice-rozelle-99", "alice", ""); !errors.Is(err, ErrPasswordContainsIdentity) {
		t.Fatalf("username identity: %v", err)
	}
	if err := ValidatePassword("alice-rozelle-99", "bob", "alice@example.com"); !errors.Is(err, ErrPasswordContainsIdentity) {
		t.Fatalf("email identity: %v", err)
	}
	if err := ValidatePassword("Lantern-xy-9!", "other-user", "xy@example.com"); !errors.Is(err, ErrPasswordContainsIdentity) {
		t.Fatalf("short email identity: %v", err)
	}

	var common string
	for candidate := range commonPasswords {
		if len(candidate) >= minPassword {
			common = candidate
			break
		}
	}
	if common == "" {
		t.Fatal("no denylist entry long enough to test")
	}
	if err := ValidatePassword(common, "", ""); !errors.Is(err, ErrPasswordCommon) {
		t.Fatalf("common password %q: %v", common, err)
	}
	if err := ValidatePassword(strings.ToUpper(common), "", ""); !errors.Is(err, ErrPasswordCommon) {
		t.Fatal("denylist must be case-insensitive")
	}
}

func TestParamsValidate(t *testing.T) {
	if err := DefaultParams().Validate(); err != nil {
		t.Fatalf("default params: %v", err)
	}
	bad := []Params{
		{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32},
		{Memory: 8192, Iterations: 0, Parallelism: 1, SaltLength: 16, KeyLength: 32},
		{Memory: 8192, Iterations: 1, Parallelism: 0, SaltLength: 16, KeyLength: 32},
		{Memory: 8192, Iterations: 1, Parallelism: 1, SaltLength: 4, KeyLength: 32},
		{Memory: 8192, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 8},
		{Memory: 262145, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32},
		{Memory: 8192, Iterations: 11, Parallelism: 1, SaltLength: 16, KeyLength: 32},
		{Memory: 8192, Iterations: 1, Parallelism: 17, SaltLength: 16, KeyLength: 32},
		{Memory: 8192, Iterations: 1, Parallelism: 1, SaltLength: 65, KeyLength: 32},
		{Memory: 8192, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 65},
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("params %+v accepted", p)
		}
	}
}

func TestParsePHCRejectsExcessiveWorkBeforeHashing(t *testing.T) {
	encoded := "$argon2id$v=19$m=262145,t=1,p=1$MDEyMzQ1Njc$MDEyMzQ1Njc4OWFiY2RlZg"
	if _, _, _, err := parsePHC(encoded); !errors.Is(err, ErrInvalidPHC) {
		t.Fatalf("excessive PHC work factor: %v", err)
	}
}

func TestDummyHashUsesConfiguredParams(t *testing.T) {
	params := fastParams()
	params.Memory = 16384
	encoded, err := dummyHashForParams(params)
	if err != nil {
		t.Fatalf("prepare dummy hash: %v", err)
	}
	got, _, _, err := parsePHC(encoded)
	if err != nil {
		t.Fatalf("parse dummy hash: %v", err)
	}
	if got != params {
		t.Fatalf("dummy params = %+v, want %+v", got, params)
	}
}
