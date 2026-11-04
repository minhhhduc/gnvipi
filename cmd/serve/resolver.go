package main

import (
	"github.com/minhhhduc/gnvipi/internal/gateway/provider"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

// refreshRoutingLocked publishes provider/mask changes before existing reload
// callbacks run. The caller holds p.mu, so the snapshot cannot mix generations.
func (p *modelPrefs) refreshRoutingLocked() error {
	legacy := make([]provider.LegacyProvider, len(p.providers))
	for i := range p.providers {
		entry := &p.providers[i]
		legacy[i] = provider.LegacyProvider{Name: entry.Name, BaseURL: entry.BaseURL, APIKey: entry.APIKey, Model: entry.Model, Alias: entry.Alias, MessagesOnly: entry.MessagesOnly, ResponsesOnly: entry.ResponsesOnly}
	}
	hidden := make([]string, 0, len(p.hidden)+len(p.deleted))
	for id := range p.hidden {
		hidden = append(hidden, id)
	}
	for id := range p.deleted {
		hidden = append(hidden, id)
	}
	if p.resolver == nil {
		r, err := routing.NewLegacyResolver(legacy, hidden)
		if err != nil {
			return err
		}
		p.resolver = r
		return nil
	}
	return p.resolver.Update(legacy, hidden)
}

// modelResolver gives the future dispatcher a stable resolver instance. Existing
// legacy middleware remains unchanged; admin saves update this same instance.
func (p *modelPrefs) modelResolver() (*routing.LegacyResolver, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resolver == nil {
		if err := p.refreshRoutingLocked(); err != nil {
			return nil, err
		}
	}
	return p.resolver, nil
}
