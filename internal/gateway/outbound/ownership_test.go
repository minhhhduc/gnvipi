package outbound_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

func TestNestedStreamFailureClosesBodyOnce(t *testing.T) {
	body := &closeBody{Reader: strings.NewReader("broken")}
	a := outbound.Wrap(outbound.Wrap(rejectStream{}, outbound.Hooks{}), outbound.Hooks{})
	stream, err := a.ParseStream(t.Context(), &http.Response{Body: body})
	if stream != nil || err == nil || body.closes != 1 {
		t.Fatalf("stream=%v error=%v closes=%d", stream, err, body.closes)
	}
}

type invalidStreamAdapter struct {
	chatAdapter
	returnStream bool
}

func (a invalidStreamAdapter) ParseStream(ctx context.Context, resp *http.Response) (adapter.Stream, error) {
	if !a.returnStream {
		return nil, nil
	}
	stream, err := a.chatAdapter.ParseStream(ctx, resp)
	if err != nil {
		return stream, err
	}
	return stream, errors.New("creation failed after taking ownership")
}

func TestFailedStreamCreationOwnership(t *testing.T) {
	for _, returnStream := range []bool{false, true} {
		body := &closeBody{Reader: strings.NewReader("data: [DONE]\n\n")}
		a := outbound.Wrap(invalidStreamAdapter{returnStream: returnStream}, outbound.Hooks{})
		stream, err := a.ParseStream(t.Context(), &http.Response{Body: body})
		if stream != nil || err == nil || body.closes != 1 {
			t.Fatalf("returnedStream=%v stream=%v error=%v closes=%d", returnStream, stream, err, body.closes)
		}
	}
}

func TestExtraBodyFailureReleasesBaseRequest(t *testing.T) {
	for _, wire := range []string{"not-json", "[]", "null"} {
		body := &closeBody{Reader: strings.NewReader(wire)}
		a, err := outbound.Configure(buildAdapter{body: body}, &provider.Provider{ExtraBody: []byte(`{"tenant":"a"}`)})
		if err != nil {
			t.Fatal(err)
		}
		r, err := a.BuildRequest(t.Context(), &ir.Request{}, adapter.Target{BaseURL: "https://up.invalid"})
		if r != nil || err == nil || body.closes != 1 {
			t.Fatalf("body=%s request=%v error=%v closes=%d", wire, r, err, body.closes)
		}
	}
}

func TestAPIKeyOverrideMovesOnlyResolvedValue(t *testing.T) {
	a, err := outbound.Configure(chatAdapter{}, &provider.Provider{Auth: provider.Auth{Scheme: provider.AuthAPIKey, KeyEnv: "UNREAD_ENV", HeaderName: "X-Custom-Key"}})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{"x-api-key": {"resolved-key"}, "x-custom-key": {"stale"}, "X-App": {"keep"}}
	wantHeaders := headers.Clone()
	r, err := a.BuildRequest(t.Context(), &ir.Request{}, adapter.Target{BaseURL: "https://up.invalid", Model: "m", Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := r.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if r.Header.Get("X-Custom-Key") != "resolved-key" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-App") != "keep" {
		t.Fatalf("headers=%v", r.Header)
	}
	if !reflect.DeepEqual(headers, wantHeaders) {
		t.Fatal("shared headers mutated")
	}
}

func TestNonStreamingResponseHookUsesRequestContext(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(t.Context(), contextKey{}, "caller")
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://up.invalid", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	a := outbound.Wrap(chatAdapter{}, outbound.Hooks{Response: func(hookCtx context.Context, resp *http.Response) error {
		if hookCtx.Value(contextKey{}) != "caller" {
			return errors.New("response hook detached from request context")
		}
		return nil
	}})
	resp := &http.Response{Request: r, Body: io.NopCloser(strings.NewReader(`{"id":"id","model":"m","choices":[{"message":{"content":"ok"}}]}`))}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := a.ParseResponse(resp)
	if err != nil || result == nil || result.Content[0].Text != "ok" {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestEndpointOverrideDoesNotEscapeResolvedTarget(t *testing.T) {
	a, err := outbound.Configure(chatAdapter{}, &provider.Provider{EndpointPaths: map[ir.Protocol]string{ir.ProtocolOpenAIChat: "/api/chat"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"https://user:secret@up.invalid", "https://up.invalid/prefix?key=secret", "https://up.invalid/prefix#fragment", "https://up.invalid/a/../prefix", "https://up.invalid/%2e%2e/prefix", "https://up.invalid/prefix\\escape"} {
		if r, err := a.BuildRequest(t.Context(), &ir.Request{}, adapter.Target{BaseURL: base, Model: "m"}); err == nil {
			if r != nil {
				if err := r.Body.Close(); err != nil {
					t.Error(err)
				}
			}
			t.Errorf("unsafe resolved target accepted: %s", base)
		}
	}
}

func TestCustomInvalidRegistrationCannotInstallAdapter(t *testing.T) {
	for _, name := range []string{"", "../escape", "UPPER", "a/b"} {
		t.Run(name, func(t *testing.T) {
			registry := adapter.NewRegistry()
			defer func() {
				if recover() == nil {
					t.Fatal("invalid registration did not panic")
				}
				if _, ok := registry.Outbound(ir.Protocol("custom:" + name)); ok {
					t.Fatal("invalid adapter was installed")
				}
			}()
			outbound.RegisterCustom(registry, name, chatAdapter{})
		})
	}
}
