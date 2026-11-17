package auditchain_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// plan-01b: a plan's events and the model passes it was built from are two
// append-only chains, so a plan can be replayed against a later model from
// a tamper-evident record. The eight chains before them do not change.

// No concurrent access tests here: the writer ordering of the shared link
// table is covered by concurrency_db_test.go, and the plan writers by
// changeplan's events_db_test.go (20 goroutines, 5 plans).

func TestPlanChainSpecsAreOptionalAppendOnlyAndAppended(t *testing.T) {
	specs := auditchain.Specs()
	var names []string
	for _, s := range specs {
		names = append(names, fmt.Sprintf("%s:v%d", s.Chain, s.Version))
	}
	want := []string{"action_log:v1", "auth_audit:v1", "config_audit:v1",
		"guard_query_audit:v1", "pgaudit_events:v1", "fact_events:v1", "decision:v1",
		"policy_approvals:v1", "plan_events:v1", "plan_model_records:v1"}
	if !slices.Equal(names, want) {
		t.Fatalf("chains = %v, want the eight released chains unchanged, then the "+
			"two plan chains: %v", names, want)
	}
	cases := []struct {
		spec   auditchain.Spec
		table  string
		sealed []string
	}{
		{auditchain.PlanEvents, "sage.plan_event", []string{"id", "plan_id", "step_id",
			"kind", "plan_hash", "actor", "detail", "created_at"}},
		{auditchain.PlanModelRecords, "sage.plan_model_record", []string{"id", "plan_id",
			"pack_hash", "model", "prompt_version", "raw_output", "raw_output_sha256",
			"created_at"}},
	}
	for _, tc := range cases {
		s := tc.spec
		if s.Table != tc.table || !s.Optional || len(s.State) != 0 || s.Version != 1 {
			t.Errorf("%s spec = %+v, want table %s, optional, append-only, v1", s.Chain, s,
				tc.table)
		}
		if got := s.Columns(); !slices.Equal(got, tc.sealed) {
			t.Errorf("%s seals %v, want %v", s.Chain, got, tc.sealed)
		}
		types, ok := auditchain.SealedColumnTypes[auditchain.SpecKey{Chain: s.Chain,
			Version: 1}]
		if !ok || len(types) != len(tc.sealed) {
			t.Errorf("%s pinned column types = %v, want one per sealed column", s.Chain,
				types)
		}
		if got, ok := auditchain.SpecFor(s.Chain); !ok || got.Table != tc.table {
			t.Errorf("SpecFor(%s) = %+v, %v", s.Chain, got, ok)
		}
	}
}

// Integration: on a database whose sage schema has none of the plan
// tables, the chain migration installs (twice) without error and leaves
// the plan chains uninstalled; once the tables exist the next run chains
// them.
func TestPlanChainsInstallOnlyOnceTheTablesExist(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "chain_plan_absent")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	exec(t, pool, "CREATE SCHEMA sage")
	exec(t, pool, auditchain.MigrationSQL())
	exec(t, pool, auditchain.MigrationSQL())
	installed, err := auditchain.Installed(ctx, pool)
	if err != nil {
		t.Fatalf("installed: %v", err)
	}
	if slices.Contains(installed, "plan_events") ||
		slices.Contains(installed, "plan_model_records") {
		t.Fatalf("installed = %v with no plan tables", installed)
	}
	exec(t, pool, `CREATE TABLE sage.plan_event (id bigserial PRIMARY KEY,
		plan_id bigint NOT NULL, step_id bigint, kind text NOT NULL,
		plan_hash text NOT NULL, actor text NOT NULL, detail jsonb NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now())`)
	exec(t, pool, auditchain.MigrationSQL())
	installed, err = auditchain.Installed(ctx, pool)
	if err != nil {
		t.Fatalf("installed: %v", err)
	}
	if !slices.Contains(installed, "plan_events") ||
		slices.Contains(installed, "plan_model_records") {
		t.Fatalf("installed = %v, want plan_events only", installed)
	}
}

var planHashA = strings.Repeat("a1", 32)

// insertPlanEvents writes n events of plan 1 and returns their ids.
func insertPlanEvents(t *testing.T, pool *pgxpool.Pool, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := range n {
		var id int64
		err := pool.QueryRow(context.Background(), `INSERT INTO sage.plan_event (plan_id,
			step_id, kind, plan_hash, actor, detail) VALUES (1, $1, 'step_started', $2,
			'pg_sage', jsonb_build_object('i', $3::int)) RETURNING id`,
			int64(10+i), planHashA, i).Scan(&id)
		if err != nil {
			t.Fatalf("insert plan event %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// Tamper: an edited event, a deleted one and a reordered pair of links
// are each found by chain verification.
func TestPlanEventChainFindsEditsDeletesAndReordering(t *testing.T) {
	cases := []struct {
		name, kind string
		tamper     func(t *testing.T, pool *pgxpool.Pool, ids []int64)
	}{
		{"edited event", auditchain.KindRowEdited, func(t *testing.T, pool *pgxpool.Pool,
			ids []int64) {
			bypass(t, pool, `UPDATE sage.plan_event SET kind = 'approved' WHERE id = $1`,
				ids[1])
		}},
		{"deleted event", auditchain.KindRowMissing, func(t *testing.T, pool *pgxpool.Pool,
			ids []int64) {
			bypass(t, pool, `DELETE FROM sage.plan_event WHERE id = $1`, ids[1])
		}},
		{"reordered pair", auditchain.KindPrevMismatch, func(t *testing.T,
			pool *pgxpool.Pool, _ []int64) {
			exec(t, pool, `UPDATE sage.audit_chain_link SET seq = -seq
				WHERE chain = 'plan_events' AND seq IN (2, 3)`)
			exec(t, pool, `UPDATE sage.audit_chain_link
				SET seq = CASE seq WHEN -2 THEN 3 ELSE 2 END
				WHERE chain = 'plan_events' AND seq IN (-2, -3)`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := freshDB(t, "chain_plan_"+strings.ReplaceAll(tc.name, " ", "_"))
			ids := insertPlanEvents(t, pool, 4)
			rep := verify(t, pool, auditchain.PlanEvents, auditchain.Window{})
			if !rep.OK() || rep.Links != 4 || rep.RowsChecked != 4 {
				t.Fatalf("intact plan events report = %+v", rep)
			}
			tc.tamper(t, pool, ids)
			rep = verify(t, pool, auditchain.PlanEvents, auditchain.Window{})
			if rep.OK() || !hasKind(rep, tc.kind) {
				t.Fatalf("%s: want %s, got %+v", tc.name, tc.kind, rep.Problems)
			}
		})
	}
}

// Both plan tables are append-only: an ordinary update or delete is
// refused, and the refusal leaves the chain intact.
func TestPlanChainTablesRefuseAnUpdateOrDelete(t *testing.T) {
	pool := freshDB(t, "chain_plan_append_only")
	ids := insertPlanEvents(t, pool, 1)
	var rec int64
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.plan_model_record
		(plan_id, model, prompt_version, raw_output, raw_output_sha256)
		VALUES (1, 'model-x', 'p-1', '', $1) RETURNING id`,
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855").Scan(&rec)
	if err != nil {
		t.Fatalf("insert model record: %v", err)
	}
	for _, sql := range []string{
		fmt.Sprintf(`UPDATE sage.plan_event SET actor = 'mallory' WHERE id = %d`, ids[0]),
		fmt.Sprintf(`UPDATE sage.plan_model_record SET raw_output = 'x' WHERE id = %d`, rec),
		fmt.Sprintf(`DELETE FROM sage.plan_event WHERE id = %d`, ids[0]),
		fmt.Sprintf(`DELETE FROM sage.plan_model_record WHERE id = %d`, rec),
	} {
		_, err := pool.Exec(context.Background(), sql)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v, want an append-only refusal", sql, err)
		}
	}
	for _, spec := range []auditchain.Spec{auditchain.PlanEvents,
		auditchain.PlanModelRecords} {
		rep := verify(t, pool, spec, auditchain.Window{})
		if !rep.OK() || rep.Links != 1 {
			t.Errorf("%s after the refused update = %+v", spec.Chain, rep)
		}
	}
}
