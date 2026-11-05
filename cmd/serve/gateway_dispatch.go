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
)

// gatewayV2Middleware leaves the legacy chain untouched while disabled.
// Catalog publication updates the stable resolver before invoking onChange.
func gatewayV2Middleware(prefs *modelPrefs, enabled bool, proxyURL string) (gin.HandlerFunc, error) {
	if !enabled {
		return func(c *gin.Context) { c.Next() }, nil
	}
	resolver, err := prefs.modelResolver()
	if err != nil {
		return nil, err
	}
	registry := adapter.NewRegistry()
	registry.RegisterInbound(inboundanthropic.New())
	registry.RegisterInbound(inboundchat.New())
	registry.RegisterInbound(inboundresponses.New())
	registry.RegisterOutbound(outboundanthropic.New())
	registry.RegisterOutbound(outboundchat.New())
	registry.RegisterOutbound(outboundresponses.New())
	dispatcher := dispatch.New(resolver, &relay.Options{ProxyURL: proxyURL})
	dispatcher.Registry = registry
	previousReload := prefs.onChange
	prefs.onChange = func(list []customProvider) {
		dispatcher.Reset()
		if previousReload != nil {
			previousReload(list)
		}
	}
	return dispatcher.Handle, nil
}
