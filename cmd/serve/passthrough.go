package main

// Middleware that forwards requests for "messages-only" custom providers
// straight to the upstream's /v1/messages endpoint. Some upstreams sit behind
// a WAF that blocks /v1/chat/completions — the path CLIProxyAPI's OpenAI-compat
// executor always calls — while allowing /v1/messages. For those, the gateway
// relays the Anthropic-format request byte-for-byte instead of translating.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/provider/nvidia"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// messagesOnlyDialTLS performs the TLS handshake with a Chrome ClientHello so
// WAFs that fingerprint Go's default TLS stack see a browser instead. The Chrome
// spec's ALPN negotiates h2, which pairs with the http2.Transport below —
// matching what a real Chrome does (Chrome ClientHello + HTTP/2).
func messagesOnlyDialTLS(network, addr string, _ *tls.Config) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(addr)
	uconn := utls.UClient(raw, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return uconn, nil
}

// messagesOnlyClient is shared by all passthrough forwards. No timeout on the
// client itself; streams stay open as long as the upstream sends.
var messagesOnlyClient = &http.Client{
	Transport: &http2.Transport{
		DialTLS: messagesOnlyDialTLS,
	},
}

// compatClient serves the OpenAI-compat passthrough: plain transport with ALPN,
// so h1-only upstreams (tokenrouter) don't get h2 frames pasted onto HTTP/1.1.
var compatClient = &http.Client{}

// customOpenAICompatPassthrough routes custom providers directly. CLIProxyAPI's
// compatibility registry can lag behind the admin file watcher; the local model
// catalog must not advertise an alias that then fails with "unknown provider".
func customOpenAICompatPassthrough(p *modelPrefs) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost || c.Request.URL.Path != "/v1/chat/completions" {
			c.Next()
			return
		}
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			c.Abort()
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		var payload map[string]any
		if json.Unmarshal(body, &payload) != nil {
			c.Next()
			return
		}
		model, _ := payload["model"].(string)
		prov := lookupCustomProvider(p, model)
		if prov == nil && customModelHidden(p, model) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model is hidden"})
			c.Abort()
			return
		}
		if prov == nil || prov.MessagesOnly {
			c.Next()
			return
		}
		payload["model"] = prov.Model
		body, err = json.Marshal(payload)
		if err != nil {
			c.Next()
			return
		}

		target := providerEndpoint(prov.BaseURL, "/chat/completions")
		start := time.Now()
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			statFail(model, start)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			c.Abort()
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if prov.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+prov.APIKey)
		}
		if v := c.Request.Header.Get("User-Agent"); v != "" {
			req.Header.Set("User-Agent", v)
		}
		resp, err := compatClient.Do(req)
		if err != nil {
			statFail(model, start)
			c.JSON(http.StatusBadGateway, gin.H{"error": "upstream: " + err.Error()})
			c.Abort()
			return
		}
		defer resp.Body.Close()
		copyProxyResponse(c, resp, model, start)
		c.Abort()
	}
}

func providerEndpoint(baseURL, suffix string) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + suffix
	}
	return baseURL + "/v1" + suffix
}

// statTee forwards every byte to dst unchanged while keeping the first
// statTeeCap bytes for stats scraping. Streaming to the client is unaffected.
type statTee struct {
	dst io.Writer
	buf bytes.Buffer
}

const statTeeCap = 256 << 10

func (t *statTee) Write(p []byte) (int, error) {
	if n := t.buf.Len(); n < statTeeCap {
		head := p
		if len(head) > statTeeCap-n {
			head = head[:statTeeCap-n]
		}
		t.buf.Write(head)
	}
	return t.dst.Write(p)
}

// statFail records a passthrough call that never produced an upstream body
// (dial failure): 0 tokens, flagged as an error.
func statFail(model string, start time.Time) {
	nvidia.GlobalStats.RecordResponse(model, nil, time.Since(start), false, true)
}

// statCopyDone feeds the captured head of a proxied response to the collector.
func statCopyDone(model string, start time.Time, t *statTee, status int, streamed bool, copyErr error) {
	isErr := status >= 400 || copyErr != nil && !errors.Is(copyErr, io.EOF)
	nvidia.GlobalStats.RecordResponse(model, t.buf.Bytes(), time.Since(start), streamed, isErr)
}

func copyProxyResponse(c *gin.Context, resp *http.Response, model string, start time.Time) {
	for k, vv := range resp.Header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vv {
			c.Writer.Header().Add(k, v)
		}
	}
	if c.Writer.Header().Get("Content-Type") == "" {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Writer.WriteHeader(resp.StatusCode)
	streamed := isEventStream(resp.Header.Get("Content-Type"))
	tee := &statTee{dst: c.Writer}
	var copyErr error
	if !streamed {
		_, copyErr = io.Copy(tee, resp.Body)
	} else {
		flusher, ok := c.Writer.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := tee.Write(buf[:n]); werr != nil {
					copyErr = werr
					break
				}
				if ok {
					flusher.Flush()
				}
			}
			if rerr != nil {
				copyErr = rerr
				break
			}
		}
	}
	statCopyDone(model, start, tee, resp.StatusCode, streamed, copyErr)
}

// claudeDefaultModelAlias rewrites requests whose model is a built-in Claude
// model name (e.g. "claude-sonnet-5") — sent by Claude Code helpers that ignore
// the user's /model choice, e.g. auto-mode permission checks — to the gateway's
// default playground model so they route instead of 502ing on unknown model.
// ponytail: rewrites only /v1/messages; add /v1/chat/completions if helpers ever hit it too.
func claudeDefaultModelAlias(defaultModel string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost ||
			!strings.HasPrefix(c.Request.URL.Path, "/v1/messages") {
			c.Next()
			return
		}
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			c.Abort()
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &payload) == nil &&
			strings.HasPrefix(payload.Model, "claude-") {
			if rewritten, changed := rewriteJSONModel(body, payload.Model, defaultModel); changed {
				body = rewritten
			}
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		c.Next()
	}
}

// sanitizeClaudeToolResults runs on every POST /v1/messages before any routing
// (NVIDIA executor, CLIProxyAPI translators, passthrough). Claude Code sends
// tool_result content as an array of parts that may include {"type":"image"}
// blocks (Read on an image file); downstream translators pass unrecognized
// shapes through raw and the upstream rejects the whole request with
// "tool message `content` must be a string..." — so wrap anything that is not
// plain text into a JSON string, exactly what the error prescribes.
func sanitizeClaudeToolResults() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost ||
			!strings.HasPrefix(c.Request.URL.Path, "/v1/messages") {
			c.Next()
			return
		}
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			c.Abort()
			return
		}
		out, changed, err := sanitizeAnthropicToolResults(body)
		if err != nil {
			// Not JSON / not parseable: leave the body untouched and let the
			// downstream handler produce its own error.
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			c.Next()
			return
		}
		if changed {
			log.Printf("sanitize: wrapped non-text tool_result content (%d -> %d bytes)", len(body), len(out))
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(out))
		c.Request.ContentLength = int64(len(out))
		c.Next()
	}
}

// sanitizeAnthropicToolResults rewrites Claude tool_result content that is not
// a plain string or a text-only parts array into a JSON string. Returns the
// body unchanged when nothing needs wrapping.
func sanitizeAnthropicToolResults(body []byte) ([]byte, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, false, nil
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return body, false, nil
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return body, false, nil
	}
	changed := false
	for i, m := range msgs {
		role, _ := m["role"]
		if string(role) != `"user"` {
			continue
		}
		content, ok := m["content"]
		if !ok {
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(content, &parts) != nil {
			continue // string content — nothing to do
		}
		wrappedAny := false
		for j, p := range parts {
			typ, _ := p["type"]
			if string(typ) != `"tool_result"` {
				continue
			}
			tc, ok := p["content"]
			if !ok || string(tc) == "null" {
				continue
			}
			if anthropicToolResultOK(tc) {
				continue
			}
			wrapped, err := json.Marshal(string(tc))
			if err != nil {
				continue
			}
			parts[j]["content"] = wrapped
			wrappedAny = true
		}
		if !wrappedAny {
			continue
		}
		out, err := json.Marshal(parts)
		if err != nil {
			return body, false, nil
		}
		msgs[i]["content"] = out
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	outMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body, false, nil
	}
	raw["messages"] = outMsgs
	out, err := json.Marshal(raw)
	if err != nil {
		return body, false, nil
	}
	return out, true, nil
}

// anthropicToolResultOK reports whether a tool_result content value is a
// string or an array of pure text parts — the only shapes every downstream
// translator forwards safely.
func anthropicToolResultOK(content json.RawMessage) bool {
	var v any
	if json.Unmarshal(content, &v) != nil {
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
			if t, _ := pm["type"].(string); t != "text" {
				return false
			}
		}
		return true
	}
	return false
}

// messagesOnlyPassthrough intercepts /v1/messages (and /v1/messages/count_tokens)
// whose "model" resolves to a custom provider with messages_only=true and
// forwards the request to that provider's /v1/messages, streaming the response
// back untouched. Everything else falls through to CLIProxyAPI's handlers.
func messagesOnlyPassthrough(p *modelPrefs) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost ||
			!strings.HasPrefix(c.Request.URL.Path, "/v1/messages") {
			c.Next()
			return
		}

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			c.Abort()
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		var payload struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Model == "" {
			c.Next() // let CLIProxyAPI produce its own error for a bad body
			return
		}

		prov := lookupMessagesOnly(p, payload.Model)
		if prov == nil && customModelHidden(p, payload.Model) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model is hidden"})
			c.Abort()
			return
		}
		if prov == nil {
			c.Next()
			return
		}

		// The upstream only knows its own model name — strip our alias prefix
		// ("justworker/gpt-5.6-terra" -> "gpt-5.6-terra") before forwarding.
		if rewritten, changed := rewriteJSONModel(body, payload.Model, prov.Model); changed {
			body = rewritten
		}

		target := strings.TrimRight(prov.BaseURL, "/")
		if strings.HasSuffix(target, "/v1") {
			target = strings.TrimSuffix(target, "/v1")
		}
		target += "/v1/messages"

		start := time.Now()
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			statFail(payload.Model, start)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			c.Abort()
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", prov.APIKey)
		// ponytail: CF WAF on some upstreams blocks the Go TLS fingerprint;
		// spoof a curl UA (helps only if the block is header-based). If it
		// fingerprints the ClientHello, this needs utls instead.
		req.Header.Set("User-Agent", "curl/8.4.0")
		if v := c.Request.Header.Get("anthropic-version"); v != "" {
			req.Header.Set("anthropic-version", v)
		}

		resp, err := messagesOnlyClient.Do(req)
		if err != nil {
			statFail(payload.Model, start)
			c.JSON(http.StatusBadGateway, gin.H{"error": "upstream: " + err.Error()})
			c.Abort()
			return
		}
		defer resp.Body.Close()

		outH := c.Writer.Header()
		for k, vv := range resp.Header {
			if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
				continue
			}
			for _, v := range vv {
				outH.Add(k, v)
			}
		}
		if ct := outH.Get("Content-Type"); ct == "" {
			outH.Set("Content-Type", "application/json")
		}
		c.Writer.WriteHeader(resp.StatusCode)
		streamed := isEventStream(resp.Header.Get("Content-Type"))
		tee := &statTee{dst: c.Writer}
		var copyErr error
		if resp.StatusCode == http.StatusOK && streamed {
			// SSE: flush each chunk as it arrives so the client sees tokens live.
			flusher, ok := c.Writer.(http.Flusher)
			for {
				buf := make([]byte, 4096)
				n, rerr := resp.Body.Read(buf)
				if n > 0 {
					if _, werr := tee.Write(buf[:n]); werr != nil {
						copyErr = werr
						break
					}
					if ok {
						flusher.Flush()
					}
				}
				if rerr != nil {
					copyErr = rerr
					break
				}
			}
		} else {
			_, copyErr = io.Copy(tee, resp.Body)
		}
		statCopyDone(payload.Model, start, tee, resp.StatusCode, streamed, copyErr)
		c.Abort()
	}
}

// rewriteJSONModel replaces the top-level model field structurally, so valid
// JSON with arbitrary whitespace or field order is handled consistently.
func rewriteJSONModel(body []byte, from, to string) ([]byte, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, false
	}
	var model string
	if err := json.Unmarshal(raw["model"], &model); err != nil || model != from {
		return body, false
	}
	replacement, err := json.Marshal(to)
	if err != nil {
		return body, false
	}
	raw["model"] = replacement
	out, err := json.Marshal(raw)
	if err != nil {
		return body, false
	}
	return out, true
}

// lookupMessagesOnly finds the messages-only provider owning modelID. modelID
// is matched either as the full alias ("justworker/gpt-5.6-terra") or as the
// bare upstream name when the provider registered it without an alias prefix.
func lookupCustomProvider(p *modelPrefs, modelID string) *customProvider {
	prov := customProviderForModel(p, modelID)
	if prov == nil || customModelHidden(p, modelID) {
		return nil
	}
	return prov
}

func customProviderForModel(p *modelPrefs, modelID string) *customProvider {
	list := p.listProviders()
	for i := range list {
		prov := &list[i]
		if prov.alias() == modelID || prov.Model == modelID {
			return prov
		}
	}
	return nil
}

func customModelHidden(p *modelPrefs, modelID string) bool {
	prov := customProviderForModel(p, modelID)
	return prov != nil && (p.isHidden(modelID) || p.isHidden(prov.alias()))
}

func lookupMessagesOnly(p *modelPrefs, modelID string) *customProvider {
	prov := lookupCustomProvider(p, modelID)
	if prov == nil || !prov.MessagesOnly {
		return nil
	}
	return prov
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}
