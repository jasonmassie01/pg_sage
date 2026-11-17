package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre"
)

// observability-06: the Sage SRE investigation backlog on /metrics.

// No concurrent access tests here: the refresher publishes immutable
// snapshots, and its concurrent scrapes are covered by
// TestRefresherConcurrentScrapesSeeOneSnapshot.

func backlogView(name string, b *sreBacklog) metricsView {
	return metricsView{name: name, sources: []string{metricsSourceSRE},
		snap: &metricsSnapshot{ok: map[string]bool{metricsSourceSRE: true}, sre: b}}
}

func renderSRE(views ...metricsView) string {
	var b strings.Builder
	writeSREInvestigationMetrics(&b, views)
	return b.String()
}

// Happy path: counts by state per database, every live state sampled
// (zero included), terminal states only when counted, and the age of the
// oldest open investigation.
func TestWriteSREInvestigationMetrics(t *testing.T) {
	// Views arrive in name order (metricsRegistry.views sorts them).
	out := renderSRE(
		backlogView("alpha", &sreBacklog{byState: map[string]int64{}}),
		backlogView("zeta", &sreBacklog{byState: map[string]int64{"queued": 2,
			"needs_evidence": 1, "concluded": 1},
			oldestOpen: time.Now().Add(-90 * time.Minute)}))
	for _, want := range []string{
		"# TYPE pg_sage_sre_investigations gauge",
		`pg_sage_sre_investigations{database="zeta",state="queued"} 2`,
		`pg_sage_sre_investigations{database="zeta",state="needs_evidence"} 1`,
		`pg_sage_sre_investigations{database="zeta",state="concluded"} 1`,
		`pg_sage_sre_investigations{database="zeta",state="collecting"} 0`,
		`pg_sage_sre_investigations{database="zeta",state="paused"} 0`,
		`pg_sage_sre_investigations{database="alpha",state="queued"} 0`,
		"# TYPE pg_sage_sre_investigation_oldest_open_age_seconds gauge",
		`pg_sage_sre_investigation_oldest_open_age_seconds{database="alpha"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{`state="failed"`, `state="expired"`,
		`database="alpha",state="concluded"`} {
		if strings.Contains(out, absent) {
			t.Errorf("uncounted terminal state exported: %q\n%s", absent, out)
		}
	}
	age := sreSampleValue(t, out,
		`pg_sage_sre_investigation_oldest_open_age_seconds{database="zeta"}`)
	if age < 5390 || age > 5420 {
		t.Errorf("oldest open age = %v, want about 5400s", age)
	}
	if strings.Index(out, `database="alpha"`) > strings.Index(out, `database="zeta"`) {
		t.Error("databases are not exported in view (name) order")
	}
	lintScrape(t, out)
}

// Nil/empty: with SRE off no view has a backlog, so neither family is
// written, not even its HELP.
func TestWriteSREInvestigationMetricsAbsentWithoutSRE(t *testing.T) {
	plain := metricsView{name: "orders", sources: []string{metricsSourceValue},
		snap: &metricsSnapshot{ok: map[string]bool{metricsSourceValue: true}}}
	unrefreshed := metricsView{name: "fresh", sources: []string{metricsSourceSRE}}
	for name, views := range map[string][]metricsView{"none": nil,
		"no sre source": {plain}, "never refreshed": {unrefreshed}} {
		if out := renderSRE(views...); out != "" {
			t.Errorf("%s: SRE families written without a backlog:\n%s", name, out)
		}
	}
}

// Boundary: a clock step that puts the oldest open investigation in the
// future never exports a negative age.
func TestWriteSREInvestigationMetricsNeverNegativeAge(t *testing.T) {
	out := renderSRE(backlogView("orders", &sreBacklog{byState: map[string]int64{
		"queued": 1}, oldestOpen: time.Now().Add(time.Hour)}))
	if got := sreSampleValue(t, out,
		`pg_sage_sre_investigation_oldest_open_age_seconds{database="orders"}`); got != 0 {
		t.Fatalf("future oldest open age = %v, want 0", got)
	}
}

// The terminal states the backlog query names are the state machine's.
func TestSRETerminalStatesMatchTheStateMachine(t *testing.T) {
	var want []string
	for _, s := range sre.AllStates() {
		if s.Terminal() {
			want = append(want, "'"+string(s)+"'")
		}
	}
	if got := "(" + strings.Join(want, ", ") + ")"; got != sreTerminalStates {
		t.Fatalf("sreTerminalStates = %s, want %s", sreTerminalStates, got)
	}
	if strings.Count(sreBacklogSQL, sreTerminalStates) != 2 {
		t.Fatalf("backlog query does not filter on the terminal states twice:\n%s",
			sreBacklogSQL)
	}
}

// Error propagation: a refresh whose backlog read times out keeps the
// previous sample (the backlog-age alert does not flap), marks the source
// failed, logs once, and recovers.
func TestSRESourceTimeoutKeepsThePreviousSample(t *testing.T) {
	withMetricsRegistry(t)
	var mu sync.Mutex
	fail := false
	read := func(context.Context) (sreBacklog, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return sreBacklog{}, context.DeadlineExceeded
		}
		return sreBacklog{byState: map[string]int64{"queued": 3}}, nil
	}
	m := newMetricsRefresher("orders", fixedInterval(time.Hour), sreMetricsSource(read))
	ctx := context.Background()
	m.refresh(ctx)
	first := m.snapshot()
	mu.Lock()
	fail = true
	mu.Unlock()
	logs := captureStderr(t, func() {
		m.refresh(ctx)
		m.refresh(ctx)
	})
	kept := m.snapshot()
	if kept.ok[metricsSourceSRE] || kept.sre == nil || kept.sre.byState["queued"] != 3 {
		t.Fatalf("after a timeout ok=%v sre=%+v, want failed with the previous sample",
			kept.ok[metricsSourceSRE], kept.sre)
	}
	if !kept.lastSuccess[metricsSourceSRE].Equal(first.lastSuccess[metricsSourceSRE]) {
		t.Fatal("a failed refresh moved the SRE source's last success")
	}
	if n := strings.Count(logs, "refreshing sre metrics failed"); n != 1 {
		t.Fatalf("two failed refreshes logged %d warnings, want 1:\n%s", n, logs)
	}
	if !strings.Contains(logs, "deadline exceeded") {
		t.Fatalf("the warning does not name the timeout:\n%s", logs)
	}
}

// A source that never succeeded has nothing to keep.
func TestSRESourceFirstFailureHasNoSample(t *testing.T) {
	read := func(context.Context) (sreBacklog, error) {
		return sreBacklog{}, errors.New("permission denied for table sre_investigations")
	}
	m := newMetricsRefresher("orders", fixedInterval(time.Hour), sreMetricsSource(read))
	captureStderr(t, func() { m.refresh(context.Background()) })
	if s := m.snapshot(); s.sre != nil || s.ok[metricsSourceSRE] {
		t.Fatalf("first failed refresh snapshot sre=%+v ok=%v, want none", s.sre,
			s.ok[metricsSourceSRE])
	}
}

// Invalid input: the reader refuses a runtime without a control pool.
func TestReadSREBacklogWithoutAPool(t *testing.T) {
	if _, err := readSREBacklog(context.Background(), nil, "db:1"); !errors.Is(err,
		errNoMetricsPool) {
		t.Fatalf("read without a pool = %v, want errNoMetricsPool", err)
	}
}

// sampleValue is the float value of the one sample line starting with series.
func sreSampleValue(t *testing.T, out, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, series+" ") {
			v, err := strconv.ParseFloat(strings.TrimPrefix(line, series+" "), 64)
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("no sample %s in:\n%s", series, out)
	return 0
}

// State transitions: a runtime whose investigator runs refreshes its SRE
// backlog; one without (SRE off or not started) does not.
func TestRuntimeMetricsRefresherAddsTheSRESource(t *testing.T) {
	p, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	t.Cleanup(p.Close)
	store, err := sre.NewPostgresStore(p, sre.DefaultLimits())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: store, Runner: chainRunner{},
		Config: sre.DefaultCoordinatorConfig("db:7"), LogFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	rtCfg := config.DefaultConfig()
	off := &databaseRuntime{spec: databaseRuntimeSpec{Name: "orders", Pool: p,
		ControlPool: p}, cfg: rtCfg}
	if got := sourceNames(newRuntimeMetricsRefresher(off).sources); strings.Join(got,
		",") != "value,actions,facts" {
		t.Fatalf("sources without SRE = %v, want value,actions,facts", got)
	}
	on := &databaseRuntime{spec: databaseRuntimeSpec{Name: "orders", Pool: p,
		ControlPool: p}, cfg: rtCfg, sre: coord}
	if got := sourceNames(newRuntimeMetricsRefresher(on).sources); strings.Join(got,
		",") != "value,actions,facts,sre" {
		t.Fatalf("sources with SRE = %v, want value,actions,facts,sre", got)
	}
}
