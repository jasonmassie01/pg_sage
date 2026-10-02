package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Stored PGIncidentBench and game-day reports (sage.sre_eval_runs, M7
// promotion evidence) age out after sre.autonomy.report_retention_days,
// except a report that is the evidence of a current ledger level or of a
// pending promotion, and the newest report of each family (per source,
// and per database for game days) — what LatestBench and the evidence
// window read.

func retentionCfg(days int) *config.Config {
	return &config.Config{SRE: config.SREConfig{
		Autonomy: config.SREAutonomyConfig{ReportRetentionDays: days}}}
}

func newDeployment(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var dep string
	if err := pool.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&dep); err != nil {
		t.Fatalf("deployment id: %v", err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"sre_eval_runs", "sre_family_autonomy",
			"sre_autonomy_proposals"} {
			_, _ = pool.Exec(context.Background(),
				"DELETE FROM sage."+table+" WHERE deployment_id = $1::uuid", dep)
		}
	})
	return dep
}

// insertRun stores a report of source scoring families, ingested (and
// generated) ago before now; db is set for game days.
func insertRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, dep, source, db string,
	ago time.Duration, families ...string) string {
	t.Helper()
	var cells []map[string]any
	for _, f := range families {
		cells = append(cells, map[string]any{"arm": "graph", "family": f, "runs": 10,
			"top1": map[string]int{"k": 9, "n": 10}, "safe_pass": map[string]int{"k": 10,
				"n": 10}, "mechanism_precision": 0.95, "forbidden_actions": 0})
	}
	raw, err := json.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	var dbArg any
	if db != "" {
		dbArg = db
	}
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.sre_eval_runs (deployment_id, id,
		source, schema_version, generated_at, ingested_at, ingested_by, database_name,
		report_sha256, gated_arms, cells)
		VALUES ($1::uuid, gen_random_uuid(), $2, 'pgincidentbench/v1',
		        now() - $3::interval, now() - $3::interval, 'test', $4,
		        decode(md5(random()::text) || md5(random()::text), 'hex'),
		        '["graph"]'::jsonb, $5::jsonb)
		RETURNING id::text`, dep, source, fmt.Sprintf("%d seconds", int(ago.Seconds())),
		dbArg, string(raw)).Scan(&id); err != nil {
		t.Fatalf("insert eval run: %v", err)
	}
	return id
}

func runExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.sre_eval_runs
		WHERE id = $1::uuid`, id).Scan(&n); err != nil {
		t.Fatalf("count eval run: %v", err)
	}
	return n == 1
}

const day = 24 * time.Hour

func TestRun_AgesOutEvalRunsButKeepsEvidenceAndNewestPerFamily(t *testing.T) {
	pool, ctx := requireDB(t)
	dep, other := newDeployment(t, ctx, pool), newDeployment(t, ctx, pool)
	supersededCkpt := insertRun(t, ctx, pool, dep, "bench", "", 200*day, "checkpoint_storm")
	midCkpt := insertRun(t, ctx, pool, dep, "bench", "", 150*day, "checkpoint_storm")
	lwlockOnly := insertRun(t, ctx, pool, dep, "bench", "", 200*day, "temp_file_explosion",
		"lwlock_contention")
	newerTemp := insertRun(t, ctx, pool, dep, "bench", "", 100*day, "temp_file_explosion")
	ledgerBench := insertRun(t, ctx, pool, dep, "bench", "", 300*day, "wal_retention")
	insertRun(t, ctx, pool, dep, "bench", "", 120*day, "wal_retention")
	ledgerGameDay := insertRun(t, ctx, pool, dep, "game_day", "db1", 250*day, "lock_blocking")
	pendingBench := insertRun(t, ctx, pool, dep, "bench", "", 400*day, "plan_regression")
	insertRun(t, ctx, pool, dep, "bench", "", 110*day, "plan_regression")
	oldGameDay := insertRun(t, ctx, pool, dep, "game_day", "db1", 200*day, "lock_blocking")
	insertRun(t, ctx, pool, dep, "game_day", "db1", 100*day, "lock_blocking")
	otherDB := insertRun(t, ctx, pool, dep, "game_day", "db2", 200*day, "lock_blocking")
	recent := insertRun(t, ctx, pool, dep, "bench", "", 10*day, "checkpoint_storm")
	newestCkpt := insertRun(t, ctx, pool, dep, "bench", "", 5*day, "checkpoint_storm")
	// Another deployment's newer report does not supersede this one's.
	insertRun(t, ctx, pool, other, "bench", "", day, "connection_pressure")
	ownConn := insertRun(t, ctx, pool, dep, "bench", "", 200*day, "connection_pressure")

	execRetry(t, ctx, `INSERT INTO sage.sre_family_autonomy (deployment_id, family,
		action_class, level, evidence, changed_by, change_reason)
		VALUES ($1::uuid, 'wal_retention', 'wal_bound', 2,
		        jsonb_build_object('evidence', jsonb_build_object(
		            'bench', jsonb_build_object('id', $2::text),
		            'game_days', jsonb_build_array(jsonb_build_object('id', $3::text)))),
		        'user:1:a@b', 'promotion approved')`, dep, ledgerBench, ledgerGameDay)
	execRetry(t, ctx, `INSERT INTO sage.sre_autonomy_proposals (deployment_id, id, family,
		action_class, from_level, to_level, evidence, evidence_sha256, status, expires_at)
		VALUES ($1::uuid, gen_random_uuid(), 'plan_regression', 'query_hint', 1, 2,
		        jsonb_build_object('evidence', jsonb_build_object('bench',
		            jsonb_build_object('id', $2::text))),
		        decode(md5('p') || md5('q'), 'hex'), 'pending', now() + interval '1 day')`,
		dep, pendingBench)

	New(pool, retentionCfg(90), noopLog).Run(ctx)

	for name, c := range map[string]struct {
		id   string
		kept bool
	}{
		"superseded checkpoint report": {supersededCkpt, false},
		"older checkpoint report":      {midCkpt, false},
		"newest checkpoint report":     {newestCkpt, true},
		"newest lwlock report":         {lwlockOnly, true},
		"newest temp report":           {newerTemp, true},
		"ledger bench evidence":        {ledgerBench, true},
		"ledger game-day evidence":     {ledgerGameDay, true},
		"pending promotion evidence":   {pendingBench, true},
		"superseded game day":          {oldGameDay, false},
		"newest game day of db2":       {otherDB, true},
		"report inside the window":     {recent, true},
		"newest of its deployment":     {ownConn, true},
	} {
		if got := runExists(t, ctx, pool, c.id); got != c.kept {
			t.Errorf("%s: kept %v, want %v", name, got, c.kept)
		}
	}
}

func TestRun_EvalRunRetentionBoundary(t *testing.T) {
	pool, ctx := requireDB(t)
	dep := newDeployment(t, ctx, pool)
	inside := insertRun(t, ctx, pool, dep, "bench", "", 89*day, "checkpoint_storm")
	outside := insertRun(t, ctx, pool, dep, "bench", "", 91*day, "checkpoint_storm")
	insertRun(t, ctx, pool, dep, "bench", "", day, "checkpoint_storm")
	New(pool, retentionCfg(90), noopLog).Run(ctx)
	if !runExists(t, ctx, pool, inside) || runExists(t, ctx, pool, outside) {
		t.Fatalf("89 days kept %v (want true), 91 days kept %v (want false)",
			runExists(t, ctx, pool, inside), runExists(t, ctx, pool, outside))
	}
	old := insertRun(t, ctx, pool, dep, "bench", "", 1000*day, "checkpoint_storm")
	New(pool, retentionCfg(0), noopLog).Run(ctx)
	if !runExists(t, ctx, pool, old) {
		t.Fatal("a zero retention deleted a report")
	}
}

// In meta-database mode the ledger lives in the control database: the
// reports are pruned there, never in the monitored database.
func TestRun_EvalRunRetentionUsesTheControlPool(t *testing.T) {
	pool, ctx := requireDB(t)
	control, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "ret_control"))
	if err != nil {
		t.Fatalf("connect control: %v", err)
	}
	t.Cleanup(control.Close)
	if err := schema.Bootstrap(ctx, control); err != nil {
		t.Fatalf("bootstrap control: %v", err)
	}
	ctlDep, monDep := newDeployment(t, ctx, control), newDeployment(t, ctx, pool)
	ctlOld := insertRun(t, ctx, control, ctlDep, "bench", "", 200*day, "checkpoint_storm")
	insertRun(t, ctx, control, ctlDep, "bench", "", day, "checkpoint_storm")
	monOld := insertRun(t, ctx, pool, monDep, "bench", "", 200*day, "checkpoint_storm")
	insertRun(t, ctx, pool, monDep, "bench", "", day, "checkpoint_storm")

	New(pool, retentionCfg(90), noopLog).WithControlPool(control).Run(ctx)
	if runExists(t, ctx, control, ctlOld) {
		t.Error("the control database kept a superseded report")
	}
	if !runExists(t, ctx, pool, monOld) {
		t.Error("the monitored database's reports were pruned while a control pool is set")
	}
	New(pool, retentionCfg(90), noopLog).WithControlPool(nil).Run(ctx)
	if runExists(t, ctx, pool, monOld) {
		t.Error("without a control pool the monitored database kept a superseded report")
	}
}

func TestRetentionRules_EvalRunsAreAgedOut(t *testing.T) {
	var rule *purgeRule
	for _, r := range purgeRules(retentionCfg(45)) {
		if r.table == "sre_eval_runs" {
			r := r
			rule = &r
		}
	}
	if rule == nil || rule.days != 45 || rule.timeCol != "ingested_at" || rule.extra == "" {
		t.Fatalf("sre_eval_runs rule = %+v, want ingested_at, 45 days and keep predicates",
			rule)
	}
	if reason, exempt := retentionExemptions["sre_eval_runs"]; exempt {
		t.Fatalf("sre_eval_runs has a purge rule and an exemption (%q)", reason)
	}
}

// In meta-database mode every database runtime's cleaner prunes the same
// control database: concurrent passes neither fail nor over-delete.
func TestRun_EvalRunRetentionConcurrentCleaners(t *testing.T) {
	_, ctx := requireDB(t)
	shared, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(shared.Close)
	dep := newDeployment(t, ctx, shared)
	var old []string
	for i := 0; i < 20; i++ {
		old = append(old, insertRun(t, ctx, shared, dep, "bench", "",
			time.Duration(200+i)*day, "checkpoint_storm"))
	}
	newest := insertRun(t, ctx, shared, dep, "bench", "", day, "checkpoint_storm")
	var mu sync.Mutex
	var errorsLogged []string
	logFn := func(level, msg string, args ...any) {
		if level == "ERROR" {
			mu.Lock()
			errorsLogged = append(errorsLogged, fmt.Sprintf(msg, args...))
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			New(shared, retentionCfg(90), logFn).Run(ctx)
		}()
	}
	wg.Wait()
	if len(errorsLogged) != 0 {
		t.Fatalf("concurrent cleaners logged errors: %v", errorsLogged)
	}
	for _, id := range old {
		if runExists(t, ctx, shared, id) {
			t.Errorf("superseded report %s survived", id)
		}
	}
	if !runExists(t, ctx, shared, newest) {
		t.Fatal("concurrent cleaners deleted the newest report")
	}
}
