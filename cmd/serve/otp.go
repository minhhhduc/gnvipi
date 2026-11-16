package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

const (
	revealOTPTTL         = 5 * time.Minute
	revealOTPMaxAttempts = 5
)

type otpChallenge struct {
	hash      [32]byte
	expiresAt time.Time
	attempts  int
}

// keyRevealOTPStore manages short-lived single-use email OTP challenges for
// revealing encrypted provider keys.
type keyRevealOTPStore struct {
	mu      sync.Mutex
	pending map[string]otpChallenge
}

func newKeyRevealOTPStore() *keyRevealOTPStore {
	return &keyRevealOTPStore{
		pending: make(map[string]otpChallenge),
	}
}

func (s *keyRevealOTPStore) Issue(userID, keyID string, now time.Time) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", fmt.Errorf("generate otp: %w", err)
	}
	code := fmt.Sprintf("%06d", n.Int64())
	sum := sha256.Sum256([]byte(code))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[userID+":"+keyID] = otpChallenge{
		hash:      sum,
		expiresAt: now.Add(revealOTPTTL),
	}
	return code, nil
}

func (s *keyRevealOTPStore) Verify(userID, keyID, code string, now time.Time) bool {
	if s == nil {
		return false
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	sum := sha256.Sum256([]byte(code))
	k := userID + ":" + keyID
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.pending[k]
	if !ok {
		return false
	}
	if now.After(ch.expiresAt) {
		delete(s.pending, k)
		return false
	}
	if subtle.ConstantTimeCompare(ch.hash[:], sum[:]) == 1 {
		delete(s.pending, k)
		return true
	}
	ch.attempts++
	if ch.attempts >= revealOTPMaxAttempts {
		delete(s.pending, k)
	} else {
		s.pending[k] = ch
	}
	return false
}

func maskEmail(email string) string {
	local, domain, ok := strings.Cut(strings.TrimSpace(email), "@")
	if !ok || local == "" || domain == "" {
		return "***"
	}
	runes := []rune(local)
	if len(runes) <= 2 {
		return string(runes[0]) + "***@" + domain
	}
	return string(runes[:2]) + "***@" + domain
}
