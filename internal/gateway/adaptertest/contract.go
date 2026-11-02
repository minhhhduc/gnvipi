package adaptertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	"github.com/minhhhduc/gnvipi/internal/gateway/sse"
)

// RegisterFlags enables -update before test argument parsing, normally in TestMain.
// Reuse a flag owned by the adopting package rather than registering during init.
func RegisterFlags() {
	if flag.Lookup("update") == nil {
		flag.Bool("update", false, "update adapter contract wire goldens")
	}
}

func updating() bool {
	update := flag.Lookup("update")
	return update != nil && update.Value.String() == "true"
}

type wireInput struct {
	Method  string      `json:"method,omitempty"`
	URL     string      `json:"url,omitempty"`
	Status  int         `json:"status,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Body    string      `json:"body"`
}
type decodeCase struct {
	Name    string    `json:"name"`
	Request wireInput `json:"request"`
	IR      string    `json:"ir,omitempty"`
	Error   string    `json:"error,omitempty"`
}
type encodeCase struct {
	Name   string `json:"name"`
	IR     string `json:"ir"`
	Golden string `json:"golden"`
}
type encodeErrorCase struct {
	Name   string `json:"name"`
	Error  string `json:"error"`
	Golden string `json:"golden"`
}
type inboundFixtures struct {
	Decode    []decodeCase      `json:"decode"`
	Responses []encodeCase      `json:"responses"`
	Streams   []encodeCase      `json:"streams"`
	Errors    []encodeErrorCase `json:"errors"`
}
type buildCase struct {
	Name   string         `json:"name"`
	IR     string         `json:"ir"`
	Target adapter.Target `json:"target"`
	Golden string         `json:"golden"`
}
type parseCase struct {
	Name     string    `json:"name"`
	Response wireInput `json:"response"`
	IR       string    `json:"ir,omitempty"`
	Error    string    `json:"error,omitempty"`
}
type outboundFixtures struct {
	Requests  []buildCase `json:"requests"`
	Responses []parseCase `json:"responses"`
	Streams   []parseCase `json:"streams"`
}
type errorFixture struct {
	Kind              gwerr.Kind `json:"kind"`
	Message           string     `json:"message"`
	Status            int        `json:"status,omitempty"`
	Code              string     `json:"code,omitempty"`
	UpstreamStatus    int        `json:"upstream_status,omitempty"`
	UpstreamRequestID string     `json:"upstream_request_id,omitempty"`
	RetryAfterMS      int64      `json:"retry_after_ms,omitempty"`
}

func (e *errorFixture) gatewayError() *gwerr.Error {
	return &gwerr.Error{Kind: e.Kind, Message: e.Message, Status: e.Status, Code: e.Code,
		UpstreamStatus: e.UpstreamStatus, UpstreamRequestID: e.UpstreamRequestID, RetryAfter: time.Duration(e.RetryAfterMS) * time.Millisecond}
}

type requestGolden struct {
	Method  string          `json:"method"`
	URL     string          `json:"url"`
	Headers http.Header     `json:"headers"`
	Body    json.RawMessage `json:"body"`
}
type responseGolden struct {
	Status  int             `json:"status"`
	Headers http.Header     `json:"headers"`
	Body    json.RawMessage `json:"body"`
}

// RunInboundContract checks the inbound adapter against inbound.json in
// fixturesDir. See the package documentation for the fixture schema.
func RunInboundContract(t *testing.T, a adapter.Inbound, fixturesDir string) {
	t.Helper()
	f, err := loadInbound(fixturesDir)
	must(t, err)
	if a == nil {
		t.Fatal("nil inbound adapter")
	}
	for _, c := range f.Decode {
		t.Run("decode/"+c.Name, func(t *testing.T) {
			body := readBytes(t, resolve(fixturesDir, c.Request.Body))
			method, url := c.Request.Method, c.Request.URL
			if method == "" {
				method = http.MethodPost
			}
			if url == "" {
				url = "http://client.invalid/"
			}
			req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
			must(t, err)
			defer func(body io.ReadCloser) { must(t, body.Close()) }(req.Body)
			req.Header = c.Request.Headers.Clone()
			got, err := a.DecodeRequest(req)
			if c.Error != "" {
				must(t, compareError(err, readError(t, resolve(fixturesDir, c.Error))))
				if got != nil {
					t.Fatal("DecodeRequest returned IR alongside an error")
				}
				return
			}
			must(t, err)
			var want ir.Request
			must(t, readStrict(resolve(fixturesDir, c.IR), &want))
			must(t, compareValue(got, &want))
		})
	}
	for _, c := range f.Responses {
		t.Run("response/"+c.Name, func(t *testing.T) {
			var input ir.Response
			must(t, readStrict(resolve(fixturesDir, c.IR), &input))
			got, err := a.EncodeResponse(&input)
			must(t, err)
			must(t, compareGolden(resolve(fixturesDir, c.Golden), got, false, updating()))
		})
	}
	for _, c := range f.Streams {
		t.Run("stream/"+c.Name, func(t *testing.T) {
			events, err := readEvents(resolve(fixturesDir, c.IR))
			must(t, err)
			input := &fixtureStream{events: events}
			recorder := httptest.NewRecorder()
			writer, err := sse.NewWriter(recorder)
			must(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			must(t, a.EncodeStream(ctx, input, writer))
			if input.afterTerminal {
				t.Fatal("EncodeStream read after the terminal event")
			}
			if !input.terminal {
				t.Fatal("EncodeStream did not consume the terminal event")
			}
			must(t, compareGolden(resolve(fixturesDir, c.Golden), recorder.Body.Bytes(), true, updating()))
		})
	}
	// Inbound has no EncodeError method: gwerr.Write is the existing shared
	// encoder selected by the adapter's Protocol, not a fabricated adapter API.
	for _, c := range f.Errors {
		t.Run("error/"+c.Name, func(t *testing.T) {
			e := readError(t, resolve(fixturesDir, c.Error))
			recorder := httptest.NewRecorder()
			must(t, gwerr.Write(recorder, gwerr.Protocol(a.Protocol()), e.gatewayError()))
			got, err := json.Marshal(responseGolden{Status: recorder.Code, Headers: recorder.Header(), Body: recorder.Body.Bytes()})
			must(t, err)
			must(t, compareGolden(resolve(fixturesDir, c.Golden), got, false, updating()))
		})
	}
}

// RunOutboundContract checks the outbound adapter against outbound.json in
// fixturesDir, including normalized error mapping and stream ownership.
func RunOutboundContract(t *testing.T, a adapter.Outbound, fixturesDir string) {
	t.Helper()
	f, err := loadOutbound(fixturesDir)
	must(t, err)
	if a == nil {
		t.Fatal("nil outbound adapter")
	}
	for _, c := range f.Requests {
		t.Run("request/"+c.Name, func(t *testing.T) {
			var input ir.Request
			must(t, readStrict(resolve(fixturesDir, c.IR), &input))
			target := c.Target
			target.Headers = c.Target.Headers.Clone()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := a.BuildRequest(ctx, &input, target)
			if req != nil && req.Body != nil {
				defer func(body io.ReadCloser) { must(t, body.Close()) }(req.Body)
			}
			must(t, err)
			if req == nil || req.URL == nil || req.Body == nil {
				t.Fatal("BuildRequest returned an incomplete HTTP request")
			}
			if !reflect.DeepEqual(target.Headers, c.Target.Headers) {
				t.Fatal("BuildRequest mutated Target.Headers")
			}
			for key, values := range c.Target.Headers {
				if !reflect.DeepEqual(req.Header.Values(key), values) {
					t.Fatalf("BuildRequest did not preserve target header %q", key)
				}
			}
			body, err := io.ReadAll(req.Body)
			must(t, err)
			got, err := json.Marshal(requestGolden{Method: req.Method, URL: req.URL.String(), Headers: req.Header, Body: body})
			must(t, err)
			must(t, compareGolden(resolve(fixturesDir, c.Golden), got, false, updating()))
		})
	}
	for _, c := range f.Responses {
		t.Run("response/"+c.Name, func(t *testing.T) {
			resp, _ := fixtureResponse(t, fixturesDir, c.Response)
			defer func() { must(t, resp.Body.Close()) }()
			got, err := a.ParseResponse(resp)
			if c.Error != "" {
				must(t, compareError(err, readError(t, resolve(fixturesDir, c.Error))))
				if got != nil {
					t.Fatal("ParseResponse returned IR alongside an error")
				}
				return
			}
			must(t, err)
			var want ir.Response
			must(t, readStrict(resolve(fixturesDir, c.IR), &want))
			must(t, compareValue(got, &want))
		})
	}
	for _, c := range f.Streams {
		t.Run("stream/"+c.Name, func(t *testing.T) {
			resp, body := fixtureResponse(t, fixturesDir, c.Response)
			defer func() { must(t, resp.Body.Close()) }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := a.ParseStream(ctx, resp)
			if stream != nil {
				// This later defer checks stream ownership before the fallback body close.
				defer func() {
					if err := stream.Close(); err != nil {
						t.Error(err)
					}
					if !body.closed {
						t.Error("Stream.Close did not close the upstream response body")
					}
				}()
			}
			if c.Error != "" {
				if stream != nil {
					t.Fatal("ParseStream returned a stream alongside an error")
				}
				must(t, compareError(err, readError(t, resolve(fixturesDir, c.Error))))
				return
			}
			must(t, err)
			if stream == nil {
				t.Fatal("ParseStream returned nil stream")
			}
			want, err := readEvents(resolve(fixturesDir, c.IR))
			must(t, err)
			var got []ir.StreamEvent
			for {
				ev, err := stream.Next(ctx)
				if errors.Is(err, io.EOF) {
					break
				}
				must(t, err)
				got = append(got, ev)
				if len(got) > len(want)+1 {
					t.Fatal("stream failed to end after its expected terminal event")
				}
			}
			must(t, validateEvents(got))
			must(t, compareValue(got, want))
		})
	}
}

func loadInbound(dir string) (inboundFixtures, error) {
	var f inboundFixtures
	if err := readStrict(resolve(dir, "inbound.json"), &f); err != nil {
		return f, err
	}
	if len(f.Decode) == 0 || len(f.Responses) == 0 || len(f.Streams) == 0 || len(f.Errors) == 0 {
		return f, fmt.Errorf("inbound fixture requires decode, responses, streams and errors cases")
	}
	success, failure := false, false
	for _, c := range f.Decode {
		if err := validCase(c.Name, c.Request.Body); err != nil {
			return f, err
		}
		if (c.IR == "") == (c.Error == "") {
			return f, fmt.Errorf("decode %q requires exactly one of ir or error", c.Name)
		}
		success = success || c.IR != ""
		failure = failure || c.Error != ""
	}
	if !success || !failure {
		return f, fmt.Errorf("decode fixtures require both success and error cases")
	}
	for _, cases := range [][]encodeCase{f.Responses, f.Streams} {
		for _, c := range cases {
			if err := validCase(c.Name, c.IR, c.Golden); err != nil {
				return f, err
			}
		}
	}
	for _, c := range f.Errors {
		if err := validCase(c.Name, c.Error, c.Golden); err != nil {
			return f, err
		}
	}
	if err := validateStreamCases(dir, f.Streams); err != nil {
		return f, err
	}
	return f, nil
}
func loadOutbound(dir string) (outboundFixtures, error) {
	var f outboundFixtures
	if err := readStrict(resolve(dir, "outbound.json"), &f); err != nil {
		return f, err
	}
	if len(f.Requests) == 0 || len(f.Responses) == 0 || len(f.Streams) == 0 {
		return f, fmt.Errorf("outbound fixture requires requests, responses and streams cases")
	}
	for _, c := range f.Requests {
		if err := validCase(c.Name, c.IR, c.Golden, c.Target.BaseURL, c.Target.Model); err != nil {
			return f, err
		}
	}
	for _, cases := range [][]parseCase{f.Responses, f.Streams} {
		for _, c := range cases {
			if err := validCase(c.Name, c.Response.Body); err != nil {
				return f, err
			}
			if c.Response.Status < 100 || c.Response.Status > 599 {
				return f, fmt.Errorf("case %q has invalid response status", c.Name)
			}
			if (c.IR == "") == (c.Error == "") {
				return f, fmt.Errorf("case %q requires exactly one of ir or error", c.Name)
			}
		}
	}
	success, failure := false, false
	for _, c := range f.Responses {
		success = success || c.IR != ""
		failure = failure || c.Error != ""
	}
	if !success || !failure {
		return f, fmt.Errorf("response fixtures require both success and error cases")
	}
	var streams []encodeCase
	for _, c := range f.Streams {
		if c.IR != "" {
			streams = append(streams, encodeCase{Name: c.Name, IR: c.IR})
		}
	}
	if err := validateStreamCases(dir, streams); err != nil {
		return f, err
	}
	return f, nil
}
func validCase(name string, required ...string) error {
	if name == "" {
		return fmt.Errorf("fixture case has no name")
	}
	for _, field := range required {
		if field == "" {
			return fmt.Errorf("fixture case %q has an empty required field", name)
		}
	}
	return nil
}
func validateStreamCases(dir string, cases []encodeCase) error {
	success, failure := false, false
	for _, c := range cases {
		events, err := readEvents(resolve(dir, c.IR))
		if err != nil {
			return fmt.Errorf("stream fixture %q: %w", c.Name, err)
		}
		success = success || events[len(events)-1].Type == ir.EventMessageStop
		failure = failure || events[len(events)-1].Type == ir.EventError
	}
	if !success || !failure {
		return fmt.Errorf("stream fixtures require both message_stop and error terminal cases")
	}
	return nil
}
func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, filepath.FromSlash(path))
}
func readStrict(path string, dst any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return fmt.Errorf("fixture %s cannot be null", path)
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("fixture %s: %w", path, err)
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("fixture %s has trailing data", path)
	}
	return nil
}
func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	must(t, err)
	return body
}
func readError(t *testing.T, path string) *errorFixture {
	t.Helper()
	var e errorFixture
	must(t, readStrict(path, &e))
	switch e.Kind {
	case gwerr.KindInvalidRequest, gwerr.KindAuthentication, gwerr.KindPermission, gwerr.KindNotFound, gwerr.KindRateLimit, gwerr.KindOverloaded, gwerr.KindTimeout, gwerr.KindUpstreamError, gwerr.KindInternal:
	default:
		t.Fatalf("fixture %s has unknown error kind %q", path, e.Kind)
	}
	if e.Message == "" || e.RetryAfterMS < 0 {
		t.Fatalf("fixture %s has invalid error payload", path)
	}
	return &e
}
func compareError(err error, want *errorFixture) error {
	if err == nil {
		return fmt.Errorf("error mismatch: want *gwerr.Error, got nil")
	}
	var e *gwerr.Error
	if !errors.As(err, &e) {
		return fmt.Errorf("error mismatch: want *gwerr.Error, got %w", err)
	}
	if e == nil {
		return fmt.Errorf("error mismatch: want non-nil *gwerr.Error, got nil *gwerr.Error")
	}
	got := &errorFixture{Kind: e.Kind, Message: e.ScrubbedMessage(), Status: e.Status, Code: e.Code,
		UpstreamStatus: e.UpstreamStatus, UpstreamRequestID: e.UpstreamRequestID, RetryAfterMS: e.RetryAfter.Milliseconds()}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("error mismatch: got %+v, want %+v", got, want)
	}
	return nil
}
func readEvents(path string) ([]ir.StreamEvent, error) {
	var events []ir.StreamEvent
	if err := readStrict(path, &events); err != nil {
		return nil, err
	}
	if err := validateEvents(events); err != nil {
		return nil, fmt.Errorf("fixture %s: %w", path, err)
	}
	return events, nil
}
func validateEvents(events []ir.StreamEvent) error {
	if len(events) > 0 && events[0].Type != ir.EventMessageStart {
		return fmt.Errorf("message_start must be first")
	}
	if err := ir.ValidateEventOrder(events); err != nil {
		return err
	}
	for i, e := range events {
		switch e.Type {
		case ir.EventMessageStart:
			if e.Message == nil {
				return fmt.Errorf("message_start has no message")
			}
		case ir.EventBlockStart:
			if e.Block == nil || e.Index < 0 || e.Index != e.Block.Index {
				return fmt.Errorf("block_start has inconsistent block index or missing block")
			}
		case ir.EventBlockDelta:
			if e.Delta == nil {
				return fmt.Errorf("block_delta has no delta")
			}
		case ir.EventUsageUpdate:
			if e.Usage == nil {
				return fmt.Errorf("usage_update has no usage")
			}
			if i != len(events)-2 {
				return fmt.Errorf("usage_update must be immediately before the terminal event")
			}
		}
	}
	return nil
}

type fixtureStream struct {
	events                  []ir.StreamEvent
	terminal, afterTerminal bool
}

func (s *fixtureStream) Next(ctx context.Context) (ir.StreamEvent, error) {
	if err := ctx.Err(); err != nil {
		return ir.StreamEvent{}, err
	}
	if s.terminal {
		s.afterTerminal = true
		return ir.StreamEvent{}, fmt.Errorf("read after the terminal event")
	}
	if len(s.events) == 0 {
		return ir.StreamEvent{}, io.EOF
	}
	e := s.events[0]
	s.events = s.events[1:]
	s.terminal = e.Type == ir.EventMessageStop || e.Type == ir.EventError
	return e, nil
}
func (*fixtureStream) Close() error { return nil }

type trackedBody struct {
	*bytes.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }
func fixtureResponse(t *testing.T, dir string, in wireInput) (*http.Response, *trackedBody) {
	t.Helper()
	body := &trackedBody{Reader: bytes.NewReader(readBytes(t, resolve(dir, in.Body)))}
	return &http.Response{StatusCode: in.Status, Status: fmt.Sprintf("%d %s", in.Status, http.StatusText(in.Status)),
		Header: in.Headers.Clone(), Body: body}, body
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Normalize numbers as exact rationals, so 1, 1.0 and 1e0 compare equal
// without float64 silently rounding distinct large integer identifiers.
type semanticNumber string

func semanticJSON(body []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("JSON has trailing data")
	}
	return normalizeNumbers(v), nil
}
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		n, ok := new(big.Rat).SetString(string(x))
		if ok {
			return semanticNumber(n.RatString())
		}
		return semanticNumber(x)
	case map[string]any:
		for key, value := range x {
			x[key] = normalizeNumbers(value)
		}
	case []any:
		for i, value := range x {
			x[i] = normalizeNumbers(value)
		}
	}
	return v
}
func compareJSON(got, want []byte) error {
	g, err := semanticJSON(got)
	if err != nil {
		return fmt.Errorf("invalid actual JSON: %w", err)
	}
	w, err := semanticJSON(want)
	if err != nil {
		return fmt.Errorf("invalid expected JSON: %w", err)
	}
	if !reflect.DeepEqual(g, w) {
		return fmt.Errorf("JSON mismatch:\ngot: %s\nwant: %s", got, want)
	}
	return nil
}
func compareValue(got, want any) error {
	g, err := json.Marshal(got)
	if err != nil {
		return err
	}
	w, err := json.Marshal(want)
	if err != nil {
		return err
	}
	return compareJSON(g, w)
}
func parseSSE(body []byte) ([]sse.Event, error) {
	r := sse.NewReader(bytes.NewReader(body))
	defer r.Close()
	var events []sse.Event
	for {
		e, err := r.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("SSE contains no dispatched events")
	}
	return events, nil
}
func compareSSE(got, want []byte) error {
	g, err := parseSSE(got)
	if err != nil {
		return err
	}
	w, err := parseSSE(want)
	if err != nil {
		return err
	}
	if len(g) != len(w) {
		return fmt.Errorf("SSE event count: got %d, want %d", len(g), len(w))
	}
	for i := range g {
		if g[i].Type != w[i].Type || g[i].ID != w[i].ID || g[i].Retry != w[i].Retry {
			return fmt.Errorf("SSE event %d metadata differs", i)
		}
		if json.Valid([]byte(w[i].Data)) {
			if err := compareJSON([]byte(g[i].Data), []byte(w[i].Data)); err != nil {
				return fmt.Errorf("SSE event %d: %w", i, err)
			}
		} else if g[i].Data != w[i].Data {
			return fmt.Errorf("SSE event %d data differs", i)
		}
	}
	return nil
}
func compareGolden(path string, got []byte, stream, update bool) error {
	if stream {
		if _, err := parseSSE(got); err != nil {
			return err
		}
	} else {
		if _, err := semanticJSON(got); err != nil {
			return err
		}
	}
	if update {
		if !stream {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, got, "", "  "); err != nil {
				return err
			}
			got = append(pretty.Bytes(), '\n')
		}
		return os.WriteFile(path, got, 0o644)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("golden %s: %w (use -update to create wire goldens)", path, err)
	}
	if stream {
		err = compareSSE(got, want)
	} else {
		err = compareJSON(got, want)
	}
	if err != nil {
		return fmt.Errorf("golden mismatch %s: %w", path, err)
	}
	return nil
}
