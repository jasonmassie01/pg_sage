package api

import "net"

// ClientLimiterKey is the rate-limit identity of a client address: an IPv6
// address is masked to its /64, the smallest block a single customer is
// usually given, so rotating addresses inside it buys no extra budget. An
// IPv4 address (including an IPv4-mapped one) is returned in dotted form,
// and anything that is not an IP literal is returned unchanged.
func ClientLimiterKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
