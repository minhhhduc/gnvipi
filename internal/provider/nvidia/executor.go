package nvidia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/minhhhduc/gnvipi/internal/gnvipi"
	"github.com/minhhhduc/gnvipi/internal/captcha"
	"github.com/minhhhduc/gnvipi/internal/models"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	clipexec "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const providerKey = "nvidia"

// Options configures the Nvidia playground executor.
type Options struct {
	Auto         bool
	FlagCaptcha  string
	Coalesce     time.Duration
	MaxInflight  int
	InflightWait time.Duration
	CaptchaWait  time.Duration
	HTTPClient   *http.Client
	Pool         *captcha.Pool

	// PredictURL optionally overrides models.ModelInfo.PredictEndpoint (tests).
	PredictURL func(models.ModelInfo) string
}

// Executor implements coreauth.ProviderExecutor for NVIDIA playground predict.
type Executor struct {
	auto         bool
	coalesce     time.Duration
	httpClient   *http.Client
	inflight     chan struct{}
	inflightWait time.Duration
	captchaWait  time.Duration
	pool         *captcha.Pool
	predictURL   func(models.ModelInfo) string

	beforeInflightWait func()
	beforeSend         func()

	mu          sync.Mutex
	flagCaptcha string
}

// NewExecutor builds an Executor from Options.
func NewExecutor(opts Options) *Executor {
	e := &Executor{
		auto:         opts.Auto,
		coalesce:     opts.Coalesce,
		httpClient:   opts.HTTPClient,
		inflightWait: opts.InflightWait,
		captchaWait:  opts.CaptchaWait,
		pool:         opts.Pool,
		predictURL:   opts.PredictURL,
		flagCaptcha:  opts.FlagCaptcha,
	}
	if e.httpClient == nil {
		e.httpClient = http.DefaultClient
	}
	if opts.MaxInflight > 0 {
		e.inflight = make(chan struct{}, opts.MaxInflight)
	}
	return e
}

// Identifier returns the provider key.
func (e *Executor) Identifier() string { return providerKey }

// Pool returns the captcha pool (may be nil).
func (e *Executor) Pool() *captcha.Pool { return e.pool }

// PrepareRequest is a no-op for playground auth (captcha is per-request).
func (e *Executor) PrepareRequest(_ *http.Request, _ *coreauth.Auth) error { return nil }

// Refresh returns auth unchanged.
func (e *Executor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}

// HttpRequest injects nothing and executes via the shared client.
func (e *Executor) HttpRequest(ctx context.Context, a *coreauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("nvidia executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, a); err != nil {
		return nil, err
	}
	return e.httpClient.Do(httpReq)
}

// Execute handles non-streaming chat completions against NVIDIA predict.
func (e *Executor) Execute(ctx context.Context, _ *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (clipexec.Response, error) {
	body, info, err := e.preparePayload(req, opts, false)
	if err != nil {
		return clipexec.Response{}, err
	}
	start := time.Now()
	upResp, release, err := e.doPredict(ctx, info, body, opts)
	if err != nil {
		GlobalStats.Observe(modelFrom(body), 0, 0, time.Since(start), 0, false, true)
		return clipexec.Response{}, err
	}
	defer release()
	defer upResp.Body.Close()

	raw, err := io.ReadAll(upResp.Body)
	if err != nil {
		GlobalStats.Observe(modelFrom(body), 0, 0, time.Since(start), 0, false, true)
		return clipexec.Response{}, err
	}
	in, out := scrapeUsage(raw)
	GlobalStats.Observe(modelFrom(body), in, out, time.Since(start), 0, false, false)
	from := sdktranslator.FormatOpenAI
	to := clipexec.ResponseFormatOrSource(opts)
	outPayload := sdktranslator.TranslateNonStream(ctx, from, to, req.Model, opts.OriginalRequest, body, raw, nil)
	return clipexec.Response{Payload: outPayload, Headers: upResp.Header.Clone()}, nil
}

// modelFrom pulls the model id out of the prepared upstream body (for stats;
// falls back to "" when unparseable, never fails the request).
func modelFrom(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// ExecuteStream handles streaming chat completions against NVIDIA predict.
func (e *Executor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, req clipexec.Request, opts clipexec.Options) (*clipexec.StreamResult, error) {
	body, info, err := e.preparePayload(req, opts, true)
	if err != nil {
		return nil, err
	}
	upResp, release, err := e.doPredict(ctx, info, body, opts)
	if err != nil {
		return nil, err
	}

	out := make(chan clipexec.StreamChunk, 16)
	go func() {
		defer close(out)
		defer release()
		defer upResp.Body.Close()

		start := time.Now()
		statModel := modelFrom(body)
		var inTok, outTok uint64
		var sawErr bool
		var firstOut time.Time // when the first real output chunk left us (TTFB)

		from := sdktranslator.FormatOpenAI
		to := clipexec.ResponseFormatOrSource(opts)
		var param any

		emitLine := func(line string) error {
			// Stats: sniff SSE lines for a usage object (include_usage is
			// forced on in preparePayload, so the final chunk carries it).
			if in, out := scrapeUsage([]byte(line)); in != 0 || out != 0 {
				inTok, outTok = in, out
			}
			chunks := sdktranslator.TranslateStream(ctx, from, to, req.Model, opts.OriginalRequest, body, []byte(line), &param)
			for _, chunk := range chunks {
				if len(chunk) == 0 {
					// Skip empty frames: the CLIProxyAPI consumer treats a
					// zero-length payload as end-of-stream and closes the
					// connection, which surfaces to the client as an
					// interrupt. Some SSE lines (keep-alives, role-only
					// deltas, control frames) translate to empty chunks —
					// drop them rather than forward.
					continue
				}
				select {
				case out <- clipexec.StreamChunk{Payload: chunk}:
					if firstOut.IsZero() {
						firstOut = time.Now()
					}
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}

		if err := coalesceSSEEvents(upResp.Body, e.coalesce, emitLine); err != nil && ctx.Err() == nil {
			sawErr = true
			select {
			case out <- clipexec.StreamChunk{Err: err}:
			case <-ctx.Done():
			}
		}
		ttfb := time.Duration(0)
		if !firstOut.IsZero() {
			ttfb = firstOut.Sub(start)
		}
		GlobalStats.Observe(statModel, inTok, outTok, time.Since(start), ttfb, true, sawErr)
	}()

	return &clipexec.StreamResult{Headers: upResp.Header.Clone(), Chunks: out}, nil
}

func (e *Executor) preparePayload(req clipexec.Request, opts clipexec.Options, stream bool) ([]byte, models.ModelInfo, error) {
	from := opts.SourceFormat
	if from == "" {
		from = sdktranslator.FormatOpenAI
	}
	model := req.Model
	payload, err := translateToChat(from, model, req.Payload, stream)
	if err != nil {
		return nil, models.ModelInfo{}, requestErr(http.StatusBadRequest, "invalid json body")
	}

	body, err := NormalizeRequestBody(payload)
	if err != nil {
		return nil, models.ModelInfo{}, requestErr(http.StatusBadRequest, "invalid json body")
	}

	// Validate tools[] before forwarding upstream: NVIDIA returns a generic
	// 400 for malformed tool entries but the diagnostic body is unhelpful
	// (e.g. "Cannot parse function_id with value None"), so we surface a
	// precise message to the caller. Claude Code's tool wiring has
	// occasionally drifted (missing "function" key, missing "name"), so a
	// caller-side mistake would otherwise reach NVIDIA verbatim.
	if err := validateTools(body); err != nil {
		return nil, models.ModelInfo{}, requestErr(http.StatusBadRequest, err.Error())
	}

	body, err = sanitizeToolMessages(body)
	if err != nil {
		return nil, models.ModelInfo{}, requestErr(http.StatusBadRequest, "invalid json body")
	}

	// Ensure stream flag matches the execution mode after translation.
	body, err = forceStreamFlag(body, stream)
	if err != nil {
		return nil, models.ModelInfo{}, requestErr(http.StatusBadRequest, "invalid json body")
	}

	var modelProbe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &modelProbe)
	lookupModel := modelProbe.Model
	if lookupModel == "" {
		lookupModel = model
	}
	log.Printf("nvidia executor got model=%q", lookupModel)
	info, err := models.Lookup(lookupModel)
	if err != nil {
		if uerr, ok := err.(*models.ErrUnknownModel); ok {
			return nil, models.ModelInfo{}, requestErr(http.StatusBadRequest, uerr.Error())
		}
		return nil, models.ModelInfo{}, err
	}

	return body, info, nil
}

func forceStreamFlag(body []byte, stream bool) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	raw["stream"] = stream
	if stream {
		opts, _ := raw["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
			raw["stream_options"] = opts
		}
		if _, ok := opts["include_usage"]; !ok {
			opts["include_usage"] = true
		}
		opts["continuous_usage_stats"] = false
	}
	return json.Marshal(raw)
}

func (e *Executor) doPredict(ctx context.Context, info models.ModelInfo, body []byte, opts clipexec.Options) (*http.Response, func(), error) {
	clientToken := ""
	if opts.Headers != nil {
		clientToken = opts.Headers.Get("nv-captcha-token")
	}
	maxAttempts := 1
	if clientToken == "" && (e.pool != nil || e.auto) {
		maxAttempts = 3
	}

	var release func()
	cleanup := func() {
		if release != nil {
			release()
			release = nil
		}
	}

	endpoint := info.PredictEndpoint()
	if e.predictURL != nil {
		endpoint = e.predictURL(info)
	}

	var upResp *http.Response
	staleLeases := 0
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		token, lease, err := e.resolveCaptcha(ctx, clientToken, attempt == 1)
		if err != nil {
			cleanup()
			return nil, nil, captchaErr(err)
		}

		rel, err := e.acquireInflight(ctx)
		if err != nil {
			if lease != nil {
				lease.Release()
			}
			cleanup()
			return nil, nil, &coreauth.Error{
				Code:       "request_scoped",
				Message:    err.Error(),
				HTTPStatus: http.StatusServiceUnavailable,
			}
		}
		release = rel

		upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			cleanup()
			if lease != nil {
				lease.Release()
			}
			return nil, nil, err
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Accept", "text/event-stream")
		upReq.Header.Set("nv-function-id", info.FunctionID)
		upReq.Header.Set("nv-captcha-token", token)
		upReq.Header.Set("Origin", "https://build.nvidia.com")
		upReq.Header.Set("Referer", "https://build.nvidia.com/")

		if e.beforeSend != nil {
			e.beforeSend()
		}
		if err := ctx.Err(); err != nil {
			cleanup()
			if lease != nil {
				lease.Release()
			}
			return nil, nil, captchaErr(err)
		}
		if lease != nil && !lease.Commit() {
			cleanup()
			staleLeases++
			if staleLeases >= maxAttempts {
				return nil, nil, &coreauth.Error{
					Code:       "request_scoped",
					Message:    "captcha token invalid or expired; retry the request",
					HTTPStatus: http.StatusUnauthorized,
				}
			}
			attempt--
			continue
		}
		upResp, err = e.httpClient.Do(upReq)
		if err != nil {
			cleanup()
			return nil, nil, &coreauth.Error{
				Code:       "upstream_error",
				Message:    fmt.Sprintf("upstream: %v", err),
				HTTPStatus: http.StatusBadGateway,
			}
		}

		if upResp.StatusCode < 400 {
			return upResp, cleanup, nil
		}

		raw, _ := io.ReadAll(io.LimitReader(upResp.Body, 4<<10))
		status := upResp.StatusCode
		_ = upResp.Body.Close()
		upResp = nil
		release()
		release = nil

		retryable := isRetryableCaptchaFailure(status, raw)
		if retryable && attempt < maxAttempts {
			log.Printf("upstream captcha failure status=%d (attempt %d/%d); fetching a fresh token",
				status, attempt, maxAttempts)
			continue
		}
		if retryable {
			return nil, nil, &coreauth.Error{
				Code:       "request_scoped",
				Message:    "captcha token invalid or expired; retry the request",
				HTTPStatus: http.StatusUnauthorized,
			}
		}
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = "upstream request failed"
		}
		log.Printf("upstream error status=%d model=%q body=%.500s", status, info.Slug, msg)
		return nil, nil, &coreauth.Error{
			Code:       "upstream_error",
			Message:    msg,
			HTTPStatus: http.StatusBadGateway,
		}
	}
	return nil, nil, &coreauth.Error{
		Code:       "request_scoped",
		Message:    "captcha token invalid or expired; retry the request",
		HTTPStatus: http.StatusUnauthorized,
	}
}

func (e *Executor) acquireInflight(ctx context.Context) (release func(), err error) {
	if e.inflight == nil {
		return func() {}, nil
	}
	release = func() { <-e.inflight }

	if e.inflightWait <= 0 {
		select {
		case e.inflight <- struct{}{}:
			return release, nil
		default:
			return nil, fmt.Errorf("max in-flight upstream streams reached; retry later")
		}
	}

	timer := time.NewTimer(e.inflightWait)
	defer timer.Stop()
	if e.beforeInflightWait != nil {
		e.beforeInflightWait()
	}
	select {
	case e.inflight <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, fmt.Errorf("max in-flight upstream streams reached; retry later")
	case <-ctx.Done():
		return nil, fmt.Errorf("client cancelled before a stream slot opened")
	}
}

func (e *Executor) resolveCaptcha(ctx context.Context, clientToken string, allowFlag bool) (string, *captcha.TokenLease, error) {
	if clientToken != "" {
		return clientToken, nil, nil
	}

	if allowFlag {
		e.mu.Lock()
		flagToken := e.flagCaptcha
		if flagToken != "" {
			e.flagCaptcha = ""
		}
		e.mu.Unlock()
		if flagToken != "" {
			return flagToken, nil, nil
		}
	}

	if e.pool != nil {
		takeCtx := ctx
		var cancel context.CancelFunc
		if e.captchaWait > 0 {
			takeCtx, cancel = context.WithTimeout(ctx, e.captchaWait)
			defer cancel()
		}
		if e.pool.Ready() == 0 {
			waitFor := "indefinitely"
			if e.captchaWait > 0 {
				waitFor = e.captchaWait.String()
			}
			log.Printf("captcha pool empty; waiting up to %s (errors will surface from workers)", waitFor)
		}
		lease, err := e.pool.TakeLease(takeCtx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				fills, takes, errs, expired := e.pool.Stats()
				return "", nil, fmt.Errorf("captcha pool empty after %s (ready=%d fills=%d takes=%d errors=%d expired=%d); retry later",
					e.captchaWait, e.pool.Ready(), fills, takes, errs, expired)
			}
			return "", nil, err
		}
		token := lease.Token()
		return token, lease, nil
	}
	if e.auto {
		token, err := captcha.Extract(ctx)
		return token, nil, err
	}
	return "", nil, fmt.Errorf("captcha token required: send nv-captcha-token, or restart with -captcha / -auto")
}

func captchaErr(err error) error {
	status := http.StatusUnauthorized
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "captcha pool empty after") || strings.Contains(err.Error(), "captcha token required") {
		status = http.StatusServiceUnavailable
	} else if errors.Is(err, context.Canceled) {
		status = http.StatusRequestTimeout
	}
	return &coreauth.Error{
		Code:       "request_scoped",
		Message:    err.Error(),
		HTTPStatus: status,
	}
}

func requestErr(status int, msg string) error {
	return &coreauth.Error{
		Code:       "request_scoped",
		Message:    msg,
		HTTPStatus: status,
	}
}

// isRetryableCaptchaFailure reports whether an upstream 4xx is a captcha /
// hCaptcha token failure fixed by fetching a fresh token and retrying.
func isRetryableCaptchaFailure(status int, raw []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	if len(raw) == 0 {
		return false
	}
	var er gnvipi.ErrorResponse
	if json.Unmarshal(raw, &er) == nil {
		desc := strings.ToLower(er.RequestStatus.StatusDescription)
		if strings.Contains(desc, "token is invalid") || strings.Contains(desc, "invalid token") {
			return true
		}
		if er.RequestStatus.StatusCode == "INVALID_REQUEST" && strings.Contains(desc, "token") {
			return true
		}
	}
	low := strings.ToLower(string(raw))
	if strings.Contains(low, "token is invalid") {
		return true
	}
	return strings.Contains(low, "captcha") || strings.Contains(low, "hcaptcha")
}

// sanitizeToolMessages coerces role:"tool" content into the shape NVIDIA's
// playground accepts: a string, an array of {text,image_url,video_url} parts,
// or absent. Upstream translators can leak an object or non-conforming part
// (e.g. Claude tool_result with object content), which NVIDIA rejects with
// "tool message `content` must be a string..." — so wrap anything else in a
// JSON string, exactly what NVIDIA's error message prescribes.
func sanitizeToolMessages(body []byte) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return body, nil
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return nil, err
	}

	changed := false
	for i, m := range msgs {
		role, _ := m["role"]
		if string(role) != `"tool"` {
			continue
		}
		content, ok := m["content"]
		if !ok || string(content) == "null" {
			continue
		}
		if toolContentOK(content) {
			continue
		}
		wrapped, err := json.Marshal(string(content))
		if err != nil {
			return nil, err
		}
		msgs[i]["content"] = wrapped
		changed = true
	}
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(msgs)
	if err != nil {
		return nil, err
	}
	raw["messages"] = out
	return json.Marshal(raw)
}

// toolContentOK reports whether a role:"tool" content value already matches
// NVIDIA's accepted shapes: JSON string, or array of parts whose type is
// text / image_url / video_url (unknown part types rejected upstream).
func toolContentOK(content json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(content, &v); err != nil {
		return false
	}
	switch c := v.(type) {
	case string:
		return true
	case []any:
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				return false
			}
			switch pm["type"] {
			case "text", "image_url", "video_url":
			default:
				return false
			}
		}
		return true
	}
	return false
}

// validateTools walks body.tools[] and rejects entries that NVIDIA would
// reject with an opaque 400. Returns nil if the key is absent or empty
// (callers may legitimately send no tools).
func validateTools(body []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("tools: body is not valid JSON: %w", err)
	}
	tools, ok := raw["tools"].([]any)
	if !ok || len(tools) == 0 {
		return nil
	}
	for i, e := range tools {
		em, ok := e.(map[string]any)
		if !ok {
			return fmt.Errorf("tools[%d]: expected JSON object, got %T", i, e)
		}
		if typ, _ := em["type"].(string); typ != "function" {
			return fmt.Errorf("tools[%d]: type=%q (want \"function\")", i, typ)
		}
		fn, ok := em["function"].(map[string]any)
		if !ok {
			return fmt.Errorf("tools[%d]: missing \"function\" object", i)
		}
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("tools[%d]: function.name is empty", i)
		}
	}
	return nil
}
