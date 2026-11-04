package provider

import (
	"maps"
	"slices"
)

// Clone returns an owned snapshot of every provider collection. Scalar fields,
// including credential strings, proxy URLs and model positions, copy by value.
func Clone(p *Provider) Provider {
	out := *p
	out.Headers = maps.Clone(p.Headers)
	out.Models = slices.Clone(p.Models)
	out.ForwardHeaders = slices.Clone(p.ForwardHeaders)
	out.EndpointPaths = maps.Clone(p.EndpointPaths)
	out.ExtraBody = slices.Clone(p.ExtraBody)
	return out
}
