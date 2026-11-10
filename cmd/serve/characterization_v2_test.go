package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

var characterizationProxyURL string

func characterizationV2Enabled() bool {
	return os.Getenv("GNVIPI_TEST_GATEWAY_V2") != "0"
}

// DNS stays at the network boundary: the relay still validates and pins every
// returned address. net.Pipe avoids external DNS and extra listening sockets.
func characterizationResolver(t *testing.T) *net.Resolver {
	t.Helper()
	return &net.Resolver{PreferGo: true, Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer func() {
				if err := server.Close(); err != nil {
					t.Error(err)
				}
			}()
			var length [2]byte
			if _, err := io.ReadFull(server, length[:]); err != nil {
				return
			}
			query := make([]byte, int(binary.BigEndian.Uint16(length[:])))
			if _, err := io.ReadFull(server, query); err != nil {
				return
			}
			var message dnsmessage.Message
			if err := message.Unpack(query); err != nil {
				t.Error(err)
				return
			}
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: message.ID, Response: true, RecursionDesired: true, RecursionAvailable: true}, Questions: message.Questions}
			for i := range message.Questions {
				question := &message.Questions[i]
				header := dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET, TTL: 60}
				switch question.Type {
				case dnsmessage.TypeA:
					response.Answers = append(response.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}})
				case dnsmessage.TypeAAAA:
					response.Answers = append(response.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.AAAAResource{AAAA: [16]byte{15: 1}}})
				}
			}
			payload, err := response.Pack()
			if err != nil {
				t.Error(err)
				return
			}
			framed := make([]byte, len(payload)+2)
			binary.BigEndian.PutUint16(framed, uint16(len(payload)))
			copy(framed[2:], payload)
			if _, err := server.Write(framed); err != nil {
				return
			}
		}()
		return client, nil
	}}
}

// The real relay uses its guarded transport, not the legacy test clients.
// A local CONNECT proxy exercises that transport with the same HTTP fixtures.
func characterizationProxy(t *testing.T, log *upstreamLog, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("proxy writer cannot hijack")
			return
		}
		conn, buffered, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() {
			if err := conn.Close(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			t.Error(err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Error(err)
			return
		}
		request, err := http.ReadRequest(buffered.Reader)
		if err != nil {
			t.Error(err)
			return
		}
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if err := request.Body.Close(); err != nil {
			t.Error(err)
			return
		}
		request.URL.Scheme, request.URL.Host = "http", request.Host
		log.record(upstreamCall{URL: request.URL.String(), Header: request.Header.Clone(), Body: string(payload)})
		response := &http.Response{StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Close: true}
		if err := response.Write(buffered.Writer); err != nil {
			t.Error(err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// Run the same characterization functions in both flag modes. The standalone
// tests cover the default flag-on dispatcher; these nested cases cover explicit
// flag-on as well as the deprecated legacy escape hatch (flag-off).
func TestGatewayV2Characterization(t *testing.T) {
	t.Setenv("GNVIPI_TEST_GATEWAY_V2", "1")
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"chat", TestChatRoutePassthrough},
		{"responses", TestResponsesRoutePassthrough},
		{"messages", TestMessagesRouteMessagesOnlyPassthrough},
		{"legacy-cross-protocol", TestMessagesRouteResponsesAdapter},
		{"hidden-models", TestHiddenModelsRejected},
		{"nvidia-fallthrough", TestUnknownModelsFallThrough},
		{"hidden-responses", TestResponsesHiddenModelRouting},
		{"client-identity", TestAgentRouterClientSpoofed},
		{"automode", TestAutomodeRewritten},
		{"cycle-retry", TestCycleRetriesAndCommits},
		{"cycle-committed", TestCycleDoesNotRetryCommittedResponse},
		{"cycle-count-tokens", TestCycleCountTokensTriesFirstModelOnly},
		{"tool-results", TestToolResultsSanitized},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestLegacyEscapeHatchCharacterization(t *testing.T) {
	t.Setenv("GNVIPI_TEST_GATEWAY_V2", "0")
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"chat", TestChatRoutePassthrough},
		{"responses", TestResponsesRoutePassthrough},
		{"messages", TestMessagesRouteMessagesOnlyPassthrough},
		{"legacy-cross-protocol", TestMessagesRouteResponsesAdapter},
		{"hidden-models", TestHiddenModelsRejected},
		{"nvidia-fallthrough", TestUnknownModelsFallThrough},
		{"hidden-responses", TestResponsesHiddenModelRouting},
		{"client-identity", TestAgentRouterClientSpoofed},
		{"automode", TestAutomodeRewritten},
		{"cycle-retry", TestCycleRetriesAndCommits},
		{"cycle-committed", TestCycleDoesNotRetryCommittedResponse},
		{"cycle-count-tokens", TestCycleCountTokensTriesFirstModelOnly},
		{"tool-results", TestToolResultsSanitized},
	} {
		t.Run(tc.name, tc.run)
	}
}
