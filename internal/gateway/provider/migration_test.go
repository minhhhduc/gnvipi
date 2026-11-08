package provider

import (
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

func TestMergeLegacyCredentialTransitions(t *testing.T) {
	tests := []struct {
		name       string
		oldAuth    Auth
		legacyItem LegacyProvider
		wantAuth   Auth
	}{
		{
			name:    "switch from bearer key to key_env",
			oldAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				KeyEnv:  "NEW_KEY_ENV",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthBearer, Key: "", KeyEnv: "NEW_KEY_ENV", KeyRef: ""},
		},
		{
			name:    "switch from bearer key to auth none",
			oldAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				APIKey:  "",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthNone, Key: "", KeyEnv: "", KeyRef: ""},
		},
		{
			name:    "retain bearer key when using [REDACTED] sentinel",
			oldAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				APIKey:  "[REDACTED]",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
		},
		{
			name:    "retain bearer key when using unchanged sentinel",
			oldAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				APIKey:  "unchanged",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
		},
		{
			name:    "retain bearer key when using redacted hint",
			oldAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				APIKey:  "sk-...1234",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
		},
		{
			name:    "update bearer key to new literal key",
			oldAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				APIKey:  "sk-new-5678",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthBearer, Key: "sk-new-5678", KeyEnv: "", KeyRef: "prov-id"},
		},
		{
			name:    "switch from key_env to new literal key",
			oldAuth: Auth{Scheme: AuthBearer, KeyEnv: "OLD_KEY_ENV"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				APIKey:  "sk-literal-key",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthBearer, Key: "sk-literal-key", KeyEnv: ""},
		},
		{
			name:    "switch from key_env to auth none",
			oldAuth: Auth{Scheme: AuthBearer, KeyEnv: "OLD_KEY_ENV"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				APIKey:  "",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthNone, Key: "", KeyEnv: ""},
		},
		{
			name:    "switch from key_env A to key_env B",
			oldAuth: Auth{Scheme: AuthBearer, KeyEnv: "ENV_A"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				KeyEnv:  "ENV_B",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthBearer, Key: "", KeyEnv: "ENV_B"},
		},
		{
			name:    "preserve auth when credentials unchanged",
			oldAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
			legacyItem: LegacyProvider{
				Name:    "TestProvider",
				BaseURL: "https://api.test.example/v1",
				APIKey:  "sk-old-1234",
				Model:   "m",
				Alias:   "TestProvider/m",
			},
			wantAuth: Auth{Scheme: AuthBearer, Key: "sk-old-1234", KeyRef: "prov-id"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			current := []Provider{
				{
					ID:       "prov-id",
					Name:     "TestProvider",
					BaseURL:  "https://api.test.example/v1",
					Protocol: ir.ProtocolOpenAIChat,
					Auth:     tc.oldAuth,
					Models:   []ModelMapping{{Alias: "TestProvider/m", UpstreamModel: "m"}},
					Enabled:  true,
				},
			}

			merged, err := MergeLegacy(current, []LegacyProvider{tc.legacyItem})
			if err != nil {
				t.Fatalf("MergeLegacy failed: %v", err)
			}
			if len(merged) != 1 {
				t.Fatalf("expected 1 merged provider, got %d", len(merged))
			}

			got := merged[0].Auth
			if got.Scheme != tc.wantAuth.Scheme {
				t.Errorf("Scheme = %q, want %q", got.Scheme, tc.wantAuth.Scheme)
			}
			if got.Key != tc.wantAuth.Key {
				t.Errorf("Key = %q, want %q", got.Key, tc.wantAuth.Key)
			}
			if got.KeyEnv != tc.wantAuth.KeyEnv {
				t.Errorf("KeyEnv = %q, want %q", got.KeyEnv, tc.wantAuth.KeyEnv)
			}
			if got.KeyRef != tc.wantAuth.KeyRef {
				t.Errorf("KeyRef = %q, want %q", got.KeyRef, tc.wantAuth.KeyRef)
			}
		})
	}
}
