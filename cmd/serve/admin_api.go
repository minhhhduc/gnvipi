package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/ir"
	gatewayprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/redact"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
	"github.com/minhhhduc/gnvipi/internal/gateway/upstream"
)

type ConnectionTestResult struct {
	OK         bool   `json:"ok"`
	Status     string `json:"status"` // "success", "auth_failure", "connect_failure", or "error"
	StatusCode int    `json:"status_code,omitempty"`
	LatencyMs  int64  `json:"latency_ms"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
}

func maskSecret(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if strings.HasPrefix(key, "sk-") && len(key) >= 8 {
		return "sk-..." + key[len(key)-4:]
	}
	if len(key) >= 8 {
		return key[:3] + "..." + key[len(key)-4:]
	}
	return "[REDACTED]"
}

func isRedactedKey(key string) bool {
	key = strings.TrimSpace(key)
	return key == "[REDACTED]" || strings.Contains(key, "...")
}

func isSensitiveHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "apikey":
		return true
	default:
		return false
	}
}

func redactProvider(p *gatewayprovider.Provider) gatewayprovider.Provider {
	cloned := gatewayprovider.Clone(p)
	if cloned.Auth.Key != "" {
		cloned.Auth.Key = maskSecret(cloned.Auth.Key)
	}
	if len(cloned.Headers) > 0 {
		headers := make(map[string]string, len(cloned.Headers))
		for k, v := range cloned.Headers {
			if isSensitiveHeader(k) {
				headers[k] = "[REDACTED]"
			} else {
				headers[k] = v
			}
		}
		cloned.Headers = headers
	}
	return cloned
}

func preserveHeaders(incoming, existing map[string]string) map[string]string {
	if len(incoming) == 0 {
		return incoming
	}
	merged := make(map[string]string, len(incoming))
	for k, v := range incoming {
		if isSensitiveHeader(k) && (v == "[REDACTED]" || isRedactedKey(v)) {
			found := false
			for ek, ev := range existing {
				if strings.EqualFold(ek, k) {
					merged[k] = ev
					found = true
					break
				}
			}
			if !found {
				merged[k] = v
			}
		} else {
			merged[k] = v
		}
	}
	return merged
}

func preserveSecretIfNeeded(updated, existing *gatewayprovider.Provider) {
	if updated == nil || existing == nil {
		return
	}
	if len(updated.Headers) > 0 {
		updated.Headers = preserveHeaders(updated.Headers, existing.Headers)
	}
	if updated.Auth.Scheme == gatewayprovider.AuthNone {
		updated.Auth.Key = ""
		updated.Auth.KeyEnv = ""
		return
	}
	if updated.Auth.KeyEnv != "" {
		updated.Auth.Key = ""
		return
	}
	if (strings.TrimSpace(updated.Auth.Key) == "" || isRedactedKey(updated.Auth.Key)) && existing.Auth.Key != "" {
		updated.Auth.Key = existing.Auth.Key
	}
}

func validationErrorResponse(c *gin.Context, err error) {
	var valErr *gatewayprovider.ValidationError
	if errors.As(err, &valErr) {
		fieldsMap := make(map[string]string, len(valErr.Errors))
		for _, fe := range valErr.Errors {
			fieldsMap[fe.Field] = fe.Problem
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":        valErr.Error(),
			"errors":       valErr.Errors,
			"fields":       fieldsMap,
			"field_errors": valErr.Errors,
		})
		return
	}
	var collisionErr *routing.CollisionError
	if errors.As(err, &collisionErr) {
		fe := &gatewayprovider.FieldError{Field: "models", Problem: collisionErr.Error()}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":        collisionErr.Error(),
			"errors":       []*gatewayprovider.FieldError{fe},
			"fields":       map[string]string{"models": collisionErr.Error()},
			"field_errors": []*gatewayprovider.FieldError{fe},
		})
		return
	}
	var singleField *gatewayprovider.FieldError
	if errors.As(err, &singleField) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":        singleField.Error(),
			"errors":       []*gatewayprovider.FieldError{singleField},
			"fields":       map[string]string{singleField.Field: singleField.Problem},
			"field_errors": []*gatewayprovider.FieldError{singleField},
		})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

func getProviderID(c *gin.Context) string {
	if id := c.Param("id"); id != "" {
		return strings.TrimSpace(id)
	}
	path := c.Request.URL.Path
	rest := strings.TrimPrefix(path, "/admin/api/providers/")
	rest = strings.TrimSuffix(rest, "/test")
	return strings.TrimSpace(rest)
}

func registerAdminAPIRoutes(engine *gin.Engine, p *modelPrefs) {
	engine.GET("/admin/api/providers", func(c *gin.Context) { handleAdminAPIListProviders(c, p) })
	engine.POST("/admin/api/providers", func(c *gin.Context) { handleAdminAPICreateProvider(c, p) })
	engine.POST("/admin/api/providers/test", func(c *gin.Context) { handleAdminAPITestProviderDirect(c, p) })
	engine.GET("/admin/api/providers/:id", func(c *gin.Context) { handleAdminAPIGetProvider(c, p) })
	engine.PUT("/admin/api/providers/:id", func(c *gin.Context) { handleAdminAPIUpdateProvider(c, p) })
	engine.DELETE("/admin/api/providers/:id", func(c *gin.Context) { handleAdminAPIDeleteProvider(c, p) })
	engine.POST("/admin/api/providers/:id/test", func(c *gin.Context) { handleAdminAPITestProvider(c, p) })
}

func handleAdminAPIDispatch(c *gin.Context, p *modelPrefs) {
	path := c.Request.URL.Path
	method := c.Request.Method

	switch {
	case path == "/admin/api/providers":
		switch method {
		case http.MethodGet:
			handleAdminAPIListProviders(c, p)
		case http.MethodPost:
			handleAdminAPICreateProvider(c, p)
		default:
			c.Header("Allow", "GET, POST")
			c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
		}
	case path == "/admin/api/providers/test":
		if method == http.MethodPost {
			handleAdminAPITestProviderDirect(c, p)
		} else {
			c.Header("Allow", "POST")
			c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
		}
	case strings.HasPrefix(path, "/admin/api/providers/"):
		sub := strings.TrimPrefix(path, "/admin/api/providers/")
		if strings.HasSuffix(sub, "/test") {
			id := strings.TrimSuffix(sub, "/test")
			if id == "" {
				c.JSON(http.StatusNotFound, gin.H{"error": "provider id required"})
				return
			}
			if method == http.MethodPost {
				c.Params = append(c.Params, gin.Param{Key: "id", Value: id})
				handleAdminAPITestProvider(c, p)
			} else {
				c.Header("Allow", "POST")
				c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
			}
			return
		}
		id := sub
		if id == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": "provider id required"})
			return
		}
		c.Params = append(c.Params, gin.Param{Key: "id", Value: id})
		switch method {
		case http.MethodGet:
			handleAdminAPIGetProvider(c, p)
		case http.MethodPut:
			handleAdminAPIUpdateProvider(c, p)
		case http.MethodDelete:
			handleAdminAPIDeleteProvider(c, p)
		default:
			c.Header("Allow", "GET, PUT, DELETE")
			c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed"})
		}
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown admin API path"})
	}
}

func handleAdminAPIListProviders(c *gin.Context, p *modelPrefs) {
	list, err := p.listProvidersV2()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gatewayprovider.Provider, len(list))
	for i := range list {
		out[i] = redactProvider(&list[i])
	}
	c.JSON(http.StatusOK, out)
}

func handleAdminAPIGetProvider(c *gin.Context, p *modelPrefs) {
	id := getProviderID(c)
	pv, found, err := p.getProviderV2(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("provider %q not found", id)})
		return
	}
	c.JSON(http.StatusOK, redactProvider(&pv))
}

func handleAdminAPICreateProvider(c *gin.Context, p *modelPrefs) {
	var pv gatewayprovider.Provider
	if err := c.ShouldBindJSON(&pv); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	pv.ID = strings.TrimSpace(pv.ID)
	if pv.ID == "" {
		fe := &gatewayprovider.FieldError{Field: "id", Problem: "must not be empty"}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":        "id is required",
			"field":        "id",
			"errors":       []*gatewayprovider.FieldError{fe},
			"fields":       map[string]string{"id": fe.Problem},
			"field_errors": []*gatewayprovider.FieldError{fe},
		})
		return
	}
	_, found, err := p.getProviderV2(pv.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if found {
		fe := &gatewayprovider.FieldError{Provider: pv.ID, Field: "id", Problem: "already exists"}
		c.JSON(http.StatusConflict, gin.H{
			"error":        fmt.Sprintf("provider %q already exists", pv.ID),
			"field":        "id",
			"errors":       []*gatewayprovider.FieldError{fe},
			"fields":       map[string]string{"id": fe.Problem},
			"field_errors": []*gatewayprovider.FieldError{fe},
		})
		return
	}
	if err := pv.Validate(); err != nil {
		validationErrorResponse(c, err)
		return
	}
	if err := p.upsertProviderV2(&pv); err != nil {
		validationErrorResponse(c, err)
		return
	}
	c.JSON(http.StatusCreated, redactProvider(&pv))
}

func handleAdminAPIUpdateProvider(c *gin.Context, p *modelPrefs) {
	id := getProviderID(c)
	existing, found, err := p.getProviderV2(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("provider %q not found", id)})
		return
	}

	var pv gatewayprovider.Provider
	if err := c.ShouldBindJSON(&pv); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if pv.ID == "" {
		pv.ID = id
	} else if pv.ID != id {
		fe := &gatewayprovider.FieldError{Field: "id", Problem: "id in path does not match id in body"}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":        "id in path does not match id in body",
			"field":        "id",
			"errors":       []*gatewayprovider.FieldError{fe},
			"fields":       map[string]string{"id": fe.Problem},
			"field_errors": []*gatewayprovider.FieldError{fe},
		})
		return
	}

	preserveSecretIfNeeded(&pv, &existing)
	if err := pv.Validate(); err != nil {
		validationErrorResponse(c, err)
		return
	}
	if err := p.upsertProviderV2(&pv); err != nil {
		validationErrorResponse(c, err)
		return
	}
	c.JSON(http.StatusOK, redactProvider(&pv))
}

func handleAdminAPIDeleteProvider(c *gin.Context, p *modelPrefs) {
	id := getProviderID(c)
	_, found, err := p.getProviderV2(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("provider %q not found", id)})
		return
	}
	if err := p.deleteProviderV2(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "deleted": id})
}

func handleAdminAPITestProviderDirect(c *gin.Context, _ *modelPrefs) {
	var target gatewayprovider.Provider
	if err := c.ShouldBindJSON(&target); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if target.ID == "" {
		target.ID = "test-provider"
	}
	if target.Name == "" {
		target.Name = "Test Provider"
	}
	if err := target.Validate(); err != nil {
		validationErrorResponse(c, err)
		return
	}
	res := testProviderConnection(c.Request.Context(), &target)
	c.JSON(http.StatusOK, res)
}

func handleAdminAPITestProvider(c *gin.Context, p *modelPrefs) {
	id := getProviderID(c)
	existing, found, err := p.getProviderV2(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("provider %q not found", id)})
		return
	}

	target := gatewayprovider.Clone(&existing)
	var overlay gatewayprovider.Provider
	if c.Request.Body != nil && c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&overlay); err == nil {
			if overlay.BaseURL != "" {
				target.BaseURL = overlay.BaseURL
			}
			if overlay.Protocol != "" {
				target.Protocol = overlay.Protocol
			}
			if overlay.Auth.Scheme != "" {
				target.Auth.Scheme = overlay.Auth.Scheme
			}
			if overlay.Auth.Key != "" && !isRedactedKey(overlay.Auth.Key) {
				target.Auth.Key = overlay.Auth.Key
			}
			if overlay.Auth.KeyEnv != "" {
				target.Auth.KeyEnv = overlay.Auth.KeyEnv
			}
			if overlay.Auth.HeaderName != "" {
				target.Auth.HeaderName = overlay.Auth.HeaderName
			}
			if len(overlay.Models) > 0 {
				target.Models = overlay.Models
			}
			if len(overlay.Headers) > 0 {
				target.Headers = preserveHeaders(overlay.Headers, existing.Headers)
			}
			if overlay.EndpointPaths != nil {
				target.EndpointPaths = overlay.EndpointPaths
			}
			if overlay.ProxyURL != "" {
				target.ProxyURL = overlay.ProxyURL
			}
			if overlay.TLS != "" {
				target.TLS = overlay.TLS
			}
		}
	}

	if err := target.Validate(); err != nil {
		validationErrorResponse(c, err)
		return
	}
	res := testProviderConnection(c.Request.Context(), &target)
	c.JSON(http.StatusOK, res)
}

func testProviderConnection(ctx context.Context, p *gatewayprovider.Provider) ConnectionTestResult {
	targetProvider := gatewayprovider.Clone(p)
	if strings.TrimSpace(targetProvider.ID) == "" {
		targetProvider.ID = "test-provider"
	}

	if targetProvider.Auth.Scheme != gatewayprovider.AuthNone && targetProvider.Auth.KeyEnv != "" && os.Getenv(targetProvider.Auth.KeyEnv) == "" {
		return ConnectionTestResult{
			OK:     false,
			Status: "auth_failure",
			Error:  fmt.Sprintf("environment variable %q is not set", targetProvider.Auth.KeyEnv),
		}
	}

	secret := targetProvider.Auth.Key
	if targetProvider.Auth.KeyEnv != "" {
		secret = os.Getenv(targetProvider.Auth.KeyEnv)
	}

	policy := gatewayprovider.DefaultURLPolicy()
	proxy := targetProvider.ProxyURL
	var proxyURL *url.URL
	if proxy != "" {
		var parseErr error
		proxyURL, parseErr = url.Parse(proxy)
		if parseErr != nil {
			return ConnectionTestResult{
				OK:        false,
				Status:    "connect_failure",
				LatencyMs: 0,
				Error:     redact.Default.TextWithSecrets(parseErr.Error(), secret),
			}
		}
	} else if origin, parseErr := url.Parse(targetProvider.BaseURL); parseErr == nil {
		var envErr error
		proxyURL, envErr = http.ProxyFromEnvironment(&http.Request{URL: origin})
		if envErr != nil {
			return ConnectionTestResult{
				OK:        false,
				Status:    "connect_failure",
				LatencyMs: 0,
				Error:     redact.Default.TextWithSecrets(envErr.Error(), secret),
			}
		}
	}

	options := &upstream.Options{
		Timeouts:      targetProvider.Timeouts,
		ProxyURL:      proxy,
		Control:       policy.Control,
		DialContext:   policy.Dialer(proxyURL, net.DefaultResolver, nil),
		DisableProxy:  true,
		CheckRedirect: policy.CheckRedirect,
	}

	factory := upstream.NewFactory()
	defer factory.CloseIdleConnections()

	client, err := factory.NewClient(&targetProvider, options)
	if err != nil {
		return ConnectionTestResult{
			OK:        false,
			Status:    "connect_failure",
			LatencyMs: 0,
			Error:     redact.Default.TextWithSecrets(err.Error(), secret),
		}
	}

	timeout := 10 * time.Second
	if targetProvider.Timeouts.Connect > 0 && targetProvider.Timeouts.Connect < timeout {
		timeout = targetProvider.Timeouts.Connect
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := strings.TrimRight(targetProvider.BaseURL, "/")
	modelName := "default"
	for _, m := range targetProvider.Models {
		if m.UpstreamModel != "" {
			modelName = m.UpstreamModel
			break
		}
		if m.Alias != "" && !strings.Contains(m.Alias, "*") {
			modelName = m.Alias
			break
		}
	}

	switch targetProvider.Protocol {
	case ir.ProtocolAnthropicMessages:
		return testAnthropic(reqCtx, client, &targetProvider, base, modelName, secret)
	case ir.ProtocolOpenAIResponses:
		return testOpenAIResponses(reqCtx, client, &targetProvider, base, modelName, secret)
	default:
		return testOpenAIChat(reqCtx, client, &targetProvider, base, modelName, secret)
	}
}

func testOpenAIChat(ctx context.Context, client *http.Client, p *gatewayprovider.Provider, base, modelName, secret string) ConnectionTestResult {
	if custom, ok := p.EndpointPaths[ir.ProtocolOpenAIChat]; ok && custom != "" {
		u, err := endpointURL(base, custom)
		if err != nil {
			return ConnectionTestResult{OK: false, Status: "connect_failure", Error: redact.Default.TextWithSecrets(err.Error(), secret)}
		}
		return do1TokenChat(ctx, client, p, u.String(), modelName, secret)
	}

	modelsURL := base + "/models"
	if !strings.HasSuffix(base, "/v1") {
		modelsURL = base + "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, http.NoBody)
	if err != nil {
		return ConnectionTestResult{OK: false, Status: "connect_failure", Error: redact.Default.TextWithSecrets(err.Error(), secret)}
	}
	applyHeadersAndAuth(req, p, secret)

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return ConnectionTestResult{
			OK:        false,
			Status:    "connect_failure",
			LatencyMs: latency,
			Error:     redact.Default.TextWithSecrets(err.Error(), secret),
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			return
		}
	}()
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if readErr != nil {
		bodyBytes = nil
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ConnectionTestResult{
			OK:         true,
			Status:     "success",
			StatusCode: resp.StatusCode,
			LatencyMs:  latency,
			Message:    "Connection successful",
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return authFailureResult(resp.StatusCode, latency, bodyBytes, secret)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		chatURL := base + "/chat/completions"
		if !strings.HasSuffix(base, "/v1") {
			chatURL = base + "/v1/chat/completions"
		}
		return do1TokenChat(ctx, client, p, chatURL, modelName, secret)
	}
	return errorResult(resp.StatusCode, latency, bodyBytes, secret)
}

func do1TokenChat(ctx context.Context, client *http.Client, p *gatewayprovider.Provider, urlStr, modelName, secret string) ConnectionTestResult {
	reqBody, err := json.Marshal(map[string]any{
		"model":      modelName,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	if err != nil {
		return ConnectionTestResult{OK: false, Status: "connect_failure", Error: redact.Default.TextWithSecrets(err.Error(), secret)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(reqBody))
	if err != nil {
		return ConnectionTestResult{OK: false, Status: "connect_failure", Error: redact.Default.TextWithSecrets(err.Error(), secret)}
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeadersAndAuth(req, p, secret)

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return ConnectionTestResult{
			OK:        false,
			Status:    "connect_failure",
			LatencyMs: latency,
			Error:     redact.Default.TextWithSecrets(err.Error(), secret),
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			return
		}
	}()
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if readErr != nil {
		bodyBytes = nil
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ConnectionTestResult{
			OK:         true,
			Status:     "success",
			StatusCode: resp.StatusCode,
			LatencyMs:  latency,
			Message:    "Connection successful",
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return authFailureResult(resp.StatusCode, latency, bodyBytes, secret)
	}
	return errorResult(resp.StatusCode, latency, bodyBytes, secret)
}

func testAnthropic(ctx context.Context, client *http.Client, p *gatewayprovider.Provider, base, modelName, secret string) ConnectionTestResult {
	urlStr := base + "/messages"
	if !strings.HasSuffix(base, "/v1") {
		urlStr = base + "/v1/messages"
	}
	if custom, ok := p.EndpointPaths[ir.ProtocolAnthropicMessages]; ok && custom != "" {
		u, err := endpointURL(base, custom)
		if err == nil {
			urlStr = u.String()
		}
	}
	reqBody, err := json.Marshal(map[string]any{
		"model":      modelName,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	})
	if err != nil {
		return ConnectionTestResult{OK: false, Status: "connect_failure", Error: redact.Default.TextWithSecrets(err.Error(), secret)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(reqBody))
	if err != nil {
		return ConnectionTestResult{OK: false, Status: "connect_failure", Error: redact.Default.TextWithSecrets(err.Error(), secret)}
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeadersAndAuth(req, p, secret)
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return ConnectionTestResult{
			OK:        false,
			Status:    "connect_failure",
			LatencyMs: latency,
			Error:     redact.Default.TextWithSecrets(err.Error(), secret),
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			return
		}
	}()
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if readErr != nil {
		bodyBytes = nil
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ConnectionTestResult{
			OK:         true,
			Status:     "success",
			StatusCode: resp.StatusCode,
			LatencyMs:  latency,
			Message:    "Connection successful",
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return authFailureResult(resp.StatusCode, latency, bodyBytes, secret)
	}
	return errorResult(resp.StatusCode, latency, bodyBytes, secret)
}

func testOpenAIResponses(ctx context.Context, client *http.Client, p *gatewayprovider.Provider, base, modelName, secret string) ConnectionTestResult {
	urlStr := base + "/responses"
	if !strings.HasSuffix(base, "/v1") {
		urlStr = base + "/v1/responses"
	}
	if custom, ok := p.EndpointPaths[ir.ProtocolOpenAIResponses]; ok && custom != "" {
		u, err := endpointURL(base, custom)
		if err == nil {
			urlStr = u.String()
		}
	}
	reqBody, err := json.Marshal(map[string]any{
		"model": modelName,
		"input": "hi",
	})
	if err != nil {
		return ConnectionTestResult{OK: false, Status: "connect_failure", Error: redact.Default.TextWithSecrets(err.Error(), secret)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(reqBody))
	if err != nil {
		return ConnectionTestResult{OK: false, Status: "connect_failure", Error: redact.Default.TextWithSecrets(err.Error(), secret)}
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeadersAndAuth(req, p, secret)

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return ConnectionTestResult{
			OK:        false,
			Status:    "connect_failure",
			LatencyMs: latency,
			Error:     redact.Default.TextWithSecrets(err.Error(), secret),
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			return
		}
	}()
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if readErr != nil {
		bodyBytes = nil
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ConnectionTestResult{
			OK:         true,
			Status:     "success",
			StatusCode: resp.StatusCode,
			LatencyMs:  latency,
			Message:    "Connection successful",
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return authFailureResult(resp.StatusCode, latency, bodyBytes, secret)
	}
	return errorResult(resp.StatusCode, latency, bodyBytes, secret)
}

func applyHeadersAndAuth(req *http.Request, p *gatewayprovider.Provider, secret string) {
	authHeader := p.Auth.HeaderName
	switch p.Auth.Scheme {
	case gatewayprovider.AuthBearer:
		if authHeader == "" {
			authHeader = "Authorization"
		}
		req.Header.Set(authHeader, "Bearer "+secret)
	case gatewayprovider.AuthAPIKey:
		if authHeader == "" {
			authHeader = "x-api-key"
		}
		req.Header.Set(authHeader, secret)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
}

func extractErrorMessage(bodyBytes []byte, defaultMsg string) string {
	if len(bodyBytes) == 0 {
		return defaultMsg
	}
	var envelope struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(bodyBytes, &envelope) == nil && envelope.Error != nil {
		if s, ok := envelope.Error.(string); ok && s != "" {
			return defaultMsg + ": " + s
		}
		if m, ok := envelope.Error.(map[string]any); ok {
			if msg, ok := m["message"].(string); ok && msg != "" {
				return defaultMsg + ": " + msg
			}
		}
	}
	return defaultMsg
}

func authFailureResult(statusCode int, latency int64, bodyBytes []byte, secret string) ConnectionTestResult {
	rawMsg := extractErrorMessage(bodyBytes, fmt.Sprintf("upstream authentication failed (HTTP %d)", statusCode))
	return ConnectionTestResult{
		OK:         false,
		Status:     "auth_failure",
		StatusCode: statusCode,
		LatencyMs:  latency,
		Error:      redact.Default.TextWithSecrets(rawMsg, secret),
	}
}

func errorResult(statusCode int, latency int64, bodyBytes []byte, secret string) ConnectionTestResult {
	rawMsg := extractErrorMessage(bodyBytes, fmt.Sprintf("upstream returned HTTP %d", statusCode))
	return ConnectionTestResult{
		OK:         false,
		Status:     "error",
		StatusCode: statusCode,
		LatencyMs:  latency,
		Error:      redact.Default.TextWithSecrets(rawMsg, secret),
	}
}

func endpointURL(raw, endpoint string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("target base URL: %w", err)
	}
	prefix := strings.TrimRight(u.Path, "/")
	u.Path = prefix + endpoint
	u.RawPath = ""
	return u, nil
}
