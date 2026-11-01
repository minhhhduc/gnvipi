package ir

import "fmt"

// Protocol identifies one of the wire protocols the gateway speaks on both
// the inbound (client-facing) and the outbound (provider-facing) side.
type Protocol string

const (
	// ProtocolAnthropicMessages is the Anthropic Messages API
	// (POST /v1/messages).
	ProtocolAnthropicMessages Protocol = "anthropic-messages"
	// ProtocolOpenAIChat is the OpenAI Chat Completions API
	// (POST /v1/chat/completions).
	ProtocolOpenAIChat Protocol = "openai-chat"
	// ProtocolOpenAIResponses is the OpenAI Responses API
	// (POST /v1/responses).
	ProtocolOpenAIResponses Protocol = "openai-responses"
)

// UnknownProtocolError is returned by ParseProtocol for values that are not
// one of the supported protocols.
type UnknownProtocolError struct {
	Protocol string
}

func (e *UnknownProtocolError) Error() string {
	return fmt.Sprintf("ir: unknown protocol %q (want one of %s)", e.Protocol, supportedProtocols())
}

func (e *UnknownProtocolError) Is(target error) bool {
	_, ok := target.(*UnknownProtocolError)
	return ok
}

// ParseProtocol converts a raw protocol string into a Protocol, rejecting
// unknown values with a typed error.
func ParseProtocol(s string) (Protocol, error) {
	switch Protocol(s) {
	case ProtocolAnthropicMessages, ProtocolOpenAIChat, ProtocolOpenAIResponses:
		return Protocol(s), nil
	}
	return "", &UnknownProtocolError{Protocol: s}
}

// String returns the wire value of the protocol.
func (p Protocol) String() string { return string(p) }

func supportedProtocols() string {
	return string(ProtocolAnthropicMessages) + ", " +
		string(ProtocolOpenAIChat) + ", " +
		string(ProtocolOpenAIResponses)
}
