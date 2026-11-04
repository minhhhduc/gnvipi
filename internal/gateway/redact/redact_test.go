package redact

import (
	"bytes"
	"encoding/json"
	"log"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestRedactorEscapesAndCredentialRotation(t *testing.T) {
	var r Redactor
	old := "opaque-secret/with space?+\"<>"
	newKey := "next-opaque-credential"
	r.Update(old)
	r.Update(newKey)
	encoded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{old, url.QueryEscape(old), url.PathEscape(old), string(encoded[1 : len(encoded)-1]), newKey} {
		got := r.Text("upstream failed for " + secret)
		if strings.Contains(got, secret) {
			t.Fatalf("credential remained in %q", got)
		}
		if r.Text(got) != got {
			t.Fatalf("redaction is not idempotent: %q", got)
		}
	}
}

func TestRedactorHeaderPatternsAndJSON(t *testing.T) {
	var r Redactor
	message := `Authorization: Bearer unregistered-auth x-api-key="unregistered-key" https://user:password@example.com/?api_key=url-secret sk-abc123_token`
	got := r.Text(message)
	for _, secret := range []string{"unregistered-auth", "unregistered-key", "password", "url-secret", "sk-abc123_token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("pattern credential remained in %q", got)
		}
	}
	r.Update("opaque-known-key")
	body := []byte(`{"error":{"message":"opaque-known-key","code":"normal"},"usage":7}`)
	clean, err := r.JSON(body)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(clean, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["usage"] != float64(7) || strings.Contains(string(clean), "opaque-known-key") {
		t.Fatalf("sanitized response=%s", clean)
	}
}

func TestRedactorJSONDecodesUnicodeEscapedCredentials(t *testing.T) {
	var r Redactor
	const key = "opaque-json-unicode-credential"
	r.Update(key)
	raw := []byte(`{"error":{"message":"failed \u006fpaque-json-unicode-credential","\u006fpaque-json-unicode-credential":"detail"},"count":9007199254740993,"retry":false,"extra":null}`)
	clean, err := r.JSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Error map[string]string `json:"error"`
		Count json.Number       `json:"count"`
		Retry bool              `json:"retry"`
		Extra any               `json:"extra"`
	}
	if err := json.Unmarshal(clean, &result); err != nil {
		t.Fatal(err)
	}
	for name, value := range result.Error {
		if strings.Contains(name, key) || strings.Contains(value, key) {
			t.Fatalf("decoded diagnostic exposed credential: %s", clean)
		}
	}
	if !strings.HasPrefix(result.Error["message"], "failed ") || result.Count.String() != "9007199254740993" || result.Retry || result.Extra != nil {
		t.Fatalf("diagnostic content or scalar types changed: %s", clean)
	}
}

func TestRedactorJSONMasksAuthenticationPropertyValues(t *testing.T) {
	var r Redactor
	raw := []byte(`{"error":{"headers":{"\u0041uthorization":"Bearer opaque-unregistered-auth","Proxy-Authorization":"Basic dXNlcjpwYXNzd29yZA==","x-api-key":"opaque-unregistered-key","API_KEY":"\u006fpaque-unregistered-api-key","ApiKey":["opaque-array-key"],"Accept":"application/json"}},"status":401}`)
	clean, err := r.JSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Error struct {
			Headers map[string]any `json:"headers"`
		} `json:"error"`
		Status int `json:"status"`
	}
	if err := json.Unmarshal(clean, &result); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Authorization", "Proxy-Authorization", "x-api-key", "API_KEY"} {
		if result.Error.Headers[name] != "[redacted]" {
			t.Fatalf("authentication value not masked for %s: %s", name, clean)
		}
	}
	values, ok := result.Error.Headers["ApiKey"].([]any)
	if !ok || len(values) != 1 || values[0] != "[redacted]" {
		t.Fatalf("multivalue authentication was disclosed or its shape changed: %s", clean)
	}
	if result.Error.Headers["Accept"] != "application/json" || result.Status != 401 {
		t.Fatalf("unrelated diagnostic fields changed: %s", clean)
	}
}

func TestRedactorLogsMaskSOCKSProxyUserinfo(t *testing.T) {
	var r Redactor
	var logs bytes.Buffer
	logger := log.New(r.WrapWriter(&logs), "", 0)
	logger.Print("proxy=socks5://proxyuser:opaque-socks-password@proxy.example:1080/path")
	logger.Print("proxy=SOCKS5H://proxyuser:opaque-socks5h-password@proxy.example:1080/path")
	got := logs.String()
	for _, secret := range []string{"proxyuser", "opaque-socks-password", "opaque-socks5h-password"} {
		if strings.Contains(got, secret) {
			t.Fatalf("SOCKS log disclosed userinfo: %s", got)
		}
	}
	if !strings.Contains(got, "socks5://[redacted]@proxy.example:1080/path") || !strings.Contains(got, "SOCKS5H://[redacted]@proxy.example:1080/path") {
		t.Fatalf("proxy address was not preserved: %s", got)
	}
}

func TestConcurrentRedactorUpdatesKeepInflightKeys(t *testing.T) {
	var r Redactor
	r.Update("original-inflight-key")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			r.Update("rotated-active-key")
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			if strings.Contains(r.Text("original-inflight-key"), "original-inflight-key") {
				t.Error("rotation exposed in-flight key")
				return
			}
		}
	}()
	wg.Wait()
}
