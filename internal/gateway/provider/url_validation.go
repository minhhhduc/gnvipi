package provider

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

// URLPolicy permits local/LAN services by default, but blocks link-local and
// metadata targets. DenyPrivate is stronger than AllowUnsafe when both are set.
type URLPolicy struct{ AllowUnsafe, DenyPrivate bool }

var defaultURLPolicy atomic.Pointer[URLPolicy]

// SetDefaultURLPolicy configures the process-wide provider policy at startup.
// Publication is atomic; configuration validators use the same policy as dials.
func SetDefaultURLPolicy(p URLPolicy) { defaultURLPolicy.Store(&p) }
func DefaultURLPolicy() URLPolicy {
	if p := defaultURLPolicy.Load(); p != nil {
		return *p
	}
	return URLPolicy{}
}

func (p URLPolicy) Validate(raw string) error {
	if err := p.validate(raw, true); err != nil {
		return &FieldError{Field: "base_url", Problem: err.Error()}
	}
	return nil
}

func (p URLPolicy) ValidateRequest(req *http.Request) error {
	if req == nil || req.URL == nil {
		return &FieldError{Field: "base_url", Problem: "request URL is required"}
	}
	if err := p.validateParsed(req.URL, false); err != nil {
		return &FieldError{Field: "base_url", Problem: err.Error()}
	}
	if req.Response != nil && req.Response.Request != nil {
		previous := req.Response.Request.URL
		if previous != nil && (req.URL.Scheme != previous.Scheme || !strings.EqualFold(req.URL.Host, previous.Host)) {
			return &FieldError{Field: "base_url", Problem: "cross-origin upstream redirect is forbidden"}
		}
	}
	return nil
}

func (p URLPolicy) validate(raw string, base bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("must be a valid URL")
	}
	return p.validateParsed(u, base)
}

func (p URLPolicy) validateParsed(u *url.URL, base bool) error {
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("must use http or https")
	case u.Hostname() == "" || strings.TrimRight(u.Hostname(), ".") == "":
		return errors.New("must include a host")
	case u.User != nil:
		return errors.New("must not contain userinfo")
	case base && (u.RawQuery != "" || u.Fragment != ""):
		return errors.New("must not include a query or fragment")
	case strings.HasSuffix(u.Host, ":"):
		return errors.New("must include a valid port")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("port must be between 1 and 65535")
		}
	}
	return p.checkHost(u.Hostname())
}

func (p URLPolicy) checkHost(host string) error {
	normalized := strings.TrimRight(host, ".")
	if strings.EqualFold(normalized, "metadata.google.internal") && (!p.AllowUnsafe || p.DenyPrivate) {
		return errors.New("cloud metadata upstream is forbidden")
	}
	if p.DenyPrivate && (strings.EqualFold(normalized, "localhost") || strings.HasSuffix(strings.ToLower(normalized), ".localhost")) {
		return errors.New("private upstream is forbidden")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return p.checkIP(addr)
	}
	return nil
}

var awsIPv6Metadata = netip.MustParseAddr("fd00:ec2::254")

func (p URLPolicy) checkIP(addr netip.Addr) error {
	addr = addr.Unmap()
	local := addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr == awsIPv6Metadata
	if local && (!p.AllowUnsafe || p.DenyPrivate) {
		return errors.New("link-local or metadata upstream is forbidden")
	}
	if p.DenyPrivate && (addr.IsPrivate() || addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast()) {
		return errors.New("private upstream is forbidden")
	}
	return nil
}

// Control is net.Dialer's post-resolution, pre-connect hook. It checks the
// actual IP, not an earlier hostname lookup, so direct DNS rebinding cannot
// bypass policy. ProxyDialer also pins the final destination before a tunnel.
func (p URLPolicy) Control(_, address string, _ syscall.RawConn) error {
	host, _, err := splitDialAddress(address)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return &FieldError{Field: "base_url", Problem: "dial control requires a resolved IP"}
	}
	if err := p.checkIP(addr); err != nil {
		return &FieldError{Field: "base_url", Problem: err.Error()}
	}
	return nil
}

// CheckRedirect rejects cross-origin redirects before credentials can be sent
// to another host. Same-origin redirects still pass the final-IP dial guard.
func (p URLPolicy) CheckRedirect(req *http.Request, via []*http.Request) error {
	if err := p.ValidateRequest(req); err != nil {
		return err
	}
	if len(via) >= 10 {
		return errors.New("too many upstream redirects")
	}
	if len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || !strings.EqualFold(req.URL.Host, via[0].URL.Host)) {
		return errors.New("cross-origin upstream redirect is forbidden")
	}
	return nil
}
