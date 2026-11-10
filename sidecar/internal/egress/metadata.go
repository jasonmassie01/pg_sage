package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// MetadataClient is the one exemption from the floor: a client for the
// cloud metadata token source that may reach only the configured metadata
// host (for example metadata.google.internal or $GCE_METADATA_HOST). It
// never uses a proxy and never follows redirects.
func MetadataClient(host string, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	want := metadataDialTarget(host)
	dialer := &net.Dialer{Timeout: timeout, Control: metadataControl}
	transport := &http.Transport{
		Proxy:                 nil,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if want == "" || !strings.EqualFold(address, want) {
				return nil, fmt.Errorf("%w: metadata client may reach only %q", ErrBlocked, host)
			}
			return dialer.DialContext(ctx, network, address)
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("%w: metadata redirects are refused", ErrBlocked)
		},
	}
}

// metadataDialTarget is the host:port the transport dials for host; the
// metadata service is plain http, so a bare host means port 80.
func metadataDialTarget(host string) string {
	host = strings.TrimSuffix(strings.TrimSpace(host), "/")
	if host == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), "80")
}

// metadataControl keeps the floor except for link-local unicast, where the
// metadata service lives.
func metadataControl(_, address string, _ syscall.RawConn) error {
	addr, err := dialAddr(address)
	if err != nil {
		return err
	}
	if addr.Unmap().IsLinkLocalUnicast() {
		return nil
	}
	return CheckAddr(addr)
}
