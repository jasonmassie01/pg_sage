package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet/discovery"
	"github.com/pg-sage/sidecar/internal/review/trigger"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleetpol-01d: an observe-only member's first grade comes from the
// sweeper, not from a review queue it does not have.
// No concurrent access tests: the bridge holds no state of its own.

func observedGrades(sw *store.ObserveSweep, observe bool, err error) metaDiscoveryGrades {
	return metaDiscoveryGrades{hub: trigger.NewHub(),
		pool: func(string) *pgxpool.Pool { return nil },
		observed: func(context.Context, string) (*store.ObserveSweep, bool, error) {
			return sw, observe, err
		}}
}

func TestObserveOnlyGrades_FromTheNewestSweep(t *testing.T) {
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	sw := &store.ObserveSweep{Status: store.ObserveOK, SweptAt: at,
		Grade: []byte(`{"overall":"","dimensions":{"lifecycle":"D","schema":"B"}}`)}
	g := observedGrades(sw, true, nil)
	row := store.DiscoveredRow{ID: 3, Source: "cluster"}
	if err := g.RequestFirstGrade(context.Background(), "obs", row); err != nil {
		t.Fatalf("first grade of an observe-only member: %v, want nil (the sweeper "+
			"grades it)", err)
	}
	got, err := g.Grades(context.Background(), "obs")
	if err != nil || len(got) != 1 || !got[0].At.Equal(at) ||
		got[0].Letters["lifecycle"] != "D" || got[0].Letters["schema"] != "B" {
		t.Fatalf("grades %+v (%v), want the sweep's letters", got, err)
	}
}

func TestObserveOnlyGrades_NotSweptOrUnreachableHasNone(t *testing.T) {
	for _, sw := range []*store.ObserveSweep{nil,
		{Status: store.ObserveUnreachable, Error: "dial: refused"},
		{Status: store.ObserveOK, Grade: []byte(`{}`)}} {
		got, err := observedGrades(sw, true, nil).Grades(context.Background(), "obs")
		if err != nil || len(got) != 0 {
			t.Errorf("sweep %+v: grades %+v (%v), want none", sw, got, err)
		}
	}
	bad := &store.ObserveSweep{Status: store.ObserveOK, Grade: []byte(`{"dimensions":7}`)}
	if _, err := observedGrades(bad, true, nil).Grades(context.Background(), "obs"); err == nil {
		t.Fatal("an unreadable stored grade was read as no grade")
	}
}

func TestObserveOnlyGrades_ReadErrorPropagates(t *testing.T) {
	g := observedGrades(nil, false, errors.New("meta db: connection refused"))
	row := store.DiscoveredRow{ID: 3}
	if err := g.RequestFirstGrade(context.Background(), "obs", row); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("request: %v, want the read error", err)
	}
	if _, err := g.Grades(context.Background(), "obs"); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("grades: %v, want the read error", err)
	}
}

func TestObserveOnlyGrades_FullMemberKeepsTheReviewQueue(t *testing.T) {
	g := observedGrades(nil, false, nil)
	err := g.RequestFirstGrade(context.Background(), "full-1", store.DiscoveredRow{ID: 3})
	if !errors.Is(err, discovery.ErrMemberNotReady) {
		t.Fatalf("a full member without a queue: %v, want ErrMemberNotReady", err)
	}
	if _, err := g.Grades(context.Background(), "full-1"); !errors.Is(err,
		discovery.ErrMemberNotReady) {
		t.Fatalf("a full member without a pool: %v, want ErrMemberNotReady", err)
	}
}
