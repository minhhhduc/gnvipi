package main

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

type clientIPResolver struct {
	trusted []netip.Prefix
}

func newClientIPResolver(raw string) (clientIPResolver, error) {
	var resolver clientIPResolver
	if strings.TrimSpace(raw) == "" {
		return resolver, nil
	}
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			addr, addrErr := netip.ParseAddr(field)
			if addrErr != nil || addr.Zone() != "" {
				return clientIPResolver{}, fmt.Errorf("invalid trusted proxy %q", field)
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		if prefix.Addr().Zone() != "" {
			return clientIPResolver{}, fmt.Errorf("invalid trusted proxy %q", field)
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return clientIPResolver{}, fmt.Errorf("invalid mapped trusted proxy %q", field)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		resolver.trusted = append(resolver.trusted, prefix.Masked())
	}
	return resolver, nil
}

func (r clientIPResolver) requestIP(req *http.Request) string {
	peer, ok := parseRequestIP(req.RemoteAddr)
	if !ok {
		return ""
	}
	if !r.isTrusted(peer) {
		return peer.String()
	}

	forwarded := req.Header.Values("X-Forwarded-For")
	if len(forwarded) > 0 {
		var hops []string
		for _, value := range forwarded {
			hops = append(hops, strings.Split(value, ",")...)
		}
		current := peer
		for i := len(hops) - 1; i >= 0 && r.isTrusted(current); i-- {
			next, valid := parseRequestIP(strings.TrimSpace(hops[i]))
			if !valid {
				return peer.String()
			}
			current = next
		}
		return current.String()
	}

	realIPs := req.Header.Values("X-Real-IP")
	if len(realIPs) == 1 {
		if realIP, valid := parseRequestIP(strings.TrimSpace(realIPs[0])); valid {
			return realIP.String()
		}
	}
	return peer.String()
}

func (r clientIPResolver) isTrusted(addr netip.Addr) bool {
	for _, prefix := range r.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func parseRequestIP(raw string) (netip.Addr, bool) {
	if addrPort, err := netip.ParseAddrPort(raw); err == nil {
		addr := addrPort.Addr().WithZone("").Unmap()
		return addr, addr.IsValid()
	}
	addr, err := netip.ParseAddr(strings.Trim(raw, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	addr = addr.WithZone("").Unmap()
	return addr, addr.IsValid()
}
