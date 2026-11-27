package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/notify"
)

// earned-06: an expiring earned level reaches a human once, keyed by the
// pair and the expiry, through the rules for failed actions.
func TestAutonomyNotifierTellsAHumanAboutAnExpiringLevel(t *testing.T) {
	d := &capturedDispatch{}
	until := time.Date(2026, 12, 3, 9, 30, 0, 0, time.UTC)
	w := earned.ExpiryWarning{Database: "orders", Family: earned.FamilyWraparound,
		Class: earned.ClassFreeze, Level: earned.L3, HoldsUntil: until,
		Checks: []string{"bench_fresh"}}
	if err := (autonomyNotifier{dispatcher: d}).NotifyEvidenceExpiring(context.Background(),
		w); err != nil || len(d.events) != 1 {
		t.Fatalf("notification = %+v (%v)", d.events, err)
	}
	e := d.events[0]
	if e.Type != "action_failed" || e.Severity != "warning" ||
		!strings.Contains(e.Subject, "L3 holds until 2026-12-03") ||
		!strings.Contains(e.Subject, "a new bench report") ||
		!strings.Contains(e.Body, "bench_fresh") || e.Data["database"] != "orders" ||
		e.Data["holds_until"] != "2026-12-03T09:30:00Z" ||
		e.DedupKey != "trust-expiring:orders:wraparound_runway:freeze:2026-12-03T09:30:00Z" {
		t.Fatalf("expiry event = %+v", e)
	}
	if err := (autonomyNotifier{}).NotifyEvidenceExpiring(context.Background(),
		earned.ExpiryWarning{}); err != nil {
		t.Fatalf("no dispatcher must log, not fail: %v", err)
	}
}

type failingDispatch struct{}

func (failingDispatch) Dispatch(context.Context, notify.Event) error {
	return errors.New("webhook refused")
}

func TestAutonomyNotifierReturnsAnExpiryDispatchFailure(t *testing.T) {
	err := (autonomyNotifier{dispatcher: failingDispatch{}}).NotifyEvidenceExpiring(
		context.Background(), earned.ExpiryWarning{Database: "orders", Level: earned.L2})
	if err == nil || !strings.Contains(err.Error(), "webhook refused") {
		t.Fatalf("err = %v, want the dispatch failure", err)
	}
}
