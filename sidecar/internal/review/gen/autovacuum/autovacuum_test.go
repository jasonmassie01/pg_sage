package autovacuum

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Happy path. orders: 10M rows, target 5% = 500k dead. Growth is the
// median daily rise, 600k, so 25k an hour; the lag is the 60s naptime plus
// the one-hour margin (1.0167h), so the trigger budget is 500k - 25k *
// 1.0167 = 474,583. The threshold stays 50 (under 10% of the budget), so
// the scale factor is (474,583 - 50) / 10M = 0.0474, floored to 0.047, and
// the analyze scale factor half of it, 0.023. The predicted peak is 50 +
// 470,000 + 25,417 = 495,467.
func TestGenerate_SizesTheTriggerToTheDeadTupleTarget(t *testing.T) {
	cands, cov := generate(t, packOf(t, stockSettings(), orders()))
	if len(cands) != 1 {
		t.Fatalf("%d candidates %+v, want 1", len(cands), cands)
	}
	c := cands[0]
	wantSQL := `ALTER TABLE "public"."orders" SET (autovacuum_vacuum_scale_factor = 0.047, ` +
		`autovacuum_analyze_scale_factor = 0.023)`
	wantRollback := `ALTER TABLE "public"."orders" RESET (autovacuum_vacuum_scale_factor, ` +
		`autovacuum_analyze_scale_factor)`
	if c.ActionType != "set_table_autovacuum" || c.ForwardSQL != wantSQL ||
		c.RollbackSQL != wantRollback || !c.Reversible || c.Kind != KindTable ||
		c.Target != "public.orders" {
		t.Fatalf("candidate %+v", c)
	}
	q := c.Prediction.Quantities
	if q["live_tuples"] != 1e7 || q["dead_tuple_peak"] != 2e6 || q["target_dead"] != 5e5 ||
		q["growth_per_hour"] != 25000 || math.Abs(q["predicted_peak"]-495466.67) > 0.01 {
		t.Fatalf("quantities %v", q)
	}
	if c.EvidenceIDs[0] != "table/av/public.orders" ||
		!strings.Contains(strings.Join(c.EvidenceIDs, ","),
			"setting/autovacuum_vacuum_scale_factor") {
		t.Fatalf("evidence %v", c.EvidenceIDs)
	}
	if c.Prediction.Metric != MetricDeadPeak || c.Prediction.Tier != candidate.TierNone ||
		c.Prediction.SizePct == nil || math.Abs(*c.Prediction.SizePct-75.23) > 0.01 {
		t.Fatalf("prediction %+v", c.Prediction)
	}
	if got, _ := executor.ClassifySQL(c.ForwardSQL); got != "set_table_autovacuum" {
		t.Fatalf("the SQL classifies as %q", got)
	}
	if err := executor.ValidateExecutorSQL(c.ForwardSQL); err != nil {
		t.Fatalf("the executor refuses the SQL: %v", err)
	}
	if cov.Status != candidate.CoverageComplete {
		t.Fatalf("coverage %+v", cov)
	}
}

// Boundary: a dead-tuple target exactly at the current peak gives no
// candidate; one tuple over it gives one.
func TestGenerate_TargetExactlyAtThePeak(t *testing.T) {
	f := orders()
	f.DeadTuples = series(100_000, 300_000, 500_000)
	cands, _ := generate(t, packOf(t, stockSettings(), f))
	if len(cands) != 0 {
		t.Fatalf("peak at the target: %+v, want none", cands)
	}
	f.DeadTuples = series(100_000, 300_000, 500_001)
	cands, _ = generate(t, packOf(t, stockSettings(), f))
	if len(cands) != 1 || cands[0].ActionType != "set_table_autovacuum" {
		t.Fatalf("peak one over the target: %+v, want one step", cands)
	}
}

// Nil and empty: no vacuum history (fewer than two days of dead tuples),
// live rows unknown and a small table give no candidate, never a default
// value; a pack without the section is a coverage gap; a nil pack is an
// error.
func TestGenerate_NoHistoryNoCandidate(t *testing.T) {
	oneDay := orders()
	oneDay.ID, oneDay.Table = "table/av/public.one_day", "public.one_day"
	oneDay.DeadTuples = series(-1, -1, 2_000_000)
	blind := orders()
	blind.ID, blind.Table, blind.LiveTuples = "table/av/public.blind", "public.blind",
		"unknown"
	small := orders()
	small.ID, small.Table, small.LiveTuples = "table/av/public.small", "public.small",
		int64(99_999)
	cands, cov := generate(t, packOf(t, stockSettings(), blind, oneDay, small))
	if len(cands) != 0 {
		t.Fatalf("candidates %+v, want none", cands)
	}
	for _, want := range []string{"public.one_day", "public.blind"} {
		if !strings.Contains(strings.Join(cov.Notes, "\n"), want) {
			t.Fatalf("notes %v lack %s", cov.Notes, want)
		}
	}
	cands, cov = generate(t, packOf(t, stockSettings()))
	if len(cands) != 0 || strings.Join(cov.Missing, ",") != "autovacuum" {
		t.Fatalf("no section: %+v %+v", cands, cov)
	}
	if _, _, err := New().Generate(context.Background(), nil, env); err == nil {
		t.Fatal("a nil pack was not an error")
	}
	if _, _, err := New().Generate(context.Background(), packOf(t, nil, orders()),
		candidate.Env{}); err == nil {
		t.Fatal("an env naming no database was accepted")
	}
}

// Without the settings section PostgreSQL's defaults are assumed, and the
// coverage says so.
func TestGenerate_NoSettingsAssumesDefaults(t *testing.T) {
	cands, cov := generate(t, packOf(t, nil, orders()))
	if len(cands) != 1 || !strings.Contains(cands[0].ForwardSQL, "= 0.047") {
		t.Fatalf("candidates %+v", cands)
	}
	if cov.Status != candidate.CoveragePartial || !strings.Contains(
		strings.Join(cov.Missing, ","), "settings") {
		t.Fatalf("coverage %+v", cov)
	}
}

// A table already tuned tighter than the computed trigger, or whose dead
// tuples grow faster than any trigger can hold, is advice: autovacuum is
// falling behind, which settings on the trigger do not fix.
func TestGenerate_FallingBehindIsAdvice(t *testing.T) {
	tight := orders()
	tight.Reloptions = map[string]string{"autovacuum_vacuum_scale_factor": "0.01"}
	fast := orders()
	fast.ID, fast.Table = "table/av/public.fast", "public.fast"
	fast.DeadTuples = series(0, 20_000_000, 40_000_000)
	cands, _ := generate(t, packOf(t, stockSettings(), fast, tight))
	if len(cands) != 2 {
		t.Fatalf("candidates %+v, want two pieces of advice", cands)
	}
	for _, c := range cands {
		if c.ActionType != candidate.ActionAdvice || c.Kind != KindFallingBehind ||
			c.ForwardSQL != "" || !strings.Contains(c.Reason, "falling behind") {
			t.Fatalf("candidate %+v", c)
		}
	}
}

// A table's own settings: an explicit scale factor is restored by SET on
// rollback, so a key with no prior value (the analyze factor) is left out
// of the same statement (no single statement could restore a mix); an
// explicit threshold far above the budget is lowered to 10% of it.
func TestGenerate_TableOptionsDecideTheRollback(t *testing.T) {
	f := orders()
	f.Reloptions = map[string]string{"autovacuum_vacuum_scale_factor": "0.1",
		"autovacuum_vacuum_threshold": "100000"}
	cands, _ := generate(t, packOf(t, stockSettings(), f))
	if len(cands) != 1 {
		t.Fatalf("candidates %+v", cands)
	}
	c := cands[0]
	wantSQL := `ALTER TABLE "public"."orders" SET (autovacuum_vacuum_scale_factor = 0.042, ` +
		`autovacuum_vacuum_threshold = 47458)`
	wantRollback := `ALTER TABLE "public"."orders" SET (autovacuum_vacuum_scale_factor = 0.1, ` +
		`autovacuum_vacuum_threshold = 100000)`
	if c.ForwardSQL != wantSQL || c.RollbackSQL != wantRollback {
		t.Fatalf("forward %q\nrollback %q", c.ForwardSQL, c.RollbackSQL)
	}
}

// Invalid input: autovacuum switched off by the owner, unparseable table
// options, and a table name that does not split into schema and name are
// left alone with a reason.
func TestGenerate_SkipsTablesItMustNotTouch(t *testing.T) {
	off := orders()
	off.ID, off.Table = "table/av/public.off", "public.off"
	off.Reloptions = map[string]string{"autovacuum_enabled": "false"}
	odd := orders()
	odd.ID, odd.Table = "table/av/public.odd", "public.odd"
	odd.ReloptionsText = []string{"garbage"}
	dotted := orders()
	dotted.ID, dotted.Table = "table/av/a.b.c", "a.b.c"
	badValue := orders()
	badValue.ID, badValue.Table = "table/av/public.bad", "public.bad"
	badValue.Reloptions = map[string]string{"autovacuum_vacuum_scale_factor": "lots"}
	cands, cov := generate(t, packOf(t, stockSettings(), off, odd, dotted, badValue))
	if len(cands) != 0 {
		t.Fatalf("candidates %+v, want none", cands)
	}
	notes := strings.Join(cov.Notes, "\n")
	for _, want := range []string{"public.off", "public.odd", "a.b.c", "public.bad"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("notes %q lack %s", notes, want)
		}
	}
}

// Every emitted reloption value passes pgconf's range check, however wild
// the table: tiny targets clamp the scale factor at 0.001.
func TestGenerate_ValuesStayInRange(t *testing.T) {
	huge := orders()
	huge.LiveTuples = int64(5_000_000_000)
	huge.DeadTuples = series(0, 1_000_000_000, 2_000_000_000)
	cands, _ := generate(t, packOf(t, stockSettings(), huge))
	for _, c := range cands {
		if c.ActionType != "set_table_autovacuum" {
			continue
		}
		stmt, ok := pgconf.ParseAlterTableReloptions(c.ForwardSQL)
		if !ok {
			t.Fatalf("unparseable %q", c.ForwardSQL)
		}
		for _, o := range stmt.Options {
			if ok, why := pgconf.ValidateReloption(o.Key, o.Value); !ok {
				t.Fatalf("%s=%s out of range: %s", o.Key, o.Value, why)
			}
		}
	}
}
