// Package adapter defines the contract every protocol adapter implements and
// the registry the dispatcher resolves them from.
//
// The gateway speaks three protocols — Anthropic Messages, OpenAI Chat
// Completions and OpenAI Responses — on both sides. Rather than six pairwise
// translations, an inbound adapter decodes a client request into the
// protocol-neutral internal representation (internal/gateway/ir) and an
// outbound adapter encodes it again for the upstream provider. The interfaces
// here are the only coupling between the two sides.
//
// # Passthrough vs translation
//
// When the inbound and outbound protocols match, the dispatcher must not
// decode and re-encode: it copies the request body and relays the upstream
// response stream verbatim (internal/gateway/core relay, #13). That is both
// faster and lossless, so the adapters are written to be bypassed entirely —
// no adapter is required to reproduce its own wire format exactly.
//
// Translation is used whenever the protocols differ. The IR is then the only
// contract between them: a feature the outbound protocol cannot express is
// dropped or rejected according to Capabilities, never silently mangled.
//
// # Errors
//
// Adapter methods return errors; failures that a client must see are
// *gwerr.Error values (or wrap one) so the dispatcher can render them in the
// inbound protocol with internal/gateway/gwerr. Adapters do not render error
// bodies themselves.
//
// # Streams
//
// Outbound adapters expose an upstream stream as a Stream of ir.StreamEvent
// values; inbound adapters render such a stream as their protocol's SSE.
// Event sequences follow the ordering contract documented on internal/gateway/ir:
// message_start, block groups, optional usage_update, then exactly one
// terminal event.
//
// # Registration
//
// Adapters live in their own packages (internal/gateway/inbound/<proto>,
// internal/gateway/outbound/<proto>) and are registered explicitly from one
// wiring point in cmd/serve. Nothing registers from init(), so adapter
// packages can be added in parallel without merge conflicts.
package adapter
