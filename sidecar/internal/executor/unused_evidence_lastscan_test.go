package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// G-P0-12 composed with the live pre-drop check (#78): an index that was
// scanned, but last longer ago than the window (PG16+ last_idx_scan), is
// unused evidence that still holds at drop time; a scan inside the window,
// or any scan without last_idx_scan, breaks it.
//
// No concurrent access tests: broken() is a pure method on a value.
func TestUnusedEvidence_BrokenWithLastScan(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	window := 7 * 24 * time.Hour
	old := now.Add(-window)
	recent := now.Add(-window + time.Second)
	epoch := now.Add(-60 * 24 * time.Hour)
	cases := []struct {
		name string
		ev   unusedEvidence
		want string // "" = evidence holds
	}{
		{"last scan exactly one window ago",
			unusedEvidence{matches: 1, scans: 5, lastScan: &old, epoch: epoch}, ""},
		{"last scan inside the window",
			unusedEvidence{matches: 1, scans: 5, lastScan: &recent, epoch: epoch}, "scanned"},
		{"scanned, no last_idx_scan (pre-PG16)",
			unusedEvidence{matches: 1, scans: 5, epoch: epoch}, "scanned"},
		{"zero scans keep the epoch rule",
			unusedEvidence{matches: 1, epoch: now.Add(-time.Hour)}, "reset"},
		{"old last scan but the index is gone",
			unusedEvidence{scans: 5, lastScan: &old, epoch: epoch}, "not found"},
	}
	for _, tc := range cases {
		got := tc.ev.broken(now, window)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: broken = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The live read returns last_idx_scan on PG16+ and nil on older servers.
func TestReadUnusedEvidence_LastScan(t *testing.T) {
	pool := evidencePool(t)
	ctx := context.Background()
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	ev, err := readUnusedEvidence(ctx, pool, "public.ev_a")
	if err != nil || ev.matches != 1 {
		t.Fatalf("evidence = %+v (%v)", ev, err)
	}
	if ev.scans == 0 && ev.lastScan != nil {
		t.Fatalf("unscanned index has last_idx_scan %v", ev.lastScan)
	}
	if version < 160000 && ev.lastScan != nil {
		t.Fatalf("PG%d reported last_idx_scan %v, want nil", version, ev.lastScan)
	}
}
