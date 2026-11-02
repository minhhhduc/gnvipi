// Package adaptertest provides fixture-driven conformance tests for gateway
// adapters. Adapter packages adopt the suite in their own _test.go files:
//
//	func TestContract(t *testing.T) {
//		adaptertest.RunInboundContract(t, New(), "testdata")
//	}
//
// Outbound adapters call RunOutboundContract with the same arguments. There is
// no registry dependency, network access, or adapter-specific implementation in
// the runners. Fixtures must describe the adapter's actual supported protocol.
// The working fake adapters in contract_test.go demonstrate wire-to-IR
// translation rather than returning fixture IR verbatim.
//
// # Fixture schema
//
// fixturesDir contains inbound.json, outbound.json, or both. Each manifest is a
// JSON object. All file references are relative to fixturesDir (including ../
// references to shared canonical IR); absolute paths also work. Wire bodies are
// files, never inline strings. Manifests and IR/error fixtures reject unknown
// fields, null roots, trailing JSON, missing required cases, and invalid event
// ordering. Every case requires a name. Add cases for protocol features such as
// tool calls, reasoning, and images using the same schema; the suite imposes no
// protocol-specific field whitelist on wire JSON.
//
// inbound.json has four nonempty arrays:
//
//   - decode: {"name":"text", "request":{"method":"POST",
//     "url":"http://client.invalid/chat", "headers":{"Content-Type":
//     ["application/json"]}, "body":"request.json"},
//     "ir":"../canonical/request.json"}. method defaults to POST and url to
//     http://client.invalid/. For rejected inputs use "error":"invalid.json"
//     instead of ir. At least one successful and one rejected input is required.
//   - responses: {"name":"text", "ir":"response.ir.json",
//     "golden":"response.golden.json"}.
//   - streams: {"name":"text", "ir":"events.ir.json",
//     "golden":"stream.golden.sse"}. The IR file is an array of StreamEvent.
//     Include both successful message_stop and terminal error sequences.
//   - errors: {"name":"invalid", "error":"invalid.json",
//     "golden":"error.golden.json"}. Inbound has no EncodeError method;
//     this checks the existing gwerr.Write encoder selected by a.Protocol().
//     The golden is {"status":400, "headers":{"Content-Type":
//     ["application/json"]}, "body":{...protocol error JSON...}}.
//
// outbound.json has three nonempty arrays:
//
//   - requests: {"name":"text", "ir":"request.ir.json", "target":
//     {"BaseURL":"https://upstream.invalid/prefix", "Model":"resolved",
//     "Headers":{"X-Fixture":["one","two"]}},
//     "golden":"request.golden.json"}. target uses adapter.Target's existing
//     Go field names. The golden is {"method":"POST", "url":"...",
//     "headers":{...}, "body":{...wire JSON...}}. Target headers must be
//     preserved without mutation, including multiple values. Use only dummy
//     credentials. Fixtures should exercise endpoint prefixes and target model
//     selection, not just the IR model.
//   - responses: {"name":"text", "response":{"status":200,
//     "headers":{"Content-Type":["application/json"]},
//     "body":"response.json"}, "ir":"response.ir.json"}.
//     Use error instead of ir for expected *gwerr.Error results. Both success
//     and error response cases are required.
//   - streams: the same shape as responses, but response.body is upstream SSE
//     and ir names an array of StreamEvent. Include message_stop and error
//     terminal sequences. error instead of ir may additionally check failures
//     returned by ParseStream itself. A returned Stream must close resp.Body.
//
// Normalized error files contain {"kind":"rate_limit", "message":"slow
// down", "status":429, "code":"...", "upstream_status":429,
// "upstream_request_id":"...", "retry_after_ms":1000}. Only kind and
// message are required; omitted fields are zero and compared exactly. status
// is the stored gateway Error.Status (zero is not replaced with its derived
// HTTP status). Wrapped *gwerr.Error values are accepted; messages are compared
// after the shared secret scrubber. IR files use the existing ir JSON tags.
// Protocol-neutral starting fixtures live in internal/gateway/testdata/
// adapter-contract/canonical; the adjacent fake directory is a complete,
// independently authored manifest and wire-golden example.
//
// # Comparison and updates
//
// JSON comparisons ignore object key order, insignificant whitespace, and
// numeric spelling (1 equals 1.0), but retain array order, null versus empty,
// string versus number, and exact large integers. SSE comparisons use the
// shared sse.Reader: ordered dispatched events, event names, IDs and retries
// must match; JSON data compares semantically and non-JSON data such as [DONE]
// compares literally. Comments and line endings are not protocol semantics.
// Stream invariants use ir.ValidateEventOrder and additionally require start
// first, consistent block prototype indices, populated event payloads, and a
// usage_update immediately before the terminal event. Inbound encoders must
// consume the terminal IR event and must not pull events after it.
//
// Enable -update before test argument parsing in the adopting package:
//
//	func TestMain(m *testing.M) {
//		adaptertest.RegisterFlags()
//		os.Exit(m.Run())
//	}
//
// RegisterFlags reuses an existing -update flag if the test package declares one.
// Run go test ./internal/gateway/inbound/<proto> -args -update (or the outbound
// package) to create or replace wire goldens. Updates never rewrite manifests,
// expected IR, error files, or upstream input fixtures; keep golden paths separate
// from those inputs. Invalid JSON and empty SSE cannot be blessed. Review updates:
// replacing a golden changes the protocol-specific oracle, not the independent
// IR ordering and terminal-consumption checks.
package adaptertest
