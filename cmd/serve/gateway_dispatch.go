package main

import (
	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/adapter"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
	inboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/inbound/anthropic"
	inboundchat "github.com/minhhhduc/gnvipi/internal/gateway/inbound/openaichat"
	inboundresponses "github.com/minhhhduc/gnvipi/internal/gateway/inbound/openairesponses"
	outboundanthropic "github.com/minhhhduc/gnvipi/internal/gateway/outbound/anthropic"
	outboundchat "github.com/minhhhduc/gnvipi/internal/gateway/outbound/openaichat"
	outboundresponses "github.com/minhhhduc/gnvipi/internal/gateway/outbound/openairesponses"
	"github.com/minhhhduc/gnvipi/internal/gateway/relay"
	"github.com/minhhhduc/gnvipi/internal/gateway/routing"
)

// gatewayV2Middleware leaves the legacy chain untouched while disabled.
// Catalog publication updates the stable resolver before invoking onChange.
// A providerRuntime enables key-aware routing for database-authenticated users;
// anonymous and file-mode requests still use the legacy registry resolver.
func gatewayV2Middleware(prefs *modelPrefs, enabled bool, proxyURL string) (gin.HandlerFunc, error) {
	dispatcher, err := newGatewayDispatcher(prefs, enabled, proxyURL, nil)
	if err != nil {
		return nil, err
	}
	if dispatcher == nil {
		return func(c *gin.Context) { c.Next() }, nil
	}
	return dispatcher.Handle, nil
}

// newGatewayDispatcher builds the shared routing/relay stack. A nil dispatcher
// means the v2 gateway is disabled. The console playground reuses this same
// dispatcher through the engine's middleware chain.
func newGatewayDispatcher(prefs *modelPrefs, enabled bool, proxyURL string, runtime *providerRuntime) (*dispatch.Dispatcher, error) {
	if !enabled {
		return nil, nil
	}
	registry, err := prefs.modelResolver()
	if err != nil {
		return nil, err
	}
	var resolver routing.Resolver = registry
	if runtime != nil {
		resolver = routing.NewKeyAwareResolver(runtime.router, registry)
	}
	registryAdapters := adapter.NewRegistry()
	registryAdapters.RegisterInbound(inboundanthropic.New())
	registryAdapters.RegisterInbound(inboundchat.New())
	registryAdapters.RegisterInbound(inboundresponses.New())
	registryAdapters.RegisterOutbound(outboundanthropic.New())
	registryAdapters.RegisterOutbound(outboundchat.New())
	registryAdapters.RegisterOutbound(outboundresponses.New())
	dispatcher := dispatch.New(resolver, &relay.Options{ProxyURL: proxyURL})
	dispatcher.Registry = registryAdapters
	previousReload := prefs.onChange
	prefs.onChange = func(list []customProvider) {
		dispatcher.Reset()
		if previousReload != nil {
			previousReload(list)
		}
	}
	return dispatcher, nil
}
