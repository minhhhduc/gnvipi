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
	if p.Models != nil {
		out.Models = make([]ModelMapping, len(p.Models))
		for i, m := range p.Models {
			out.Models[i] = m
			out.Models[i].Fallbacks = slices.Clone(m.Fallbacks)
			out.Models[i].FallbackStatuses = slices.Clone(m.FallbackStatuses)
		}
	}
	out.ForwardHeaders = slices.Clone(p.ForwardHeaders)
	out.EndpointPaths = maps.Clone(p.EndpointPaths)
	out.ExtraBody = slices.Clone(p.ExtraBody)
	out.Extensions = maps.Clone(p.Extensions)
	return out
}
