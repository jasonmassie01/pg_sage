package drill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// drill-01a integration: a restore drill against a fake provider that
// hands out a database on the second test cluster, seeded with the
// source's fixture. The control database (receipts, results, findings) is
// the package fixture database on the designated server.
// No concurrent access tests here: one drill runs per database at a time
// (scheduler_test.go covers the scheduler's concurrency).

const testDeployment = "00000000-0000-4000-8000-00000000d211"

// fixtureSQL seeds three tables with deterministic rows, all created
// before the recovery point; customers is the smallest.
const fixtureSQL = `
CREATE SCHEMA app;
CREATE TABLE app.orders (id int PRIMARY KEY, customer int NOT NULL,
    total numeric(10,2) NOT NULL, created_at timestamptz NOT NULL);
INSERT INTO app.orders SELECT g, g % 200, g * 1.25,
    timestamptz '2026-01-01 00:00:00+00' + g * interval '1 minute'
    FROM generate_series(1, 3000) g;
CREATE TABLE app.customers (id int PRIMARY KEY, name text NOT NULL,
    created_at timestamptz NOT NULL);
INSERT INTO app.customers SELECT g, 'customer ' || g,
    timestamptz '2025-06-01 00:00:00+00' + g * interval '1 hour'
    FROM generate_series(1, 200) g;
CREATE TABLE app.events (id bigint PRIMARY KEY, kind text NOT NULL);
INSERT INTO app.events SELECT g, 'k' || (g % 7) FROM generate_series(1, 1000) g;
`

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", firstLine(sql), err)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// controlPool is the bootstrapped package fixture database.
func controlPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openPool(t, testdb.SkipUnlessLive(t))
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	clean := func() {
		for _, sql := range []string{"DELETE FROM sage.restore_drills",
			"DELETE FROM sage.clone_instances",
			"DELETE FROM sage.findings WHERE category IN ('restore_drill_failed', " +
				"'recoverability_unknown')"} {
			_, _ = pool.Exec(context.Background(), sql)
		}
	}
	clean()
	t.Cleanup(clean)
	return pool
}

// drillRig is one source database, one clone database on the second
// cluster and the control database.
type drillRig struct {
	control, source, clone *pgxpool.Pool
	cloneDSN               string
	adapter                *fakeAdapter
	recoveryPoint          time.Time
	logs                   []string
	mu                     sync.Mutex
}

// newRig seeds the source and the clone with seed (empty: no tables) and,
// with sage, gives both a sage.snapshots marker; the clone's newest marker
// is markerGap before the recovery point.
func newRig(t *testing.T, seed string, sage bool, markerGap time.Duration) *drillRig {
	t.Helper()
	control := controlPool(t)
	cloneDSN := testdb.RestoreTargetDatabase(t, "drill_clone")
	sourceDSN := testdb.CreateDatabase(t, "drill_source")
	r := &drillRig{control: control, source: openPool(t, sourceDSN),
		clone: openPool(t, cloneDSN), cloneDSN: cloneDSN,
		recoveryPoint: time.Now().UTC().Truncate(time.Second)}
	for _, pool := range []*pgxpool.Pool{r.source, r.clone} {
		if seed != "" {
			mustExec(t, pool, seed)
			mustExec(t, pool, "ANALYZE")
		}
		if sage {
			mustExec(t, pool, `CREATE SCHEMA IF NOT EXISTS sage;
				CREATE TABLE sage.snapshots (id bigserial PRIMARY KEY,
				    collected_at timestamptz NOT NULL)`)
		}
	}
	if sage {
		mustExec(t, r.source, `INSERT INTO sage.snapshots (collected_at) VALUES (now())`)
		mustExec(t, r.clone, `INSERT INTO sage.snapshots (collected_at) VALUES ($1), ($2)`,
			r.recoveryPoint.Add(-markerGap), r.recoveryPoint.Add(-markerGap-time.Hour))
	}
	r.adapter = &fakeAdapter{dsn: cloneDSN, from: r.recoveryPoint}
	return r
}

func (r *drillRig) provider(t *testing.T, scope string) *clone.ReceiptingProvider {
	t.Helper()
	p, err := NewReceiptedProvider(r.adapter, clone.NewReceiptStore(r.control),
		ProviderConfig{Adapter: "neon_branch", Scope: scope,
			Deployment: func(context.Context) (string, error) { return testDeployment, nil },
			Budget:     Budget{MaxDuration: 10 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (r *drillRig) drill(t *testing.T, scope string) Drill {
	t.Helper()
	return Drill{Database: "orders", Source: r.source, Provider: r.provider(t, scope),
		ProviderName: "neon_branch", Kind: clone.KindBranch,
		CostLabel: "unpriced: up to 1.17 instance-hours (branch)",
		Budget:    Budget{MaxDuration: 10 * time.Minute, StatementTimeout: 20 * time.Second},
		Store:     NewStore(r.control),
		Logf: func(format string, args ...any) {
			r.mu.Lock()
			r.logs = append(r.logs, fmt.Sprintf(format, args...))
			r.mu.Unlock()
		}}
}

func (r *drillRig) receipts(t *testing.T) map[string]int {
	t.Helper()
	rows, err := r.control.Query(context.Background(), `SELECT status, count(*)
		FROM sage.clone_instances WHERE purpose = 'drill' GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatal(err)
		}
		out[status] = n
	}
	return out
}

type storedRow struct {
	passed           bool
	rto, rpo         *float64
	failure, cost    string
	receipt          *int64
	provider, kind   string
	invariants       []Invariant
	readyAt, finishT *time.Time
}

func onlyRow(t *testing.T, pool *pgxpool.Pool) storedRow {
	t.Helper()
	var r storedRow
	var raw []byte
	var n int
	err := pool.QueryRow(context.Background(), `SELECT count(*) OVER (), passed,
		rto_seconds, rpo_seconds, COALESCE(failure, ''), COALESCE(cost_label, ''),
		receipt_id, provider, kind, invariants, ready_at, finished_at
		FROM sage.restore_drills ORDER BY id DESC LIMIT 1`).Scan(&n, &r.passed, &r.rto,
		&r.rpo, &r.failure, &r.cost, &r.receipt, &r.provider, &r.kind, &raw, &r.readyAt,
		&r.finishT)
	if err != nil {
		t.Fatalf("restore_drills row: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d restore_drills rows, want 1", n)
	}
	if err := json.Unmarshal(raw, &r.invariants); err != nil {
		t.Fatalf("invariants %s: %v", raw, err)
	}
	return r
}

func statusesOf(invs []Invariant, name string) map[string]string {
	out := map[string]string{}
	for _, inv := range invs {
		if inv.Name == name {
			out[inv.Table] = inv.Status
		}
	}
	return out
}

func TestRunDB_HappyPathPassesAndDestroysTheClone(t *testing.T) {
	r := newRig(t, fixtureSQL, true, 90*time.Second)
	res, err := r.drill(t, "happy").Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (failure %q)", err, res.Failure)
	}
	if !res.Passed || res.Failure != "" {
		t.Fatalf("drill failed: %q %+v", res.Failure, res.Invariants)
	}
	row := onlyRow(t, r.control)
	if !row.passed || row.rto == nil || *row.rto <= 0 {
		t.Fatalf("stored row = %+v, want a pass with rto > 0", row)
	}
	if row.rpo == nil || *row.rpo < 89 || *row.rpo > 91 {
		t.Fatalf("rpo = %v, want the 90 s marker gap", row.rpo)
	}
	if row.provider != "neon_branch" || row.kind != "branch" || row.receipt == nil ||
		!strings.HasPrefix(row.cost, "unpriced") || row.readyAt == nil {
		t.Fatalf("stored row = %+v", row)
	}
	for _, name := range []string{"catalog", "readable", "row_estimate"} {
		got := statusesOf(row.invariants, name)
		if len(got) != 3 || got["app.orders"] != StatusPass {
			t.Fatalf("%s = %v, want a pass on each of the 3 tables", name, got)
		}
	}
	if got := statusesOf(row.invariants, "row_count"); len(got) != 3 ||
		got["app.customers"] != StatusPass {
		t.Fatalf("row_count = %v, want the 3 smallest counted", got)
	}
	sums := statusesOf(row.invariants, "sample_checksum")
	if sums["app.orders"] != StatusPass || sums["app.events"] != StatusUnknown {
		t.Fatalf("sample_checksum = %v, want orders checked and events unknown", sums)
	}
	if created, destroyed := r.adapter.counts(); created != 1 || destroyed != 1 {
		t.Fatalf("created %d destroyed %d, want 1 and 1", created, destroyed)
	}
	if got := r.receipts(t); got[clone.StatusDestroyed] != 1 || len(got) != 1 {
		t.Fatalf("receipts = %v, want one destroyed drill receipt", got)
	}
	last, err := NewStore(r.control).LastPassed(context.Background(), "orders")
	if err != nil || last.IsZero() || !last.Equal(*row.finishT) {
		t.Fatalf("LastPassed = %v %v, want the row's finished_at %v", last, err, row.finishT)
	}
}

func TestRunDB_CorruptedBackupFailsLoudly(t *testing.T) {
	r := newRig(t, fixtureSQL, true, 72*time.Hour)
	mustExec(t, r.clone, `DELETE FROM app.customers WHERE id % 10 = 0; ANALYZE app.customers`)
	res, err := r.drill(t, "corrupt").Run(context.Background())
	if err != nil {
		t.Fatalf("a completed drill returns no error: %v", err)
	}
	if res.Passed {
		t.Fatal("a copy missing 10% of a table passed")
	}
	if got := statusesOf(res.Invariants, "row_count"); got["app.customers"] != StatusFail {
		t.Fatalf("row_count = %v, want customers failed", got)
	}
	if res.RPOSeconds == nil || *res.RPOSeconds < 72*3600-1 || *res.RPOSeconds > 72*3600+1 {
		t.Fatalf("rpo = %v, want 3 days", res.RPOSeconds)
	}
	store := notify.NewMemoryStore()
	pager := notify.NewDefaultDispatcher(store, notify.TargetPolicy{AllowPrivate: true}, nil)
	pager.UseOutbox(store)
	id := store.AddChannel(notify.Channel{Name: "pager", Type: "slack",
		Config: map[string]string{"webhook_url": "http://127.0.0.1:9/hook"}, Enabled: true})
	store.AddRule(notify.Rule{ChannelID: id, Event: "finding_opened",
		MinSeverity: "critical", Enabled: true})
	if err := ReportResult(context.Background(), r.control, pager, res); err != nil {
		t.Fatal(err)
	}
	var severity, status string
	if err := r.control.QueryRow(context.Background(), `SELECT severity, status
		FROM sage.findings WHERE category = 'restore_drill_failed'
		AND object_identifier = 'restore_drill:orders'`).Scan(&severity, &status); err != nil {
		t.Fatalf("finding: %v", err)
	}
	if severity != "critical" || status != "open" {
		t.Fatalf("finding = %s %s, want an open critical", severity, status)
	}
	outbox := store.Outbox()
	if len(outbox) != 1 {
		t.Fatalf("%d outbox entries, want 1", len(outbox))
	}
	var evt notify.Event
	if err := json.Unmarshal(outbox[0].Payload, &evt); err != nil {
		t.Fatal(err)
	}
	if evt.Severity != "critical" || evt.Type != "finding_opened" {
		t.Fatalf("outbox event = %s %s, want a critical finding_opened", evt.Type, evt.Severity)
	}
}

func TestRunDB_TheSourceItselfIsRefused(t *testing.T) {
	r := newRig(t, fixtureSQL, false, 0)
	r.adapter.dsn = r.source.Config().ConnString()
	res, err := r.drill(t, "self").Run(context.Background())
	if !errors.Is(err, clone.ErrTargetIsProduction) {
		t.Fatalf("err = %v, want ErrTargetIsProduction", err)
	}
	if res.Passed || !strings.Contains(res.Failure, "target_is_production") {
		t.Fatalf("failure = %q, want target_is_production", res.Failure)
	}
	if len(res.Invariants) != 0 {
		t.Fatalf("invariants ran on the source: %+v", res.Invariants)
	}
	if row := onlyRow(t, r.control); row.passed || row.readyAt == nil {
		t.Fatalf("stored row = %+v, want a failed row after the clone was ready", row)
	}
	if _, destroyed := r.adapter.counts(); destroyed != 1 {
		t.Fatal("the refused clone was not destroyed")
	}
}

func TestRunDB_NoTablesPassesWithRPOUnknown(t *testing.T) {
	r := newRig(t, "", false, 0)
	res, err := r.drill(t, "empty").Run(context.Background())
	if err != nil || !res.Passed {
		t.Fatalf("empty source = %v %q, want a pass", err, res.Failure)
	}
	if got := statusesOf(res.Invariants, "catalog"); got[""] != StatusPass {
		t.Fatalf("catalog = %v, want a pass", got)
	}
	evidence := ""
	for _, inv := range res.Invariants {
		if inv.Name == "catalog" {
			evidence = inv.Evidence
		}
	}
	if evidence != "no tables" {
		t.Fatalf("catalog evidence = %q, want no tables", evidence)
	}
	if res.RPOSeconds != nil || res.RPOReason == "" {
		t.Fatalf("rpo = %v (%q), want unknown with a reason", res.RPOSeconds, res.RPOReason)
	}
	if got := statusesOf(res.Invariants, "marker"); got[""] != StatusUnknown {
		t.Fatalf("marker = %v, want unknown", got)
	}
}

func TestRunDB_OperatorMarkerQueryMeasuresRPO(t *testing.T) {
	r := newRig(t, fixtureSQL, false, 0)
	mustExec(t, r.clone, `CREATE TABLE app.heartbeat (at timestamptz NOT NULL)`)
	mustExec(t, r.clone, `INSERT INTO app.heartbeat VALUES ($1)`,
		r.recoveryPoint.Add(-5*time.Minute))
	d := r.drill(t, "marker")
	d.Invariants.MarkerQuery = "SELECT max(at) FROM app.heartbeat"
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.RPOSeconds == nil || *res.RPOSeconds < 299 || *res.RPOSeconds > 301 {
		t.Fatalf("rpo = %v, want 300 s", res.RPOSeconds)
	}
}

func TestRunDB_CreateErrorNamesTheProviderAndStep(t *testing.T) {
	r := newRig(t, "", false, 0)
	r.adapter.createErr = errors.New("429 Too Many Requests")
	res, err := r.drill(t, "create-err").Run(context.Background())
	if err == nil {
		t.Fatal("a failed create returned no error")
	}
	want := "create drill clone: neon_branch: 429 Too Many Requests"
	if !strings.Contains(res.Failure, want) {
		t.Fatalf("failure = %q, want %q", res.Failure, want)
	}
	if row := onlyRow(t, r.control); row.passed || !strings.Contains(row.failure, want) ||
		row.readyAt != nil {
		t.Fatalf("stored row = %+v, want a failed row without ready_at", row)
	}
}

func TestRunDB_DestroyErrorLeavesTheReceiptForTheReaper(t *testing.T) {
	r := newRig(t, "", false, 0)
	r.adapter.destroyErr = errors.New("neon: 503")
	res, err := r.drill(t, "destroy-err").Run(context.Background())
	if err != nil || !res.Passed {
		t.Fatalf("the restore itself passed: %v %q", err, res.Failure)
	}
	if got := r.receipts(t); got[clone.StatusDestroying] != 1 {
		t.Fatalf("receipts = %v, want the receipt left destroying", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	want := fmt.Sprintf("receipt %d", res.ReceiptID)
	logged := ""
	for _, line := range r.logs {
		if strings.Contains(line, "not destroyed") {
			logged = line
		}
	}
	if res.ReceiptID == 0 || !strings.Contains(logged, want) ||
		!strings.Contains(logged, "neon: 503") {
		t.Fatalf("logs = %v, want the receipt id %d and the error", r.logs, res.ReceiptID)
	}
}

func TestRunDB_ClosedControlDatabaseStillDestroys(t *testing.T) {
	r := newRig(t, "", false, 0)
	d := r.drill(t, "closed")
	closed, err := pgxpool.New(context.Background(), r.control.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	d.Store = NewStore(closed)
	_, err = d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "record restore drill") {
		t.Fatalf("err = %v, want record restore drill", err)
	}
	if _, destroyed := r.adapter.counts(); destroyed != 1 {
		t.Fatal("the clone was not destroyed")
	}
}

func TestRunDB_TheSharedCreateLimitDefersWithoutARow(t *testing.T) {
	r := newRig(t, "", false, 0)
	p, err := NewReceiptedProvider(r.adapter, clone.NewReceiptStore(r.control),
		ProviderConfig{Adapter: "neon_branch", Scope: "limited",
			Deployment:        func(context.Context) (string, error) { return testDeployment, nil },
			MaxCreatesPerHour: 1})
	if err != nil {
		t.Fatal(err)
	}
	d := r.drill(t, "limited")
	d.Provider = p
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("first drill: %v", err)
	}
	res, err := d.Run(context.Background())
	if !errors.Is(err, ErrDeferred) || !errors.Is(err, clone.ErrCreateRateLimited) {
		t.Fatalf("second drill = %v, want deferred by the create limit", err)
	}
	if res.Passed {
		t.Fatal("a deferred drill passed")
	}
	var rows int
	if err := r.control.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.restore_drills`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d rows (%v), want only the first drill's", rows, err)
	}
}
