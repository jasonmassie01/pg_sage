package managedparam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeResolver struct {
	mu     sync.Mutex
	target Target
	err    error
	calls  int
}

func (f *fakeResolver) Target(context.Context) (Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.target, f.err
}

func insertIntentFinding(t *testing.T, pool *pgxpool.Pool, ident string, in Intent) int64 {
	t.Helper()
	detail, err := json.Marshal(map[string]any{DetailKey: in.Detail(),
		"approval_required": "restart"})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail)
		VALUES ('mp_test_memory', 'info', 'configuration', $1, $2, $3) RETURNING id`,
		ident, "Set "+in.Parameter, detail).Scan(&id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	return id
}

func runningSetting(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(context.Background(),
		"SELECT setting FROM pg_settings WHERE name = $1", name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func newTestWorker(t *testing.T, pool *pgxpool.Pool, r Resolver) *Worker {
	t.Helper()
	w, err := NewWorker(WorkerOptions{Database: "orders", Provider: "rds", Pool: pool,
		Store: NewStore(pool), Resolver: r})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

func TestWorkerCycleProposesFromFindings(t *testing.T) {
	pool, ctx := testPool(t)
	running := runningSetting(t, pool, "random_page_cost")
	maxConns := runningSetting(t, pool, "max_connections")
	target := rdsTarget()
	delete(target.Params, "max_wal_size") // only max_connections drifts
	target.Params["max_connections"] = GroupParam{Value: "9999", Source: "user",
		ApplyType: "static", Modifiable: true}
	r := &fakeResolver{target: target}
	w := newTestWorker(t, pool, r)
	sb := insertIntentFinding(t, pool, "instance:shared_buffers",
		intent(t, "rds", "shared_buffers", "2GB"))
	insertIntentFinding(t, pool, "instance:random_page_cost",
		intent(t, "rds", "random_page_cost", running))
	res, err := w.Cycle(ctx)
	if err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	if res.Proposed != 2 || res.Applied != 1 || res.TargetError != "" {
		t.Fatalf("cycle = %+v, want 2 proposed and the running one applied", res)
	}
	assertCycleRecords(t, w, sb)
	drift := w.Drift()
	if len(drift) != 1 || drift[0].Parameter != "max_connections" ||
		drift[0].Configured != "9999" || drift[0].Running != maxConns {
		t.Fatalf("drift = %+v", drift)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved'
		WHERE id = $1`, sb); err != nil {
		t.Fatal(err)
	}
	res, err = w.Cycle(ctx)
	if err != nil || res.Superseded != 1 {
		t.Fatalf("resolved finding must supersede its proposal: %+v %v", res, err)
	}
}

func assertCycleRecords(t *testing.T, w *Worker, sharedBuffersFinding int64) {
	t.Helper()
	recs, err := w.store.List(context.Background(), nil, 10)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records = %d, %v", len(recs), err)
	}
	for _, rec := range recs {
		switch rec.Proposal.Parameter {
		case "shared_buffers":
			if rec.Status != StatusPending || rec.FindingID == nil ||
				*rec.FindingID != sharedBuffersFinding ||
				rec.Proposal.Target != "orders-pg16" || !rec.Proposal.RebootRequired {
				t.Fatalf("shared_buffers = %+v", rec)
			}
		case "random_page_cost":
			if rec.Status != StatusApplied {
				t.Fatalf("a value already running is observed applied: %+v", rec)
			}
		}
	}
}

// Without telemetry the worker still proposes (placeholders, notes) and
// reports why the target is unknown; it never fails the cycle.
func TestWorkerWithoutResolverOrWithFailingResolver(t *testing.T) {
	for name, r := range map[string]Resolver{"nil": nil,
		"failing": &fakeResolver{err: fmt.Errorf("unavailable: no credentials")}} {
		t.Run(name, func(t *testing.T) {
			pool, ctx := testPool(t)
			w := newTestWorker(t, pool, r)
			insertIntentFinding(t, pool, "instance:work_mem",
				intent(t, "rds", "work_mem", "64MB"))
			res, err := w.Cycle(ctx)
			if err != nil || res.Proposed != 1 || res.TargetError == "" {
				t.Fatalf("cycle = %+v, %v", res, err)
			}
			recs, _ := w.store.List(ctx, nil, 10)
			if len(recs) != 1 || !strings.Contains(recs[0].Proposal.CLI,
				"<db-parameter-group>") {
				t.Fatalf("records = %+v", recs)
			}
			if len(w.Drift()) != 0 {
				t.Fatal("no drift without a resolved target")
			}
		})
	}
}

func TestWorkerSkipsIntentsOfOtherProviders(t *testing.T) {
	pool, ctx := testPool(t)
	w := newTestWorker(t, pool, nil)
	insertIntentFinding(t, pool, "instance:work_mem",
		intent(t, "cloud-sql", "work_mem", "64MB"))
	res, err := w.Cycle(ctx)
	if err != nil || res.Proposed != 0 || res.Skipped != 1 {
		t.Fatalf("cycle = %+v, %v", res, err)
	}
}

func TestNewWorkerValidates(t *testing.T) {
	pool, _ := testPool(t)
	cases := []WorkerOptions{
		{Provider: "rds", Pool: pool, Store: NewStore(pool)},
		{Database: "x", Provider: "neon", Pool: pool, Store: NewStore(pool)},
		{Database: "x", Provider: "rds", Store: NewStore(pool)},
		{Database: "x", Provider: "rds", Pool: pool},
	}
	for i, opts := range cases {
		if _, err := NewWorker(opts); err == nil {
			t.Errorf("case %d accepted: %+v", i, opts)
		}
	}
}

func TestWorkerCycleCancelled(t *testing.T) {
	pool, _ := testPool(t)
	w := newTestWorker(t, pool, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Cycle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
