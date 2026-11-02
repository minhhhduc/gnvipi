package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/minhhhduc/gnvipi/internal/gateway/gwerr"
)

// requestBodyLimit must run before middleware that reads or rewrites bodies.
func requestBodyLimit(v1Limit, adminLimit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		var limit int64
		switch {
		case clientAPIPath(path):
			limit = v1Limit
		case path == "/admin" || strings.HasPrefix(path, "/admin/"):
			limit = adminLimit
		default:
			c.Next()
			return
		}

		proto := gwerr.ProtocolOpenAIChat
		switch {
		case path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/"):
			proto = gwerr.ProtocolAnthropicMessages
		case path == "/v1/responses" || strings.HasPrefix(path, "/v1/responses/") || path == "/backend-api/codex/responses" || strings.HasPrefix(path, "/backend-api/codex/responses/"):
			proto = gwerr.ProtocolOpenAIResponses
		}
		reject := func(status int, message string) {
			c.Abort()
			_ = gwerr.Write(c.Writer, proto, &gwerr.Error{
				Status: status, Kind: gwerr.KindInvalidRequest, Message: message,
			})
		}
		if c.Request.ContentLength > limit {
			reject(http.StatusRequestEntityTooLarge, "request body exceeds the size limit")
			return
		}
		if c.Request.Body == nil || c.Request.Body == http.NoBody {
			c.Next()
			return
		}

		// Read eagerly: legacy passthroughs may ignore read errors, so a
		// MaxBytesReader alone can let a truncated oversized body reach them.
		limited := http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		body, err := io.ReadAll(limited)
		if encoding := strings.TrimSpace(c.GetHeader("Content-Encoding")); err == nil && clientAPIPath(path) && encoding != "" && !strings.EqualFold(encoding, "identity") {
			body, err = decodeClientBody(body, encoding, limit)
			if err == nil {
				c.Request.Header.Del("Content-Encoding")
				c.Request.ContentLength = int64(len(body))
				c.Request.TransferEncoding = nil
				c.Request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
			}
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				reject(http.StatusRequestEntityTooLarge, "request body exceeds the size limit")
			} else {
				reject(http.StatusBadRequest, "could not read request body")
			}
			return
		}
		// Preserve Close ownership: downstream/the server closes the original
		// body, not a no-op closer around the replayed bytes.
		c.Request.Body = struct {
			io.Reader
			io.Closer
		}{bytes.NewReader(body), limited}
		c.Next()
	}
}

// Match the SDK's encoding stack and raw-JSON fallback without unbounded decoding.
func decodeClientBody(body []byte, encoding string, limit int64) ([]byte, error) {
	original := body
	parts := strings.Split(encoding, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		var err error
		switch strings.ToLower(strings.TrimSpace(parts[i])) {
		case "", "identity":
			continue
		case "zstd":
			var decoder *zstd.Decoder
			decoder, err = zstd.NewReader(bytes.NewReader(body),
				zstd.WithDecoderConcurrency(1), zstd.WithDecodeBuffersBelow(0),
				zstd.WithDecoderMaxMemory(uint64(max(limit, 1<<20))))
			if err == nil {
				body, err = io.ReadAll(http.MaxBytesReader(nil, io.NopCloser(decoder), limit))
				decoder.Close()
			}
		default:
			err = errors.New("unsupported request content encoding")
		}
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) || errors.Is(err, zstd.ErrDecoderSizeExceeded) {
				return nil, &http.MaxBytesError{Limit: limit}
			}
			if json.Valid(original) {
				return original, nil
			}
			return nil, err
		}
	}
	return body, nil
}
