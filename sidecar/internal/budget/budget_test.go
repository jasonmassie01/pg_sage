package budget

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-08: the workload budget model without a database: proposals,
// warn-mode counting, the promotion gate and the refused roles.

var t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func roleBudget(name, role string, stmtMs int64) Budget {
	return Budget{Name: name, Match: Match{Role: role}, StatementTimeoutMs: stmtMs,
		Mode: ModeWarn}
}

func longActive(pid int, role, app string, ageSeconds float64,
	tags map[string]string) collector.WorkloadLongSession {
	if tags == nil {
		tags = map[string]string{}
	}
	return collector.WorkloadLongSession{PID: pid, Role: role, Application: app,
		State: "active", QueryStart: t0.Add(-time.Duration(ageSeconds * float64(time.Second))),
		AgeSeconds: ageSeconds, Tags: tags}
}

// Happy path: a role with p99 2 s gets a proposal of 6 s.
func TestProposeStatementTimeout_ThreeTimesP99RoundedUp(t *testing.T) {
	cases := []struct {
		name          string
		p99, observed float64
		want          int64
	}{
		{"p99 2 s", 2000, 1500, 6000},
		{"rounded up to the next second", 2100, 0, 7000},
		{"never below the observed maximum", 2000, 9500, 10000},
		{"observed maximum exactly a second", 1000, 4000, 4000},
		{"tiny p99 still a whole second", 1, 0, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProposeStatementTimeoutMs(tc.p99, tc.observed); got != tc.want {
				t.Fatalf("Propose(%v, %v) = %d ms, want %d", tc.p99, tc.observed, got, tc.want)
			}
		})
	}
}

// Nil/zero: no statement time gives no proposal.
func TestProposeStatementTimeout_NoEvidenceNoProposal(t *testing.T) {
	for _, p99 := range []float64{0, -5} {
		if got := ProposeStatementTimeoutMs(p99, 3000); got != 0 {
			t.Errorf("Propose(%v) = %d, want 0 (no proposal)", p99, got)
		}
	}
}

// Happy path: warn mode counts one would-cancel for a 7 s query against a
// 6 s budget, and the next cycle does not count the same statement again.
func TestEvaluate_CountsOneWouldCancelPerStatement(t *testing.T) {
	e := NewEvaluator()
	b := []Budget{roleBudget("app-6s", "app", 6000)}
	ws := &collector.WorkloadSessions{Long: []collector.WorkloadLongSession{
		longActive(41, "app", "web", 7, nil), longActive(42, "other", "web", 30, nil)}}
	got := e.Evaluate(b, ws, nil, t0)
	if len(got) != 1 || got[0].Budget != "app-6s" || got[0].WouldCancel != 1 ||
		got[0].WorstMs != 7000 {
		t.Fatalf("tallies = %+v, want one would-cancel worst 7000 ms for app-6s", got)
	}
	ws.Long[0].AgeSeconds = 37 // the same statement, one cycle later
	again := e.Evaluate(b, ws, nil, t0.Add(30*time.Second))
	if again[0].WouldCancel != 0 || again[0].WorstMs != 37000 {
		t.Fatalf("second cycle = %+v, want no new cancel and worst 37000 ms", again[0])
	}
}

// Boundary: a query of exactly 6 s is not a violation; 6.001 s is.
func TestEvaluate_ExactTimeoutIsNotAViolation(t *testing.T) {
	for _, tc := range []struct {
		age  float64
		want int
	}{{6.0, 0}, {6.001, 1}} {
		e := NewEvaluator()
		ws := &collector.WorkloadSessions{Long: []collector.WorkloadLongSession{
			longActive(7, "app", "", tc.age, nil)}}
		got := e.Evaluate([]Budget{roleBudget("b", "app", 6000)}, ws, nil, t0)
		if got[0].WouldCancel != tc.want {
			t.Errorf("age %.3f s: would_cancel %d, want %d", tc.age, got[0].WouldCancel, tc.want)
		}
	}
}

// Matching: application_name and sqlcommenter tag budgets; a protected
// application's would-cancel is counted as protected too.
func TestEvaluate_ApplicationTagAndProtected(t *testing.T) {
	e := NewEvaluator()
	budgets := []Budget{
		{Name: "web", Match: Match{ApplicationName: "web"}, StatementTimeoutMs: 1000},
		{Name: "checkout", Match: Match{Tag: "route=/checkout"}, StatementTimeoutMs: 1000},
		{Name: "etl", Match: Match{ApplicationName: "nightly-replication-etl"},
			StatementTimeoutMs: 1000},
	}
	ws := &collector.WorkloadSessions{Long: []collector.WorkloadLongSession{
		longActive(1, "app", "web", 2, map[string]string{"route": "/checkout"}),
		longActive(2, "app", "api", 2, map[string]string{"route": "/cart"}),
		longActive(3, "etl", "nightly-replication-etl", 5, nil),
	}}
	got := byName(e.Evaluate(budgets, ws, []string{"Replication"}, t0))
	if got["web"].WouldCancel != 1 || got["checkout"].WouldCancel != 1 {
		t.Fatalf("tallies = %+v, want web and checkout to count pid 1 only", got)
	}
	if got["etl"].WouldCancel != 1 || got["etl"].ProtectedCancels != 1 ||
		got["web"].ProtectedCancels != 0 {
		t.Fatalf("protected counts = %+v, want etl's cancel protected", got)
	}
}

// Idle in transaction budgets count idle sessions past their timeout,
// and connection limits count the sessions past the limit (cluster-wide).
func TestEvaluate_IdleInTransactionAndConnectionLimit(t *testing.T) {
	e := NewEvaluator()
	b := []Budget{{Name: "app", Match: Match{Role: "app"},
		IdleInTransactionTimeoutMs: 10000, ConnectionLimit: 20}}
	idle := longActive(9, "app", "web", 11, nil)
	idle.State = "idle in transaction"
	ws := &collector.WorkloadSessions{Long: []collector.WorkloadLongSession{idle,
		longActive(10, "app", "web", 50, nil)},
		Roles: []collector.WorkloadRoleStats{{Role: "app", ClusterSessions: 23}}}
	got := e.Evaluate(b, ws, nil, t0)
	if got[0].WouldCancel != 1 || got[0].WouldReject != 3 {
		t.Fatalf("tally = %+v, want 1 idle cancel (the active query has no "+
			"statement budget) and 3 would-reject", got[0])
	}
}

// Nil/empty: no budgets give no tallies; a nil reading counts nothing.
func TestEvaluate_NilAndEmpty(t *testing.T) {
	e := NewEvaluator()
	if got := e.Evaluate(nil, &collector.WorkloadSessions{}, nil, t0); len(got) != 0 {
		t.Fatalf("no budgets: %+v, want none", got)
	}
	got := e.Evaluate([]Budget{roleBudget("b", "app", 1000)}, nil, nil, t0)
	if len(got) != 1 || got[0].WouldCancel != 0 || got[0].WouldReject != 0 {
		t.Fatalf("nil reading: %+v, want one empty tally", got)
	}
}

// Concurrency: the evaluator is safe for concurrent use and still counts
// each statement once.
func TestEvaluate_ConcurrentCyclesCountOnce(t *testing.T) {
	e := NewEvaluator()
	b := []Budget{roleBudget("b", "app", 1000)}
	ws := &collector.WorkloadSessions{Long: []collector.WorkloadLongSession{
		longActive(5, "app", "", 3, nil)}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := e.Evaluate(b, ws, nil, t0)[0].WouldCancel
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 {
		t.Fatalf("16 concurrent cycles counted %d cancels, want 1", total)
	}
}

// State: the dedupe set is per day; a new day starts it again.
func TestEvaluate_NewDayForgetsSeenStatements(t *testing.T) {
	e := NewEvaluator()
	b := []Budget{roleBudget("b", "app", 1000)}
	ws := &collector.WorkloadSessions{Long: []collector.WorkloadLongSession{
		longActive(5, "app", "", 3, nil)}}
	e.Evaluate(b, ws, nil, t0)
	next := e.Evaluate(b, ws, nil, t0.Add(24*time.Hour))
	if next[0].WouldCancel != 1 {
		t.Fatalf("next day = %+v, want the long statement counted on the new day", next[0])
	}
}

// ledgerDays is one ledger row per UTC day from back days before t0
// through t0's day, the first measured back*24 hours before t0; the row
// at index protectedOn (-1: none) records a protected would-cancel.
func ledgerDays(back, protectedOn int) []ShadowDay {
	var out []ShadowDay
	for i := back; i >= 0; i-- {
		at := t0.Add(-time.Duration(i) * 24 * time.Hour)
		d := ShadowDay{Day: at.Truncate(24 * time.Hour), FirstSeen: at}
		if len(out) == protectedOn {
			d.ProtectedCancels = 1
		}
		out = append(out, d)
	}
	return out
}

// Boundary: 6 days 23 hours of shadow data cannot promote; 7 days can;
// a protected would-cancel blocks promotion.
func TestCanPromote_SevenDayWindow(t *testing.T) {
	first := t0.Add(-7 * 24 * time.Hour)
	ok, why := CanPromote([]ShadowDay{{FirstSeen: first.Add(time.Hour), WouldCancel: 3}}, t0)
	if ok || !strings.Contains(why, "6 days 23 hours") {
		t.Fatalf("6d23h: ok=%v why=%q, want refused with the window", ok, why)
	}
	ok, why = CanPromote(ledgerDays(7, -1), t0)
	if !ok {
		t.Fatalf("7 days: refused (%s)", why)
	}
	ok, why = CanPromote(ledgerDays(7, 3), t0)
	if ok || !strings.Contains(why, "protected") {
		t.Fatalf("protected cancel: ok=%v why=%q, want refused", ok, why)
	}
	if ok, _ := CanPromote(nil, t0); ok {
		t.Fatal("no shadow data promoted")
	}
}

// Invalid input: budgets on postgres, provider admin roles, pg_sage's own
// role or a superuser are refused.
func TestRefusedRole(t *testing.T) {
	for _, role := range []string{"postgres", "rdsadmin", "cloudsqladmin", "azure_superuser",
		"sage_agent"} {
		if err := RefusedRole(role, "sage_agent", false); err == nil {
			t.Errorf("role %s was not refused", role)
		}
	}
	if err := RefusedRole("app", "sage_agent", true); err == nil ||
		!strings.Contains(err.Error(), "superuser") {
		t.Errorf("superuser app: %v, want refused as a superuser", err)
	}
	if err := RefusedRole("app", "sage_agent", false); err != nil {
		t.Errorf("app refused: %v", err)
	}
	if err := RefusedRole("", "sage_agent", false); err == nil {
		t.Error("empty role was not refused")
	}
}

// Conversion from configuration: durations, modes and matches.
func TestFromConfig(t *testing.T) {
	got, err := FromConfig([]config.BudgetConfig{{Name: "app", Match: config.BudgetMatch{
		Role: "app"}, StatementTimeout: "6s", IdleInTransactionSessionTimeout: "2min",
		ConnectionLimit: 40, PoolSize: 20, Mode: "enforce"}})
	if err != nil {
		t.Fatal(err)
	}
	want := Budget{Name: "app", Match: Match{Role: "app"}, StatementTimeoutMs: 6000,
		IdleInTransactionTimeoutMs: 120000, ConnectionLimit: 40, PoolSize: 20,
		Mode: ModeEnforce}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("FromConfig = %+v, want %+v", got, want)
	}
	if _, err := FromConfig([]config.BudgetConfig{{Name: "x", Match: config.BudgetMatch{
		Role: "app"}, StatementTimeout: "-1s"}}); err == nil {
		t.Fatal("a negative timeout was accepted")
	}
}

func byName(ts []Tally) map[string]Tally {
	out := map[string]Tally{}
	for _, t := range ts {
		out[t.Budget] = t
	}
	return out
}

// Boundary: a day without a ledger row inside the window (pg_sage did not
// run) refuses promotion, however far back the ledger reaches.
func TestCanPromote_GapInTheWindowRefuses(t *testing.T) {
	days := ledgerDays(8, -1)
	gap := days[4].Day
	withGap := append(append([]ShadowDay{}, days[:4]...), days[5:]...)
	ok, why := CanPromote(withGap, t0)
	if ok || !strings.Contains(why, gap.Format(time.DateOnly)) ||
		!strings.Contains(why, "continuous") {
		t.Fatalf("gap on %s: ok=%v why=%q, want refused naming the day", gap, ok, why)
	}
	ends := []ShadowDay{days[0], days[len(days)-1]}
	if ok, _ := CanPromote(ends, t0); ok {
		t.Fatal("a row 8 days ago and one today promoted")
	}
	if ok, why := CanPromote(days, t0); !ok {
		t.Fatalf("continuous 8 days: refused (%s)", why)
	}
}

// The ledger key: a budget's match and limits, not its mode or pool size.
func TestBudget_Definition(t *testing.T) {
	warn := roleBudget("b", "app", 6000)
	enforce := warn
	enforce.Mode, enforce.PoolSize = ModeEnforce, 20
	if warn.Definition() != enforce.Definition() {
		t.Fatalf("mode or pool size changed the definition: %q vs %q", warn.Definition(),
			enforce.Definition())
	}
	want := "role=app statement_timeout=6000ms idle_in_transaction_session_timeout=0ms " +
		"connection_limit=0"
	if warn.Definition() != want {
		t.Fatalf("definition = %q, want %q", warn.Definition(), want)
	}
	for name, other := range map[string]Budget{
		"timeout": roleBudget("b", "app", 1000), "role": roleBudget("b", "web", 6000),
		"limit": {Name: "b", Match: Match{Role: "app"}, StatementTimeoutMs: 6000,
			ConnectionLimit: 5},
	} {
		if other.Definition() == warn.Definition() {
			t.Errorf("%s change kept the definition %q", name, warn.Definition())
		}
	}
	got := NewEvaluator().Evaluate([]Budget{warn}, nil, nil, t0)
	if len(got) != 1 || got[0].Definition != want {
		t.Fatalf("tally = %+v, want the definition", got)
	}
}
