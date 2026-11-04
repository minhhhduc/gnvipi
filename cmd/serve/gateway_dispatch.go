package main

import (
	"github.com/gin-gonic/gin"
	"github.com/minhhhduc/gnvipi/internal/gateway/dispatch"
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
	dispatcher := dispatch.New(resolver, &relay.Options{ProxyURL: proxyURL})
	previousReload := prefs.onChange
	prefs.onChange = func(list []customProvider) {
		dispatcher.Reset()
		if previousReload != nil {
			previousReload(list)
		}
	}
	return dispatcher.Handle, nil
}
