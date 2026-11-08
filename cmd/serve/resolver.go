package main

import (
	"log"

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
		var err error
		resolver, err = routing.NewRegistryResolver(nil, nil, routing.WithWarningFunc(func(msg string) {
			log.Printf("routing: %s", msg)
		}))
		if err != nil {
			return err
		}
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
	var cycleModels []string
	if p.cycle != nil {
		cycleModels = p.cycle.Models
	}
	resolver.UpdateVirtual(routing.VirtualOptions{
		AutoModel:              p.autoModelIDLocked(),
		AutoModelChatResponses: p.autoModelChatResponses,
		CycleModels:            cycleModels,
		CycleIndex:             func() int { p.mu.RLock(); defer p.mu.RUnlock(); return p.cycleIndex },
		AdvanceCycle:           func(failed string) { p.advanceCycle("cycle", failed) },
	})
	p.resolver = resolver
	return nil
}

// modelResolver gives the dispatcher a stable resolver. Admin saves publish
// full provider policy and mask snapshots before reload callbacks run.
func (p *modelPrefs) modelResolver() (*routing.RegistryResolver, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.resolver == nil {
		if err := p.refreshRoutingLocked(); err != nil {
			return nil, err
		}
	}
	return p.resolver, nil
}
