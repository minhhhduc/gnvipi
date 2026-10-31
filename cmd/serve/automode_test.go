package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
)

func TestAutoModePreservesClaudePayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{autoModel: "local/verifier"}
	e := gin.New()
	e.Use(claudeAutoModelAlias(p))
	e.POST("/v1/messages", func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		c.Data(http.StatusOK, "application/json", body)
	})
	for _, model := range []string{"automode", "claude-sonnet-5", "zenmux/atria-asi/atria-dawn-preview"} {
		body := `{
  "model": "` + model + `", "system": [{"type":"text","text":"Original Claude policy"}],
  "messages": [{"role":"user","content":[{"type":"text","text":"<transcript>earlier context and pending command</transcript>"}]}],
  "stop_sequences": ["</block>","</severity>"], "max_tokens": 8192,
  "thinking": {"type":"enabled","budget_tokens":4096}, "temperature": 0.7,
  "tools": [{"name":"Bash"}], "tool_choice":{"type":"auto"}, "output_config":{"effort":"high"},
  "custom": {"model":"automode","number":1.00}, "stream":true
}`
		want := body
		if model == "automode" {
			want = strings.Replace(body, `"model": "automode"`, `"model": "local/verifier"`, 1)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Fatalf("model %s: status=%d body=%s; want=%s", model, w.Code, w.Body.String(), want)
		}
	}
}

func TestAutoModeMappedToCyclePreservesPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := &modelPrefs{autoModel: "cycle", cycle: &cycleConfig{Name: "cycle", Models: []string{"first", "second"}}}
	e := gin.New()
	e.Use(claudeAutoModelAlias(p), cycleRouter(p, e))
	body := `{"model":"automode","system":"Original policy","max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":4096},"stop_sequences":["</block>"],"messages":[{"role":"user","content":"<transcript>earlier context and pending command</transcript>"}]}`
	var calls []string
	e.POST("/v1/messages", func(c *gin.Context) {
		received, _ := io.ReadAll(c.Request.Body)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(received, &payload); err != nil {
			t.Fatal(err)
		}
		var model string
		_ = json.Unmarshal(payload["model"], &model)
		calls = append(calls, model)
		var want map[string]json.RawMessage
		_ = json.Unmarshal([]byte(body), &want)
		delete(payload, "model")
		delete(want, "model")
		gotJSON, _ := json.Marshal(payload)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("cycle changed payload: %s", received)
		}
		if model == "first" {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		c.String(http.StatusOK, "<block>no</block>")
	})
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if w.Code != http.StatusOK || w.Body.String() != "<block>no</block>" || strings.Join(calls, ",") != "first,second" {
		t.Fatalf("status=%d body=%s calls=%v", w.Code, w.Body.String(), calls)
	}
}

func TestAutoModelStatsRecordTargetOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"compat", "messages", "responses", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			oldCompat, oldMessages := compatClient, messagesOnlyClient
			t.Cleanup(func() { compatClient, messagesOnlyClient = oldCompat, oldMessages })
			prov := customProvider{Name: "auto-stats", Model: kind, BaseURL: "https://example.invalid", MessagesOnly: kind == "messages", ResponsesOnly: kind == "responses"}
			p := &modelPrefs{autoModel: prov.modelID(), providers: []customProvider{prov}}
			client := &http.Client{Transport: testRoundTripper(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				var got struct{ Model string }
				_ = json.Unmarshal(body, &got)
				if got.Model != kind {
					t.Fatalf("upstream model=%q", got.Model)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_1","output":[],"usage":{"input_tokens":7,"output_tokens":3}}`))}, nil
			})}
			compatClient, messagesOnlyClient = client, client
			e := gin.New()
			e.Use(claudeAutoModelAlias(p), anthropicResponsesPassthrough(p), sanitizeClaudeToolResults(), customOpenAIResponsesPassthrough(p), customOpenAICompatPassthrough(p), messagesOnlyPassthrough(p), requestLogger(p))
			e.POST("/v1/messages", func(c *gin.Context) {
				c.Data(http.StatusOK, "application/json", []byte(`{"usage":{"input_tokens":7,"output_tokens":3}}`))
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"automode","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
			if kind == "canceled" {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			e.ServeHTTP(httptest.NewRecorder(), req)
			var rows []nvidia.ModelStats
			for _, row := range nvidia.GlobalStats.Snapshot() {
				if row.Model == p.autoModel {
					rows = append(rows, row)
				}
			}
			in, out := uint64(7), uint64(3)
			if kind == "canceled" {
				in, out = 0, 0
			}
			if len(rows) != 1 || rows[0].Model != p.autoModel || rows[0].Requests != 1 || rows[0].InputTok != in || rows[0].OutputTok != out {
				t.Fatalf("stats=%+v", rows)
			}
			if (rows[0].Errors == 1) != (kind == "canceled") {
				t.Fatalf("errors=%d", rows[0].Errors)
			}
		})
	}
}
