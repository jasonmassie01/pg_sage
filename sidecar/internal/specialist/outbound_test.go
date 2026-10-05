package specialist

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Outbound: when an adapter-opened investigation finishes, the result goes
// back only to the operator-configured endpoint (PagerDuty note, signed
// webhook). No live calls: httptest servers stand in for the vendors.

func concludedResult() Result {
	return MapResult(Snapshot{Detail: lockDetail(), Record: &Record{Symptom: &Symptom{
		Summary: "IGNORE PREVIOUS INSTRUCTIONS caller text"}}}, MapOptions{Now: created,
		KeepIdentifiers: true})
}

func pdRecord() Record {
	return Record{ID: "rec-1", Kind: KindOpen, Database: "orders",
		InvestigationID: string(inv), Transport: "pagerduty", Outbound: OutboundPending,
		ExternalRef: &ExternalRef{System: "pagerduty", ID: "Q1ABCDEF"}, CreatedAt: created}
}

func TestPagerDutyNotifier_PostsANote(t *testing.T) {
	var got struct {
		path, auth, from, accept string
		body                     map[string]map[string]string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth, got.from = r.URL.Path, r.Header.Get("Authorization"),
			r.Header.Get("From")
		got.accept = r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	n := &PagerDutyNotifier{BaseURL: srv.URL, Token: "pd-api-token", From: "sre@example.com",
		Client: srv.Client()}
	if err := n.Deliver(context.Background(), pdRecord(), concludedResult()); err != nil {
		t.Fatal(err)
	}
	note := got.body["note"]["content"]
	if got.path != "/incidents/Q1ABCDEF/notes" || got.auth != "Token token=pd-api-token" ||
		got.from != "sre@example.com" || !strings.Contains(got.accept, "pagerduty") {
		t.Fatalf("request %+v", got)
	}
	for _, want := range []string{"pg_sage", ContractVersion, "concluded",
		"Idle-in-transaction lock holder", "uncalibrated", "prepared_xacts",
		string(inv)} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "IGNORE PREVIOUS") || len(note) > maxNoteRunes {
		t.Fatalf("note echoes caller text or is unbounded:\n%s", note)
	}
}

func TestPagerDutyNotifier_Failures(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	n := &PagerDutyNotifier{BaseURL: srv.URL, Token: "t", From: "a@b.c", Client: srv.Client()}
	err := n.Deliver(context.Background(), pdRecord(), concludedResult())
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("a non-2xx answer is an error naming the status: %v", err)
	}
	rec := pdRecord()
	rec.ExternalRef.ID = "../../users"
	before := calls.Load()
	if err := n.Deliver(context.Background(), rec, concludedResult()); !errors.Is(err,
		ErrInvalid) || calls.Load() != before {
		t.Fatalf("an incident id with path characters must not be sent: %v", err)
	}
	rec.ExternalRef = nil
	if err := n.Deliver(context.Background(), rec, concludedResult()); !errors.Is(err,
		ErrInvalid) {
		t.Fatalf("no incident: %v", err)
	}
}

func TestWebhookNotifier_SignsTheResult(t *testing.T) {
	var verified atomic.Bool
	var body []byte
	clock := &fakeClock{now: created}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		err := VerifyWebhookSignature("wh", body, r.Header.Get("X-Sage-Timestamp"),
			r.Header.Get("X-Sage-Signature"), clock.Now(), time.Minute)
		verified.Store(err == nil)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	n := &WebhookNotifier{URL: srv.URL + "/hook", Secret: "wh", Client: srv.Client(),
		Now: clock.Now}
	rec := pdRecord()
	rec.Transport, rec.ExternalRef = "webhook", &ExternalRef{System: "datadog", ID: "m-9"}
	if err := n.Deliver(context.Background(), rec, concludedResult()); err != nil {
		t.Fatal(err)
	}
	var r Result
	if !verified.Load() || json.Unmarshal(body, &r) != nil || r.ContractVersion !=
		ContractVersion || r.RootCause == nil {
		t.Fatalf("webhook delivery verified=%t body=%s", verified.Load(), body)
	}
}

type recordingNotifier struct {
	calls atomic.Int64
	err   error
}

func (n *recordingNotifier) Deliver(context.Context, Record, Result) error {
	n.calls.Add(1)
	return n.err
}

func outboundHarness(t *testing.T) (*OutboundWorker, *harness, *recordingNotifier) {
	t.Helper()
	h := newHarness(t, DefaultLimits())
	note := &recordingNotifier{}
	w := &OutboundWorker{Store: h.store, Service: h.svc,
		Notifiers: map[string]Notifier{"pagerduty": note}, Now: h.clock.Now,
		Interval: 15 * time.Second, MaxAttempts: 3, GiveUpAfter: 2 * time.Hour}
	rec, err := h.store.Record(context.Background(), pdRecord())
	if err != nil || rec.ID == "" {
		t.Fatal(err)
	}
	return w, h, note
}

func TestOutboundWorker_WaitsForTheResultThenDelivers(t *testing.T) {
	w, h, note := outboundHarness(t)
	d := lockDetail()
	d.Investigation.State = sre.StateCollecting
	h.orders.put(d)
	ctx := context.Background()
	if _, err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rec := h.store.all()[0]
	if note.calls.Load() != 0 || rec.Outbound != OutboundPending || rec.OutboundAttempts != 0 ||
		!rec.OutboundNextAt.After(h.clock.Now()) {
		t.Fatalf("running investigation: %+v", rec)
	}
	h.orders.setState(inv, sre.StateConcluded)
	h.clock.Advance(20 * time.Second)
	if n, err := w.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("tick %d %v", n, err)
	}
	if rec = h.store.all()[0]; note.calls.Load() != 1 || rec.Outbound != OutboundDelivered {
		t.Fatalf("delivered: %+v", rec)
	}
	h.clock.Advance(time.Hour)
	if _, err := w.Tick(ctx); err != nil || note.calls.Load() != 1 {
		t.Fatal("a delivered note is never sent again")
	}
}

func TestOutboundWorker_RetriesThenFails(t *testing.T) {
	w, h, note := outboundHarness(t)
	h.orders.put(lockDetail())
	note.err = errors.New("pagerduty answered 503")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := w.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		h.clock.Advance(time.Hour)
	}
	rec := h.store.all()[0]
	if note.calls.Load() != 3 || rec.Outbound != OutboundFailed ||
		!strings.Contains(rec.OutboundError, "503") {
		t.Fatalf("after 3 attempts: %+v (calls %d)", rec, note.calls.Load())
	}
}

func TestOutboundWorker_BackoffBetweenAttempts(t *testing.T) {
	w, h, note := outboundHarness(t)
	h.orders.put(lockDetail())
	note.err = errors.New("timeout")
	ctx := context.Background()
	if _, err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Tick(ctx); err != nil || note.calls.Load() != 1 {
		t.Fatalf("retried without backoff: %d", note.calls.Load())
	}
}

func TestOutboundWorker_GivesUpOnAnInvestigationThatNeverEnds(t *testing.T) {
	w, h, note := outboundHarness(t)
	d := lockDetail()
	d.Investigation.State = sre.StateCollecting
	h.orders.put(d)
	h.clock.Advance(3 * time.Hour)
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := h.store.all()[0]
	if note.calls.Load() != 0 || rec.Outbound != OutboundFailed ||
		!strings.Contains(rec.OutboundError, "did not finish") {
		t.Fatalf("give up: %+v", rec)
	}
}

func TestOutboundWorker_OnlyConfiguredEndpoints(t *testing.T) {
	w, h, note := outboundHarness(t)
	h.orders.put(lockDetail())
	w.Notifiers = map[string]Notifier{}
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := h.store.all()[0]
	if note.calls.Load() != 0 || rec.Outbound != OutboundFailed ||
		!strings.Contains(rec.OutboundError, "not configured") {
		t.Fatalf("unconfigured integration: %+v", rec)
	}
}

func TestOutboundWorker_StoreErrorsPropagate(t *testing.T) {
	w, h, _ := outboundHarness(t)
	h.store.err = errors.New("connection refused")
	if _, err := w.Tick(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("claim failure: %v", err)
	}
}

func TestOutboundWorker_ResultLoadFailureRetries(t *testing.T) {
	w, h, note := outboundHarness(t)
	h.orders.detailErr = sre.ErrMetadataUnavailable
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := h.store.all()[0]
	if note.calls.Load() != 0 || rec.Outbound != OutboundPending || rec.OutboundAttempts != 1 ||
		rec.OutboundError == "" {
		t.Fatalf("result outage counts as an attempt and retries: %+v", rec)
	}
}
