// Package ir defines the protocol-neutral internal representation (IR) that
// every gateway adapter converts to and from.
//
// The gateway accepts requests on three inbound protocols — Anthropic
// Messages, OpenAI Chat Completions and OpenAI Responses — and can forward
// them to upstreams speaking any of the same three. Rather than translating
// pairwise (six paths, twenty-one once outbound count grows), every inbound
// adapter decodes its wire format into the types in this package and every
// outbound adapter encodes from them. The IR is therefore the root of the
// adapter dependency graph: it must stay free of HTTP, Gin and CLIProxyAPI
// imports so it can be used from any layer.
//
// # Design rules
//
//   - Typed structs, not map[string]any. Provider-specific parameters that
//     have no IR equivalent ride in Request.Extensions as raw JSON so they
//     survive a round trip unchanged instead of being silently dropped.
//   - Optional or absent wire fields use pointers (or slices), so "not sent"
//     stays distinguishable from the zero value.
//   - Tool call IDs and raw payloads are preserved verbatim; protocols
//     disagree on argument types (string vs object) and on ID prefixes, so
//     the IR never normalizes them.
//   - Adding a protocol or a provider parameter must only require additive
//     changes here: new fields, never renamed or renumbered ones.
//
// # Stream event ordering contract
//
// StreamEvent sequences produced by outbound adapters and consumed by
// inbound streaming encoders follow a single shape:
//
//	message_start
//	( block_start block_delta* block_stop )*   // one group per content block
//	usage_update?                             // at most one, OpenAI sends it in the final chunk
//	message_stop                              // carries the stop reason
//
// A stream terminates with either message_stop or error — never both, and
// nothing arrives after a terminal event. ValidateEventOrder enforces the
// contract and is used by tests to keep adapters honest.
package ir
