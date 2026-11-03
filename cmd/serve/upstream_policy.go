package main

import (
	"net/http"

	gwprovider "github.com/minhhhduc/gnvipi/internal/gateway/provider"
)

type validatedUpstreamTransport struct{ base http.RoundTripper }

func (t validatedUpstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := gwprovider.DefaultURLPolicy().ValidateRequest(req); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}
func (t validatedUpstreamTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func newGuardedCompatClient(policy gwprovider.URLPolicy) *http.Client {
	return &http.Client{Transport: policy.GuardTransport(nil, nil), CheckRedirect: policy.CheckRedirect}
}

// Called before loading configuration or accepting requests. This updates the
// default schema validator as well as legacy transports, not NVIDIA's client.
func configureUpstreamURLPolicy(policy gwprovider.URLPolicy) {
	gwprovider.SetDefaultURLPolicy(policy)
	compatClient.CloseIdleConnections()
	compatClient = newGuardedCompatClient(policy)
}

func validateLegacyProviderURLs(list []customProvider) error {
	policy := gwprovider.DefaultURLPolicy()
	for i := range list {
		if err := policy.Validate(list[i].BaseURL); err != nil {
			return err
		}
	}
	return nil
}
