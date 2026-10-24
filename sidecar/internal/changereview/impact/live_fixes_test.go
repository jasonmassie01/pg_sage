//go:build cgo

package impact

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// A USING expression other than the column itself makes PostgreSQL
// rewrite the table, binary-coercible types or not.
func TestTypeChangeWithAUsingExpressionRewrites(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("ALTER TABLE rv.rv_orders ALTER COLUMN code TYPE varchar(40) " +
		"USING upper(code)")
	f := one(t, a, RuleTypeChange)
	if f.Severity != changereview.SeverityWarning || strings.Contains(f.Title,
		"binary-coercible") {
		t.Fatalf("CR-06 %+v, want the rewrite warning", f)
	}
	for _, cr01 := range byRule(a, RuleLockAndRewrite) {
		risk := decode[riskData](t, citedOfKind(t, a, cr01, "risk")[0])
		if !risk.RequiresRewrite || risk.BinaryCoercible {
			t.Fatalf("CR-01 risk %+v, want a rewrite", risk)
		}
	}
	plain := e.analyze("ALTER TABLE rv.rv_orders ALTER COLUMN code TYPE varchar(40) " +
		"USING code")
	if f := one(t, plain, RuleTypeChange); !strings.Contains(f.Title, "binary-coercible") {
		t.Fatalf("USING the column itself: CR-06 %q, want binary-coercible", f.Title)
	}
}

// Post-conditions name what the catalog will show: a serial column's
// integer type, and the name PostgreSQL gives an unnamed CHECK over
// several columns.
func TestPostConditionsOfSerialsAndMultiColumnChecks(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("CREATE TABLE rv.small_t (id int)")
	for _, c := range []struct{ sql, want string }{
		{"ALTER TABLE rv.rv_orders ADD COLUMN seq bigserial", "bigint"},
		{"ALTER TABLE rv.small_t ADD COLUMN n smallserial", "smallint"},
		{"ALTER TABLE rv.small_t ADD COLUMN m serial", "integer"},
		{"ALTER TABLE rv.rv_orders ADD CHECK (id > 0 OR customer_id > 0)",
			"rv_orders_check"},
		{"ALTER TABLE rv.rv_orders ADD CHECK (id > 0)", "rv_orders_id_check"},
	} {
		a := e.analyze(c.sql)
		pcs := a.Expectations.PostConditions
		if len(pcs) != 1 || (pcs[0].Type != c.want && pcs[0].Constraint != c.want) {
			t.Fatalf("%q: post-conditions %+v, want %s", c.sql, pcs, c.want)
		}
		if got := Landing(e.evaluate(pcs)); got != PostNotLanded {
			t.Fatalf("%q before the DDL: %s", c.sql, got)
		}
		e.f.Exec(c.sql)
		if got := e.evaluate(pcs); Landing(got) != PostLanded {
			t.Fatalf("%q after the DDL: %+v, want landed", c.sql, got)
		}
	}
}

// CR-05 reads the script in order: a backfill before SET NOT NULL, or a
// view dropped before the column it uses, is not a certain failure.
func TestWillFailRulesFollowTheScript(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("UPDATE rv.rv_orders SET customer_id = NULL WHERE id % 33 = 0")
	e.f.Exec("ANALYZE rv.rv_orders")
	a := e.analyze(`UPDATE rv.rv_orders SET customer_id = 0 WHERE customer_id IS NULL;
		ALTER TABLE rv.rv_orders ALTER COLUMN customer_id SET NOT NULL`)
	for _, f := range byRule(a, RuleWillFail) {
		if f.Severity == changereview.SeverityCritical {
			t.Fatalf("SET NOT NULL after a backfill: critical CR-05 %q", f.Title)
		}
	}
	if fs := byRule(a, RuleWillFail); len(fs) != 1 || !strings.Contains(fs[0].Text,
		"backfill") {
		t.Fatalf("want one CR-05 warning naming the backfill:%s", describe(a))
	}
	e.f.Exec("CREATE VIEW rv.order_status AS SELECT id, status FROM rv.rv_orders")
	b := e.analyze(`DROP VIEW rv.order_status;
		ALTER TABLE rv.rv_orders DROP COLUMN status`)
	none(t, b, RuleWillFail)
	c := e.analyze(`CREATE OR REPLACE VIEW rv.order_status AS SELECT id, status::text
		FROM rv.rv_orders;
		ALTER TABLE rv.rv_orders ALTER COLUMN status TYPE varchar(20)`)
	fs := byRule(c, RuleWillFail)
	if len(fs) != 1 || fs[0].Severity != changereview.SeverityWarning {
		t.Fatalf("a view replaced earlier: want one CR-05 warning:%s", describe(c))
	}
	d := e.analyze("ALTER TABLE rv.rv_orders DROP COLUMN status")
	if f := one(t, d, RuleWillFail); f.Severity != changereview.SeverityCritical {
		t.Fatalf("a view still there: CR-05 %s, want critical", f.Severity)
	}
}

// In-file opt-outs of a migration tool's transaction are honored when the
// transaction comes from the tool's default, not from the request.
func TestMigrationFileTransactionOptOuts(t *testing.T) {
	e := newEnv(t)
	const cic = "CREATE INDEX CONCURRENTLY rv_orders_status_idx ON rv.rv_orders (status);"
	cases := []struct {
		tool, content string
		fails         bool
	}{
		{"goose", "-- +goose Up\n-- +goose NO TRANSACTION\n" + cic, false},
		{"goose", "-- +goose Up\n" + cic, true},
		{"dbmate", "-- migrate:up transaction:false\n" + cic + "\n-- migrate:down\n", false},
		{"dbmate", "-- migrate:up\n" + cic + "\n-- migrate:down\n", true},
		{"liquibase", "--liquibase formatted sql\n--changeset app:1 runInTransaction:false\n" +
			cic, false},
		{"atlas", "-- atlas:txmode none\n" + cic, false},
	}
	for _, c := range cases {
		in := input(c.content)
		in.MigrationTool, in.Transactional = c.tool, true
		a := e.analyzeWith(e.eng, in)
		got := false
		for _, f := range byRule(a, RuleWillFail) {
			got = got || strings.Contains(f.Title, "CONCURRENTLY will fail")
		}
		if got != c.fails {
			t.Errorf("%s %q: CONCURRENTLY failure %v, want %v:%s", c.tool, c.content, got,
				c.fails, describe(a))
		}
	}
	in := input("-- +goose NO TRANSACTION\n" + cic)
	in.MigrationTool, in.Transactional = "goose", true
	in.TransactionalFrom = changereview.TransactionalFromRequest
	if a := e.analyzeWith(e.eng, in); len(byRule(a, RuleWillFail)) != 1 {
		t.Fatalf("a request that says transactional wins over the file:%s", describe(a))
	}
}

// A new foreign key takes SHARE ROW EXCLUSIVE on the referenced table:
// only its writes queue, not its reads.
func TestForeignKeyQueuesOnlyTheReferencedTablesWrites(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("ALTER TABLE rv.rv_items ADD COLUMN order_ref bigint " +
		"REFERENCES rv.rv_orders (id)")
	_, q := lockQueueOf(t, a)
	if slices.Contains(q.ConflictingQueryIDs, e.f.Q1.ID) ||
		slices.Contains(q.ConflictingQueryIDs, e.f.Q3.ID) ||
		!slices.Contains(q.ConflictingQueryIDs, e.f.Q2.ID) {
		t.Fatalf("conflicting %v: want Q2 (the write) and neither read", q.ConflictingQueryIDs)
	}
	if !near(q.PeakConflictingPerS, e.f.Q2.RatePerSecond(), 0.01) {
		t.Fatalf("peak %v/s, want Q2's %v/s", q.PeakConflictingPerS, e.f.Q2.RatePerSecond())
	}
}

// Unqualified statements are attributed to a table only when no other
// schema has a table of that name: in a schema-per-tenant database they
// are unknown, never another tenant's load.
func TestUnqualifiedStatementsOfHomonymTablesAreNotAttributed(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("CREATE SCHEMA rv2")
	e.f.Exec("CREATE TABLE rv2.rv_orders (id bigint PRIMARY KEY, email text)")
	conn, err := e.f.Pool.Acquire(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(e.ctx, "SET search_path = rv2"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if _, err := conn.Exec(e.ctx, "SELECT email FROM rv_orders WHERE id = $1",
			int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(e.ctx, "RESET search_path"); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	a := e.analyze("ALTER TABLE rv.rv_orders RENAME COLUMN email TO email_address")
	f := one(t, a, RuleColumnReferenced)
	if ids := queryIDsOf(t, a, f); !slices.Equal(ids, []int64{e.f.Q1.ID}) {
		t.Fatalf("CR-03 cites %v, want only the qualified Q1 %d", ids, e.f.Q1.ID)
	}
	found := false
	for _, u := range byRule(a, RuleImpactUnknown) {
		found = found || strings.Contains(u.Title+u.Text, "unqualified")
	}
	if !found {
		t.Fatalf("want a CR-17 naming the unattributed unqualified statements:%s",
			describe(a))
	}
}

// predictionWhatIf answers every Validate with a pooled improvement and
// per-statement figures.
type predictionWhatIf struct{ per map[int64]float64 }

func (p predictionWhatIf) IsAvailable(context.Context) bool { return true }

func (p predictionWhatIf) Validate(_ context.Context, _ optimizer.Recommendation,
	qs []optimizer.QueryInfo) (optimizer.WhatIfResult, error) {
	per := map[int64]float64{}
	for _, q := range qs {
		if v, ok := p.per[q.QueryID]; ok {
			per[q.QueryID] = v
		}
	}
	return optimizer.WhatIfResult{Improvement: 40, Measured: len(per), SizeBytes: 8192,
		PerQuery: per}, nil
}

// Each watched statement's prediction is its own HypoPG figure, not the
// pooled improvement of every read.
func TestPredictionsArePerTarget(t *testing.T) {
	e := newEnv(t)
	eng := e.engine(e.f.Pool, func(d *Deps) {
		d.WhatIf = predictionWhatIf{per: map[int64]float64{e.f.Q1.ID: 0, e.f.Q3.ID: 90}}
	})
	a := e.analyzeWith(eng, input("CREATE INDEX CONCURRENTLY rv_orders_cc_idx "+
		"ON rv.rv_orders (created_at, customer_id)"))
	want := map[int64]float64{e.f.Q1.ID: 0, e.f.Q3.ID: -90}
	seen := 0
	for _, p := range a.Expectations.Predictions {
		id := p.TargetQueryIDs[0]
		w, ok := want[id]
		if !ok {
			continue
		}
		seen++
		if p.ExpectedChangePct == nil || *p.ExpectedChangePct != w {
			t.Errorf("prediction of %d: %v, want %v", id, p.ExpectedChangePct, w)
		}
	}
	if seen != 2 {
		t.Fatalf("predictions %+v, want Q1 and Q3", a.Expectations.Predictions)
	}
}

// The review records whether its post-conditions already held when it
// was written; the runner then marks the review not applicable at once.
func TestAnalysisRecordsTheLandingAtReview(t *testing.T) {
	e := newEnv(t)
	held := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN IF NOT EXISTS email text")
	if held.Expectations.AtReview != changereview.AtReviewLanded {
		t.Fatalf("already there: at review %q, want landed", held.Expectations.AtReview)
	}
	todo := e.analyze("ALTER TABLE rv.rv_orders ADD COLUMN note text")
	if todo.Expectations.AtReview != changereview.AtReviewUnmet {
		t.Fatalf("still to do: at review %q, want unmet", todo.Expectations.AtReview)
	}
	nothing := e.analyze("SELECT 1")
	if nothing.Expectations.AtReview != "" {
		t.Fatalf("no post-condition: at review %q, want none", nothing.Expectations.AtReview)
	}
}

// The tools of one review session share one workload read.
func TestToolCallsShareTheSessionsWorkloadRead(t *testing.T) {
	e := newEnv(t)
	ctx := WithSessionCache(e.ctx)
	before := e.workloadReads()
	for i := 0; i < 3; i++ {
		if _, err := e.eng.QueriesTouching(ctx, "rv.rv_orders", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := e.eng.TableContext(ctx, "rv.rv_orders"); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.workloadReads() - before; n != 1 {
		t.Fatalf("six tool calls read pg_stat_statements' workload %d times, want 1", n)
	}
	if _, err := e.eng.QueriesTouching(e.ctx, "rv.rv_orders", ""); err != nil {
		t.Fatal(err)
	}
	if n := e.workloadReads() - before; n != 2 {
		t.Fatalf("a call outside the session read %d times in all, want 2", n)
	}
}

// workloadReads is how often the workload read ran in this database.
func (e *env) workloadReads() int64 {
	return e.f.Int64(`SELECT coalesce(sum(calls), 0)::int8 FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query LIKE '%s.query ~* $1%' AND query NOT LIKE '%pg_stat_statements s%LIKE%'`)
}

// Peak rates come from a dense history (a sample a minute for three
// days) read without scanning every sample: the busiest hour's calls per
// second, and the window's calls.
func TestPeakRateFromADenseHistory(t *testing.T) {
	e := newEnv(t)
	e.f.Exec("DELETE FROM sage.query_store WHERE queryid = $1", e.f.Q2.ID)
	e.f.Exec(`INSERT INTO sage.query_store (captured_at, queryid, calls, total_exec_time,
		mean_exec_time)
		SELECT now() - interval '72 hours' + g * interval '1 minute', $1,
		       60 * g + 540 * least(greatest(g - 1000, 0), 180), 1, 0.01
		  FROM generate_series(0, 4320) g`, e.f.Q2.ID)
	a := e.analyze("ALTER TABLE rv.rv_orders DROP COLUMN status")
	q := decode[queryData](t, evidence(t, a, queryEvidenceID(e.f.Q2.ID)))
	if !near(q.PeakPerS, 10, 0.02) {
		t.Fatalf("peak %v calls/s, want the busiest hour's 10/s", q.PeakPerS)
	}
	if q.Calls7d != 60*4320+540*180 {
		t.Fatalf("calls over the window %d, want %d", q.Calls7d, 60*4320+540*180)
	}
}
