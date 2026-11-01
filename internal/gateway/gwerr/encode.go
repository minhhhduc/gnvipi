package gwerr

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Encode renders e as the wire body the given protocol expects, and
// reports the HTTP status to answer with. It fails only on an unknown
// protocol.
func Encode(proto Protocol, e *Error) (body []byte, status int, err error) {
	if e.Status != 0 {
		status = e.Status
	} else {
		status = StatusForKind(e.Kind)
	}
	switch proto {
	case ProtocolAnthropicMessages:
		body, err = encodeAnthropic(e)
	case ProtocolOpenAIChat:
		body, err = encodeChat(e)
	case ProtocolOpenAIResponses:
		body, err = encodeResponses(e)
	default:
		return nil, 0, fmt.Errorf("gwerr: unknown protocol %q", proto)
	}
	return body, status, err
}

// Write encodes e for proto and writes a complete HTTP error response:
// status, JSON content type, Retry-After and X-Request-ID headers when the
// error carries them, then the body. Anthropic Messages responses also set
// the lowercase request-id header: that is the name Anthropic's API sends
// and its clients read, whereas X-Request-ID is the OpenAI convention.
func Write(w http.ResponseWriter, proto Protocol, e *Error) error {
	body, status, err := Encode(proto, e)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(e.RetryAfter.Round(time.Second).Seconds())))
	}
	if e.UpstreamRequestID != "" {
		h.Set("X-Request-ID", e.UpstreamRequestID)
		if proto == ProtocolAnthropicMessages {
			h.Set("request-id", e.UpstreamRequestID)
		}
	}
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}

// WriteSSEError writes the terminal error event(s) of proto's streaming
// form to w as raw SSE bytes. The Responses protocol emits both its error
// event and the response.failed terminator; the other protocols emit one
// error event. responseID identifies the in-flight Responses stream and is
// ignored by the other protocols. w must be the same writer the stream's
// other events went to, already flushed per event.
func WriteSSEError(w io.Writer, proto Protocol, e *Error, responseID string) error {
	switch proto {
	case ProtocolAnthropicMessages:
		body, err := encodeAnthropic(e)
		if err != nil {
			return err
		}
		return writeSSEEvent(w, "error", string(body))
	case ProtocolOpenAIChat:
		body, err := encodeChat(e)
		if err != nil {
			return err
		}
		return writeSSEEvent(w, "", string(body))
	case ProtocolOpenAIResponses:
		msg := e.ScrubbedMessage()
		code := "server_error"
		if e.Code != "" {
			code = e.Code
		}
		ev := responsesStreamError{
			Type:    "error",
			Code:    code,
			Message: msg,
			Param:   nil,
		}
		body, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if err := writeSSEEvent(w, "error", string(body)); err != nil {
			return err
		}
		failed := responsesStreamFailed{
			Type: "response.failed",
			Response: responsesStreamFailedResponse{
				ID:     responseID,
				Status: "failed",
				Error: &responsesFailedError{
					Code:    code,
					Message: msg,
				},
			},
		}
		body, err = json.Marshal(failed)
		if err != nil {
			return err
		}
		return writeSSEEvent(w, "response.failed", string(body))
	default:
		return fmt.Errorf("gwerr: unknown protocol %q", proto)
	}
}

// writeSSEEvent writes one SSE event: an "event:" line when name is set,
// one "data:" line, and the blank line that dispatches it.
func writeSSEEvent(w io.Writer, name, data string) error {
	if name != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", name); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

// --- Anthropic Messages ---
//
// {"type":"error","error":{"type":"invalid_request_error","message":"..."}}

type anthropicBody struct {
	Type  string          `json:"type"`
	Error anthropicDetail `json:"error"`
}

type anthropicDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func encodeAnthropic(e *Error) ([]byte, error) {
	body := anthropicBody{
		Type: "error",
		Error: anthropicDetail{
			Type:    anthropicErrorType(e.Kind),
			Message: e.ScrubbedMessage(),
		},
	}
	return json.Marshal(body)
}

// anthropicErrorType is the Anthropic "error.type" for a Kind, shaped after
// the mapping cmd/serve used for inbound Anthropic status codes.
func anthropicErrorType(k Kind) string {
	switch k {
	case KindInvalidRequest:
		return "invalid_request_error"
	case KindAuthentication:
		return "authentication_error"
	case KindPermission:
		return "permission_error"
	case KindNotFound:
		return "not_found_error"
	case KindRateLimit:
		return "rate_limit_error"
	case KindOverloaded:
		return "overloaded_error"
	case KindTimeout:
		return "timeout_error"
	default:
		return "api_error"
	}
}

// --- OpenAI Chat ---
//
// {"error":{"message":"...","type":"...","param":null,"code":null}}

type chatBody struct {
	Error chatDetail `json:"error"`
}

type chatDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

func encodeChat(e *Error) ([]byte, error) {
	d := chatDetail{
		Message: e.ScrubbedMessage(),
		Type:    chatErrorType(e.Kind),
	}
	if e.Code != "" {
		code := e.Code
		d.Code = &code
	}
	body := chatBody{Error: d}
	return json.Marshal(body)
}

// chatErrorType is the OpenAI Chat "error.type" for a Kind.
func chatErrorType(k Kind) string {
	switch k {
	case KindInvalidRequest:
		return "invalid_request_error"
	case KindAuthentication:
		return "authentication_error"
	case KindPermission:
		return "permission_error"
	case KindNotFound:
		return "not_found_error"
	case KindRateLimit:
		return "rate_limit_error"
	default:
		return "server_error"
	}
}

// --- OpenAI Responses ---
//
// The non-streaming form is the standard OpenAI error object:
//
//	{"error":{"code":null,"message":"...","param":null,"type":"server_error"}}
//
// The streaming form is an "error" event and, to terminate, a
// "response.failed" event whose response carries status "failed" and an
// error object: the Responses API types response.error as an object, so a
// bare string breaks typed SDK clients on response.failed.

type responsesBody struct {
	Error responsesDetail `json:"error"`
}

type responsesDetail struct {
	Code    *string `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
	Type    string  `json:"type"`
}

type responsesStreamError struct {
	Type    string  `json:"type"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

type responsesStreamFailed struct {
	Type     string                        `json:"type"`
	Response responsesStreamFailedResponse `json:"response"`
}

type responsesStreamFailedResponse struct {
	ID     string                `json:"id"`
	Status string                `json:"status"`
	Error  *responsesFailedError `json:"error"`
}

// responsesFailedError is the error object embedded in a response.failed
// event's response. Code and Message mirror the sibling "error" event so a
// client can read either.
type responsesFailedError struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

func encodeResponses(e *Error) ([]byte, error) {
	d := responsesDetail{
		Message: e.ScrubbedMessage(),
		Type:    chatErrorType(e.Kind),
	}
	if e.Code != "" {
		code := e.Code
		d.Code = &code
	}
	body := responsesBody{Error: d}
	return json.Marshal(body)
}
