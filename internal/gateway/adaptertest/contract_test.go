package adaptertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

const fakeFixtures = "../testdata/adapter-contract/fake"

func TestMain(m *testing.M) {
	RegisterFlags()
	os.Exit(m.Run())
}

func TestConformingAdapters(t *testing.T) {
	t.Run("inbound", func(t *testing.T) { RunInboundContract(t, wireInbound{}, fakeFixtures) })
	t.Run("outbound", func(t *testing.T) { RunOutboundContract(t, wireOutbound{}, fakeFixtures) })
}

func TestNonStreamingOutboundContract(t *testing.T) {
	var fixtures outboundFixtures
	if err := readStrict(resolve(fakeFixtures, "outbound.json"), &fixtures); err != nil {
		t.Fatal(err)
	}
	absolute := func(path string) string {
		path, err := filepath.Abs(resolve(fakeFixtures, path))
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	for i := range fixtures.Requests {
		fixtures.Requests[i].IR = absolute(fixtures.Requests[i].IR)
		fixtures.Requests[i].Golden = absolute(fixtures.Requests[i].Golden)
	}
	for i := range fixtures.Responses {
		fixtures.Responses[i].Response.Body = absolute(fixtures.Responses[i].Response.Body)
		if fixtures.Responses[i].IR != "" {
			fixtures.Responses[i].IR = absolute(fixtures.Responses[i].IR)
		} else {
			fixtures.Responses[i].Error = absolute(fixtures.Responses[i].Error)
		}
	}

	for _, explicitUnsupported := range []bool{false, true} {
		name := "no-stream-cases"
		if explicitUnsupported {
			name = "explicit-unsupported-stream"
		}
		t.Run(name, func(t *testing.T) {
			manifest := fixtures
			manifest.Streams = nil
			dir := t.TempDir()
			if explicitUnsupported {
				errorPath := filepath.Join(dir, "unsupported.json")
				if err := os.WriteFile(errorPath, []byte(`{"kind":"invalid_request","message":"streaming is unsupported","status":400,"code":"unsupported_feature"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				manifest.Streams = []parseCase{{Name: "unsupported", Response: wireInput{Status: http.StatusOK, Body: absolute("stream.sse")}, Error: errorPath}}
			}
			body, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "outbound.json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			RunOutboundContract(t, nonStreamingOutbound{}, dir)
		})
	}
}

type nonStreamingOutbound struct{ wireOutbound }

func (nonStreamingOutbound) Capabilities() adapter.Capabilities { return adapter.Capabilities{} }
func (nonStreamingOutbound) ParseStream(context.Context, *http.Response) (adapter.Stream, error) {
	return nil, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Status: http.StatusBadRequest, Code: "unsupported_feature", Message: "streaming is unsupported"}
}

// Re-exec the test binary, not the go command, so expected contract failures
// cannot poison the parent test's testing.T or require a compiler at runtime.
func TestViolatingAdapters(t *testing.T) {
	for _, tc := range []struct{ mode, diagnostic string }{
		{"missing-terminal", "no terminal event"},
		{"wrong-index", "unopened block"},
		{"duplicate-usage", "duplicate usage_update"},
		{"early-usage", "usage_update must be immediately before"},
		{"after-terminal", "after terminal event"},
		{"no-start", "message_start must be first"},
		{"unclosed-body", "Close did not close"},
		{"wrong-request", "golden mismatch"},
		{"wrong-response", "JSON mismatch"},
		{"wrong-error", "error mismatch"},
		{"inbound-early-return", "did not consume the terminal event"},
		{"inbound-after-terminal", "read after the terminal event"},
		{"inbound-missing-output", "golden mismatch"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestViolatingAdapterChild$", "-test.v")
			cmd.Env = append(os.Environ(), "ADAPTER_CONTRACT_VIOLATION="+tc.mode)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !strings.Contains(string(out), tc.diagnostic) {
				t.Fatalf("want contract rejection containing %q, got %v:\n%s", tc.diagnostic, err, out)
			}
		})
	}
}

func TestViolatingAdapterChild(t *testing.T) {
	mode := os.Getenv("ADAPTER_CONTRACT_VIOLATION")
	if mode == "" {
		t.Skip("only run in the isolated violation subprocess")
	}
	if strings.HasPrefix(mode, "inbound-") {
		RunInboundContract(t, wireInbound{mode: mode}, fakeFixtures)
	} else {
		RunOutboundContract(t, wireOutbound{mode: mode}, fakeFixtures)
	}
}

func TestSemanticJSON(t *testing.T) {
	for _, tc := range []struct {
		name, left, right string
		equal             bool
	}{
		{"objects and numeric spelling", `{"a":1,"b":{"x":2}}`, `{"b":{"x":2.0},"a":1e0}`, true},
		{"large integers stay exact", `{"n":9007199254740992}`, `{"n":9007199254740993}`, false},
		{"array order matters", `[1,2]`, `[2,1]`, false},
		{"number is not string", `1`, `"1"`, false},
		{"null is not empty array", `null`, `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compareJSON([]byte(tc.left), []byte(tc.right))
			if (err == nil) != tc.equal {
				t.Fatalf("compareJSON = %v, want equal=%v", err, tc.equal)
			}
		})
	}
	for _, malformed := range []string{`{"x":`, `{} {}`, `{"x":NaN}`} {
		if err := compareJSON([]byte(malformed), []byte(malformed)); err == nil {
			t.Fatalf("accepted malformed JSON %q", malformed)
		}
	}
}

func TestMalformedFixtures(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"decode":[],"typo":true}`, `{} {}`, `{"decode":"wrong"}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "inbound.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadInbound(dir); err == nil {
			t.Fatalf("accepted fixture %s", body)
		}
	}
	for _, body := range []string{`null`, `[]`, `[{"type":"message_start"}]`, `[{"type":"message_start","typo":1}]`} {
		path := filepath.Join(t.TempDir(), "events.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readEvents(path); err == nil {
			t.Fatalf("accepted event fixture %s", body)
		}
	}
}

func TestGoldenUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response.json")
	if err := compareGolden(path, []byte(`{"answer":1}`), false, false); err == nil {
		t.Fatal("missing golden accepted without -update")
	}
	if err := compareGolden(path, []byte(`{"answer":1}`), false, true); err != nil {
		t.Fatal(err)
	}
	if err := compareGolden(path, []byte(`{"answer":2}`), false, false); err == nil {
		t.Fatal("changed answer accepted")
	}
	if err := compareGolden(path, []byte(`{"answer":2}`), false, true); err != nil {
		t.Fatal(err)
	}
	if err := compareGolden(path, []byte(`{"answer":2.0}`), false, false); err != nil {
		t.Fatal(err)
	}
	if err := compareGolden(path, []byte(`not JSON`), false, true); err == nil {
		t.Fatal("-update blessed malformed output")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareJSON(body, []byte(`{"answer":2}`)); err != nil {
		t.Fatal("malformed update overwrote the valid golden")
	}
}

func TestSSEGoldenSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.sse")
	want := "event: token\ndata: {\"b\":2,\"a\":1}\n\ndata: [DONE]\n\n"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ": keepalive\r\nevent: token\r\ndata: {\"a\":1.0,\"b\":2}\r\n\r\ndata: [DONE]\r\n\r\n"
	if err := compareGolden(path, []byte(got), true, false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"event: token\ndata: {\"a\":1,\"b\":2}\n\n",
		"data: [DONE]\n\nevent: token\ndata: {\"a\":1,\"b\":2}\n\n",
		"event: other\ndata: {\"a\":1,\"b\":2}\n\ndata: [DONE]\n\n",
	} {
		if err := compareGolden(path, []byte(bad), true, false); err == nil {
			t.Fatal("accepted changed SSE event sequence")
		}
	}
}

// The fake wire format is deliberately not IR: prompts become user blocks,
// completions become assistant blocks, and named SSE frames expand to IR
// lifecycle events. Fixture goldens are independently authored wire payloads.
type wireInbound struct{ mode string }

func (wireInbound) Protocol() ir.Protocol { return ir.ProtocolOpenAIChat }
func (wireInbound) DecodeRequest(r *http.Request) (*ir.Request, error) {
	var p struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Prompt == "" {
		return nil, &gwerr.Error{Kind: gwerr.KindInvalidRequest, Message: "prompt required"}
	}
	return &ir.Request{Model: p.Model, Messages: []ir.Message{ir.NewMessage(ir.RoleUser, ir.NewTextBlock(p.Prompt))}}, nil
}
func (wireInbound) EncodeResponse(r *ir.Response) ([]byte, error) {
	return json.Marshal(struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		Completion string `json:"completion"`
		Tokens     int    `json:"tokens"`
	}{r.ID, r.Model, r.Content[0].Text, r.Usage.OutputTokens})
}
func (f wireInbound) EncodeStream(ctx context.Context, events adapter.Stream, w *sse.Writer) error {
	for {
		ev, err := events.Next(ctx)
		if err != nil {
			return err
		}
		if f.mode == "inbound-early-return" {
			return nil
		}
		var frame sse.Event
		switch ev.Type {
		case ir.EventMessageStart:
			b, err := json.Marshal(map[string]string{"id": ev.Message.ID, "model": ev.Message.Model})
			if err != nil {
				return err
			}
			frame = sse.Event{Type: "start", Data: string(b)}
		case ir.EventBlockDelta:
			b, err := json.Marshal(map[string]string{"text": ev.Delta.Text})
			if err != nil {
				return err
			}
			frame = sse.Event{Type: "token", Data: string(b)}
		case ir.EventUsageUpdate:
			b, err := json.Marshal(map[string]int{"tokens": ev.Usage.OutputTokens})
			if err != nil {
				return err
			}
			frame = sse.Event{Type: "usage", Data: string(b)}
		case ir.EventMessageStop:
			if ev.StopReason != ir.StopEndTurn {
				return fmt.Errorf("unsupported fake stop reason %q", ev.StopReason)
			}
			frame = sse.Event{Type: "end", Data: `{"finish":"complete"}`}
		case ir.EventError:
			b, err := json.Marshal(map[string]string{"kind": ev.Error.Kind, "message": ev.Error.Message})
			if err != nil {
				return err
			}
			frame = sse.Event{Type: "failure", Data: string(b)}
		default:
			continue
		}
		terminal := ev.Type == ir.EventMessageStop || ev.Type == ir.EventError
		if !terminal || f.mode != "inbound-missing-output" {
			if err := w.WriteEvent(frame); err != nil {
				return err
			}
		}
		if terminal {
			if f.mode == "inbound-after-terminal" {
				if _, err := events.Next(ctx); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

type wireOutbound struct{ mode string }

func (wireOutbound) Protocol() ir.Protocol              { return ir.ProtocolOpenAIChat }
func (wireOutbound) Capabilities() adapter.Capabilities { return adapter.Capabilities{Streaming: true} }
func (f wireOutbound) BuildRequest(ctx context.Context, r *ir.Request, target adapter.Target) (*http.Request, error) {
	model := target.Model
	if f.mode == "wrong-request" {
		model = "ignored-target"
	}
	body, err := json.Marshal(map[string]string{"model": model, "prompt": r.Messages[0].Content[0].Text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.BaseURL+"/complete", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header = target.Headers.Clone()
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}
func (f wireOutbound) ParseResponse(r *http.Response) (*ir.Response, error) {
	if r.StatusCode >= 400 {
		var p struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			return nil, err
		}
		kind := gwerr.KindFromHTTPStatus(r.StatusCode)
		if f.mode == "wrong-error" {
			kind = gwerr.KindInternal
		}
		return nil, &gwerr.Error{Kind: kind, Message: p.Message, UpstreamStatus: r.StatusCode}
	}
	var p struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		Completion string `json:"completion"`
		Tokens     int    `json:"tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return nil, err
	}
	if f.mode == "wrong-response" {
		p.Completion = "lost content"
	}
	out := ir.NewTextResponse(p.ID, p.Model, p.Completion, ir.Usage{OutputTokens: p.Tokens})
	return &out, nil
}
func (f wireOutbound) ParseStream(ctx context.Context, r *http.Response) (adapter.Stream, error) {
	reader := sse.NewReader(r.Body)
	defer reader.Close()
	var events []ir.StreamEvent
	blockClosed := false
	for {
		frame, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch frame.Type {
		case "start":
			var p struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &p); err != nil {
				return nil, err
			}
			events = append(events, ir.NewMessageStart(p.ID, ir.RoleAssistant, p.Model, ir.Usage{}), ir.NewBlockStart(0, ir.NewTextBlock("")))
		case "token":
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &p); err != nil {
				return nil, err
			}
			events = append(events, ir.NewTextDelta(0, p.Text))
		case "usage":
			var p struct {
				Tokens int `json:"tokens"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &p); err != nil {
				return nil, err
			}
			if !blockClosed {
				events = append(events, ir.NewBlockStop(0))
				blockClosed = true
			}
			events = append(events, ir.NewUsageUpdate(ir.Usage{OutputTokens: p.Tokens}))
		case "end":
			var p struct {
				Finish string `json:"finish"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &p); err != nil {
				return nil, err
			}
			if p.Finish != "complete" {
				return nil, fmt.Errorf("unsupported fake finish %q", p.Finish)
			}
			if !blockClosed {
				events = append(events, ir.NewBlockStop(0))
				blockClosed = true
			}
			events = append(events, ir.NewMessageStop(ir.StopEndTurn))
		case "failure":
			var p struct {
				Kind    string `json:"kind"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(frame.Data), &p); err != nil {
				return nil, err
			}
			events = append(events, ir.NewErrorEvent(p.Kind, p.Message))
		default:
			return nil, fmt.Errorf("unknown frame %q", frame.Type)
		}
	}
	switch f.mode {
	case "missing-terminal":
		events = events[:len(events)-1]
	case "wrong-index":
		events[2].Index = 1
	case "duplicate-usage":
		events = append(events[:len(events)-1], ir.NewUsageUpdate(ir.Usage{OutputTokens: 2}), events[len(events)-1])
	case "early-usage":
		if events[len(events)-1].Type == ir.EventMessageStop {
			usage := events[len(events)-2]
			events = append(events[:len(events)-2], events[len(events)-1])
			ordered := append([]ir.StreamEvent{}, events[:2]...)
			ordered = append(ordered, usage)
			events = append(ordered, events[2:]...)
		}
	case "after-terminal":
		events = append(events, ir.NewTextDelta(0, "late"))
	case "no-start":
		events = events[1:]
	}
	return &wireStream{events: events, body: r.Body, leaveOpen: f.mode == "unclosed-body"}, nil
}

type wireStream struct {
	events    []ir.StreamEvent
	body      io.Closer
	leaveOpen bool
}

func (s *wireStream) Next(ctx context.Context) (ir.StreamEvent, error) {
	if err := ctx.Err(); err != nil {
		return ir.StreamEvent{}, err
	}
	if len(s.events) == 0 {
		return ir.StreamEvent{}, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}
func (s *wireStream) Close() error {
	if s.leaveOpen {
		return nil
	}
	return s.body.Close()
}
