package earned

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The error-budget downgrade reads Sage SRE M5's BudgetSummary. Database
// proxy SLOs are on by default and are often unknown for structural
// reasons (no standbys, no log access, a baseline still building), so
// only registered app SLOs being unknown downgrades; any page-level burn,
// proxy included, downgrades; no app SLO and no burn is "no budget".

type fakeSummary struct {
	mu    sync.Mutex
	sum   BudgetSummary
	err   error
	calls int
}

func (f *fakeSummary) BudgetSummary(context.Context) (BudgetSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.sum, f.err
}

func TestBudgetStateFromSummary(t *testing.T) {
	cases := map[string]struct {
		sum                          BudgetSummary
		configured, burning, unknown bool
	}{
		"no SLOs at all": {BudgetSummary{}, false, false, false},
		"proxy unknown only": {BudgetSummary{Unknown: []string{"db_replication_lag"},
			UnknownApp: []string{}}, false, false, false},
		"proxy unknown beside a healthy app SLO": {BudgetSummary{AppSLOs: 1,
			Unknown: []string{"db_replication_lag"}, UnknownApp: []string{}},
			true, false, false},
		"app SLO unknown": {BudgetSummary{AppSLOs: 2, Unknown: []string{"checkout"},
			UnknownApp: []string{"checkout"}}, true, false, true},
		"proxy fast burn without app SLOs": {BudgetSummary{FastBurning: true},
			true, true, false},
		"app fast burn": {BudgetSummary{AppSLOs: 1, FastBurning: true,
			AppFastBurning: true}, true, true, false},
		"healthy app SLOs": {BudgetSummary{AppSLOs: 3}, true, false, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := BudgetStateOf(c.sum)
			if got.Configured != c.configured || got.FastBurning != c.burning ||
				got.Unknown != c.unknown {
				t.Fatalf("state = %+v, want configured=%v burning=%v unknown=%v", got,
					c.configured, c.burning, c.unknown)
			}
		})
	}
}

func TestBudgetStateDetailNamesTheCause(t *testing.T) {
	got := BudgetStateOf(BudgetSummary{AppSLOs: 2, UnknownApp: []string{"checkout", "search"}})
	if !strings.Contains(got.Detail, "checkout") || !strings.Contains(got.Detail, "search") {
		t.Fatalf("unknown detail = %q", got.Detail)
	}
	proxy := BudgetStateOf(BudgetSummary{FastBurning: true})
	if !strings.Contains(proxy.Detail, "proxy") {
		t.Fatalf("proxy burn detail = %q", proxy.Detail)
	}
	app := BudgetStateOf(BudgetSummary{AppSLOs: 1, FastBurning: true, AppFastBurning: true})
	if !strings.Contains(app.Detail, "app") || strings.Contains(app.Detail, "proxy") {
		t.Fatalf("app burn detail = %q", app.Detail)
	}
}

func TestSummaryBudgetReadsTheBoundDatabase(t *testing.T) {
	src := &fakeSummary{sum: BudgetSummary{Database: "orders", AppSLOs: 1,
		UnknownApp: []string{"checkout"}}}
	got, err := NewSummaryBudget(src).ErrorBudget(context.Background(), "orders")
	if err != nil || !got.Configured || !got.Unknown || got.FastBurning || src.calls != 1 {
		t.Fatalf("budget = %+v (%v), calls %d", got, err, src.calls)
	}
}

func TestSummaryBudgetRefusesAnotherDatabasesSummary(t *testing.T) {
	src := &fakeSummary{sum: BudgetSummary{Database: "billing", AppSLOs: 1}}
	_, err := NewSummaryBudget(src).ErrorBudget(context.Background(), "orders")
	if err == nil || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("a summary of another database was accepted: %v", err)
	}
}

func TestSummaryBudgetPropagatesErrors(t *testing.T) {
	boom := errors.New("slo state unreadable: connection refused")
	_, err := NewSummaryBudget(&fakeSummary{err: boom}).ErrorBudget(context.Background(),
		"orders")
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it wrapped", err)
	}
	if _, err := NewSummaryBudget(nil).ErrorBudget(context.Background(), "orders"); err == nil {
		t.Fatal("a nil summary source reported a budget")
	}
}

// The four cases the integration depends on, end to end through the
// limiter at L3.
func TestSummaryBudgetDowngradesThroughTheLimiter(t *testing.T) {
	cases := map[string]struct {
		sum    BudgetSummary
		reason string // "" means no downgrade
	}{
		"proxy unknown: no downgrade": {BudgetSummary{Database: "orders",
			Unknown: []string{"db_replication_lag", "db_error_rate"}, UnknownApp: []string{}}, ""},
		"app unknown: downgrade": {BudgetSummary{Database: "orders", AppSLOs: 1,
			Unknown: []string{"checkout"}, UnknownApp: []string{"checkout"}},
			DowngradeBudgetUnknown},
		"proxy fast burn: downgrade": {BudgetSummary{Database: "orders",
			FastBurning: true}, DowngradeBudgetBurn},
		"no SLOs configured: no downgrade": {BudgetSummary{Database: "orders"}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			lf := newLimiterFixture(t)
			lf.seedL3()
			lim := lf.svc.Limiter(Binding{Database: "orders",
				Budget: NewSummaryBudget(&fakeSummary{sum: c.sum}), HA: lf.ha,
				Concurrency: lf.conc})
			got, err := lim.Limit(lf.ctx, freezeRequest(lf.clock.Now()))
			if err != nil {
				t.Fatal(err)
			}
			if c.reason == "" {
				if got.Level != 3 || got.Downgraded || len(got.Reasons) != 0 {
					t.Fatalf("limit = %+v, want L3 without downgrade", got)
				}
				return
			}
			if got.Level != 1 || !got.Downgraded || !slices.Contains(got.Reasons, c.reason) {
				t.Fatalf("limit = %+v, want L1 for %s", got, c.reason)
			}
		})
	}
}
