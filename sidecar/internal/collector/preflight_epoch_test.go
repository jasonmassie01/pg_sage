package collector

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

// No concurrent access tests: epoch reads and reset marking run on the
// single collector goroutine.

func TestPreflightContractEpochChanged(t *testing.T) {
	a := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	b := a.Add(time.Second)
	cases := []struct {
		name      string
		prev, cur time.Time
		want      bool
	}{
		{"same epoch", a, a, false},
		{"same instant other zone", a, a.In(time.FixedZone("CDT", -5*3600)), false},
		{"new epoch", a, b, true},
		{"unknown previous", time.Time{}, b, false},
		{"unknown current", a, time.Time{}, false},
		{"both unknown", time.Time{}, time.Time{}, false},
	}
	for _, tc := range cases {
		if got := epochChanged(tc.prev, tc.cur); got != tc.want {
			t.Errorf("%s: epochChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPreflightContractMarkStatsResetOnEpochChange(t *testing.T) {
	a := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	queries := []QueryStats{{QueryID: 1, Calls: 10}, {QueryID: 2, Calls: 20}}
	regrown := []QueryStats{{QueryID: 1, Calls: 30}, {QueryID: 2, Calls: 40}}
	cases := []struct {
		name       string
		prevEpoch  time.Time
		curEpoch   time.Time
		curQueries []QueryStats
		want       bool
	}{
		{"regrowth in same epoch is not a reset", a, a, regrown, false},
		{"regrowth in new epoch is a reset", a, a.Add(time.Minute), regrown, true},
		{"unknown epoch falls back to counters", a, time.Time{}, regrown, false},
		{"counter collapse without epoch is a reset", time.Time{}, time.Time{},
			[]QueryStats{{QueryID: 1, Calls: 1}, {QueryID: 2, Calls: 1}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs []string
			c := &Collector{logFn: func(_, f string, _ ...any) { logs = append(logs, f) }}
			c.latest = &Snapshot{StatsEpoch: tc.prevEpoch, Queries: queries}
			snap := &Snapshot{StatsEpoch: tc.curEpoch, Queries: tc.curQueries}
			c.markStatsReset(snap)
			if snap.StatsReset != tc.want {
				t.Fatalf("StatsReset = %v, want %v", snap.StatsReset, tc.want)
			}
			if tc.want != (len(logs) == 1) {
				t.Fatalf("reset log lines = %v", logs)
			}
		})
	}
	c := &Collector{logFn: func(string, string, ...any) {}}
	first := &Snapshot{StatsEpoch: a}
	c.markStatsReset(first)
	if first.StatsReset {
		t.Fatal("first snapshot has nothing to compare with")
	}
}

func TestPreflightContractCollectStatementsEpoch(t *testing.T) {
	p, ctx, c := preflightPool(t)
	var start time.Time
	if err := p.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&start); err != nil {
		t.Fatal(err)
	}
	// Another package's pg_stat_statements_reset() on the shared server
	// moves the epoch between the two reads: repeat then.
	pgssepoch.Attempt(t, ctx, p, 3, func() []string {
		epoch := c.collectStatementsEpoch(ctx)
		if epoch.IsZero() || epoch.Before(start) || epoch.After(time.Now().Add(time.Minute)) {
			t.Fatalf("epoch = %v, want within [postmaster start %v, now]", epoch, start)
		}
		snap := preflightCollect(t, c)
		if !snap.StatsEpoch.Equal(epoch) {
			return []string{fmt.Sprintf("snapshot epoch = %v, want %v", snap.StatsEpoch, epoch)}
		}
		return nil
	})
}

func TestPreflightContractStatementsEpochUnknownOnError(t *testing.T) {
	_, _, c := preflightPool(t)
	var logs []string
	c.logFn = func(_, f string, args ...any) { logs = append(logs, f) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if epoch := c.collectStatementsEpoch(ctx); !epoch.IsZero() {
		t.Fatalf("epoch on failed read = %v, want unknown (zero)", epoch)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "epoch unknown") {
		t.Fatalf("failed epoch read must warn once, got %v", logs)
	}
}
