package auth

import (
	"net/http/httptest"
	"testing"
)

func TestNewCSRFToken(t *testing.T) {
	a, err := NewCSRFToken()
	if err != nil {
		t.Fatalf("csrf: %v", err)
	}
	b, err := NewCSRFToken()
	if err != nil {
		t.Fatalf("csrf: %v", err)
	}
	if a == b || len(a) < 40 {
		t.Fatalf("unexpected tokens %q %q", a, b)
	}
}

func TestValidDoubleSubmit(t *testing.T) {
	if !ValidDoubleSubmit("abc", "abc") {
		t.Fatal("matching values rejected")
	}
	if ValidDoubleSubmit("abc", "abd") || ValidDoubleSubmit("", "") ||
		ValidDoubleSubmit("abc", "") || ValidDoubleSubmit("abc", "abcd") {
		t.Fatal("mismatched values accepted")
	}
}

func TestCheckOrigin(t *testing.T) {
	const allowed = "https://admin.example.com"
	cases := []struct {
		name    string
		origin  string
		site    string
		allowed string
		want    bool
	}{
		{"exact origin", "https://admin.example.com", "", allowed, true},
		{"default port", "https://admin.example.com:443", "", allowed, true},
		{"other origin", "https://evil.example.com", "", allowed, false},
		{"scheme mismatch", "http://admin.example.com", "", allowed, false},
		{"origin with path", "https://admin.example.com/x", "", allowed, false},
		{"same fetch site", "", "same-origin", allowed, true},
		{"cross fetch site", "", "cross-site", allowed, false},
		{"no signal", "", "", allowed, false},
		{"invalid allowed origin", "https://admin.example.com", "", "not-a-url", false},
		{"loopback", "http://127.0.0.1:8080", "", "http://127.0.0.1:8080", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "https://admin.example.com/admin/api/users", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if got := CheckOrigin(r, tc.allowed); got != tc.want {
				t.Fatalf("CheckOrigin() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckOriginRejectsRepeatedHeaders(t *testing.T) {
	r := httptest.NewRequest("POST", "https://admin.example.com/admin", nil)
	r.Header["Origin"] = []string{"https://admin.example.com", "https://admin.example.com"}
	if CheckOrigin(r, "https://admin.example.com") {
		t.Fatal("repeated Origin accepted")
	}
}
