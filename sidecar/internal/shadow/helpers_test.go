package shadow

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/shadow"))
}

var (
	poolOnce sync.Once
	sharedDB *pgxpool.Pool
	poolErr  error
)

// testPool is the package's bootstrapped fixture database; every test
// starts from an empty shadow ledger and approval queue.
func testPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	poolOnce.Do(func() {
		ctx := context.Background()
		sharedDB, poolErr = pgxpool.New(ctx, dsn)
		if poolErr == nil {
			poolErr = schema.Bootstrap(ctx, sharedDB)
		}
	})
	if poolErr != nil {
		t.Fatalf("test database: %v", poolErr)
	}
	ctx := context.Background()
	clean := func() {
		for _, stmt := range []string{
			"DELETE FROM sage.shadow_decision",
			"DELETE FROM sage.action_queue",
			"DELETE FROM sage.action_outcome",
			"DELETE FROM sage.action_log",
			"DELETE FROM sage.query_store WHERE queryid BETWEEN 9100000 AND 9199999",
		} {
			if _, err := sharedDB.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	return sharedDB, ctx
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// sample is a shadow decision of class for sql, below its earned level.
func sample(class, sql string) Decision {
	family, _ := ClassOf(actionTypeFor(class), sql)
	expected := -40.0
	shape := Shape(sql)
	return Decision{Database: "orders", Fingerprint: Fingerprint(class, "public.o", shape),
		Family: family, Class: class, FindingID: 77, Title: "shadow " + class,
		Object: "public.o", SQL: sql, RollbackSQL: "", Shape: shape,
		Prediction: verify.Prediction{Class: class, Method: verify.MethodModel,
			Metric: verify.MetricMeanExecTime, ExpectedChangePct: &expected,
			TargetQueryIDs: []int64{9100001}, Source: "optimizer"},
		Evidence:    map[string]any{"finding_category": "missing_index"},
		GateVerdict: "observe_only", GateReason: "autonomy_level",
		TrustedVerdict: "execute", TrustedReason: "autonomy_l3", GrantedLevel: 1}
}

func actionTypeFor(class string) string {
	switch class {
	case "index_create":
		return "create_index_concurrently"
	case "index_drop":
		return "drop_unused_index"
	case "config_guc":
		return "alter_system_guc"
	case "vacuum":
		return "vacuum_table"
	case "analyze":
		return "analyze_table"
	}
	return ""
}

// record stores d and returns it as stored.
func record(t *testing.T, s *Store, d Decision) Decision {
	t.Helper()
	got, inserted, err := s.Record(context.Background(), d)
	if err != nil || !inserted {
		t.Fatalf("record %s: inserted=%v err=%v", d.Class, inserted, err)
	}
	return got
}

// age moves a decision's recording (and last sighting) back by d.
func age(t *testing.T, pool *pgxpool.Pool, id int64, d time.Duration) {
	t.Helper()
	mustExec(t, pool, `UPDATE sage.shadow_decision
		SET recorded_at = now() - make_interval(secs => $2),
		    last_seen_at = now() - make_interval(secs => $2)
		WHERE id = $1`, id, d.Seconds())
}

func get(t *testing.T, s *Store, id int64) Decision {
	t.Helper()
	d, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %d: %v", id, err)
	}
	return d
}

// seedSeries writes a cumulative query_store series for qid: one sample
// a minute from start, callsPerMinute calls of meanMs each, continuing
// the counters of the series' latest sample.
func seedSeries(t *testing.T, pool *pgxpool.Pool, qid int64, start time.Time, minutes int,
	callsPerMinute int64, meanMs float64) {
	t.Helper()
	ctx := context.Background()
	var calls int64
	var total float64
	_ = pool.QueryRow(ctx, `SELECT calls, total_exec_time FROM sage.query_store
		WHERE queryid = $1 ORDER BY captured_at DESC LIMIT 1`, qid).Scan(&calls, &total)
	for i := 0; i < minutes; i++ {
		calls += callsPerMinute
		total += float64(callsPerMinute) * meanMs * (1 + 0.02*float64(i%3-1))
		mustExec(t, pool, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time)
			VALUES ($1, $2, $3, $4, $5)`, start.Add(time.Duration(i)*time.Minute), qid,
			calls, total, total/float64(calls))
	}
}

// fastOptions are scorer options with windows a test can seed.
func fastOptions() Options {
	o := DefaultOptions()
	o.VerifyWindow, o.VerifyMaxWindow, o.DropWindow = 30*time.Minute, time.Hour, 30*time.Minute
	o.Database = "orders"
	return o
}

// newClosedPool is a pool to a real server that has been closed.
func newClosedPool(cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	p.Close()
	return p, nil
}

// decisionCount is the shadow decision counter of one label set.
func decisionCount(database, class, verdict string) uint64 {
	for _, c := range DecisionCounts() {
		if c.Database == database && c.Class == class && c.Verdict == verdict {
			return c.Count
		}
	}
	return 0
}

// scoreCount is the shadow score counter of one label set.
func scoreCount(database, class, score, source string) uint64 {
	for _, c := range ScoreCounts() {
		if c.Database == database && c.Class == class && c.Score == score &&
			c.Source == source {
			return c.Count
		}
	}
	return 0
}
