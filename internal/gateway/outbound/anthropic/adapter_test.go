package anthropic_test

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/adaptertest"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
)

func TestMain(m *testing.M) {
	adaptertest.RegisterFlags()
	os.Exit(m.Run())
}

func TestOutboundContract(t *testing.T) {
	a := anthropic.New()
	adaptertest.RunOutboundContract(t, a, "testdata")
	if a.Protocol() != ir.ProtocolAnthropicMessages {
		t.Fatalf("Protocol = %q, want %q", a.Protocol(), ir.ProtocolAnthropicMessages)
	}
	wantCapabilities := adapter.Capabilities{Streaming: true, Images: true, Reasoning: true, TopK: true}
	if got := a.Capabilities(); !reflect.DeepEqual(got, wantCapabilities) {
		t.Fatalf("Capabilities = %+v, want %+v", got, wantCapabilities)
	}
}

func TestDefaultMaxTokensIsConfigurable(t *testing.T) {
	a := anthropic.New()
	a.DefaultMaxTokens = 8192
	request, err := a.BuildRequest(t.Context(), &ir.Request{
		Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
	}, adapter.Target{BaseURL: "https://upstream.invalid", Model: "claude-resolved"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := request.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var body struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %d, want configured default 8192", body.MaxTokens)
	}
}

func TestBuildRequestAllowsStreaming(t *testing.T) {
	a := anthropic.New()
	request, err := a.BuildRequest(t.Context(), &ir.Request{
		Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
		Stream:   true,
	}, adapter.Target{BaseURL: "https://upstream.invalid", Model: "claude-resolved"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := request.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	var body struct {
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Stream {
		t.Fatalf("stream = %v, want true", body.Stream)
	}
}

func TestBuildRequestRejectsUnrepresentableInput(t *testing.T) {
	tests := []struct {
		name    string
		request ir.Request
		message string
	}{
		{
			name: "assistant first",
			request: ir.Request{Messages: []ir.Message{
				ir.NewMessage(ir.RoleAssistant, ir.NewTextBlock("prefill")),
			}},
			message: "first message",
		},
		{
			name: "tools deferred",
			request: ir.Request{
				Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))},
				Tools:    []ir.Tool{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}},
			},
			message: "tool",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := anthropic.New().BuildRequest(t.Context(), &test.request, adapter.Target{
				BaseURL: "https://upstream.invalid", Model: "claude-resolved",
			})
			if request != nil {
				if err := request.Body.Close(); err != nil {
					t.Fatal(err)
				}
				t.Fatal("BuildRequest returned a request alongside an error")
			}
			var gatewayError *gwerr.Error
			if !errors.As(err, &gatewayError) || gatewayError.Kind != gwerr.KindInvalidRequest ||
				gatewayError.Code != "unsupported_feature" || !strings.Contains(strings.ToLower(gatewayError.Message), test.message) {
				t.Fatalf("error = %#v, want unsupported %s error", err, test.message)
			}
		})
	}
}
