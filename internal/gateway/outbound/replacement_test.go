package outbound_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
)

func TestNestedHooksGuardReplacementBodies(t *testing.T) {
	for _, failHook := range []bool{false, true} {
		old := &closeBody{Reader: strings.NewReader("")}
		replacement := &closeBody{Reader: strings.NewReader("")}
		inner := outbound.Wrap(rejectStream{}, outbound.Hooks{Response: func(_ context.Context, resp *http.Response) error {
			if err := resp.Body.Close(); err != nil {
				return err
			}
			resp.Body = replacement
			if failHook {
				return errors.New("hook failure")
			}
			return nil
		}})
		outer := outbound.Wrap(inner, outbound.Hooks{})
		if _, err := outer.ParseStream(t.Context(), &http.Response{Body: old}); err == nil {
			t.Fatal("expected stream/hook failure")
		}
		if old.closes != 1 || replacement.closes != 1 {
			t.Fatalf("hook failure=%v closes old=%d replacement=%d", failHook, old.closes, replacement.closes)
		}
	}
}
