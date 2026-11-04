package main

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	gwprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

type sdkPolicyTransports struct {
	policy       gwprovider.URLPolicy
	resolver     gwprovider.IPResolver
	defaultProxy string
	mu           sync.Mutex
	byProxy      map[string]http.RoundTripper
}

// Configure this after Build, which installs its own transport provider. The
// SDK's HTTP-client helper otherwise prioritizes cfg/auth proxies over context.
func installSDKPolicyTransports(core *coreauth.Manager, cfg *config.Config, transports *sdkPolicyTransports) {
	transports.defaultProxy = strings.TrimSpace(cfg.ProxyURL)
	cfg.ProxyURL = ""
	core.SetRoundTripperProvider(transports)
}

type rejectedTransport struct{ err error }

func (t rejectedTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

// The SDK uses this injected transport for translated custom-provider requests.
// NVIDIA already owns its transport and never receives this replacement.
func (p *sdkPolicyTransports) RoundTripperFor(auth *coreauth.Auth) http.RoundTripper {
	if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), nvidiaProvider) {
		return nil
	}
	proxyRaw := p.defaultProxy
	if auth != nil {
		if override := strings.TrimSpace(auth.ProxyURL); override != "" {
			proxyRaw = override
		}
		// Manager supplies an execution clone, not its stored auth. Keep the
		// configured proxy in our transport and prevent the SDK helper from
		// replacing that transport with its unguarded explicit-proxy client.
		auth.ProxyURL = ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if transport := p.byProxy[proxyRaw]; transport != nil {
		return transport
	}
	setting, err := proxyutil.Parse(proxyRaw)
	if err != nil {
		return rejectedTransport{err: errors.New("invalid SDK upstream proxy URL")}
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	switch setting.Mode {
	case proxyutil.ModeDirect:
		base.Proxy = nil
	case proxyutil.ModeProxy:
		base.Proxy = http.ProxyURL(setting.URL)
	}
	transport := p.policy.GuardTransport(base, p.resolver)
	if p.byProxy == nil {
		p.byProxy = make(map[string]http.RoundTripper)
	}
	p.byProxy[proxyRaw] = transport
	return transport
}
