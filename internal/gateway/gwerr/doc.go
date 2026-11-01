// Package gwerr is the gateway's normalized error model: one typed error
// carrying everything the inbound protocol needs to render a correct
// response, plus the encoders that turn it into Anthropic Messages,
// OpenAI Chat and OpenAI Responses wire bodies.
//
// Every error path in the gateway — an invalid inbound request, an upstream
// 429, a read timeout, an internal fault — funnels into gwerr.Error. The
// Kind field is the protocol-independent classification; the Status field
// is the HTTP status the gateway will answer with; the encoders project
// the same error onto each inbound protocol's error shape, so one upstream
// failure produces one Anthropic-shaped response, one Chat-shaped response
// and one Responses-shaped response without three parallel hierarchies.
//
// The package depends only on the standard library. It deliberately defines
// its own Protocol type instead of importing internal/gateway/ir so the
// error model can be used where pulling the full IR would create a cycle;
// the string values match ir.Protocol's.
//
// Messages built from upstream bodies may echo provider text, which can
// embed a configured API key. Error carries the secrets to redact and the
// encoders scrub them, so a key that leaked into an upstream error message
// never reaches the client.
package gwerr
