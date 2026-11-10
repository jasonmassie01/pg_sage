package api

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-07: SSO that is configured but has not discovered its identity
// provider yet is "enabled, not ready", never "not configured".

// ssoNotReadyCode is the 503 code for an SSO step tried before discovery
// succeeded; the dashboard shows "SSO temporarily unavailable".
const ssoNotReadyCode = "sso_not_ready"

// ssoRetryAfter (seconds) tells clients when to ask again; discovery
// retries on its own backoff (at most 5 min apart) meanwhile.
const ssoRetryAfter = "30"

// routerSSOProvider is the provider the last router build registered, for
// the SSO discovery metrics; nil when OAuth is disabled.
var routerSSOProvider atomic.Pointer[auth.OAuthProvider]

// SSODiscoveryStatus is what /metrics reports about SSO discovery.
type SSODiscoveryStatus struct {
	Enabled  bool
	Ready    bool
	Failures uint64
}

// CurrentSSODiscoveryStatus reads the registered provider's discovery
// state; the zero value means OAuth is disabled.
func CurrentSSODiscoveryStatus() SSODiscoveryStatus {
	p := routerSSOProvider.Load()
	if p == nil {
		return SSODiscoveryStatus{}
	}
	ready, _ := p.Ready()
	return SSODiscoveryStatus{Enabled: true, Ready: ready, Failures: p.DiscoveryFailures()}
}

// ssoNotReady answers 503 sso_not_ready when provider has not discovered
// its identity provider, and reports whether it did.
func ssoNotReady(w http.ResponseWriter, provider *auth.OAuthProvider) bool {
	ready, reason := provider.Ready()
	if ready {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", ssoRetryAfter)
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "SSO temporarily unavailable", "code": ssoNotReadyCode, "reason": reason,
	})
	return true
}
