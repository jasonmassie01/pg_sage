package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Live PostgreSQL tests of the replace action: success, a failed create
// (nothing dropped), a failed drop (partial: the new index stays), the
// table lease held across both steps, the refusals (constraint-backed old
// index, a changed old index, binding facts) and the approval paths.

type replaceFixture struct {
	t         *testing.T
	pool      *pgxpool.Pool
	ctx       context.Context
	exec      *Executor
	table     string // public.<name>
	bare      string
	oldIndex  string // public.<name>_a
	newIndex  string // public.<name>_a_b
	oldDef    string
	oldOID    int64
	findingID int
	sql       string
	rollback  string
	userID    int
}

// newReplaceFixture builds public.<t>(a, b, c) with rows and the index
// (a), and an open finding that replaces it with (a, <newKeys>).
func newReplaceFixture(t *testing.T, newKeys string) *replaceFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	bare := fmt.Sprintf("rplc_%d", time.Now().UnixNano())
	f := &replaceFixture{t: t, pool: pool, ctx: ctx, bare: bare, table: "public." + bare,
		oldIndex: "public." + bare + "_a", newIndex: "public." + bare + "_a_b", userID: 9}
	f.must(t, "CREATE TABLE "+f.table+" (id int PRIMARY KEY, a int NOT NULL, "+
		"b int NOT NULL, c int, r int4range)")
	f.must(t, "INSERT INTO "+f.table+" SELECT g, g % 50, g % 7 + 1, g, "+
		"int4range(g, g + 1) FROM generate_series(1, 2000) g")
	f.must(t, "CREATE INDEX "+bare+"_a ON "+f.table+" (a)")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.index_replace WHERE table_name=$1", f.table)
		_, _ = pool.Exec(bg, "DELETE FROM sage.action_log WHERE finding_id=$1", f.findingID)
		_, _ = pool.Exec(bg, "DELETE FROM sage.findings WHERE id=$1", f.findingID)
		_, _ = pool.Exec(bg, "DROP TABLE IF EXISTS "+f.table+" CASCADE")
	})
	f.oldDef, f.oldOID = f.indexDef(f.oldIndex)
	create := "CREATE INDEX CONCURRENTLY " + bare + "_a_b ON " + f.table + " (a, " +
		newKeys + ")"
	f.propose(create, f.oldIndex, f.oldDef, f.oldOID)
	f.exec = withTestStandingGate(manualExecutor(pool))
	return f
}

func (f *replaceFixture) must(t *testing.T, sql string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// propose records the open finding of a replacement of old by create.
func (f *replaceFixture) propose(create, old, oldDef string, oldOID int64) {
	f.t.Helper()
	f.sql = optimizer.IndexReplaceSQL(create, old)
	f.rollback = optimizer.IndexReplaceRollbackSQL(oldDef, f.newIndex)
	detail, _ := json.Marshal(map[string]any{"table": f.table, "queryids": []int64{},
		"index_replace": map[string]any{"old_index": old, "old_index_oid": oldOID,
			"old_definition": oldDef, "new_index": f.newIndex}})
	if f.findingID > 0 {
		_, _ = f.pool.Exec(f.ctx, "DELETE FROM sage.findings WHERE id=$1", f.findingID)
	}
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, rollback_sql)
		VALUES ('tuning_index_replace', 'info', 'index', $1, 'replace', $2, 'rec', $3, $4)
		RETURNING id`, f.table, detail, f.sql, f.rollback).Scan(&f.findingID); err != nil {
		f.t.Fatalf("insert finding: %v", err)
	}
}

func (f *replaceFixture) indexDef(index string) (string, int64) {
	f.t.Helper()
	var def string
	var oid int64
	if err := f.pool.QueryRow(f.ctx, `SELECT pg_get_indexdef(to_regclass($1)),
		to_regclass($1)::oid::bigint`, index).Scan(&def, &oid); err != nil {
		f.t.Fatalf("index %s: %v", index, err)
	}
	return def, oid
}

// index reports whether index exists and is valid and ready.
func (f *replaceFixture) index(index string) (exists, valid bool) {
	f.t.Helper()
	err := f.pool.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM pg_index
		WHERE indexrelid = to_regclass($1)), COALESCE((SELECT indisvalid AND indisready
		FROM pg_index WHERE indexrelid = to_regclass($1)), false)`, index).
		Scan(&exists, &valid)
	if err != nil {
		f.t.Fatalf("read index %s: %v", index, err)
	}
	return exists, valid
}

func (f *replaceFixture) run() (int64, error) {
	return f.exec.ExecuteIndexReplace(f.ctx, f.findingID, f.sql, f.rollback, &f.userID)
}

type replaceRow struct {
	state, oldDef, err string
	actionID, newOID   int64
}

func (f *replaceFixture) row() replaceRow {
	f.t.Helper()
	var r replaceRow
	if err := f.pool.QueryRow(f.ctx, `SELECT state, old_definition, COALESCE(error, ''),
		COALESCE(action_log_id, 0), COALESCE(new_index_oid, 0) FROM sage.index_replace
		WHERE table_name = $1 ORDER BY id DESC LIMIT 1`, f.table).
		Scan(&r.state, &r.oldDef, &r.err, &r.actionID, &r.newOID); err != nil {
		f.t.Fatalf("read replace state: %v", err)
	}
	return r
}

type rpActionRow struct {
	actionType, outcome, sql, rollback string
	before                             map[string]any
}

func (f *replaceFixture) action(id int64) rpActionRow {
	f.t.Helper()
	var a rpActionRow
	var raw []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT action_type, outcome, sql_executed,
		COALESCE(rollback_sql, ''), COALESCE(before_state, '{}'::jsonb)
		FROM sage.action_log WHERE id = $1`, id).
		Scan(&a.actionType, &a.outcome, &a.sql, &a.rollback, &raw); err != nil {
		f.t.Fatalf("read action %d: %v", id, err)
	}
	_ = json.Unmarshal(raw, &a.before)
	return a
}

func (f *replaceFixture) activeLeases() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.change_lease
		WHERE state = 'active' AND (object_key LIKE '%' || $1 || '%')`, f.bare).
		Scan(&n); err != nil {
		f.t.Fatalf("count leases: %v", err)
	}
	return n
}

func (f *replaceFixture) assertSucceeded(id int64) {
	f.t.Helper()
	if exists, valid := f.index(f.newIndex); !exists || !valid {
		f.t.Fatalf("new index exists=%v valid=%v", exists, valid)
	}
	if exists, _ := f.index(f.oldIndex); exists {
		f.t.Fatal("the subsumed index is dropped")
	}
	r := f.row()
	if r.state != replaceCompleted || r.actionID != id || r.newOID <= 0 ||
		r.oldDef != f.oldDef {
		f.t.Fatalf("state row = %+v (action %d)", r, id)
	}
	a := f.action(id)
	if a.actionType != ActionTypeReplaceIndex || a.outcome != "monitoring" ||
		a.sql != f.sql || a.rollback != f.rollback {
		f.t.Fatalf("action_log = %+v", a)
	}
	if def, _ := a.before["replaced_index_definition"].(string); def != f.oldDef {
		f.t.Fatalf("before_state keeps the old definition: %v", a.before)
	}
}

func TestIndexReplaceSucceeds(t *testing.T) {
	f := newReplaceFixture(t, "b")
	id, err := f.run()
	if err != nil || id <= 0 {
		t.Fatalf("replace = %d, %v", id, err)
	}
	f.assertSucceeded(id)
	if n := f.activeLeases(); n != 0 {
		t.Fatalf("the table lease is released after both steps: %d active", n)
	}
}

func TestIndexReplaceRunsFromTheApprovalQueue(t *testing.T) {
	f := newReplaceFixture(t, "b")
	run, err := f.exec.RunApprovedAction(f.ctx, store.QueuedAction{ID: 1,
		FindingID: f.findingID, ProposedSQL: f.sql, RollbackSQL: f.rollback,
		ActionType: ActionTypeReplaceIndex, Status: "approved"}, f.userID)
	if err != nil || run.ActionLogID <= 0 {
		t.Fatalf("approved replace = %+v, %v", run, err)
	}
	f.assertSucceeded(run.ActionLogID)
}

// One fixture per test: requireDB takes the cross-package session lock.
func TestIndexReplaceRunsFromTakeAction(t *testing.T) {
	g := newReplaceFixture(t, "b")
	id, err := g.exec.ExecuteManual(g.ctx, g.findingID, g.sql, g.rollback, &g.userID)
	if err != nil || id <= 0 {
		t.Fatalf("take action on a replace = %d, %v", id, err)
	}
	g.assertSucceeded(id)
}

func TestIndexReplaceHoldsTheTableLeaseAcrossBothSteps(t *testing.T) {
	f := newReplaceFixture(t, "b")
	var seen []int
	var mu sync.Mutex
	count := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, f.activeLeases())
		return nil
	}
	f.exec.replaceHooks = replaceHooks{afterCreate: count, beforeDrop: count}
	if _, err := f.run(); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if len(seen) != 2 || seen[0] == 0 || seen[1] == 0 {
		t.Fatalf("active leases on the table after the create and before the drop: %v",
			seen)
	}
}

func TestIndexReplaceCreateFailureDropsNothing(t *testing.T) {
	// The build divides by zero on the first row: CREATE INDEX CONCURRENTLY
	// fails after registering the index, leaving an INVALID remnant.
	f := newReplaceFixture(t, "(b / 0)")
	id, err := f.run()
	if !errors.Is(err, ErrReplaceCreateFailed) {
		t.Fatalf("err = %v, want ErrReplaceCreateFailed", err)
	}
	if exists, valid := f.index(f.oldIndex); !exists || !valid {
		t.Fatalf("the old index is untouched: exists=%v valid=%v", exists, valid)
	}
	if exists, _ := f.index(f.newIndex); exists {
		t.Fatal("the INVALID remnant of the failed build is dropped")
	}
	r := f.row()
	if r.state != replaceCreateFailed || !strings.Contains(r.err, "division by zero") {
		t.Fatalf("state row = %+v", r)
	}
	if id <= 0 || f.action(id).outcome != "failed" {
		t.Fatalf("the failed attempt is recorded: action %d", id)
	}
}

func TestIndexReplaceDropFailureIsPartial(t *testing.T) {
	f := newReplaceFixture(t, "b")
	f.exec.cfg.Safety.LockTimeoutMs = 300
	holder, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer holder.Release()
	var tx interface{ Rollback(context.Context) error }
	f.exec.replaceHooks = replaceHooks{beforeDrop: func(ctx context.Context) error {
		begun, err := holder.Begin(ctx)
		if err != nil {
			return err
		}
		tx = begun
		_, err = begun.Exec(ctx, "LOCK TABLE "+f.table+" IN SHARE UPDATE EXCLUSIVE MODE")
		return err
	}}
	id, err := f.run()
	if tx != nil {
		_ = tx.Rollback(context.Background())
	}
	if !errors.Is(err, ErrReplacePartial) || id <= 0 {
		t.Fatalf("replace = %d, %v; want a partial result", id, err)
	}
	if exists, valid := f.index(f.newIndex); !exists || !valid {
		t.Fatalf("the new index stays: exists=%v valid=%v", exists, valid)
	}
	if exists, valid := f.index(f.oldIndex); !exists || !valid {
		t.Fatalf("the old index is kept: exists=%v valid=%v", exists, valid)
	}
	if r := f.row(); r.state != replaceDropFailed || r.actionID != id || r.err == "" {
		t.Fatalf("state row = %+v", r)
	}
	if a := f.action(id); a.outcome != "partial" || a.actionType != ActionTypeReplaceIndex {
		t.Fatalf("action_log = %+v", a)
	}
}

func TestIndexReplaceRefusesConstraintBackedIndexes(t *testing.T) {
	f := newReplaceFixture(t, "b")
	f.must(t, "ALTER TABLE "+f.table+" ADD CONSTRAINT "+f.bare+"_r_excl "+
		"EXCLUDE USING gist (r WITH &&)")
	cases := map[string][2]string{
		"primary key": {"public." + f.bare + "_pkey", "CREATE INDEX CONCURRENTLY " +
			f.bare + "_a_b ON " + f.table + " (id, a)"},
		"exclusion constraint": {"public." + f.bare + "_r_excl", "CREATE INDEX " +
			"CONCURRENTLY " + f.bare + "_a_b ON " + f.table + " USING gist (r) INCLUDE (b)"},
	}
	for name, c := range cases {
		def, oid := f.indexDef(c[0])
		f.propose(c[1], c[0], def, oid)
		id, err := f.run()
		if !errors.Is(err, ErrReplaceConstraintBacked) || id != 0 {
			t.Fatalf("%s: replace = %d, %v", name, id, err)
		}
		if exists, _ := f.index(f.newIndex); exists {
			t.Fatalf("%s: nothing is created", name)
		}
		if exists, valid := f.index(c[0]); !exists || !valid {
			t.Fatalf("%s: the constraint's index is untouched", name)
		}
	}
}

func TestIndexReplaceKeepsForeignKeySupport(t *testing.T) {
	f := newReplaceFixture(t, "b")
	parent := f.table + "_parent"
	f.must(t, "CREATE TABLE "+parent+" (id int PRIMARY KEY)")
	f.must(t, "INSERT INTO "+parent+" SELECT g FROM generate_series(0, 60) g")
	f.must(t, "ALTER TABLE "+f.table+" ADD FOREIGN KEY (a) REFERENCES "+parent+" (id)")
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+
			parent+" CASCADE")
	})
	id, err := f.run()
	if err != nil {
		t.Fatalf("(a, b) still supports the foreign key on (a): %d, %v", id, err)
	}
	f.assertSucceeded(id)
}

func TestIndexReplaceRefusesAChangedOldIndex(t *testing.T) {
	f := newReplaceFixture(t, "b")
	f.must(t, "DROP INDEX "+f.oldIndex)
	f.must(t, "CREATE INDEX "+f.bare+"_a ON "+f.table+" (a) WHERE c > 0")
	id, err := f.run()
	if !errors.Is(err, ErrReplaceIdentityChanged) || id != 0 {
		t.Fatalf("replace of a re-created index = %d, %v", id, err)
	}
	if exists, _ := f.index(f.newIndex); exists {
		t.Fatal("nothing is created for a changed old index")
	}
	f.must(t, "DROP INDEX "+f.oldIndex)
	f.must(t, "CREATE INDEX "+f.bare+"_a ON "+f.table+" (a)")
	if _, err := f.run(); !errors.Is(err, ErrReplaceIdentityChanged) {
		t.Fatalf("the same definition under a new OID is another index: %v", err)
	}
}

type replaceFactBinder struct {
	mu   sync.Mutex
	seen []policy.ActionRequest
	on   string
}

func (b *replaceFactBinder) Bind(_ context.Context, req policy.ActionRequest) (
	[]policy.FactBinding, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen = append(b.seen, req)
	if req.Rollback || !slices.Contains(req.TargetObjs, b.on) {
		return nil, nil
	}
	return []policy.FactBinding{{FactID: 77, Type: "owned_by_app_migrations",
		Subject: b.on, Route: "source_fix", Summary: "owned by the app's migrations",
		Object: b.on}}, nil
}

func TestIndexReplaceRefusedByABindingFact(t *testing.T) {
	f := newReplaceFixture(t, "b")
	binder := &replaceFactBinder{on: f.oldIndex}
	f.exec.WithFactBinder(binder)
	id, err := f.run()
	if err == nil || id != 0 || !strings.Contains(err.Error(), "fact") {
		t.Fatalf("a fact on the old index refuses the replace: %d, %v", id, err)
	}
	if exists, _ := f.index(f.newIndex); exists {
		t.Fatal("no DDL runs under a binding fact")
	}
	binder.mu.Lock()
	defer binder.mu.Unlock()
	if len(binder.seen) == 0 || binder.seen[0].Contract == nil ||
		binder.seen[0].Contract.ActionType != ActionTypeReplaceIndex {
		t.Fatalf("the binder sees the typed replace: %+v", binder.seen)
	}
}
