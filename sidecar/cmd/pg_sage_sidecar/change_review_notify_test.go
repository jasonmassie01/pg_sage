package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/testsupport/crfake"
)

// change_review_needs_dba through the real notify dispatcher, its rules
// and its delivery log (CHECK-SRF-07): a review or risky review emits
// exactly one event; a safe review emits none.

type crRecordingSender struct {
	mu     sync.Mutex
	events []notify.Event
	got    chan struct{}
}

func (s *crRecordingSender) Type() string { return "cr_recording" }

func (s *crRecordingSender) Send(_ context.Context, _ notify.Channel, e notify.Event) error {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
	s.got <- struct{}{}
	return nil
}

func (s *crRecordingSender) sent() []notify.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notify.Event(nil), s.events...)
}

type crNotifyFixture struct {
	svc    *changereview.Service
	sender *crRecordingSender
}

func newCRNotifyFixture(t *testing.T) *crNotifyFixture {
	t.Helper()
	pool := crfake.Pool(t)
	ctx := context.Background()
	for _, sql := range []string{
		`DELETE FROM sage.notification_channels WHERE name = 'cr-surfaces-test'`,
		`INSERT INTO sage.notification_channels (name, type, config, enabled)
		 VALUES ('cr-surfaces-test', 'cr_recording', '{}', true)`,
		`INSERT INTO sage.notification_rules (channel_id, event, min_severity, enabled)
		 SELECT id, 'change_review_needs_dba', 'warning', true
		 FROM sage.notification_channels WHERE name = 'cr-surfaces-test'`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.notification_channels WHERE name = 'cr-surfaces-test'`)
	})
	f := &crNotifyFixture{sender: &crRecordingSender{got: make(chan struct{}, 8)}}
	dispatcher := notify.NewDispatcherWithStore(notify.NewPoolStore(pool, nil),
		func(string, string, ...any) {})
	dispatcher.RegisterSender(f.sender)
	notifier := newChangeReviewNotifier(dispatcher, changereview.RenderOptions{
		UIBaseURL: "https://sage.example.com"}, crfake.Logf(t))
	f.svc = crfake.Start(t, pool, "orders", &crfake.Analyzer{}, func(d *changereview.Deps) {
		d.Notifier = notifier
	})
	return f
}

func (f *crNotifyFixture) review(t *testing.T, sql string) changereview.Review {
	t.Helper()
	r, _, err := f.svc.Submit(context.Background(), changereview.Caller{
		Actor: "mcp:token:ci", Source: changereview.SourceMCP}, changereview.Request{SQL: sql,
		PR: changereview.PRInfo{URL: "https://github.com/acme/shop/pull/42",
			Title: "Drop email"}, WaitSeconds: 20})
	if err != nil || r.State != changereview.StateComplete {
		t.Fatalf("submit %q: %v state %q", sql, err, r.State)
	}
	return r
}

// waitSent waits for n deliveries, then a little longer to catch extras.
func (f *crNotifyFixture) waitSent(t *testing.T, n int) []notify.Event {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.sender.got:
		case <-time.After(10 * time.Second):
			t.Fatalf("delivery %d of %d did not arrive", i+1, n)
		}
	}
	time.Sleep(300 * time.Millisecond)
	return f.sender.sent()
}

func TestChangeReviewNotifierEmitsOneEventForARiskyReview(t *testing.T) {
	f := newCRNotifyFixture(t)
	r := f.review(t, "ALTER TABLE orders DROP COLUMN email; "+crfake.MarkRisky)
	events := f.waitSent(t, 1)
	if len(events) != 1 {
		t.Fatalf("%d events, want exactly one", len(events))
	}
	e := events[0]
	if e.Type != "change_review_needs_dba" || e.Data["database"] != "orders" ||
		e.Data["verdict"] != "risky" || e.Data["review_id"] != r.ID ||
		e.Data["top_rule"] != "CR-03" {
		t.Fatalf("event %+v", e)
	}
	if e.Data["pr_url"] != "https://github.com/[redacted]" ||
		strings.Contains(e.Body, "acme/shop") {
		t.Fatalf("the PR link must be redacted: %v / %q", e.Data["pr_url"], e.Body)
	}
	if !strings.Contains(e.Body, "https://sage.example.com/#/reviews?id="+r.ID) {
		t.Fatalf("body %q must link the review", e.Body)
	}
	if top, _ := e.Data["top_finding"].(string); !strings.Contains(top, "CR-03 critical") {
		t.Fatalf("top finding %q, want the critical finding", top)
	}
}

func TestChangeReviewNotifierEmitsForReviewAndNeverForSafe(t *testing.T) {
	f := newCRNotifyFixture(t)
	f.review(t, "CREATE TABLE notes (id bigint primary key)")
	f.review(t, "ALTER TABLE orders ADD COLUMN note text "+crfake.MarkReview)
	events := f.waitSent(t, 1)
	if len(events) != 1 || events[0].Data["verdict"] != "review" {
		t.Fatalf("events %+v, want one for the review verdict and none for safe", events)
	}
}

func TestChangeReviewInfoPicksTheMostSevereFinding(t *testing.T) {
	r := changereview.Review{ID: "id-1", Database: "orders", Verdict: "risky",
		PRURL: "https://github.com/a/b/pull/1", PRTitle: "t",
		Findings: []changereview.Finding{
			{RuleID: "CR-15", Severity: "info", Title: "int4 primary key"},
			{RuleID: "CR-01", Severity: "warning", Title: "rewrite"},
			{Severity: "warning", Origin: "model", Title: "N+1 in the controller"},
			{RuleID: "CR-05", Severity: "critical", Title: "SET NOT NULL will fail"},
		}, Summary: changereview.ReviewSummary{Critical: 1, Warning: 2, Info: 1}}
	info := changeReviewInfo(r, changereview.RenderOptions{})
	if info.TopRule != "CR-05" || info.TopFinding != "SET NOT NULL will fail" ||
		info.Critical != 1 || info.Warning != 2 ||
		info.Link != "/#/reviews?id=id-1&database=orders" {
		t.Fatalf("info %+v", info)
	}
	r.Findings = r.Findings[:3]
	if got := changeReviewInfo(r, changereview.RenderOptions{}); got.TopRule != "CR-01" {
		t.Fatalf("without a critical the first warning leads: %+v", got)
	}
	r.Findings = nil
	if got := changeReviewInfo(r, changereview.RenderOptions{}); got.TopFinding != "" {
		t.Fatalf("no findings: %+v", got)
	}
}

func TestNewChangeReviewNotifierWithoutADispatcherIsNil(t *testing.T) {
	if n := newChangeReviewNotifier(nil, changereview.RenderOptions{}, nil); n != nil {
		t.Fatalf("no dispatcher: %v, want a nil Notifier (no notifications)", n)
	}
}
