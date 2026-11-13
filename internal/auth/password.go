package auth

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	argon2Version = 19
	minPassword   = 12
	maxPassword   = 128
)

var (
	ErrPasswordTooShort         = errors.New("auth: password must be at least 12 characters")
	ErrPasswordTooLong          = errors.New("auth: password must be at most 128 characters")
	ErrPasswordCommon           = errors.New("auth: password is too common")
	ErrPasswordContainsIdentity = errors.New("auth: password must not contain the username or email local part")
	ErrInvalidPHC               = errors.New("auth: invalid password hash")
)

// Params are the Argon2id parameters stored in the PHC string.
type Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultParams follows the design: m=65536 (64 MiB), t=3, p=2.
func DefaultParams() Params {
	return Params{Memory: 65536, Iterations: 3, Parallelism: 2, SaltLength: 16, KeyLength: 32}
}

// Validate rejects parameters that would produce weak hashes or panic Argon2.
func (p Params) Validate() error {
	switch {
	case p.Memory < 8*1024:
		return fmt.Errorf("auth: argon2 memory must be at least 8192 KiB")
	case p.Iterations < 1:
		return fmt.Errorf("auth: argon2 iterations must be positive")
	case p.Parallelism < 1:
		return fmt.Errorf("auth: argon2 parallelism must be positive")
	case p.SaltLength < 8:
		return fmt.Errorf("auth: argon2 salt length must be at least 8 bytes")
	case p.KeyLength < 16:
		return fmt.Errorf("auth: argon2 key length must be at least 16 bytes")
	}
	return nil
}

// common_passwords.txt is the SecLists 10k-most-common password list (MIT):
// https://github.com/danielmiessler/SecLists
//
//go:embed common_passwords.txt
var commonPasswordsData string

var commonPasswords = loadDenylist(commonPasswordsData)

func loadDenylist(data string) map[string]struct{} {
	set := make(map[string]struct{}, 10000)
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		line := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if line != "" {
			set[line] = struct{}{}
		}
	}
	return set
}

// PreprocessPassword applies the optional HMAC-SHA256 pepper before Argon2id,
// so the pepper is not subject to Argon2's input length limits.
func PreprocessPassword(password string, pepper []byte) []byte {
	if len(pepper) == 0 {
		return []byte(password)
	}
	mac := hmac.New(sha256.New, pepper)
	_, _ = mac.Write([]byte(password))
	return mac.Sum(nil)
}

// HashPassword returns a PHC Argon2id string:
// $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>.
func HashPassword(password string, params Params, pepper []byte) (string, error) {
	if err := params.Validate(); err != nil {
		return "", err
	}
	salt := make([]byte, params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey(PreprocessPassword(password, pepper), salt,
		params.Iterations, params.Memory, params.Parallelism, params.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version, params.Memory, params.Iterations, params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks password against encoded. needsRehash is true when the
// hash was produced with a different algorithm, version, parameter set, or
// key/salt length than params. A pepper change cannot be detected from the PHC
// string; an operator rotating the pepper must force a rehash explicitly.
func VerifyPassword(password, encoded string, params Params, pepper []byte) (ok, needsRehash bool, err error) {
	p, salt, key, err := parsePHC(encoded)
	if err != nil {
		return false, false, err
	}
	derived := argon2.IDKey(PreprocessPassword(password, pepper), salt,
		p.Iterations, p.Memory, p.Parallelism, uint32(len(key)))
	ok = subtle.ConstantTimeCompare(derived, key) == 1
	return ok, NeedsRehash(encoded, params), nil
}

// verifyPasswordWithPrevious verifies both configured peppers when a rotation
// window is active. Both hashes run even when the current pepper succeeds so
// account state does not change verification timing. A previous-pepper match
// always requests an upgrade to the current pepper.
func verifyPasswordWithPrevious(password, encoded string, params Params, pepper, previousPepper []byte, previousPepperSet bool) (ok, needsRehash bool, err error) {
	ok, needsRehash, err = VerifyPassword(password, encoded, params, pepper)
	if err != nil || !previousPepperSet {
		return ok, needsRehash, err
	}
	previousOK, _, previousErr := VerifyPassword(password, encoded, params, previousPepper)
	if previousErr != nil {
		return false, false, previousErr
	}
	return ok || previousOK, needsRehash || (!ok && previousOK), nil

}

// NeedsRehash reports whether encoded was made with different parameters.
// Invalid or empty hashes report true so callers rehash on next login.
func NeedsRehash(encoded string, params Params) bool {
	p, salt, key, err := parsePHC(encoded)
	if err != nil {
		return true
	}
	return p.Memory != params.Memory ||
		p.Iterations != params.Iterations ||
		p.Parallelism != params.Parallelism ||
		uint32(len(salt)) != params.SaltLength ||
		uint32(len(key)) != params.KeyLength
}

//nolint:gocritic // named results add no clarity here
func parsePHC(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrInvalidPHC
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2Version) {
		return Params{}, nil, nil, ErrInvalidPHC
	}
	var p Params
	for _, field := range strings.Split(parts[3], ",") {
		k, v, found := strings.Cut(field, "=")
		if !found {
			return Params{}, nil, nil, ErrInvalidPHC
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Params{}, nil, nil, ErrInvalidPHC
		}
		switch k {
		case "m":
			p.Memory = uint32(n)
		case "t":
			p.Iterations = uint32(n)
		case "p":
			if n > 255 {
				return Params{}, nil, nil, ErrInvalidPHC
			}
			p.Parallelism = uint8(n)
		default:
			return Params{}, nil, nil, ErrInvalidPHC
		}
	}
	salt, err := decodePHCSegment(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidPHC
	}
	key, err := decodePHCSegment(parts[5])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidPHC
	}
	p.SaltLength = uint32(len(salt))
	p.KeyLength = uint32(len(key))
	if err := p.Validate(); err != nil {
		return Params{}, nil, nil, ErrInvalidPHC
	}
	return p, salt, key, nil
}

func decodePHCSegment(s string) ([]byte, error) {
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

// ValidatePassword enforces length, the embedded common-password denylist and
// the rule that the password must not contain the username or the email local
// part. identity is (username, email); either may be empty.
func ValidatePassword(password, username, email string) error {
	if utf8.RuneCountInString(password) < minPassword {
		return ErrPasswordTooShort
	}
	if utf8.RuneCountInString(password) > maxPassword {
		return ErrPasswordTooLong
	}
	lower := strings.ToLower(password)
	if _, common := commonPasswords[lower]; common {
		return ErrPasswordCommon
	}
	for _, part := range []string{username, emailLocalPart(email)} {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" && strings.Contains(lower, part) {
			return ErrPasswordContainsIdentity
		}
	}
	return nil
}

func emailLocalPart(email string) string {
	local, _, found := strings.Cut(email, "@")
	if !found {
		return ""
	}
	return local
}

var (
	dummyHashOnce sync.Once
	dummyHash     string
)

// dummyVerification burns the same number of Argon2id passes as a real
// verification, including an active previous-pepper rotation window.
func dummyVerification(password string, pepper, previousPepper []byte, previousPepperSet bool) {
	dummyHashOnce.Do(func() {
		// Params match DefaultParams; a deployment using custom params sees a
		// slightly different cost on unknown users only.
		dummyHash, _ = HashPassword("gnvipi-dummy-password", DefaultParams(), nil) //nolint:errcheck // best-effort cleanup/best-effort parse
	})
	_, _, _ = verifyPasswordWithPrevious(password, dummyHash, DefaultParams(), pepper, previousPepper, previousPepperSet) //nolint:errcheck // best-effort cleanup/best-effort parse
}
