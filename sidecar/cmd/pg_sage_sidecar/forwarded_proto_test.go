package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// apiauth-10: the API believes X-Forwarded-Proto only from a peer listed
// in api.trusted_proxies.
func TestFromTrustedProxyMatchesOnlyListedPeers(t *testing.T) {
	restoreTrustedProxies := snapshotTrustedProxyNets()
	t.Cleanup(restoreTrustedProxies)
	setTrustedProxies([]string{"10.0.0.0/8"})
	for _, tc := range []struct {
		remote string
		want   bool
	}{
		{"10.2.3.4:443", true},
		{"[::1]:443", false},
		{"198.51.100.77:443", false},
		{"10.2.3.4", false}, // no port: not a peer address
		{"", false},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		req.RemoteAddr = tc.remote
		if got := fromTrustedProxy(req); got != tc.want {
			t.Errorf("fromTrustedProxy(%q) = %v, want %v", tc.remote, got, tc.want)
		}
	}
	setTrustedProxies(nil) // default: loopback only
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	if !fromTrustedProxy(req) {
		t.Fatal("loopback proxy not trusted by default")
	}
}
