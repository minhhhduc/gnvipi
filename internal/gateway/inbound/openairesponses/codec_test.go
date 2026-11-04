package openairesponses_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/inbound/openairesponses"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
)

var _ adapter.Inbound = (*openairesponses.Adapter)(nil)

func codecFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "openairesponses", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func compareCodecJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var values [2]any
	for i, data := range [][]byte{got, want} {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&values[i]); err != nil {
			t.Fatal(err)
		}
		// Function arguments are JSON encoded inside a wire string. Clients
		// parse them; indentation in an IR fixture is not observable semantics.
		if document, ok := values[i].(map[string]any); ok && document["object"] == "response" {
			items, _ := document["output"].([]any)
			for _, value := range items {
				item, ok := value.(map[string]any)
				if !ok || item["type"] != "function_call" {
					continue
				}
				arguments, ok := item["arguments"].(string)
				if !ok {
					t.Fatal("function_call.arguments must be a string")
				}
				argumentDecoder := json.NewDecoder(strings.NewReader(arguments))
				argumentDecoder.UseNumber()
				var parsed any
				if err := argumentDecoder.Decode(&parsed); err != nil {
					t.Fatal(err)
				}
				item["arguments"] = parsed
			}
		}
	}
	if !reflect.DeepEqual(values[0], values[1]) {
		t.Fatalf("JSON mismatch\ngot: %s\nwant: %s", got, want)
	}
}

// These fixtures catch dropped instructions, reordered conversation items,
// image corruption, lost optional zeros and precision loss in opaque fields.
func TestDecodeResponsesJSONGoldens(t *testing.T) {
	for _, name := range []string{"text", "instructions-images", "tool-reasoning"} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", bytes.NewReader(codecFixture(t, name+".request.json")))
			decoded, err := openairesponses.New().DecodeRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			compareCodecJSON(t, data, codecFixture(t, name+".ir.json"))
		})
	}
}

func TestEncodeResponsesJSONGoldens(t *testing.T) {
	for _, name := range []string{"completed", "incomplete"} {
		t.Run(name, func(t *testing.T) {
			var response ir.Response
			if err := json.Unmarshal(codecFixture(t, name+".response.ir.json"), &response); err != nil {
				t.Fatal(err)
			}
			codec := openairesponses.New()
			codec.Created = 1710000000
			encoded, err := codec.EncodeResponse(&response)
			if err != nil {
				t.Fatal(err)
			}
			compareCodecJSON(t, encoded, codecFixture(t, name+".response.golden.json"))
		})
	}
}

func TestResponsesStatefulTranslationIsRejected(t *testing.T) {
	for _, field := range []string{`"previous_response_id":"resp_previous"`, `"store":true`, `"conversation":{"id":"conv_1"}`} {
		t.Run(field, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"alias","input":"hello",`+field+`}`))
			_, err := openairesponses.New().DecodeRequest(request)
			var gatewayError *gwerr.Error
			if !errors.As(err, &gatewayError) || gatewayError.Code != "unsupported_feature" {
				t.Fatalf("stateful translation error = %v", err)
			}
			out := httptest.NewRecorder()
			if err := gwerr.Write(out, gwerr.ProtocolOpenAIResponses, gatewayError); err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if out.Code != http.StatusBadRequest || envelope.Error.Code != "unsupported_feature" || envelope.Error.Type == "" {
				t.Fatalf("Responses error = %d %s", out.Code, out.Body.String())
			}
		})
	}
}

func TestResponsesStatelessMarkersAndOpaqueFieldsSurvive(t *testing.T) {
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"alias","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello","image_url":17},{"type":"future_part","payload":9007199254740993}]}],"store":false,"previous_response_id":null,"conversation":null,"temperature":null}`))
	decoded, err := openairesponses.New().DecodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Messages, []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock("hello"))}) {
		t.Fatalf("decoded content = %+v", decoded.Messages)
	}
	for field, value := range map[string]string{"store": "false", "previous_response_id": "null", "conversation": "null", "temperature": "null"} {
		if string(decoded.Extensions[field]) != value {
			t.Fatalf("opaque %s = %s, want %s", field, decoded.Extensions[field], value)
		}
	}
	if !bytes.Contains(decoded.Extensions["input"], []byte("9007199254740993")) {
		t.Fatal("opaque future content lost its exact numeric value")
	}
}

func TestResponsesMalformedRequestBoundaries(t *testing.T) {
	for _, body := range []string{
		`{"model":"alias","input":"hello"} {"model":"other"}`,
		`{"model":"alias","input":17}`,
		`{"model":"alias","input":[{"role":"unknown","content":"hello"}]}`,
		`{"model":"alias","input":"hello","max_output_tokens":-1}`,
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(body))
		_, err := openairesponses.New().DecodeRequest(request)
		var gatewayError *gwerr.Error
		if !errors.As(err, &gatewayError) || gatewayError.Kind != gwerr.KindInvalidRequest {
			t.Fatalf("invalid request %s: %v", body, err)
		}
	}
}

func TestResponsesRequiredItemFields(t *testing.T) {
	for _, tc := range []struct{ name, item string }{
		{"message-content-missing", `{"role":"user"}`},
		{"message-content-null", `{"role":"user","content":null}`},
		{"part-type-missing", `{"role":"user","content":[{"text":"hello"}]}`},
		{"part-type-null", `{"role":"user","content":[{"type":null,"text":"hello"}]}`},
		{"call-arguments-missing", `{"type":"function_call","call_id":"call_a","name":"lookup"}`},
		{"call-arguments-null", `{"type":"function_call","call_id":"call_a","name":"lookup","arguments":null}`},
		{"call-output-missing", `{"type":"function_call_output","call_id":"call_a"}`},
		{"call-output-null", `{"type":"function_call_output","call_id":"call_a","output":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"alias","input":[`+tc.item+`]}`))
			_, err := openairesponses.New().DecodeRequest(request)
			var invalid *gwerr.Error
			if !errors.As(err, &invalid) || invalid.Kind != gwerr.KindInvalidRequest || invalid.Status != http.StatusBadRequest {
				t.Fatalf("malformed Responses item did not return 400: %s; error=%v", tc.item, err)
			}
		})
	}
}

func TestResponsesEncodingDoesNotSilentlyDropInvalidBlocks(t *testing.T) {
	for _, block := range []ir.Block{ir.NewImageBlockURL("https://images.invalid/a.png"), ir.NewToolUseBlock("", "lookup", json.RawMessage(`{}`))} {
		response := ir.NewTextResponse("resp_invalid", "model", "", ir.Usage{})
		response.Content = []ir.Block{block}
		if _, err := openairesponses.New().EncodeResponse(&response); err == nil {
			t.Fatalf("unrepresentable response block was silently accepted: %+v", block)
		}
	}
}
