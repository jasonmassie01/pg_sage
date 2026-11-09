package runway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// forecast-02 on real PostgreSQL: a gap in the samples, or a trends read
// cut at its row cap, is not a cleared runway. The open finding stays
// open with the same id (marked stale) and no second finding, case or
// investigation key is created for the same object.
//
// No concurrency test: one monitor goroutine ticks per runtime and the
// holds are computed per tick.

// resetXID removes every xid/mxid sample and wraparound finding, so the
// test controls the series.
func resetXID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	clean := func(ctx context.Context) error {
		_, err := pool.Exec(ctx, `DELETE FROM sage.runway_samples
			WHERE kind IN ('xid', 'mxid');
			DELETE FROM sage.findings WHERE category = '`+CategoryWraparound+`'`)
		return err
	}
	if err := clean(ctx); err != nil {
		t.Fatalf("reset xid: %v", err)
	}
	t.Cleanup(func() { _ = clean(context.Background()) })
}

// seedXIDTrend writes 12 xid samples over 55 minutes ending ago before
// now, rising 1000/s toward the warning age it reaches 24 h after the last.
func seedXIDTrend(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	ago time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, counter, limit_value)
		SELECT 'xid', 'cluster', 'seed',
		       now() - make_interval(secs => $1) - make_interval(mins => 55 - i * 5),
		       $2 - 86400000::float8 - (11 - i) * 300000::float8,
		       3e9 + i * 300000::float8, $2
		FROM generate_series(0, 11) i`, ago.Seconds(),
		float64(WraparoundWarnAge)); err != nil {
		t.Fatalf("seed xid: %v", err)
	}
}

// openXIDFinding opens the critical XID forecast finding directly.
func openXIDFinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	r := Runway{Category: CategoryWraparound, ObjectType: "database", Identifier: "xid",
		Kind: sre.TriggerWraparound, Subject: "xid", Severity: SeverityCritical,
		Title:          "The XID age reaches the wraparound warning limit in about 24h0m0s",
		Recommendation: "test", Detail: map[string]any{"runway_kind": "xid"}}
	if _, err := analyzer.UpsertFindingsWithResult(ctx, pool,
		[]analyzer.Finding{r.Finding()}); err != nil {
		t.Fatalf("open xid finding: %v", err)
	}
	id, sev, ok := openFinding(t, ctx, pool, CategoryWraparound, "xid")
	if !ok || sev != SeverityCritical {
		t.Fatalf("xid finding open=%v severity %q", ok, sev)
	}
	return id
}

// staleDetail reads an open finding's stale keys.
func staleDetail(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cat,
	ident string) (bool, string, bool) {
	t.Helper()
	var stale, since bool
	var reason string
	err := pool.QueryRow(ctx, `SELECT COALESCE((detail->>'stale')::bool, false),
		COALESCE(detail->>'stale_reason', ''), detail ? 'stale_since'
		FROM sage.findings
		WHERE category = $1 AND object_identifier = $2 AND status = 'open'`, cat, ident).
		Scan(&stale, &reason, &since)
	if err != nil {
		t.Fatalf("read %s/%s detail: %v", cat, ident, err)
	}
	return stale, reason, since
}

func liveRunner(pool *pgxpool.Pool) ProbeRunner {
	return probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
}

// An open critical XID finding whose samples are all older than the
// lookback stays open with the same id, marked stale, after a tick.
func TestMonitorTick_StaleHoldsAnUnmeasuredXIDFinding(t *testing.T) {
	pool, ctx := livePool(t)
	resetXID(t, ctx, pool)
	seedXIDTrend(t, ctx, pool, 7*time.Hour)
	id := openXIDFinding(t, ctx, pool)
	m := newTestMonitor(t, pool, liveRunner(pool), nil)
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got, _, ok := openFinding(t, ctx, pool, CategoryWraparound, "xid")
	if !ok || got != id {
		t.Fatalf("xid finding open=%v id %d, want %d still open", ok, got, id)
	}
	stale, reason, since := staleDetail(t, ctx, pool, CategoryWraparound, "xid")
	if !stale || reason != HoldUnmeasured || !since {
		t.Fatalf("detail stale=%v reason %q since=%v, want stale %q with stale_since",
			stale, reason, since, HoldUnmeasured)
	}
}

// 210 wal_slot series cut the trends read at its 200-row cap before xid
// (it sorts last): the XID finding is not resolved and not guessed stale.
func TestMonitorTick_TruncatedTrendsKeepTheXIDFinding(t *testing.T) {
	pool, ctx := livePool(t)
	resetXID(t, ctx, pool)
	prefix := fmt.Sprintf("trunc_%d_", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.runway_samples
			WHERE kind = 'wal_slot' AND subject LIKE $1 || '%'`, prefix)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value)
		SELECT 'wal_slot', $1 || lpad(i::text, 3, '0'), 'e', now(), 1
		FROM generate_series(1, 210) i`, prefix); err != nil {
		t.Fatalf("seed slots: %v", err)
	}
	id := openXIDFinding(t, ctx, pool)
	m := newTestMonitor(t, pool, liveRunner(pool), nil)
	res, err := m.Tick(ctx)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got, _, ok := openFinding(t, ctx, pool, CategoryWraparound, "xid"); !ok ||
		got != id {
		t.Fatalf("xid finding open=%v id %d after a truncated read (resolved %v)", ok,
			got, res.Resolved)
	}
	for _, cat := range res.Resolved {
		if cat == CategoryWraparound || cat == CategoryWAL {
			t.Fatalf("a truncated read resolved %s (resolved %v)", cat, res.Resolved)
		}
	}
	if stale, reason, _ := staleDetail(t, ctx, pool, CategoryWraparound,
		"xid"); stale {
		t.Fatalf("a series cut by the row cap was marked stale (%q)", reason)
	}
}

// State transition logging: becoming stale and being measured again are
// each logged once, however many ticks the state lasts.
func TestMonitorTick_StaleTransitionsLogOnce(t *testing.T) {
	pool, ctx := livePool(t)
	resetXID(t, ctx, pool)
	seedXIDTrend(t, ctx, pool, 7*time.Hour)
	openXIDFinding(t, ctx, pool)
	m := newTestMonitor(t, pool, pastGateXID(liveRunner(pool)), nil)
	var mu sync.Mutex
	var logs []string
	m.logFn = func(_, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(msg, args...))
	}
	count := func(sub string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range logs {
			if strings.Contains(l, sub) && strings.Contains(l, " xid ") {
				n++
			}
		}
		return n
	}
	for i := 0; i < 3; i++ {
		mustTick(t, ctx, m)
	}
	if n := count("held open, marked stale"); n != 1 {
		t.Fatalf("%d stale logs over three held ticks, want 1 (%v)", n, logs)
	}
	seedXIDTrend(t, ctx, pool, 0)
	for i := 0; i < 2; i++ {
		mustTick(t, ctx, m)
	}
	if n := count("no longer held stale"); n != 1 {
		t.Fatalf("%d release logs over two measured ticks, want 1 (%v)", n, logs)
	}
}
