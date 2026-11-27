package api

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

// forwardedProtoTrust decides whether the request's immediate peer is a
// trusted proxy whose X-Forwarded-Proto may be believed; nil trusts none.
var forwardedProtoTrust atomic.Pointer[func(*http.Request) bool]

// SetForwardedProtoTrust installs the trusted-proxy check used to read
// X-Forwarded-Proto when deciding a cookie's Secure flag. The sidecar sets
// it from api.trusted_proxies; nil trusts no proxy.
func SetForwardedProtoTrust(fn func(*http.Request) bool) {
	if fn == nil {
		forwardedProtoTrust.Store(nil)
		return
	}
	forwardedProtoTrust.Store(&fn)
}

// secureCookie reports whether a cookie set on this response should be
// Secure: the request arrived over TLS, or a trusted proxy says the
// browser connected over HTTPS.
func secureCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if r.Header.Get("X-Forwarded-Proto") != "https" {
		return false
	}
	if trusted := forwardedProtoTrust.Load(); trusted != nil && (*trusted)(r) {
		return true
	}
	warnUntrustedForwardedProto(r)
	return false
}

// untrustedProtoWarning makes the untrusted X-Forwarded-Proto warning fire
// once per process, so a client spoofing the header cannot flood the log.
var untrustedProtoWarning sync.Once

// warnUntrustedForwardedProto tells the operator, once, that a TLS proxy
// outside api.trusted_proxies is costing session cookies their Secure flag.
func warnUntrustedForwardedProto(r *http.Request) {
	untrustedProtoWarning.Do(func() {
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			peer = r.RemoteAddr
		}
		slog.Warn("session cookie not marked Secure: X-Forwarded-Proto: https came from "+
			"a peer outside api.trusted_proxies; list the TLS proxy there", "peer", peer)
	})
}
