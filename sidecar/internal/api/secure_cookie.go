package api

import (
	"net/http"
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
	trusted := forwardedProtoTrust.Load()
	return trusted != nil && (*trusted)(r)
}
