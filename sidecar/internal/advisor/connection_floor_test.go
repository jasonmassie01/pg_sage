package advisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// advisors-07: the max_connections floor is the window's peak of total
// backends, not one moment, plus the server's reserved slots and a margin.

// No concurrent access tests: one advisor goroutine reviews a database,
// and the history read is a single statement (fakeConnHistory is guarded
// only because the advisor may be shared in other tests).

type fakeConnHistory struct {
	mu     sync.Mutex
	w      connWindow
	err    error
	asked  []time.Duration
	called int
}

func (f *fakeConnHistory) readConnWindow(_ context.Context, window time.Duration) (
	connWindow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	f.asked = append(f.asked, window)
	return f.w, f.err
}

func floorSettings(extra ...collector.PGSetting) []collector.PGSetting {
	return append([]collector.PGSetting{
		{Name: "max_connections", Setting: "500", Context: "postmaster"},
		{Name: "superuser_reserved_connections", Setting: "3", Context: "postmaster"},
	}, extra...)
}

func maxConnFinding(n int) analyzer.Finding {
	return analyzer.Finding{Category: "connection_tuning", ObjectIdentifier: "instance",
		Severity: "warning", Recommendation: "resize",
		RecommendedSQL: fmt.Sprintf("ALTER SYSTEM SET max_connections = %d;", n),
		ActionRisk:     "moderate"}
}

// targets returns the max_connections of each executable finding, and the
// count of findings left without SQL.
func targets(findings []analyzer.Finding) (kept []int, advisory int) {
	for _, f := range findings {
		if strings.TrimSpace(f.RecommendedSQL) == "" {
			advisory++
			continue
		}
		if n, ok := parseMaxConnectionsTarget(f.RecommendedSQL); ok {
			kept = append(kept, n)
		}
	}
	return kept, advisory
}

func TestConnectionFloor_PeakNotMoment(t *testing.T) {
	sys := collector.SystemStats{TotalBackends: 50, MaxConnections: 500}
	w := connWindow{window: 24 * time.Hour, peakTotal: 400, peakKnown: true}
	fl := connectionFloor(sys, floorSettings(), 17, w)
	if !fl.known || fl.value != 413 {
		t.Fatalf("floor = %+v, want known 413 (peak 400 + reserved 3 + margin 10)", fl)
	}
	in := []analyzer.Finding{maxConnFinding(60), maxConnFinding(412), maxConnFinding(413),
		maxConnFinding(420), maxConnFinding(1000)}
	kept, advisory := targets(filterUnsafeMaxConnections(in, fl, nil))
	if fmt.Sprint(kept) != "[413 420 1000]" || advisory != 0 {
		t.Errorf("kept %v (advisory %d), want [413 420 1000]: every value below "+
			"peak + reserved + 10 is refused", kept, advisory)
	}
}

// The current count still floors the value when it is above the peak (the
// history is behind the snapshot).
func TestConnectionFloor_CurrentAbovePeak(t *testing.T) {
	sys := collector.SystemStats{TotalBackends: 450, MaxConnections: 500}
	w := connWindow{window: time.Hour, peakTotal: 400, peakKnown: true}
	if fl := connectionFloor(sys, floorSettings(), 17, w); fl.value != 463 {
		t.Errorf("floor = %d, want 463 (current 450 + 3 + 10)", fl.value)
	}
}

func TestConnectionFloor_ReservedConnectionsByVersion(t *testing.T) {
	sys := collector.SystemStats{TotalBackends: 50, MaxConnections: 500}
	w := connWindow{window: 24 * time.Hour, peakTotal: 400, peakKnown: true}
	reserved := collector.PGSetting{Name: "reserved_connections", Setting: "10",
		Context: "postmaster"}
	cases := []struct {
		major int
		want  int
	}{
		{16, 423}, {17, 423}, {18, 423},
		{15, 413}, {14, 413},
		// Unknown version: a reserved_connections row exists only on 16+.
		{0, 423},
	}
	for _, c := range cases {
		fl := connectionFloor(sys, floorSettings(reserved), c.major, w)
		if fl.value != c.want {
			t.Errorf("PG%d: floor %d, want %d", c.major, fl.value, c.want)
		}
	}
	in := []analyzer.Finding{maxConnFinding(413), maxConnFinding(423)}
	kept, _ := targets(filterUnsafeMaxConnections(in,
		connectionFloor(sys, floorSettings(reserved), 16, w), nil))
	if fmt.Sprint(kept) != "[423]" {
		t.Errorf("PG16 kept %v, want [423]", kept)
	}
}

// A missing or unreadable superuser_reserved_connections counts as the
// PostgreSQL default (3), never as 0.
func TestConnectionFloor_ReservedDefaults(t *testing.T) {
	sys := collector.SystemStats{TotalBackends: 50, MaxConnections: 500}
	w := connWindow{peakTotal: 100, peakKnown: true}
	for _, settings := range [][]collector.PGSetting{
		nil,
		{{Name: "superuser_reserved_connections", Setting: "x"}},
		{{Name: "superuser_reserved_connections", Setting: "-4"}},
	} {
		if fl := connectionFloor(sys, settings, 17, w); fl.value != 113 {
			t.Errorf("settings %v: floor %d, want 113", settings, fl.value)
		}
	}
	big := []collector.PGSetting{{Name: "superuser_reserved_connections", Setting: "20"}}
	if fl := connectionFloor(sys, big, 17, w); fl.value != 130 {
		t.Errorf("reserved 20: floor %d, want 130", fl.value)
	}
}

func TestConnectionFloor_UnknownPeakStripsDecreases(t *testing.T) {
	sys := collector.SystemStats{TotalBackends: 50, MaxConnections: 500}
	fl := connectionFloor(sys, floorSettings(), 17, connWindow{})
	if fl.known {
		t.Fatalf("no history: floor %+v must be unknown", fl)
	}
	var logs []string
	logFn := func(_, msg string, args ...any) { logs = append(logs, fmt.Sprintf(msg, args...)) }
	in := []analyzer.Finding{maxConnFinding(300), maxConnFinding(600), maxConnFinding(40),
		maxConnFinding(500)}
	out := filterUnsafeMaxConnections(in, fl, logFn)
	kept, advisory := targets(out)
	// 300: a decrease without a peak is advice only; 40 is under the live
	// floor (63) and is refused outright; 500 is no change.
	if fmt.Sprint(kept) != "[600 500]" || advisory != 1 || len(out) != 3 {
		t.Fatalf("kept %v advisory %d (of %d), want [600 500] and one advisory", kept,
			advisory, len(out))
	}
	for _, f := range out {
		if f.RecommendedSQL == "" && (f.Severity != "info" || f.ActionRisk != "" ||
			!strings.Contains(f.Recommendation, "peak")) {
			t.Errorf("advisory decrease must be info, riskless and say why: %+v", f)
		}
	}
	if !strings.Contains(strings.Join(logs, "\n"), "unknown") {
		t.Errorf("the stripped decrease must be logged with its reason: %v", logs)
	}
}

// An unknown max_connections (0) cannot prove a value is an increase.
func TestConnectionFloor_UnknownPeakAndMax(t *testing.T) {
	fl := connectionFloor(collector.SystemStats{TotalBackends: 5}, nil, 17, connWindow{})
	_, advisory := targets(filterUnsafeMaxConnections(
		[]analyzer.Finding{maxConnFinding(800)}, fl, nil))
	if advisory != 1 {
		t.Errorf("unknown peak and max: the change must be advisory, got %d", advisory)
	}
}

func TestConnectionFloor_UnrelatedFindingsPass(t *testing.T) {
	fl := connectionFloor(collector.SystemStats{TotalBackends: 10, MaxConnections: 100},
		nil, 17, connWindow{})
	in := []analyzer.Finding{{Category: "connection_tuning",
		RecommendedSQL: "ALTER SYSTEM SET idle_in_transaction_session_timeout = '5min'"},
		{Category: "connection_tuning", Recommendation: "Use PgBouncer"}}
	out := filterUnsafeMaxConnections(in, fl, nil)
	if len(out) != 2 || out[0].RecommendedSQL != in[0].RecommendedSQL {
		t.Errorf("non-max_connections findings must pass unchanged: %+v", out)
	}
	if got := filterUnsafeMaxConnections(nil, fl, nil); len(got) != 0 {
		t.Errorf("nil in, %d out", len(got))
	}
}

// Error propagation: a history read error is logged with the database name
// and the peak stays unknown; it is never a peak of 0.
func TestConnectionHistory_ReadErrorIsLoggedNotZero(t *testing.T) {
	src := &fakeConnHistory{err: errors.New("connection refused"),
		w: connWindow{peakTotal: 0, peakKnown: true}}
	var logs []string
	logFn := func(_, msg string, args ...any) { logs = append(logs, fmt.Sprintf(msg, args...)) }
	env := readConnEnv(context.Background(), src, 24*time.Hour, "orders_db", logFn)
	if env.hist.peakKnown || env.hist.sessionsKnown {
		t.Fatalf("read error must leave the window unknown: %+v", env.hist)
	}
	if env.database != "orders_db" || env.hist.window != 24*time.Hour {
		t.Errorf("env = %+v, want database orders_db and the 24h window", env)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "orders_db") || !strings.Contains(joined, "connection refused") {
		t.Errorf("log must name the database and the error: %q", joined)
	}
	if len(src.asked) != 1 || src.asked[0] != 24*time.Hour {
		t.Errorf("history asked for %v, want one 24h read", src.asked)
	}
	sys := collector.SystemStats{TotalBackends: 50, MaxConnections: 500}
	fl := connectionFloor(sys, floorSettings(), 17, env.hist)
	_, advisory := targets(filterUnsafeMaxConnections([]analyzer.Finding{maxConnFinding(100)},
		fl, nil))
	if fl.known || advisory != 1 {
		t.Errorf("after a read error a decrease must be advisory: floor %+v", fl)
	}
}

func TestConnectionHistory_NilSourceIsUnknown(t *testing.T) {
	env := readConnEnv(context.Background(), nil, time.Hour, "db", noopLog)
	if env.hist.peakKnown || env.hist.sessionsKnown || env.database != "db" {
		t.Errorf("nil source: env %+v, want unknown window", env)
	}
}

// The advisor reads the window of its own interval for its own database.
func TestConnectionHistory_AdvisorWindowAndDatabase(t *testing.T) {
	a := newTestAdvisor(true, true, 3600)
	a.WithDatabaseName("billing")
	src := &fakeConnHistory{w: connWindow{peakTotal: 77, peakKnown: true}}
	a.connHist = src
	env := connEnvFrom(a.connectionContext(context.Background()))
	if env.database != "billing" || !env.hist.peakKnown || env.hist.peakTotal != 77 {
		t.Errorf("env = %+v, want billing with peak 77", env)
	}
	if len(src.asked) != 1 || src.asked[0] != time.Hour {
		t.Errorf("asked %v, want the 1h advisor interval", src.asked)
	}
	if env.hist.window != time.Hour {
		t.Errorf("window = %v, want 1h", env.hist.window)
	}
}

func TestConnChurn_SessionsDeltaOverWindow(t *testing.T) {
	epoch := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	snap := &collector.Snapshot{CollectedAt: now,
		System: collector.SystemStats{Sessions: 1600, RelationStatsEpoch: epoch}}
	w := connWindow{sessionsKnown: true, firstSessions: 1000, firstEpoch: epoch,
		firstAt: now.Add(-10 * time.Minute)}
	opened, perMin, ok := connChurn(snap, w)
	if !ok || opened != 600 || perMin != 60 {
		t.Fatalf("churn = %d %.1f/min %v, want 600 at 60/min", opened, perMin, ok)
	}
	cases := map[string]func(*collector.Snapshot, *connWindow){
		"epoch moved": func(_ *collector.Snapshot, w *connWindow) {
			w.firstEpoch = epoch.Add(-time.Hour)
		},
		"counter fell": func(s *collector.Snapshot, _ *connWindow) { s.System.Sessions = 900 },
		"no sessions column": func(s *collector.Snapshot, _ *connWindow) {
			s.System.Sessions = 0
		},
		"no history":   func(_ *collector.Snapshot, w *connWindow) { w.sessionsKnown = false },
		"same instant": func(_ *collector.Snapshot, w *connWindow) { w.firstAt = now },
		"unknown epoch": func(s *collector.Snapshot, _ *connWindow) {
			s.System.RelationStatsEpoch = time.Time{}
		},
	}
	for name, mutate := range cases {
		s, ww := *snap, w
		mutate(&s, &ww)
		if _, _, ok := connChurn(&s, ww); ok {
			t.Errorf("%s: churn must be unknown", name)
		}
	}
	if _, _, ok := connChurn(nil, w); ok {
		t.Error("nil snapshot: churn must be unknown")
	}
}

// The prompt labels the scopes and gives the model the peak, the floor and
// the window's churn it is asked to size against.
func TestConnectionPrompt_WindowFacts(t *testing.T) {
	epoch := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	snap := connectionSnapshot()
	snap.CollectedAt = now
	snap.System.TotalBackends, snap.System.Sessions = 50, 1600
	snap.System.RelationStatsEpoch = epoch
	env := connEnv{database: "orders", hist: connWindow{window: 2 * time.Hour,
		peakTotal: 400, peakKnown: true, sessionsKnown: true, firstSessions: 1000,
		firstEpoch: epoch, firstAt: now.Add(-10 * time.Minute)}}
	mgr, capture := capturingManager(t, "[]")
	ctx := withConnEnv(context.WithValue(context.Background(), promptEnvKey{},
		promptEnv{pgMajor: 17}), env)
	if _, err := analyzeConnections(ctx, mgr, snap, config.DefaultConfig(),
		noopLog); err != nil {
		t.Fatalf("analyzeConnections: %v", err)
	}
	p := onlyPrompt(t, capture())
	facts := promptFacts(p.user)
	want := map[string]string{
		"Advisor window":                    "2h0m0s",
		"Sessions opened in the window":     "600 (60.0/min)",
		"Total backends":                    "50",
		"Peak total backends in the window": "400",
		"Reserved slots":                    "3",
		"Lowest safe max_connections": "413 (peak or current total backends + " +
			"reserved slots + 10)",
	}
	for k, v := range want {
		if facts[k] != v {
			t.Errorf("%s = %q, want %q", k, facts[k], v)
		}
	}
	for _, label := range []string{"This database (orders):", "Instance (all databases):"} {
		if !strings.Contains(p.user, label) {
			t.Errorf("prompt must label the scope %q:\n%s", label, p.user)
		}
	}
}

// Invalid input: the TO form and a RESET (to the default 100) are sized
// like any other value; the old parser read no number from "TO 50" and let
// it through.
func TestConnectionFloor_AlternateForms(t *testing.T) {
	sys := collector.SystemStats{TotalBackends: 50, MaxConnections: 500}
	fl := connectionFloor(sys, floorSettings(), 17,
		connWindow{peakTotal: 400, peakKnown: true})
	in := []analyzer.Finding{
		{Category: "connection_tuning", RecommendedSQL: "ALTER SYSTEM SET max_connections TO 50"},
		{Category: "connection_tuning", RecommendedSQL: "ALTER SYSTEM RESET max_connections"},
		{Category: "connection_tuning", RecommendedSQL: "ALTER SYSTEM SET max_connections TO 450"},
	}
	out := filterUnsafeMaxConnections(in, fl, nil)
	if len(out) != 1 || !strings.HasSuffix(out[0].RecommendedSQL, "450") {
		t.Errorf("want only the 450 kept: %+v", out)
	}
}
