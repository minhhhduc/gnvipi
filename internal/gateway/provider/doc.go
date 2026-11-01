// Package provider defines the gateway's typed provider configuration and the
// conversion from the legacy admin format.
//
// The pre-v2 admin page stores one customProvider entry per model and encodes
// the upstream protocol as two mutually exclusive booleans. A provider here
// carries several model mappings, an explicit protocol, request headers and
// credential references, which is what the routing engine and the admin API
// (internal/gateway/routing, #35, #38) need.
//
// The package is types and pure functions only: no file IO, no HTTP, no
// credential storage policy. Secrets referenced here are resolved elsewhere
// (#36), and base URL SSRF policy is enforced at parse and dial time (#37);
// Validate only rejects URLs that are syntactically unusable as a base.
package provider
