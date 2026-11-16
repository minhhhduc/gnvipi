package main

import (
	"context"
	"net/http"
	"regexp"
	"testing"

	"github.com/gin-gonic/gin"
)

type tokenTestCaptureMailer struct {
	lastTo      string
	lastSubject string
	lastBody    string
}

func (m *tokenTestCaptureMailer) Enabled() bool { return true }

func (m *tokenTestCaptureMailer) Send(_ context.Context, to, subject, body string) error {
	m.lastTo = to
	m.lastSubject = subject
	m.lastBody = body
	return nil
}

func TestDBAdminTokens_RevealOTPFlow(t *testing.T) {
	h := newDBAdminHarness(t)
	capture := &tokenTestCaptureMailer{}
	h.state.mailer = capture
	headers := map[string]string{"Authorization": "Bearer " + h.token}

	// 1. Create a gateway token; keyring is active so it should be revealable.
	createRes := makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens", gin.H{
		"name": "ci-reveal-token",
		"kind": "gateway",
	}, headers)
	if createRes.Code != http.StatusCreated {
		t.Fatalf("create token = %d, want 201 (%s)", createRes.Code, createRes.Body.String())
	}
	created := decodeBody(t, createRes)
	tokenID, _ := created["id"].(string)
	plaintext, _ := created["token"].(string)
	if tokenID == "" || plaintext == "" {
		t.Fatalf("create token missing id/token: %v", created)
	}

	// 2. Listing tokens reports can_reveal=true without exposing the secret.
	listRes := makeRequest(t, h.engine, http.MethodGet, "/admin/api/tokens", nil, headers)
	if listRes.Code != http.StatusOK {
		t.Fatalf("list tokens = %d, want 200 (%s)", listRes.Code, listRes.Body.String())
	}
	listBody := decodeBody(t, listRes)
	rawTokens, _ := listBody["tokens"].([]any)
	var found bool
	for _, entry := range rawTokens {
		m, _ := entry.(map[string]any)
		if m["id"] == tokenID {
			found = true
			if canReveal, _ := m["can_reveal"].(bool); !canReveal {
				t.Fatalf("expected can_reveal=true for newly created token, got %v", m["can_reveal"])
			}
		}
	}
	if !found {
		t.Fatalf("created token %s missing from list", tokenID)
	}

	// 3. Request OTP -> sends 6-digit code to token owner's email.
	reqRes := makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens/"+tokenID+"/reveal/request", nil, headers)
	if reqRes.Code != http.StatusOK {
		t.Fatalf("reveal request = %d, want 200 (%s)", reqRes.Code, reqRes.Body.String())
	}
	if capture.lastTo != "owner@example.test" {
		t.Fatalf("expected OTP email to owner@example.test, got %q", capture.lastTo)
	}
	codeMatch := regexp.MustCompile(`\b(\d{6})\b`).FindStringSubmatch(capture.lastBody)
	if len(codeMatch) < 2 {
		t.Fatalf("expected 6-digit OTP in email body, got %q", capture.lastBody)
	}
	otp := codeMatch[1]

	// 4. Wrong OTP is rejected.
	badOTP := "000000"
	if otp == badOTP {
		badOTP = "111111"
	}
	badRes := makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens/"+tokenID+"/reveal", gin.H{
		"otp": badOTP,
	}, headers)
	if badRes.Code != http.StatusForbidden {
		t.Fatalf("wrong OTP = %d, want 403 (%s)", badRes.Code, badRes.Body.String())
	}

	// 5. Valid OTP returns the exact plaintext token.
	okRes := makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens/"+tokenID+"/reveal", gin.H{
		"otp": otp,
	}, headers)
	if okRes.Code != http.StatusOK {
		t.Fatalf("valid OTP = %d, want 200 (%s)", okRes.Code, okRes.Body.String())
	}
	okBody := decodeBody(t, okRes)
	if okBody["token"] != plaintext {
		t.Fatalf("revealed token = %q, want %q", okBody["token"], plaintext)
	}

	// 6. OTP is single-use; replaying it fails.
	replayRes := makeRequest(t, h.engine, http.MethodPost, "/admin/api/tokens/"+tokenID+"/reveal", gin.H{
		"otp": otp,
	}, headers)
	if replayRes.Code != http.StatusForbidden {
		t.Fatalf("replayed OTP = %d, want 403 (%s)", replayRes.Code, replayRes.Body.String())
	}
}
