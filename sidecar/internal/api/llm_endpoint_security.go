package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func validateLLMDiscoveryEndpoint(ctx context.Context, endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parse endpoint: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("endpoint scheme must be http or https")
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("endpoint host is required")
	}
	if parsed.User != nil {
		return fmt.Errorf("endpoint userinfo is forbidden")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "metadata.google.internal" {
		return fmt.Errorf("metadata endpoint is forbidden")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return fmt.Errorf("endpoint DNS lookup failed")
	}
	for _, address := range addresses {
		if unsafeLLMEndpointIP(address.IP) {
			return fmt.Errorf("endpoint resolves to a non-public address")
		}
	}
	return nil
}

func unsafeLLMEndpointIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

func sameLLMEndpoint(left, right string) bool {
	leftURL, leftErr := url.Parse(strings.TrimRight(left, "/"))
	rightURL, rightErr := url.Parse(strings.TrimRight(right, "/"))
	if leftErr != nil || rightErr != nil {
		return false
	}
	return strings.EqualFold(leftURL.Scheme, rightURL.Scheme) &&
		strings.EqualFold(leftURL.Host, rightURL.Host) &&
		leftURL.EscapedPath() == rightURL.EscapedPath()
}

func safeLLMDiscoveryHTTPClient() *http.Client {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(
			ctx context.Context, network, address string,
		) (net.Conn, error) {
			return dialSafeLLMEndpoint(ctx, dialer, network, address)
		},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(
			_ *http.Request, _ []*http.Request,
		) error {
			return fmt.Errorf("provider redirects are forbidden")
		},
	}
}

func dialSafeLLMEndpoint(
	ctx context.Context,
	dialer *net.Dialer,
	network string,
	address string,
) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("split provider address: %w", err)
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("provider DNS lookup failed")
	}
	for _, resolved := range addresses {
		if unsafeLLMEndpointIP(resolved.IP) {
			return nil, fmt.Errorf("provider resolved to a non-public address")
		}
	}
	for _, resolved := range addresses {
		pinned := net.JoinHostPort(resolved.IP.String(), port)
		conn, dialErr := dialer.DialContext(ctx, network, pinned)
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
	}
	return nil, fmt.Errorf("connect to provider: %w", err)
}
