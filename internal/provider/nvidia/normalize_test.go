package nvidia

import (
	"encoding/json"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/models"
)

func TestNormalizeRequestBody(t *testing.T) {
	in := []byte(`{"stream":true,"messages":[]}`)
	out, err := NormalizeRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	opts := raw["stream_options"].(map[string]any)
	if opts["continuous_usage_stats"] != false {
		t.Fatalf("got %#v", opts["continuous_usage_stats"])
	}
	if _, ok := raw["chat_template_kwargs"]; ok {
		t.Fatalf("thinking kwargs should not be injected without a supported model: %#v", raw)
	}
}

func TestNormalizeRequestBodyEmptyOrNullKwargs(t *testing.T) {
	cases := []string{
		`{"model":"qwen/qwen3-next-80b-a3b-instruct","stream":false,"chat_template_kwargs":{}}`,
		`{"model":"qwen/qwen3-next-80b-a3b-instruct","stream":false,"chat_template_kwargs":null}`,
	}
	for _, in := range cases {
		out, err := NormalizeRequestBody([]byte(in))
		if err != nil {
			t.Fatalf("in=%s: %v", in, err)
		}
		var raw map[string]any
		if err := json.Unmarshal(out, &raw); err != nil {
			t.Fatal(err)
		}
		if kw, ok := raw["chat_template_kwargs"].(map[string]any); ok && len(kw) != 0 {
			t.Fatalf("in=%s thinking kwargs = %#v", in, kw)
		}
	}
}

func TestNormalizeRequestBodyPreservesThinking(t *testing.T) {
	in := []byte(`{"model":"qwen/qwen3.5-397b-a17b","stream":false,"chat_template_kwargs":{"enable_thinking":false}}`)
	out, err := NormalizeRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	kw := raw["chat_template_kwargs"].(map[string]any)
	if kw["enable_thinking"] != false {
		t.Fatalf("should preserve caller kwargs, got %#v", kw)
	}
}


func TestNormalizeRequestBodyMapsReasoningEffortByModel(t *testing.T) {
	cases := []struct {
		name  string
		model string
		in    string
		key   string
		want  any
	}{
		{name: "deepseek low rounds up to high", model: "deepseek-ai/deepseek-v4-pro", in: "low", key: "reasoning_effort", want: "high"},
		{name: "deepseek medium rounds up to high", model: "deepseek-ai/deepseek-v4-flash", in: "medium", key: "reasoning_effort", want: "high"},
		{name: "deepseek xhigh rounds up to max", model: "deepseek-ai/deepseek-v4-pro", in: "xhigh", key: "reasoning_effort", want: "max"},
		{name: "gpt oss max caps at high", model: "openai/gpt-oss-120b", in: "max", key: "reasoning_effort", want: "high"},
		{name: "mistral low rounds up to high", model: "mistralai/mistral-medium-3.5-128b", in: "low", key: "reasoning_effort", want: "high"},
		{name: "nemotron super medium rounds up to high", model: "nvidia/nemotron-3-super-120b-a12b", in: "medium", key: "reasoning_effort", want: "high"},
		{name: "nemotron ultra low rounds up to medium", model: "nvidia/nemotron-3-ultra-550b-a55b", in: "low", key: "reasoning_effort", want: "medium"},
		{name: "qwen none disables thinking", model: "qwen/qwen3.5-397b-a17b", in: "none", key: "enable_thinking", want: false},
		{name: "qwen high enables thinking", model: "qwen/qwen3.5-397b-a17b", in: "high", key: "enable_thinking", want: true},
		{name: "minimax medium uses adaptive thinking", model: "minimaxai/minimax-m3", in: "medium", key: "thinking_mode", want: "adaptive"},
		{name: "minimax xhigh enables thinking", model: "minimaxai/minimax-m3", in: "xhigh", key: "thinking_mode", want: "enabled"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","reasoning_effort":"` + tc.in + `","stream":false}`)
			out, err := NormalizeRequestBody(body)
			if err != nil {
				t.Fatal(err)
			}

			var raw map[string]any
			if err := json.Unmarshal(out, &raw); err != nil {
				t.Fatal(err)
			}
			kw, ok := raw["chat_template_kwargs"].(map[string]any)
			if !ok {
				t.Fatalf("chat_template_kwargs missing from %s", out)
			}
			if got := kw[tc.key]; got != tc.want {
				t.Fatalf("%s=%#v want %#v (body=%s)", tc.key, got, tc.want, out)
			}
			if _, ok := raw["reasoning_effort"]; ok {
				t.Fatalf("reasoning_effort alias was not consumed: %s", out)
			}
		})
	}
}

func TestNormalizeRequestBodyDoesNotInjectThinkingIntoUnsupportedModel(t *testing.T) {
	out, err := NormalizeRequestBody([]byte(`{"model":"qwen/qwen3-next-80b-a3b-instruct","messages":[],"stream":false}`))
	if err != nil {
		t.Fatal(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["chat_template_kwargs"]; ok {
		t.Fatalf("unsupported model received thinking kwargs: %s", out)
	}
}

func TestReasoningProfilesReferenceRegisteredModels(t *testing.T) {
	for model := range reasoningProfiles {
		if _, err := models.Lookup(model); err != nil {
			t.Errorf("reasoning profile references unsupported model %q: %v", model, err)
		}
	}
}
