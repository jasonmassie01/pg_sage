package alerting

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-11: alerting channels connect through the notification target
// policy, like the notify senders (G7-B21).

func targetAlert() Alert {
	return Alert{
		Findings:  []AlertFinding{{ID: 1, Title: "Target", Severity: "critical"}},
		Severity:  "critical",
		Timestamp: time.Now(),
	}
}

// countingServer counts accepted connections and handled requests.
type countingServer struct {
	srv   *httptest.Server
	conns atomic.Int32
	hits  atomic.Int32
}

func newCountingServer(t *testing.T, useTLS bool, h http.HandlerFunc) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.srv = httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			cs.hits.Add(1)
			h(w, r)
		}))
	cs.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			cs.conns.Add(1)
		}
	}
	if useTLS {
		cs.srv.StartTLS()
	} else {
		cs.srv.Start()
	}
	t.Cleanup(cs.srv.Close)
	return cs
}

func okHandler(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

// trustServer makes a policy client trust the test server's certificate
// without touching the policy's dialer or redirect check.
func trustServer(t *testing.T, client *http.Client, srv *httptest.Server) {
	t.Helper()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("policy client transport is %T, want *http.Transport", client.Transport)
	}
	srvTr, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("test server transport is %T", srv.Client().Transport)
	}
	tr.TLSClientConfig = srvTr.TLSClientConfig.Clone()
}

func TestTarget_StrictPolicyRefusesLoopbackBeforeAnyRequest(t *testing.T) {
	cs := newCountingServer(t, false, okHandler)
	client := notify.TargetPolicy{}.HTTPClient(HTTPTimeout)
	ch := NewWebhook("hook", cs.srv.URL+"/secret-token", nil, client, noopLog)
	err := ch.Send(context.Background(), targetAlert())
	if !errors.Is(err, notify.ErrTargetBlocked) {
		t.Fatalf("err = %v, want ErrTargetBlocked", err)
	}
	if got := cs.conns.Load(); got != 0 {
		t.Fatalf("listener saw %d connections, want none", got)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error %q echoes the webhook URL", err)
	}
}

func TestTarget_AllowPrivateTLSDelivers(t *testing.T) {
	var body atomic.Value
	cs := newCountingServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		w.WriteHeader(http.StatusOK)
	})
	client := notify.TargetPolicy{AllowPrivate: true}.HTTPClient(HTTPTimeout)
	trustServer(t, client, cs.srv)
	ch := NewWebhook("hook", cs.srv.URL, map[string]string{"X-Key": "v"}, client, noopLog)
	if err := ch.Send(context.Background(), targetAlert()); err != nil {
		t.Fatalf("send through allow-private policy: %v", err)
	}
	if cs.hits.Load() != 1 {
		t.Fatalf("server handled %d requests, want 1", cs.hits.Load())
	}
	got, _ := body.Load().(string)
	if !strings.Contains(got, `"Target"`) || !strings.Contains(got, `"critical"`) {
		t.Fatalf("payload = %q, want the alert JSON", got)
	}
}

func TestTarget_SlackUsesSuppliedClient(t *testing.T) {
	cs := newCountingServer(t, true, okHandler)
	client := notify.TargetPolicy{AllowPrivate: true}.HTTPClient(HTTPTimeout)
	trustServer(t, client, cs.srv)
	if err := NewSlack(cs.srv.URL, client, noopLog).Send(
		context.Background(), targetAlert()); err != nil {
		t.Fatalf("slack send: %v", err)
	}
	if cs.hits.Load() != 1 {
		t.Fatalf("slack server handled %d requests, want 1", cs.hits.Load())
	}
}

// A redirect from an allowed host to loopback is refused by the policy's
// redirect check; the loopback server never sees a request.
func TestTarget_RedirectToLoopbackRefused(t *testing.T) {
	inner := newCountingServer(t, true, okHandler)
	outer := newCountingServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.srv.URL+"/internal", http.StatusFound)
	})
	client := notify.TargetPolicy{}.HTTPClient(HTTPTimeout)
	trustServer(t, client, outer.srv)
	tr := client.Transport.(*http.Transport)
	policyDial := tr.DialContext
	outerAddr := outer.srv.Listener.Addr().String()
	// "example.com" stands for a public host: it alone is routed to the
	// outer server, every other address goes through the policy dialer.
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "example.com:443" {
			return (&net.Dialer{}).DialContext(ctx, network, outerAddr)
		}
		return policyDial(ctx, network, addr)
	}
	ch := NewWebhook("hook", "https://example.com/hook", nil, client, noopLog)
	err := ch.Send(context.Background(), targetAlert())
	if !errors.Is(err, notify.ErrTargetBlocked) {
		t.Fatalf("err = %v, want the redirect refused by policy", err)
	}
	if outer.hits.Load() != 1 {
		t.Fatalf("allowed host handled %d requests, want 1", outer.hits.Load())
	}
	if inner.conns.Load() != 0 || inner.hits.Load() != 0 {
		t.Fatalf("loopback target saw %d conns / %d requests, want none",
			inner.conns.Load(), inner.hits.Load())
	}
}

// Link-local stays blocked even when private targets are allowed.
func TestTarget_RedirectToMetadataRefusedEvenWhenPrivateAllowed(t *testing.T) {
	outer := newCountingServer(t, true, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	})
	client := notify.TargetPolicy{AllowPrivate: true}.HTTPClient(HTTPTimeout)
	trustServer(t, client, outer.srv)
	err := NewWebhook("hook", outer.srv.URL, nil, client, noopLog).Send(
		context.Background(), targetAlert())
	if !errors.Is(err, notify.ErrTargetBlocked) {
		t.Fatalf("err = %v, want the metadata redirect refused", err)
	}
}

// A nil client fails closed: the strict policy applies, so a loopback
// target is refused rather than reached by a default client.
func TestTarget_NilClientFailsClosed(t *testing.T) {
	cs := newCountingServer(t, false, okHandler)
	ch := NewWebhook("hook", cs.srv.URL, nil, nil, noopLog)
	err := ch.Send(context.Background(), targetAlert())
	if !errors.Is(err, notify.ErrTargetBlocked) {
		t.Fatalf("nil client err = %v, want ErrTargetBlocked", err)
	}
	if cs.conns.Load() != 0 {
		t.Fatalf("listener saw %d connections, want none", cs.conns.Load())
	}
	if NewSlack("https://hooks.slack.com/x", nil, noopLog).client == nil ||
		NewPagerDuty("rk", nil, noopLog).client == nil {
		t.Fatal("nil client left a channel without a policy client")
	}
}

// PagerDuty's fixed endpoint goes through the supplied client too.
func TestTarget_PagerDutyUsesSuppliedClient(t *testing.T) {
	var sawHost atomic.Value
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sawHost.Store(r.URL.Host)
		return nil, errors.New("stub transport")
	})}
	ch := NewPagerDuty("rk", client, noopLog)
	if err := ch.doPost(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("stub transport error was swallowed")
	}
	if got, _ := sawHost.Load().(string); got != "events.pagerduty.com" {
		t.Fatalf("pagerduty request host = %q, want events.pagerduty.com via the client", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Fleet mode shares one policy client across channels and databases.
func TestTarget_SharedClientConcurrentSends(t *testing.T) {
	cs := newCountingServer(t, true, okHandler)
	client := notify.TargetPolicy{AllowPrivate: true}.HTTPClient(HTTPTimeout)
	trustServer(t, client, cs.srv)
	ch := NewWebhook("hook", cs.srv.URL, nil, client, noopLog)
	const senders = 8
	var wg sync.WaitGroup
	errs := make(chan error, senders)
	for range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ch.Send(context.Background(), targetAlert())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent send: %v", err)
		}
	}
	if cs.hits.Load() != senders {
		t.Fatalf("server handled %d requests, want %d", cs.hits.Load(), senders)
	}
}

// legacyTestClient is the client every channel built before alerting-11:
// the existing tests aim channels at loopback httptest servers over plain
// http, which the policy client refuses by design.
var legacyTestClient = &http.Client{Timeout: 10 * time.Second}
