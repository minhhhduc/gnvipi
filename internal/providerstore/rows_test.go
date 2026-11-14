package providerstore

import (
	"errors"
	"math"
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
