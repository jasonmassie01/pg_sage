package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrTargetBlocked is returned when a notification target resolves to
// an address the TargetPolicy forbids (G7-B21).
var ErrTargetBlocked = errors.New("notification target blocked by policy")

// TargetPolicy governs where notification senders may connect.
// Link-local addresses (including the 169.254.169.254 cloud metadata
// endpoint), unspecified and multicast addresses are always refused.
// Loopback, RFC 1918 / ULA and CGNAT ranges, "localhost" and plain http
// are refused unless AllowPrivate is set (e.g. for an on-prem relay).
type TargetPolicy struct {
	AllowPrivate bool
}

var blockedHostnames = map[string]bool{
	"metadata.google.internal": true,
	"metadata":                 true,
}

// ValidateURL checks a webhook URL statically (no DNS). The dialer
// re-checks every resolved address at connect time.
func (p TargetPolicy) ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("%w: unparseable URL", ErrTargetBlocked)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowPrivate {
			return fmt.Errorf("%w: only https webhooks are allowed", ErrTargetBlocked)
		}
	default:
		return fmt.Errorf("%w: scheme %q not allowed", ErrTargetBlocked, u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: URL has no host", ErrTargetBlocked)
	}
	return p.ValidateHost(u.Hostname())
}

// ValidateHost checks a hostname or IP literal statically.
func (p TargetPolicy) ValidateHost(host string) error {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "" {
		return fmt.Errorf("%w: empty host", ErrTargetBlocked)
	}
	if blockedHostnames[h] {
		return fmt.Errorf("%w: metadata host %q", ErrTargetBlocked, h)
	}
	if (h == "localhost" || strings.HasSuffix(h, ".localhost")) && !p.AllowPrivate {
		return fmt.Errorf("%w: loopback host %q", ErrTargetBlocked, h)
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return p.checkAddr(addr)
	}
	return nil
}

func (p TargetPolicy) checkAddr(addr netip.Addr) error {
	addr = addr.Unmap()
	if addr.IsUnspecified() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() ||
		addr.IsInterfaceLocalMulticast() {
		return fmt.Errorf("%w: address %s", ErrTargetBlocked, addr)
	}
	if p.AllowPrivate {
		return nil
	}
	if addr.IsLoopback() || addr.IsPrivate() || cgnat.Contains(addr) {
		return fmt.Errorf("%w: private address %s", ErrTargetBlocked, addr)
	}
	return nil
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// dialControl rejects the resolved address right before connect, which
// also defeats DNS names that resolve to internal addresses.
func (p TargetPolicy) dialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTargetBlocked, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTargetBlocked, err)
	}
	return p.checkAddr(addr)
}

// Dialer returns a net.Dialer enforcing the policy.
func (p TargetPolicy) Dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: p.dialControl}
}

// HTTPClient returns an http.Client whose every connection (including
// redirects) is checked against the policy.
func (p TargetPolicy) HTTPClient(timeout time.Duration) *http.Client {
	dialer := p.Dialer(timeout)
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return p.ValidateURL(req.URL.String())
		},
	}
}

// dialContext is a helper for non-HTTP senders (SMTP).
func (p TargetPolicy) dialContext(
	ctx context.Context, timeout time.Duration, addr string,
) (net.Conn, error) {
	return p.Dialer(timeout).DialContext(ctx, "tcp", addr)
}
