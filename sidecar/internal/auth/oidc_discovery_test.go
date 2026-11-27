package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/fakeidp"
)

// apiauth-07: one failed OIDC discovery must not turn SSO off until a
// restart. Discovery retries with capped backoff, refreshes daily, keeps
// the last good state, and reports why it is not ready.

// fakeDiscoveryClock records every wait StartDiscovery asks for and ends
// the loop after stopAfter waits, so no test sleeps for real.
type fakeDiscoveryClock struct {
	mu        sync.Mutex
	delays    []time.Duration
	stopAfter int
}

func (c *fakeDiscoveryClock) wait(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delays = append(c.delays, d)
	return len(c.delays) < c.stopAfter && ctx.Err() == nil
}

func (c *fakeDiscoveryClock) seen() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.delays...)
}

// lockedBuffer is a log sink safe for the discovery goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) count(level string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), `"level":"`+level+`"`)
}

// newDiscoveryProvider is an OIDC provider for idp with a fake clock, an
// identity jitter and a captured log; it has not discovered yet.
func newDiscoveryProvider(
	t *testing.T, idp *fakeidp.IdP, stopAfter int,
) (*OAuthProvider, *fakeDiscoveryClock, *lockedBuffer) {
	t.Helper()
	p := NewOAuthProvider(&config.OAuthConfig{
		Provider: "oidc", IssuerURL: idp.Issuer(), ClientID: idp.ClientID,
		ClientSecret: "fixture-client-secret", RedirectURL: "https://sage.test/cb",
	})
	clock := &fakeDiscoveryClock{stopAfter: stopAfter}
	logs := &lockedBuffer{}
	p.wait = clock.wait
	p.jitter = func(d time.Duration) time.Duration { return d }
	p.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{
		Level: slog.LevelDebug}))
	return p, clock, logs
}

func assertDelays(t *testing.T, got, want []time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wait %d = %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestDiscoveryRetry_TransientFailuresThenReadyWithoutRestart(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	idp.FailDiscovery(2, http.StatusServiceUnavailable)
	p, clock, _ := newDiscoveryProvider(t, idp, 3)

	err := p.Discover(context.Background())
	if err == nil || errors.Is(err, ErrDiscoveryMisconfigured) {
		t.Fatalf("first discovery: err = %v, want a transient failure", err)
	}
	if ready, reason := p.Ready(); ready || reason != ReasonIdPUnreachable {
		t.Fatalf("after 503: Ready() = %v, %q; want false, %q",
			ready, reason, ReasonIdPUnreachable)
	}
	if _, _, err := p.AuthorizationURL(); err == nil {
		t.Fatal("AuthorizationURL succeeded before discovery")
	}

	p.StartDiscovery(context.Background())

	assertDelays(t, clock.seen(), []time.Duration{
		2 * time.Second, 4 * time.Second, 24 * time.Hour})
	if ready, reason := p.Ready(); !ready || reason != "" {
		t.Fatalf("after recovery: Ready() = %v, %q; want true, \"\"", ready, reason)
	}
	authURL, _, err := p.AuthorizationURL()
	if err != nil || !strings.HasPrefix(authURL, idp.Issuer()+"/authorize?") {
		t.Fatalf("AuthorizationURL = %q, %v; want the IdP authorize endpoint", authURL, err)
	}
	if _, err := login(t, idp, p); err != nil {
		t.Fatalf("login after recovery: %v", err)
	}
	if got := p.DiscoveryFailures(); got != 2 {
		t.Fatalf("DiscoveryFailures = %d, want 2", got)
	}
	if got := idp.DiscoveryRequests(); got != 3 {
		t.Fatalf("discovery requests = %d, want 3", got)
	}
}

func TestDiscoveryMisconfigured_LoggedOnceAndRetriedDaily(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	idp.SetDiscovery(map[string]any{"issuer": "https://other-issuer.invalid"})
	p, clock, logs := newDiscoveryProvider(t, idp, 3)

	err := p.Discover(context.Background())
	if !errors.Is(err, ErrDiscoveryMisconfigured) || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("issuer mismatch: err = %v, want ErrDiscoveryMisconfigured", err)
	}
	if ready, reason := p.Ready(); ready || reason != ReasonMisconfigured {
		t.Fatalf("Ready() = %v, %q; want false, %q", ready, reason, ReasonMisconfigured)
	}

	p.StartDiscovery(context.Background())

	// Never retried inside 24 h: every wait after a misconfiguration is a day.
	assertDelays(t, clock.seen(), []time.Duration{24 * time.Hour, 24 * time.Hour,
		24 * time.Hour})
	if got := idp.DiscoveryRequests(); got != 3 {
		t.Fatalf("discovery requests = %d, want 3 (startup + one per day)", got)
	}
	if got := logs.count("ERROR"); got != 1 {
		t.Fatalf("ERROR logs = %d, want exactly 1 for a repeated misconfiguration", got)
	}
	if got := p.DiscoveryFailures(); got != 3 {
		t.Fatalf("DiscoveryFailures = %d, want 3", got)
	}
}

func TestDiscoveryMisconfigured_ClassifiesConfigAndDocumentErrors(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	cases := []struct {
		name string
		cfg  config.OAuthConfig
		doc  map[string]any
	}{
		{"missing issuer_url", config.OAuthConfig{Provider: "oidc"}, nil},
		{"unknown provider", config.OAuthConfig{Provider: "okta-ish"}, nil},
		{"missing endpoints", config.OAuthConfig{Provider: "oidc", IssuerURL: idp.Issuer()},
			map[string]any{"token_endpoint": nil}},
		{"missing jwks_uri", config.OAuthConfig{Provider: "oidc", IssuerURL: idp.Issuer()},
			map[string]any{"jwks_uri": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp.SetDiscovery(tc.doc)
			p := NewOAuthProvider(&tc.cfg)
			err := p.Discover(context.Background())
			if !errors.Is(err, ErrDiscoveryMisconfigured) {
				t.Fatalf("err = %v, want ErrDiscoveryMisconfigured", err)
			}
			if ready, reason := p.Ready(); ready || reason != ReasonMisconfigured {
				t.Fatalf("Ready() = %v, %q; want false, misconfigured", ready, reason)
			}
		})
	}
}

func TestDiscoveryTransient_OutageAndCancelAreNotMisconfigured(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	idp.FailDiscovery(1, http.StatusNotFound)
	p, _, _ := newDiscoveryProvider(t, idp, 1)
	if err := p.Discover(context.Background()); err == nil ||
		errors.Is(err, ErrDiscoveryMisconfigured) {
		t.Fatalf("404 page: err = %v, want transient", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Discover(ctx); err == nil || errors.Is(err, ErrDiscoveryMisconfigured) {
		t.Fatalf("cancelled: err = %v, want transient", err)
	}
	if _, reason := p.Ready(); reason != ReasonIdPUnreachable {
		t.Fatalf("reason = %q, want %q", reason, ReasonIdPUnreachable)
	}
}

func TestDiscoveryBackoff_CapsAtFiveMinutes(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	idp.FailDiscovery(1000, http.StatusBadGateway)
	p, clock, _ := newDiscoveryProvider(t, idp, 11)
	p.StartDiscovery(context.Background())
	s := time.Second
	assertDelays(t, clock.seen(), []time.Duration{0, 2 * s, 4 * s, 8 * s, 16 * s,
		32 * s, 64 * s, 128 * s, 256 * s, 300 * s, 300 * s})
	if ready, _ := p.Ready(); ready {
		t.Fatal("Ready() true while every discovery failed")
	}
}

func TestDiscoveryBackoff_DefaultJitterStaysUnderCap(t *testing.T) {
	for _, d := range []time.Duration{2 * time.Second, 256 * time.Second, 5 * time.Minute} {
		for i := 0; i < 200; i++ {
			got := discoveryJitter(d)
			if got > d || got < d*4/5 {
				t.Fatalf("discoveryJitter(%v) = %v, want within [80%%, 100%%]", d, got)
			}
		}
	}
	if got := discoveryJitter(0); got != 0 {
		t.Fatalf("discoveryJitter(0) = %v, want 0", got)
	}
}

func TestDiscoveryRefresh_FailureKeepsLastGoodVerifier(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	p, clock, _ := newDiscoveryProvider(t, idp, 2)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	idp.FailDiscovery(1, http.StatusServiceUnavailable)
	if err := p.Discover(context.Background()); err == nil {
		t.Fatal("refresh against a 503 succeeded")
	}
	if ready, reason := p.Ready(); !ready || reason != "" {
		t.Fatalf("after failed refresh: Ready() = %v, %q; want true", ready, reason)
	}
	if id, err := login(t, idp, p); err != nil || id.Subject == "" {
		t.Fatalf("login with the kept verifier: id = %+v, err = %v", id, err)
	}
	// A failed refresh retries on the short backoff, then goes back to daily.
	p.StartDiscovery(context.Background())
	assertDelays(t, clock.seen(), []time.Duration{2 * time.Second, 24 * time.Hour})
}

func TestDiscoveryRefresh_ConcurrentAuthorizeSeesOneState(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	p, _, _ := newDiscoveryProvider(t, idp, 1)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	endpoints := []string{idp.Issuer() + "/authorize", idp.Issuer() + "/authorize-v2"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			idp.SetDiscovery(map[string]any{"authorization_endpoint": endpoints[i%2]})
			_ = p.Discover(context.Background())
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan string, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, _, err := p.AuthorizationURL()
			ok := err == nil && (strings.HasPrefix(u, endpoints[0]+"?") ||
				strings.HasPrefix(u, endpoints[1]+"?")) &&
				strings.Contains(u, "nonce=") && strings.Contains(u, "code_challenge=")
			if !ok {
				errs <- u
			}
		}()
	}
	wg.Wait()
	<-done
	close(errs)
	for u := range errs {
		t.Errorf("authorize during refresh swap built %q", u)
	}
	if ready, _ := p.Ready(); !ready {
		t.Fatal("provider lost readiness during refresh swaps")
	}
}

func TestDiscoveryGitHub_StaticReadyAndNoLoop(t *testing.T) {
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "github", ClientID: "c"})
	clock := &fakeDiscoveryClock{stopAfter: 5}
	p.wait = clock.wait
	if ready, reason := p.Ready(); ready || reason != ReasonPending {
		t.Fatalf("before Discover: Ready() = %v, %q; want false, %q", ready, reason,
			ReasonPending)
	}
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("Discover(github): %v", err)
	}
	if ready, _ := p.Ready(); !ready {
		t.Fatal("github provider not ready after static discovery")
	}
	p.StartDiscovery(context.Background())
	if got := clock.seen(); len(got) != 0 {
		t.Fatalf("github discovery loop waited %v; static endpoints need no refresh", got)
	}
}

func TestDiscoveryLoop_StopsWhenContextEnds(t *testing.T) {
	idp := fakeidp.New(t, "cid")
	p := NewOAuthProvider(&config.OAuthConfig{Provider: "oidc", IssuerURL: idp.Issuer(),
		ClientID: "cid"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished := make(chan struct{})
	go func() {
		p.StartDiscovery(ctx)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("StartDiscovery kept running after its context ended")
	}
	if !waitDiscovery(ctx, time.Millisecond) && waitDiscovery(context.Background(), 0) {
		return
	}
	t.Fatal("waitDiscovery: want false for an ended context, true for a zero wait")
}

// An issuer_url that is not an absolute http(s) URL is a configuration
// typo, not an outage: it must say misconfigured and wait a day, not retry
// every few minutes as "identity provider unreachable".
func TestDiscoveryMisconfigured_MalformedIssuerURL(t *testing.T) {
	for _, issuer := range []string{"idp.corp.example", "ftp://idp.corp.example",
		"https://", "http://%zz", "/relative/issuer"} {
		t.Run(issuer, func(t *testing.T) {
			p := NewOAuthProvider(&config.OAuthConfig{Provider: "oidc",
				IssuerURL: issuer, ClientID: "cid"})
			err := p.Discover(context.Background())
			if !errors.Is(err, ErrDiscoveryMisconfigured) ||
				!strings.Contains(err.Error(), "issuer_url") {
				t.Fatalf("Discover(%q) = %v, want ErrDiscoveryMisconfigured naming "+
					"issuer_url", issuer, err)
			}
			if ready, reason := p.Ready(); ready || reason != ReasonMisconfigured {
				t.Fatalf("Ready() = %v, %q; want false, %q", ready, reason,
					ReasonMisconfigured)
			}
		})
	}
}
