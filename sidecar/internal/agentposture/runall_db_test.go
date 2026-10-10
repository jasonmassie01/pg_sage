package agentposture

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// sqlDetector runs one statement and reports one finding per row of
// text it returns.
type sqlDetector struct {
	spec Spec
	sql  string
}

func (d sqlDetector) Spec() Spec { return d.spec }

func (d sqlDetector) Detect(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, Statement(d.spec.ID, d.sql))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var obj string
		if err := rows.Scan(&obj); err != nil {
			return nil, err
		}
		out = append(out, Finding{Severity: d.spec.Severity, ObjectType: "test",
			Object: obj, Title: "row " + obj})
	}
	return out, rows.Err()
}

func sqlDet(id, sql string) sqlDetector {
	return sqlDetector{spec: Spec{ID: id, Title: "sql " + id, Severity: Warning}, sql: sql}
}

func TestRunAll_OneFailingDetectorDoesNotStopTheOthers(t *testing.T) {
	pool, ctx := livePool(t)
	reg := NewRegistry()
	for _, d := range []Detector{
		sqlDet("AP-01", "SELECT 'a' UNION ALL SELECT 'b'"),
		sqlDet("AP-02", "SELECT * FROM no_such_table_posture"),
		sqlDet("AP-03", "SELECT pg_sleep(2)::text"),
		sqlDet("AP-04", "SELECT 'c'"),
	} {
		if err := reg.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	res, err := RunAll(ctx, pool, RunOptions{Registry: reg, Config: DefaultConfig(),
		StatementTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	got := map[string]int{}
	for _, o := range res.Outcomes {
		got[o.Detector] = len(o.Findings)
	}
	if got["AP-01"] != 2 || got["AP-04"] != 1 || len(got) != 2 {
		t.Fatalf("outcomes = %v, want AP-01:2 and AP-04:1 only", got)
	}
	var pgErr *pgconn.PgError
	if !errors.As(res.Failed["AP-02"], &pgErr) || pgErr.Code != "42P01" {
		t.Fatalf("AP-02 failure = %v, want undefined_table", res.Failed["AP-02"])
	}
	if !errors.As(res.Failed["AP-03"], &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("AP-03 failure = %v, want a statement timeout", res.Failed["AP-03"])
	}
	if res.Env.VersionNum == 0 || len(res.Env.Exposed) == 0 {
		t.Fatalf("env not resolved: %+v", res.Env)
	}
	cats := res.EvaluatedCategories()
	if strings.Join(cats, ",") != "agent_posture:AP-01,agent_posture:AP-04" {
		t.Fatalf("evaluated = %v, want only the detectors that completed", cats)
	}
	if n := len(res.Findings()); n != 3 {
		t.Fatalf("findings = %d, want 3", n)
	}
}

func TestRunAll_IsReadOnly(t *testing.T) {
	pool, ctx := livePool(t)
	table := "posture_ro_" + suffix(t)
	execAll(t, ctx, pool, "CREATE TABLE "+table+" (id int)")
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+table) })
	reg := NewRegistry()
	_ = reg.Register(sqlDet("AP-01", "INSERT INTO "+table+" VALUES (1) RETURNING 'x'"))
	res, err := RunAll(ctx, pool, RunOptions{Registry: reg, Config: DefaultConfig()})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(res.Failed["AP-01"], &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write failure = %v, want read_only_sql_transaction", res.Failed["AP-01"])
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d (%v): a detector wrote", n, err)
	}
}

func TestRunAll_Errors(t *testing.T) {
	if _, err := RunAll(context.Background(), nil, RunOptions{}); !errors.Is(err, ErrNoPool) {
		t.Fatalf("nil pool: %v", err)
	}
	pool, ctx := livePool(t)
	bad := DefaultConfig()
	bad.ClientPatterns = []string{"x"}
	if _, err := RunAll(ctx, pool, RunOptions{Config: bad}); !errors.Is(err,
		ErrInvalidConfig) {
		t.Fatalf("invalid config: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := RunAll(cctx, pool, RunOptions{Config: DefaultConfig()}); !errors.Is(err,
		context.Canceled) {
		t.Fatalf("ended context: %v", err)
	}
}

// An empty registry still resolves the environment and evaluates nothing.
func TestRunAll_EmptyRegistry(t *testing.T) {
	pool, ctx := livePool(t)
	res, err := RunAll(ctx, pool, RunOptions{Registry: NewRegistry(),
		Config: DefaultConfig()})
	if err != nil || len(res.Outcomes) != 0 || len(res.EvaluatedCategories()) != 0 ||
		res.Env.VersionNum == 0 {
		t.Fatalf("RunAll(empty) = %+v, %v", res, err)
	}
}
