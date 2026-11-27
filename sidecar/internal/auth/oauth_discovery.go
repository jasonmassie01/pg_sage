package auth

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// apiauth-07: discovery is not a one-shot startup step. A failed attempt
// leaves SSO "enabled but not ready"; StartDiscovery retries with capped
// backoff, refreshes daily, and keeps the last good state on failure.

// Discovery readiness reasons, reported by Ready and /auth/oauth/config.
const (
	// ReasonPending: no discovery attempt has finished yet.
	ReasonPending = "identity provider not contacted yet"
	// ReasonIdPUnreachable: the last attempt failed in transport, with a
	// non-200 answer, or with a body that was not a discovery document.
	ReasonIdPUnreachable = "identity provider unreachable"
	// ReasonMisconfigured: the configuration or the discovery document is
	// wrong (issuer mismatch, missing endpoints); retrying soon cannot help.
	ReasonMisconfigured = "misconfigured"
)

// ErrDiscoveryMisconfigured marks a discovery failure that only a
// configuration change fixes. It is retried once a day, not on backoff.
var ErrDiscoveryMisconfigured = errors.New("oauth: identity provider misconfigured")

const (
	discoveryBackoffMin      = 2 * time.Second
	discoveryBackoffMax      = 5 * time.Minute
	discoveryRefreshInterval = 24 * time.Hour
)

type discoveryKind int

const (
	discoveryOK discoveryKind = iota
	discoveryTransient
	discoveryMisconfigured
)

// discoveryOutcome is the result of the last finished discovery attempt.
type discoveryOutcome struct {
	attempted bool
	kind      discoveryKind
	// loggedMisconfig is the misconfiguration last logged at ERROR, so a
	// daily retry that fails the same way does not log it again.
	loggedMisconfig string
}

// Ready reports whether SSO can start a login, and why not when it cannot.
// It stays true once any discovery succeeded: a failed refresh keeps the
// last good endpoints and verifier.
func (p *OAuthProvider) Ready() (bool, string) {
	p.discMu.RLock()
	defer p.discMu.RUnlock()
	switch {
	case p.discovery != nil:
		return true, ""
	case !p.outcome.attempted:
		return false, ReasonPending
	case p.outcome.kind == discoveryMisconfigured:
		return false, ReasonMisconfigured
	default:
		return false, ReasonIdPUnreachable
	}
}

// DiscoveryFailures is how many discovery attempts failed since start
// (pg_sage_sso_discovery_failures_total).
func (p *OAuthProvider) DiscoveryFailures() uint64 {
	return p.failures.Load()
}

// StartDiscovery keeps OIDC discovery current until ctx ends. It schedules
// from the last Discover outcome (an immediate attempt when there was
// none): transient failures retry on jittered backoff from 2 s to 5 min,
// a success or a misconfiguration waits 24 h. GitHub's endpoints are
// static, so it returns at once for GitHub.
func (p *OAuthProvider) StartDiscovery(ctx context.Context) {
	if !p.isOIDC() {
		return
	}
	backoff := discoveryBackoffMin
	for p.waitFunc()(ctx, p.nextDiscoveryDelay(&backoff)) {
		err := p.discover(ctx)
		if ctx.Err() != nil {
			return
		}
		p.recordDiscovery(err)
	}
}

// nextDiscoveryDelay is the wait before the next attempt; backoff is the
// loop's current transient backoff, doubled up to the cap on each use.
func (p *OAuthProvider) nextDiscoveryDelay(backoff *time.Duration) time.Duration {
	p.discMu.RLock()
	outcome := p.outcome
	p.discMu.RUnlock()
	switch {
	case !outcome.attempted:
		return 0
	case outcome.kind == discoveryTransient:
		d := p.jitterFunc()(*backoff)
		*backoff = min(*backoff*2, discoveryBackoffMax)
		return d
	default:
		*backoff = discoveryBackoffMin
		return discoveryRefreshInterval
	}
}

// recordDiscovery stores an attempt's outcome, counts failures, and logs:
// WARN per transient failure, ERROR once per distinct misconfiguration,
// INFO on recovery.
func (p *OAuthProvider) recordDiscovery(err error) {
	kind := classifyDiscovery(err)
	if err != nil {
		p.failures.Add(1)
	}
	p.discMu.Lock()
	prev := p.outcome
	p.outcome = discoveryOutcome{attempted: true, kind: kind}
	logError := false
	if kind == discoveryMisconfigured {
		logError = prev.loggedMisconfig != err.Error()
		p.outcome.loggedMisconfig = err.Error()
	}
	ready := p.discovery != nil
	p.discMu.Unlock()

	log := p.log()
	switch {
	case kind == discoveryOK && prev.attempted && prev.kind != discoveryOK:
		log.Info("oidc discovery recovered; SSO sign-in is available")
	case kind == discoveryTransient:
		log.Warn("oidc discovery failed; retrying with backoff",
			"error", err, "sso_ready", ready, "failures", p.failures.Load())
	case logError:
		log.Error("oidc discovery rejected the identity provider configuration; "+
			"fix oauth.issuer_url or the IdP, then restart or wait for the daily retry",
			"error", err, "sso_ready", ready)
	case kind == discoveryMisconfigured:
		log.Debug("oidc discovery still misconfigured", "error", err)
	}
}

func classifyDiscovery(err error) discoveryKind {
	switch {
	case err == nil:
		return discoveryOK
	case errors.Is(err, ErrDiscoveryMisconfigured):
		return discoveryMisconfigured
	default:
		return discoveryTransient
	}
}

// discovered is one consistent snapshot of the discovery state.
func (p *OAuthProvider) discovered() (*OIDCDiscovery, *oidc.IDTokenVerifier) {
	p.discMu.RLock()
	defer p.discMu.RUnlock()
	return p.discovery, p.verifier
}

// storeDiscovered replaces the endpoints and verifier together.
func (p *OAuthProvider) storeDiscovered(disc *OIDCDiscovery, v *oidc.IDTokenVerifier) {
	p.discMu.Lock()
	defer p.discMu.Unlock()
	p.discovery, p.verifier = disc, v
}

func (p *OAuthProvider) waitFunc() func(context.Context, time.Duration) bool {
	if p.wait != nil {
		return p.wait
	}
	return waitDiscovery
}

func (p *OAuthProvider) jitterFunc() func(time.Duration) time.Duration {
	if p.jitter != nil {
		return p.jitter
	}
	return discoveryJitter
}

func (p *OAuthProvider) log() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

// waitDiscovery sleeps d; false when ctx ended first (or already had).
func waitDiscovery(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// discoveryJitter shortens d by up to 20% so restarted replicas do not
// retry in step; it never exceeds d, so the 5 min cap holds.
func discoveryJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d - time.Duration(rand.Int64N(int64(d/5)+1))
}
