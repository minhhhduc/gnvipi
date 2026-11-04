package main

import (
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

// refreshRoutingLocked publishes provider/mask changes before existing reload
// callbacks run. The caller holds p.mu, so the snapshot cannot mix generations.
func (p *modelPrefs) refreshRoutingLocked() error {
	hidden := make([]string, 0, len(p.hidden)+len(p.deleted))
	for id := range p.hidden {
		hidden = append(hidden, id)
	}
	for id := range p.deleted {
		hidden = append(hidden, id)
	}
	resolver := p.resolver
	if resolver == nil {
		resolver = &routing.LegacyResolver{}
	}
	var err error
	if p.providerStore != nil {
		err = resolver.UpdateProviders(p.providerStore.List(), hidden)
	} else {
		err = resolver.Update(toLegacyProviders(p.providers), hidden)
	}
	if err != nil {
		return err
	}
	p.resolver = resolver
	return nil
}

// modelResolver gives the dispatcher a stable resolver. Admin saves publish
// full provider policy and mask snapshots before reload callbacks run.
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
