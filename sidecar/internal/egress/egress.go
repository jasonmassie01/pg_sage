// Package egress is the one floor every outbound client dials through.
//
// It always refuses destinations that are never a legitimate integration
// target: unspecified addresses, link-local unicast (169.254.0.0/16 and
// fe80::/10, which hold the cloud instance metadata services), multicast,
// broadcast and the AWS IPv6 metadata address fd00:ec2::254. The check runs
// on every resolved address right before connect, so a hostname that
// resolves to a metadata address is refused too.
//
// Private and loopback addresses stay allowed: Ollama on localhost, on-prem
// relays and Prometheus inside the VPC are normal targets. Stricter callers
// (notify.TargetPolicy) layer their own rules on top of this floor.
//
// The single exemption is MetadataClient, which may reach only the one
// configured cloud metadata host.
package egress

import (
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

// ErrBlocked is returned (wrapped) when a destination is below the floor.
var ErrBlocked = errors.New("egress: destination blocked")

// defaultDialTimeout bounds a connect when the caller gives no timeout.
const defaultDialTimeout = 30 * time.Second

var (
	awsIMDSv6 = netip.MustParseAddr("fd00:ec2::254")
	thisNet   = netip.MustParsePrefix("0.0.0.0/8")
	broadcast = netip.MustParseAddr("255.255.255.255")
)

// metadataHostnames resolve to a metadata service; refusing the name too
// covers requests that go through a proxy, where the dial sees only the
// proxy's address.
var metadataHostnames = map[string]bool{
	"metadata.google.internal": true,
	"metadata":                 true,
}

// CheckAddr reports whether addr is below the egress floor.
func CheckAddr(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrBlocked)
	}
	addr = addr.Unmap()
	switch {
	case addr.IsUnspecified(), thisNet.Contains(addr):
		return fmt.Errorf("%w: unspecified address %s", ErrBlocked, addr)
	case addr.IsLinkLocalUnicast():
		return fmt.Errorf("%w: link-local address %s (instance metadata range)",
			ErrBlocked, addr)
	case addr.IsMulticast(), addr == broadcast:
		return fmt.Errorf("%w: multicast or broadcast address %s", ErrBlocked, addr)
	case addr == awsIMDSv6:
		return fmt.Errorf("%w: instance metadata address %s", ErrBlocked, addr)
	}
	return nil
}

// Control is a net.Dialer Control hook that refuses the resolved address
// right before connect.
func Control(_, address string, _ syscall.RawConn) error {
	addr, err := dialAddr(address)
	if err != nil {
		return err
	}
	return CheckAddr(addr)
}

func dialAddr(address string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: unparseable dial address", ErrBlocked)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: dial address %q is not an IP", ErrBlocked, host)
	}
	return addr, nil
}

// Dialer returns a net.Dialer enforcing the floor. A zero timeout uses
// defaultDialTimeout.
func Dialer(timeout time.Duration) *net.Dialer {
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: Control}
}

// Transport returns an http.Transport with the standard library defaults
// whose every connection goes through Dialer. Callers may adjust TLS,
// keep-alive or proxy settings; they must keep DialContext.
func Transport(timeout time.Duration) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = Dialer(timeout).DialContext
	t.Proxy = guardedProxy
	return t
}

// guardedProxy keeps the environment proxy settings and refuses metadata
// destinations by name or literal before a proxy could reach them.
func guardedProxy(req *http.Request) (*url.URL, error) {
	if err := CheckURL(req.URL); err != nil {
		return nil, err
	}
	return http.ProxyFromEnvironment(req)
}

// CheckURL statically refuses a URL whose host is a metadata hostname or an
// address literal below the floor. It does not resolve names.
func CheckURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: no URL", ErrBlocked)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if metadataHostnames[host] {
		return fmt.Errorf("%w: metadata host %q", ErrBlocked, host)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return CheckAddr(addr)
	}
	return nil
}

// HTTPClient returns an http.Client whose connections and redirects are
// checked against the floor. A zero timeout leaves the deadline to the
// request context.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: Transport(timeout),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return CheckURL(req.URL)
		},
	}
}

// SameOrigin reports whether two URLs share scheme, host and effective port.
// Unparseable or host-less URLs are never the same origin.
func SameOrigin(a, b string) bool {
	oa, okA := origin(a)
	ob, okB := origin(b)
	return okA && okB && oa == ob
}

func origin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	port := u.Port()
	if port == "" {
		port = defaultPorts[scheme]
	}
	return scheme + "://" + net.JoinHostPort(host, port), true
}

var defaultPorts = map[string]string{"http": "80", "https": "443"}
