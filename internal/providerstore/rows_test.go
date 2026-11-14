package providerstore

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestValidateKeyRowRejectsControlCharactersInKeyRef(t *testing.T) {
	key := KeyRow{
		ID:         "11111111-1111-1111-1111-111111111111",
		ProviderID: "p",
		OwnerID:    "22222222-2222-2222-2222-222222222222",
		AuthScheme: SchemeBearer,
		KeyRef:     "a\x1ab",
		Enabled:    true,
	}
	if err := ValidateKeyRow(key); !errors.Is(err, ErrInvalidCredentialSource) {
		t.Fatalf("control character in key_ref = %v, want ErrInvalidCredentialSource", err)
	}
	key.KeyRef = "env:OK"
	if err := ValidateKeyRow(key); err != nil {
		t.Fatalf("plain key_ref rejected: %v", err)
	}
}

func TestValidateKeyModelRowBoundsLegacyPosition(t *testing.T) {
	row := KeyModelRow{
		KeyID:          "11111111-1111-1111-1111-111111111111",
		Alias:          "a",
		UpstreamModel:  "m",
		LegacyPosition: math.MaxInt32 + 1,
	}
	if err := ValidateKeyModelRow(row); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("legacy_position above int4 = %v, want ErrInvalidConfig", err)
	}
	row.LegacyPosition = math.MaxInt32
	if err := ValidateKeyModelRow(row); err != nil {
		t.Fatalf("max int4 legacy_position rejected: %v", err)
	}
}

func TestValidateKeyModelRowBoundsColumnLengths(t *testing.T) {
	row := KeyModelRow{
		KeyID:         "11111111-1111-1111-1111-111111111111",
		Alias:         strings.Repeat("a", 201),
		UpstreamModel: "m",
	}
	if err := ValidateKeyModelRow(row); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("alias above 200 = %v, want ErrInvalidConfig", err)
	}
	row.Alias, row.UpstreamModel = "a", strings.Repeat("m", 201)
	if err := ValidateKeyModelRow(row); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("upstream_model above 200 = %v, want ErrInvalidConfig", err)
	}
	row.UpstreamModel = strings.Repeat("m", 200)
	if err := ValidateKeyModelRow(row); err != nil {
		t.Fatalf("200-character upstream_model rejected: %v", err)
	}
}
