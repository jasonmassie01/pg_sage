package agentposture

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// countingDetector reports one finding per run and counts its runs.
type countingDetector struct {
	id   string
	mu   sync.Mutex
	runs int
	fail error
}

func (d *countingDetector) Spec() Spec {
	return Spec{ID: d.id, Title: "count " + d.id, Severity: Warning}
}

func (d *countingDetector) Detect(ctx context.Context, in Input) ([]Finding, error) {
	d.mu.Lock()
	d.runs++
	fail := d.fail
	d.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	var one int
	if err := in.Q.QueryRow(ctx, Statement(d.id, "SELECT 1")).Scan(&one); err != nil {
		return nil, err
	}
	return []Finding{{Severity: Warning, ObjectType: "database", Object: "db",
		Title: "seen"}}, nil
}

func (d *countingDetector) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runs
}

func newTestMonitor(t *testing.T, pool *pgxpool.Pool, now *time.Time,
	dets ...Detector) *Monitor {
	t.Helper()
	reg := NewRegistry()
	for _, d := range dets {
		if err := reg.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	return NewMonitor(pool, MonitorOptions{Registry: reg,
		Config: func() Config { return DefaultConfig() },
		Now:    func() time.Time { return *now }})
}

func TestMonitor_RunsOnFirstSightThenOnlyOnCatalogChangeOrDaily(t *testing.T) {
	pool, ctx := livePool(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	ok := &countingDetector{id: "AP-01"}
	m := newTestMonitor(t, pool, &now, ok)

	got, err := m.Detect(ctx)
	if err != nil || len(got) != 1 || ok.count() != 1 {
		t.Fatalf("first Detect = %d findings, %v, runs %d", len(got), err, ok.count())
	}
	if got[0].Category != "agent_posture:AP-01" || got[0].RecommendedSQL != "" {
		t.Fatalf("finding = %+v", got[0])
	}
	if c := m.LastEvaluatedCategories(); !slices.Equal(c, []string{"agent_posture:AP-01"}) {
		t.Fatalf("evaluated = %v", c)
	}

	// Roles are cluster-wide: another package's test may change them, so
	// an unchanged catalog is awaited for a few attempts.
	runs := ok.count()
	for i := 0; ; i++ {
		now = now.Add(time.Minute)
		got, err = m.Detect(ctx)
		if err != nil {
			t.Fatalf("unchanged catalog: %v", err)
		}
		if got == nil {
			break
		}
		runs++
		if i == 3 {
			t.Fatal("the monitor ran on every cycle of an unchanged catalog")
		}
	}
	if ok.count() != runs {
		t.Fatalf("runs = %d, want %d", ok.count(), runs)
	}
	if c := m.LastEvaluatedCategories(); len(c) != 0 {
		t.Fatalf("a skipped run evaluated %v; open findings would resolve", c)
	}

	table := "posture_mon_" + suffix(t)
	execAll(t, ctx, pool, "CREATE TABLE "+table+" (id int)",
		"GRANT SELECT ON "+table+" TO PUBLIC")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+table) })
	now = now.Add(time.Minute)
	if got, err = m.Detect(ctx); err != nil || len(got) != 1 || ok.count() != runs+1 {
		t.Fatalf("after a grant: %d findings, %v, runs %d", len(got), err, ok.count())
	}

	now = now.Add(24 * time.Hour)
	if got, err = m.Detect(ctx); err != nil || len(got) != 1 || ok.count() != runs+2 {
		t.Fatalf("next day: %d findings, %v, runs %d", len(got), err, ok.count())
	}
}

func TestMonitor_FailedDetectorIsNotEvaluated(t *testing.T) {
	pool, ctx := livePool(t)
	now := time.Now()
	good := &countingDetector{id: "AP-01"}
	bad := &countingDetector{id: "AP-02", fail: errors.New("boom")}
	m := newTestMonitor(t, pool, &now, good, bad)
	got, err := m.Detect(ctx)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(got) != 1 || got[0].RuleID != "AP-01" {
		t.Fatalf("findings = %+v", got)
	}
	c := m.LastEvaluatedCategories()
	if !slices.Contains(c, "agent_posture:AP-01") || slices.Contains(c, "agent_posture:AP-02") {
		t.Fatalf("evaluated = %v: a failed detector must keep its open findings", c)
	}
}

func TestMonitor_NilPoolAndEndedContext(t *testing.T) {
	m := NewMonitor(nil, MonitorOptions{})
	if _, err := m.Detect(context.Background()); !errors.Is(err, ErrNoPool) {
		t.Fatalf("nil pool: %v", err)
	}
	if len(m.LastEvaluatedCategories()) != 0 {
		t.Fatal("a failed run evaluated categories")
	}
	pool, ctx := livePool(t)
	now := time.Now()
	m = newTestMonitor(t, pool, &now, &countingDetector{id: "AP-01"})
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.Detect(cctx); err == nil {
		t.Fatal("Detect succeeded on an ended context")
	}
	// The failed attempt does not count as a run: the next one runs.
	if got, err := m.Detect(ctx); err != nil || len(got) != 1 {
		t.Fatalf("after a failed attempt: %d findings, %v", len(got), err)
	}
}

// Concurrent access: /metrics and the analyzer may read the evaluated
// categories while a cycle runs.
func TestMonitor_ConcurrentDetectAndRead(t *testing.T) {
	pool, ctx := livePool(t)
	now := time.Now()
	m := NewMonitor(pool, MonitorOptions{Registry: func() *Registry {
		r := NewRegistry()
		_ = r.Register(&countingDetector{id: "AP-01"})
		return r
	}(), Config: DefaultConfig, Now: func() time.Time { return now }})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := m.Detect(ctx); err != nil {
				t.Errorf("Detect: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			_ = m.LastEvaluatedCategories()
		}()
	}
	wg.Wait()
}

func TestFingerprint_ChangesWithPostureRelevantCatalogOnly(t *testing.T) {
	pool, ctx := livePool(t)
	s := suffix(t)
	table, role := "posture_fp_"+s, "posture_fp_role_"+s
	execAll(t, ctx, pool, "CREATE TABLE "+table+" (id int)")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	createRole(t, ctx, pool, role, "NOLOGIN")
	fp := func() int64 {
		t.Helper()
		var v int64
		readTx(t, ctx, pool, func(tx pgxTx) {
			var err error
			if v, err = Fingerprint(ctx, tx); err != nil {
				t.Fatalf("Fingerprint: %v", err)
			}
		})
		return v
	}
	execAll(t, ctx, pool, "INSERT INTO "+table+" SELECT generate_series(1, 100)")
	stable := false
	for i := 0; i < 4 && !stable; i++ { // cluster-wide roles may change meanwhile
		before := fp()
		execAll(t, ctx, pool, "INSERT INTO "+table+" SELECT generate_series(1, 100)",
			"ANALYZE "+table)
		stable = fp() == before
	}
	if !stable {
		t.Fatal("data and statistics changed the fingerprint")
	}
	before := fp()
	changes := []string{
		"GRANT SELECT ON " + table + " TO " + role,
		"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY p_" + s + " ON " + table + " FOR SELECT TO " + role + " USING (true)",
		"ALTER ROLE " + role + " BYPASSRLS",
		"ALTER ROLE " + role + " SET statement_timeout = '5s'",
		"GRANT pg_read_all_data TO " + role,
		"CREATE FUNCTION f_" + s + "() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'",
		"ALTER FUNCTION f_" + s + "() SET search_path = pg_catalog",
		"CREATE VIEW v_" + s + " AS SELECT * FROM " + table,
		"ALTER TABLE " + table + " OWNER TO " + role,
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS f_"+s+"()")
		_, _ = pool.Exec(context.Background(), "DROP VIEW IF EXISTS v_"+s)
	})
	prev := before
	for _, c := range changes {
		execAll(t, ctx, pool, c)
		now := fp()
		if now == prev {
			t.Errorf("fingerprint unchanged after %s", strings.SplitN(c, " ", 4)[:3])
		}
		prev = now
	}
}
