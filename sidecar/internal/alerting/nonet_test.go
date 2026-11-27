package alerting

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
)

// guardDefaultTransport makes the default transport refuse every
// non-loopback address, so a channel that regresses to a client without
// the injected policy fails its test instead of reaching a real service
// such as events.pagerduty.com (alerting-11).
func guardDefaultTransport() {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic(fmt.Sprintf("http.DefaultTransport is %T", http.DefaultTransport))
	}
	tr = tr.Clone()
	tr.Proxy = nil
	dialer := &net.Dialer{}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("test transport: %w", err)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("test transport refuses non-loopback %s", addr)
		}
		return dialer.DialContext(ctx, network, addr)
	}
	http.DefaultTransport = tr
}
