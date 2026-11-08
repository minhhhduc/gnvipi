package dispatch_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	"github.com/minhhhduc/gnvipi/internal/gateway/gatewaytest"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gwpolicy "github.com/minhhhduc/gnvipi/internal/gateway/policy"
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

func TestImageCrossProtocolMatrix(t *testing.T) {
	chatUpstreamResponse := `{
		"id": "chatcmpl_img_test",
		"object": "chat.completion",
		"model": "upstream-model",
		"choices": [{
			"index": 0,
			"message": {"role": "assistant", "content": "I see the image"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 15, "completion_tokens": 5, "total_tokens": 20}
	}`

	anthropicUpstreamResponse := `{
		"id": "msg_img_test",
		"type": "message",
		"role": "assistant",
		"model": "upstream-model",
		"content": [{"type": "text", "text": "I see the image"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 15, "output_tokens": 5}
	}`

	responsesUpstreamResponse := `{
		"id": "resp_img_test",
		"object": "response",
		"status": "completed",
		"model": "upstream-model",
		"output": [{
			"id": "msg_img_0",
			"type": "message",
			"status": "completed",
			"role": "assistant",
			"content": [{"type": "output_text", "text": "I see the image"}]
		}],
		"usage": {"input_tokens": 15, "output_tokens": 5, "total_tokens": 20}
	}`

	const (
		testBase64    = "aGVsbG8=" // "hello" in base64
		testDataURL   = "data:image/png;base64,aGVsbG8="
		testRemoteURL = "https://example.com/photo.png"
	)

	// Inbound payloads for Base64 image
	chatPayloadBase64 := `{
		"model": "client-test-model",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "Look at this"},
				{"type": "image_url", "image_url": {"url": "` + testDataURL + `"}}
			]
		}],
		"max_tokens": 50
	}`

	anthropicPayloadBase64 := `{
		"model": "client-test-model",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "Look at this"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "` + testBase64 + `"}}
			]
		}],
		"max_tokens": 50
	}`

	responsesPayloadBase64 := `{
		"model": "client-test-model",
		"input": [{
			"role": "user",
			"content": [
				{"type": "input_text", "text": "Look at this"},
				{"type": "input_image", "image_url": "` + testDataURL + `"}
			]
		}],
		"max_output_tokens": 50
	}`

	// Inbound payloads for URL image
	chatPayloadURL := `{
		"model": "client-test-model",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "Look at this"},
				{"type": "image_url", "image_url": {"url": "` + testRemoteURL + `"}}
			]
		}],
		"max_tokens": 50
	}`

	anthropicPayloadURL := `{
		"model": "client-test-model",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "Look at this"},
				{"type": "image", "source": {"type": "url", "url": "` + testRemoteURL + `"}}
			]
		}],
		"max_tokens": 50
	}`

	responsesPayloadURL := `{
		"model": "client-test-model",
		"input": [{
			"role": "user",
			"content": [
				{"type": "input_text", "text": "Look at this"},
				{"type": "input_image", "image_url": "` + testRemoteURL + `"}
			]
		}],
		"max_output_tokens": 50
	}`

	// Check upstream received Base64 image
	checkChatUpstreamBase64 := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text,omitempty"`
					ImageURL *struct {
						URL string `json:"url"`
					} `json:"image_url,omitempty"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal chat upstream: %v\n%s", err, body)
		}
		if len(req.Messages) == 0 || len(req.Messages[0].Content) < 2 {
			t.Fatalf("expected message with at least 2 content parts, got %+v", req.Messages)
		}
		imgPart := req.Messages[0].Content[1]
		if imgPart.Type != "image_url" || imgPart.ImageURL == nil || imgPart.ImageURL.URL != testDataURL {
			t.Errorf("image_url = %+v, want url: %q", imgPart.ImageURL, testDataURL)
		}
	}

	checkAnthropicUpstreamBase64 := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type   string `json:"type"`
					Text   string `json:"text,omitempty"`
					Source *struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source,omitempty"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal anthropic upstream: %v\n%s", err, body)
		}
		if len(req.Messages) == 0 || len(req.Messages[0].Content) < 2 {
			t.Fatalf("expected message with at least 2 content blocks, got %+v", req.Messages)
		}
		imgBlock := req.Messages[0].Content[1]
		if imgBlock.Type != "image" || imgBlock.Source == nil {
			t.Fatalf("expected image block with source, got %+v", imgBlock)
		}
		if imgBlock.Source.Type != "base64" || imgBlock.Source.MediaType != "image/png" || imgBlock.Source.Data != testBase64 {
			t.Errorf("source = %+v, want base64 image/png data=%q", imgBlock.Source, testBase64)
		}
	}

	checkResponsesUpstreamBase64 := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Input []struct {
				Role    string `json:"role"`
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text,omitempty"`
					ImageURL string `json:"image_url,omitempty"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal responses upstream: %v\n%s", err, body)
		}
		if len(req.Input) == 0 || len(req.Input[0].Content) < 2 {
			t.Fatalf("expected input with at least 2 parts, got %+v", req.Input)
		}
		imgPart := req.Input[0].Content[1]
		if imgPart.Type != "input_image" || imgPart.ImageURL != testDataURL {
			t.Errorf("input_image = %+v, want %q", imgPart, testDataURL)
		}
	}

	// Check upstream received URL image
	checkChatUpstreamURL := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type     string `json:"type"`
					ImageURL *struct {
						URL string `json:"url"`
					} `json:"image_url,omitempty"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal chat upstream: %v\n%s", err, body)
		}
		if len(req.Messages) == 0 || len(req.Messages[0].Content) < 2 {
			t.Fatalf("expected message with at least 2 parts, got %+v", req.Messages)
		}
		imgPart := req.Messages[0].Content[1]
		if imgPart.Type != "image_url" || imgPart.ImageURL == nil || imgPart.ImageURL.URL != testRemoteURL {
			t.Errorf("image_url = %+v, want url: %q", imgPart.ImageURL, testRemoteURL)
		}
	}

	checkAnthropicUpstreamURL := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type   string `json:"type"`
					Source *struct {
						Type string `json:"type"`
						URL  string `json:"url"`
					} `json:"source,omitempty"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal anthropic upstream: %v\n%s", err, body)
		}
		if len(req.Messages) == 0 || len(req.Messages[0].Content) < 2 {
			t.Fatalf("expected message with at least 2 blocks, got %+v", req.Messages)
		}
		imgBlock := req.Messages[0].Content[1]
		if imgBlock.Type != "image" || imgBlock.Source == nil || imgBlock.Source.Type != "url" || imgBlock.Source.URL != testRemoteURL {
			t.Errorf("source = %+v, want url: %q", imgBlock.Source, testRemoteURL)
		}
	}

	checkResponsesUpstreamURL := func(t *testing.T, body string) {
		t.Helper()
		var req struct {
			Input []struct {
				Role    string `json:"role"`
				Content []struct {
					Type     string `json:"type"`
					ImageURL string `json:"image_url,omitempty"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("unmarshal responses upstream: %v\n%s", err, body)
		}
		if len(req.Input) == 0 || len(req.Input[0].Content) < 2 {
			t.Fatalf("expected input with at least 2 parts, got %+v", req.Input)
		}
		imgPart := req.Input[0].Content[1]
		if imgPart.Type != "input_image" || imgPart.ImageURL != testRemoteURL {
			t.Errorf("input_image = %+v, want %q", imgPart, testRemoteURL)
		}
	}

	type imageTestCase struct {
		name                 string
		inboundProto         ir.Protocol
		inboundPath          string
		upstreamProto        ir.Protocol
		upstreamPath         string
		clientPayload        string
		cannedResponse       string
		checkUpstreamRequest func(t *testing.T, body string)
	}

	matrixCases := []imageTestCase{
		// --- Base64 image tests for each protocol pair ---
		{
			name:                 "Chat->Anthropic [Base64]",
			inboundProto:         ir.ProtocolOpenAIChat,
			inboundPath:          gatewaytest.RouteChat,
			upstreamProto:        ir.ProtocolAnthropicMessages,
			upstreamPath:         gatewaytest.RouteMessages,
			clientPayload:        chatPayloadBase64,
			cannedResponse:       anthropicUpstreamResponse,
			checkUpstreamRequest: checkAnthropicUpstreamBase64,
		},
		{
			name:                 "Chat->Responses [Base64]",
			inboundProto:         ir.ProtocolOpenAIChat,
			inboundPath:          gatewaytest.RouteChat,
			upstreamProto:        ir.ProtocolOpenAIResponses,
			upstreamPath:         gatewaytest.RouteResponses,
			clientPayload:        chatPayloadBase64,
			cannedResponse:       responsesUpstreamResponse,
			checkUpstreamRequest: checkResponsesUpstreamBase64,
		},
		{
			name:                 "Anthropic->Chat [Base64]",
			inboundProto:         ir.ProtocolAnthropicMessages,
			inboundPath:          gatewaytest.RouteMessages,
			upstreamProto:        ir.ProtocolOpenAIChat,
			upstreamPath:         gatewaytest.RouteChat,
			clientPayload:        anthropicPayloadBase64,
			cannedResponse:       chatUpstreamResponse,
			checkUpstreamRequest: checkChatUpstreamBase64,
		},
		{
			name:                 "Anthropic->Responses [Base64]",
			inboundProto:         ir.ProtocolAnthropicMessages,
			inboundPath:          gatewaytest.RouteMessages,
			upstreamProto:        ir.ProtocolOpenAIResponses,
			upstreamPath:         gatewaytest.RouteResponses,
			clientPayload:        anthropicPayloadBase64,
			cannedResponse:       responsesUpstreamResponse,
			checkUpstreamRequest: checkResponsesUpstreamBase64,
		},
		{
			name:                 "Responses->Chat [Base64]",
			inboundProto:         ir.ProtocolOpenAIResponses,
			inboundPath:          gatewaytest.RouteResponses,
			upstreamProto:        ir.ProtocolOpenAIChat,
			upstreamPath:         gatewaytest.RouteChat,
			clientPayload:        responsesPayloadBase64,
			cannedResponse:       chatUpstreamResponse,
			checkUpstreamRequest: checkChatUpstreamBase64,
		},
		{
			name:                 "Responses->Anthropic [Base64]",
			inboundProto:         ir.ProtocolOpenAIResponses,
			inboundPath:          gatewaytest.RouteResponses,
			upstreamProto:        ir.ProtocolAnthropicMessages,
			upstreamPath:         gatewaytest.RouteMessages,
			clientPayload:        responsesPayloadBase64,
			cannedResponse:       anthropicUpstreamResponse,
			checkUpstreamRequest: checkAnthropicUpstreamBase64,
		},

		// --- URL image tests for each protocol pair ---
		{
			name:                 "Chat->Anthropic [URL]",
			inboundProto:         ir.ProtocolOpenAIChat,
			inboundPath:          gatewaytest.RouteChat,
			upstreamProto:        ir.ProtocolAnthropicMessages,
			upstreamPath:         gatewaytest.RouteMessages,
			clientPayload:        chatPayloadURL,
			cannedResponse:       anthropicUpstreamResponse,
			checkUpstreamRequest: checkAnthropicUpstreamURL,
		},
		{
			name:                 "Chat->Responses [URL]",
			inboundProto:         ir.ProtocolOpenAIChat,
			inboundPath:          gatewaytest.RouteChat,
			upstreamProto:        ir.ProtocolOpenAIResponses,
			upstreamPath:         gatewaytest.RouteResponses,
			clientPayload:        chatPayloadURL,
			cannedResponse:       responsesUpstreamResponse,
			checkUpstreamRequest: checkResponsesUpstreamURL,
		},
		{
			name:                 "Anthropic->Chat [URL]",
			inboundProto:         ir.ProtocolAnthropicMessages,
			inboundPath:          gatewaytest.RouteMessages,
			upstreamProto:        ir.ProtocolOpenAIChat,
			upstreamPath:         gatewaytest.RouteChat,
			clientPayload:        anthropicPayloadURL,
			cannedResponse:       chatUpstreamResponse,
			checkUpstreamRequest: checkChatUpstreamURL,
		},
		{
			name:                 "Anthropic->Responses [URL]",
			inboundProto:         ir.ProtocolAnthropicMessages,
			inboundPath:          gatewaytest.RouteMessages,
			upstreamProto:        ir.ProtocolOpenAIResponses,
			upstreamPath:         gatewaytest.RouteResponses,
			clientPayload:        anthropicPayloadURL,
			cannedResponse:       responsesUpstreamResponse,
			checkUpstreamRequest: checkResponsesUpstreamURL,
		},
		{
			name:                 "Responses->Chat [URL]",
			inboundProto:         ir.ProtocolOpenAIResponses,
			inboundPath:          gatewaytest.RouteResponses,
			upstreamProto:        ir.ProtocolOpenAIChat,
			upstreamPath:         gatewaytest.RouteChat,
			clientPayload:        responsesPayloadURL,
			cannedResponse:       chatUpstreamResponse,
			checkUpstreamRequest: checkChatUpstreamURL,
		},
		{
			name:                 "Responses->Anthropic [URL]",
			inboundProto:         ir.ProtocolOpenAIResponses,
			inboundPath:          gatewaytest.RouteResponses,
			upstreamProto:        ir.ProtocolAnthropicMessages,
			upstreamPath:         gatewaytest.RouteMessages,
			clientPayload:        responsesPayloadURL,
			cannedResponse:       anthropicUpstreamResponse,
			checkUpstreamRequest: checkAnthropicUpstreamURL,
		},
	}

	for _, tc := range matrixCases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := gatewaytest.New(t)
			upstream.Script(tc.upstreamPath, gatewaytest.Step{
				Status: http.StatusOK,
				Body:   tc.cannedResponse,
			})

			prov := provider.Provider{
				ID:       "test-provider",
				Name:     "Test Provider",
				Protocol: tc.upstreamProto,
				BaseURL:  upstream.URL(),
				Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret-key"},
			}

			resolver := staticResolver{
				routes: map[string]routing.Route{
					"client-test-model": {
						Kind:             routing.Custom,
						Provider:         &prov,
						UpstreamModel:    "upstream-model",
						UpstreamProtocol: tc.upstreamProto,
					},
				},
			}

			recorder := newMatrixRecorder()
			dispatcher := dispatch.New(resolver, &relay.Options{Recorder: recorder})
			dispatcher.Registry = setupMatrixRegistry()

			engine := gin.New()
			engine.Use(dispatcher.Handle)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.inboundPath, strings.NewReader(tc.clientPayload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			engine.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200. Body: %s", w.Code, w.Body.String())
			}

			requests := upstream.Requests()
			if len(requests) == 0 {
				t.Fatalf("expected at least 1 upstream request, got 0")
			}
			tc.checkUpstreamRequest(t, requests[0].Body)
		})
	}
}

func TestImageCapabilityPolicyAndLimits(t *testing.T) {
	const (
		testBase64    = "aGVsbG8="
		testDataURL   = "data:image/png;base64,aGVsbG8="
		testRemoteURL = "https://example.com/photo.png"
	)

	anthropicCannedResponse := `{
		"id": "msg_test",
		"type": "message",
		"role": "assistant",
		"model": "upstream-model",
		"content": [{"type": "text", "text": "ok"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 2}
	}`

	setupDispatcher := func(t *testing.T, caps adapter.ImageCapability, mode gwpolicy.Mode, maxBytes int64) (*gin.Engine, *gatewaytest.Upstream) {
		upstream := gatewaytest.New(t)
		upstream.Script(gatewaytest.RouteMessages, gatewaytest.Step{
			Status: http.StatusOK,
			Body:   anthropicCannedResponse,
		})

		prov := provider.Provider{
			ID:       "img-provider",
			Name:     "Image Provider",
			Protocol: ir.ProtocolAnthropicMessages,
			BaseURL:  upstream.URL(),
			Auth:     provider.Auth{Scheme: provider.AuthBearer, Key: "secret"},
		}

		resolver := staticResolver{
			routes: map[string]routing.Route{
				"img-model": {
					Kind:             routing.Custom,
					Provider:         &prov,
					UpstreamModel:    "upstream-model",
					UpstreamProtocol: ir.ProtocolAnthropicMessages,
				},
			},
		}

		dispatcher := dispatch.New(resolver, &relay.Options{Recorder: newMatrixRecorder()})
		dispatcher.Registry = setupMatrixRegistry()
		dispatcher.FeaturePolicy = gwpolicy.Config{
			ProviderModes: map[string]gwpolicy.Mode{
				"img-provider": mode,
			},
			ProviderOverrides: map[string]gwpolicy.CapabilityOverrides{
				"img-provider": {
					Images:        &caps,
					MaxImageBytes: &maxBytes,
				},
			},
		}

		engine := gin.New()
		engine.Use(dispatcher.Handle)
		return engine, upstream
	}

	t.Run("capability base64 drops remote URL in lenient mode", func(t *testing.T) {
		engine, upstream := setupDispatcher(t, adapter.ImageCapabilityBase64, gwpolicy.ModeLenient, 0)

		payload := `{
			"model": "img-model",
			"messages": [{
				"role": "user",
				"content": [
					{"type": "text", "text": "Keep this"},
					{"type": "image_url", "image_url": {"url": "` + testRemoteURL + `"}}
				]
			}]
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. Body: %s", w.Code, w.Body.String())
		}
		if dropped := w.Header().Get(gwpolicy.DroppedParamsHeader); dropped != "images" {
			t.Errorf("header %s = %q, want 'images'", gwpolicy.DroppedParamsHeader, dropped)
		}

		// Verify upstream received only the text part, URL image was dropped
		reqs := upstream.Requests()
		if len(reqs) == 0 {
			t.Fatalf("expected upstream request")
		}
		if strings.Contains(reqs[0].Body, testRemoteURL) {
			t.Errorf("expected URL image to be dropped from upstream request: %s", reqs[0].Body)
		}
	})

	t.Run("capability base64 preserves base64 image in lenient mode", func(t *testing.T) {
		engine, upstream := setupDispatcher(t, adapter.ImageCapabilityBase64, gwpolicy.ModeLenient, 0)

		payload := `{
			"model": "img-model",
			"messages": [{
				"role": "user",
				"content": [
					{"type": "text", "text": "Keep this"},
					{"type": "image_url", "image_url": {"url": "` + testDataURL + `"}}
				]
			}]
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. Body: %s", w.Code, w.Body.String())
		}
		if dropped := w.Header().Get(gwpolicy.DroppedParamsHeader); dropped != "" {
			t.Errorf("expected no dropped header, got %q", dropped)
		}

		reqs := upstream.Requests()
		if len(reqs) == 0 || !strings.Contains(reqs[0].Body, testBase64) {
			t.Errorf("expected base64 image preserved: %s", reqs[0].Body)
		}
	})

	t.Run("capability base64 rejects URL image in strict mode", func(t *testing.T) {
		engine, _ := setupDispatcher(t, adapter.ImageCapabilityBase64, gwpolicy.ModeStrict, 0)

		payload := `{
			"model": "img-model",
			"messages": [{
				"role": "user",
				"content": [
					{"type": "text", "text": "Keep this"},
					{"type": "image_url", "image_url": {"url": "` + testRemoteURL + `"}}
				]
			}]
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400. Body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("capability url drops base64 in lenient mode", func(t *testing.T) {
		engine, upstream := setupDispatcher(t, adapter.ImageCapabilityURL, gwpolicy.ModeLenient, 0)

		payload := `{
			"model": "img-model",
			"messages": [{
				"role": "user",
				"content": [
					{"type": "text", "text": "Keep this"},
					{"type": "image_url", "image_url": {"url": "` + testDataURL + `"}}
				]
			}]
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. Body: %s", w.Code, w.Body.String())
		}
		if dropped := w.Header().Get(gwpolicy.DroppedParamsHeader); dropped != "images" {
			t.Errorf("header = %q, want 'images'", dropped)
		}

		reqs := upstream.Requests()
		if len(reqs) == 0 || strings.Contains(reqs[0].Body, testBase64) {
			t.Errorf("expected base64 image dropped: %s", reqs[0].Body)
		}
	})

	t.Run("capability none drops all images in lenient mode", func(t *testing.T) {
		engine, upstream := setupDispatcher(t, adapter.ImageCapabilityNone, gwpolicy.ModeLenient, 0)

		payload := `{
			"model": "img-model",
			"messages": [{
				"role": "user",
				"content": [
					{"type": "text", "text": "Only text survives"},
					{"type": "image_url", "image_url": {"url": "` + testDataURL + `"}},
					{"type": "image_url", "image_url": {"url": "` + testRemoteURL + `"}}
				]
			}]
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. Body: %s", w.Code, w.Body.String())
		}
		if dropped := w.Header().Get(gwpolicy.DroppedParamsHeader); dropped != "images" {
			t.Errorf("header = %q, want 'images'", dropped)
		}

		reqs := upstream.Requests()
		if len(reqs) == 0 || strings.Contains(reqs[0].Body, "image_url") {
			t.Errorf("expected all images dropped: %s", reqs[0].Body)
		}
	})

	t.Run("image size exceeding max_image_bytes returns 400", func(t *testing.T) {
		// Set max bytes to 2 bytes; "hello" decoded is 5 bytes
		engine, _ := setupDispatcher(t, adapter.ImageCapabilityBoth, gwpolicy.ModeLenient, 2)

		payload := `{
			"model": "img-model",
			"messages": [{
				"role": "user",
				"content": [
					{"type": "text", "text": "Check size"},
					{"type": "image_url", "image_url": {"url": "` + testDataURL + `"}}
				]
			}]
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for image exceeding size limit. Body: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "exceeds maximum allowed size") {
			t.Errorf("expected size limit error message, got: %s", w.Body.String())
		}
	})

	t.Run("invalid media type returns 400", func(t *testing.T) {
		engine, _ := setupDispatcher(t, adapter.ImageCapabilityBoth, gwpolicy.ModeLenient, 0)

		payload := `{
			"model": "img-model",
			"messages": [{
				"role": "user",
				"content": [
					{"type": "text", "text": "Check type"},
					{"type": "image_url", "image_url": {"url": "data:application/pdf;base64,aGVsbG8="}}
				]
			}]
		}`

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, gatewaytest.RouteChat, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for invalid media type. Body: %s", w.Code, w.Body.String())
		}
	})
}
