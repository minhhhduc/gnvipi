package routing

import (
	"errors"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/providerstore"
)

func TestKeyAwareRouteErrorModelNotAvailable(t *testing.T) {
	err := keyAwareRouteError(providerstore.ErrModelNotAvailable)
	var gatewayErr *gwerr.Error
	if !errors.As(err, &gatewayErr) {
		t.Fatalf("error = %v, want *gwerr.Error", err)
	}
	if gatewayErr.Status != 404 || gatewayErr.Kind != gwerr.KindNotFound {
		t.Fatalf("kind/status = %s/%d, want not_found/404", gatewayErr.Kind, gatewayErr.Status)
	}
	if gatewayErr.Code != "model_not_available" {
		t.Fatalf("code = %q, want model_not_available", gatewayErr.Code)
	}
	if !strings.Contains(gatewayErr.Message, "no longer available for this account") {
		t.Fatalf("message = %q", gatewayErr.Message)
	}
	if strings.Contains(gatewayErr.Message, "model not found:") {
		t.Fatalf("message leaks the generic prefix: %q", gatewayErr.Message)
	}
}
