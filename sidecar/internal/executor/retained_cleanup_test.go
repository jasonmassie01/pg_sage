package executor

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

func TestRetainedIndexCleanupRequiresVerdictAndSeparatePolicy(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		retain, allowed, emergency bool
		wantDrop                   bool
	}{
		{"retain authorized", true, true, false, true},
		{"pending", false, true, false, false},
		{"policy denied", true, false, false, false},
		{"emergency stop", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, id, gate := retainedFixture(t, tc.allowed, "")
			if err := SetEmergencyStop(t.Context(), e.pool, tc.emergency); err != nil {
				t.Fatal(err)
			}
			err := (&executorIndexActions{exec: e}).Retain(t.Context(), id,
				verify.Verdict{Retain: tc.retain, Status: "success"})
			if (err == nil) != tc.wantDrop {
				t.Fatalf("Retain error=%v", err)
			}
			assertCleanupIndexes(t, e, tc.wantDrop)
			if tc.wantDrop {
				if gate.calls != 1 || !strings.HasPrefix(gate.request.SQL, "DROP INDEX") {
					t.Fatalf("separate DROP authorization = %#v", gate)
				}
				var n int
				_ = e.pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.action_log
					WHERE action_type='drop_index' AND before_state->>'retained_action_id'=$1`,
					strconv.FormatInt(id, 10)).Scan(&n)
				if n != 1 {
					t.Fatalf("cleanup audit rows=%d", n)
				}
			}
		})
	}
}

func TestRetainedIndexCleanupRejectsSelfDropAndReplacedIndex(t *testing.T) {
	cases := []string{"self", "replaced", "invalid SQL", "invalid new", "wrong keys", "unique old"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			drop := ""
			if kind == "self" {
				drop = "DROP INDEX CONCURRENTLY public.cleanup_new"
			}
			if kind == "invalid SQL" {
				drop = "DROP TABLE public.cleanup_orders"
			}
			e, id, gate := retainedFixture(t, true, drop)
			mutateRetainedFixture(t, e, id, kind)
			err := (&executorIndexActions{exec: e}).Retain(t.Context(), id,
				verify.Verdict{Retain: true, Status: "success"})
			if kind != "self" && err == nil {
				t.Fatal("unsafe cleanup unexpectedly accepted")
			}
			var oldExists bool
			_ = e.pool.QueryRow(t.Context(), `SELECT to_regclass('public.cleanup_old') IS NOT NULL`).
				Scan(&oldExists)
			if !oldExists || gate.calls != 0 {
				t.Fatalf("old=%v gate calls=%d", oldExists, gate.calls)
			}
		})
	}
}

func retainedFixture(
	t *testing.T, allowed bool, drop string,
) (*Executor, int64, *custodianGateCapture) {
	t.Helper()
	pool, ctx := requireDB(t)
	_, err := pool.Exec(ctx, `CREATE TABLE public.cleanup_orders (id int, value text);
		CREATE INDEX cleanup_old ON public.cleanup_orders(id);
		CREATE INDEX cleanup_new ON public.cleanup_orders(id) INCLUDE(value)`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE public.cleanup_orders`)
		_ = SetEmergencyStop(context.Background(), pool, false)
	})
	var oid int64
	err = pool.QueryRow(ctx, `SELECT 'public.cleanup_old'::regclass::bigint`).Scan(&oid)
	if err != nil {
		t.Fatal(err)
	}
	if drop == "" {
		drop = "DROP INDEX CONCURRENTLY public.cleanup_old"
	}
	before, _ := json.Marshal(map[string]any{
		"superseded_index_drop_sql": drop, "superseded_index_oid": oid,
	})
	var id int64
	err = pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type,sql_executed,before_state,outcome)
		VALUES ('create_index',
		'CREATE INDEX cleanup_new ON public.cleanup_orders(id) INCLUDE(value)',
		$1,'pending') RETURNING id`, before).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.action_log
		WHERE id=$1 OR before_state->>'retained_action_id'=$2`, id, strconv.FormatInt(id, 10))
	})
	seedRetainedVerification(t, pool, id)
	e := New(pool, &config.Config{}, nil, time.Time{}, func(string, string, ...any) {})
	gate := &custodianGateCapture{verdict: policy.Decision{Verdict: policy.VerdictPark}}
	if allowed {
		gate.verdict.Verdict = policy.VerdictExecute
	}
	e.WithPolicyGate(gate)
	return e, id, gate
}

func assertCleanupIndexes(t *testing.T, e *Executor, dropped bool) {
	t.Helper()
	var oldExists, newValid bool
	err := e.pool.QueryRow(t.Context(), `SELECT to_regclass('public.cleanup_old') IS NOT NULL,
		(SELECT indisvalid FROM pg_index WHERE indexrelid='public.cleanup_new'::regclass)`).
		Scan(&oldExists, &newValid)
	if err != nil || oldExists == dropped || !newValid {
		t.Fatalf("old=%v new valid=%v error=%v", oldExists, newValid, err)
	}
}

// This fixture represents the durable result of a completed verifier watch.
// The hook tests isolate finalization; lifecycle tests independently prove ordering.
func seedRetainedVerification(t *testing.T, pool *pgxpool.Pool, actionID int64) {
	t.Helper()
	var decisionID int64
	err := pool.QueryRow(t.Context(), `INSERT INTO sage.decision
 (feature,intent,verdict,risk_tier,reason,evidence_id)
 VALUES ('index','retain fixture','execute','safe','fixture',gen_random_uuid())
 RETURNING id`).Scan(&decisionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(t.Context(), `INSERT INTO sage.verification
 (decision_id,action_log_id,criterion,baseline,minimum_samples,next_evaluation_at,
 hard_deadline_at,verdict,completed_at) VALUES ($1,$2,'{}','{}',1,now(),now(),'success',now())`,
		decisionID, actionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.verification
			WHERE action_log_id=$1`, actionID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.decision WHERE id=$1`, decisionID)
	})
}

func TestRetainedCleanupPersistsIntentAndIsIdempotent(t *testing.T) {
	e, id, gate := retainedFixture(t, true, "")
	state := map[string]any{}
	finding := analyzer.Finding{
		RecommendedSQL: "CREATE INDEX cleanup_new ON public.cleanup_orders(id) INCLUDE(value)",
		Detail:         map[string]any{"drop_ddl": "DROP INDEX CONCURRENTLY public.cleanup_old"}}
	if err := e.snapshotSupersededIndex(t.Context(), finding, state); err != nil {
		t.Fatal(err)
	}
	if state["superseded_index_drop_sql"] != finding.Detail["drop_ddl"] ||
		state["superseded_index_oid"].(int64) <= 0 {
		t.Fatalf("persisted cleanup intent=%#v", state)
	}
	for range 2 {
		err := (&executorIndexActions{exec: e}).Retain(
			t.Context(), id, verify.Verdict{Retain: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	if gate.calls != 1 {
		t.Fatalf("repeated retention authorization count=%d", gate.calls)
	}
	assertCleanupIndexes(t, e, true)
}

func TestSupersededSnapshotEmptySelfAndMissingTargets(t *testing.T) {
	e, _, _ := retainedFixture(t, true, "")
	for _, drop := range []string{"", "DROP INDEX CONCURRENTLY public.cleanup_new"} {
		state := map[string]any{}
		err := e.snapshotSupersededIndex(t.Context(), analyzer.Finding{
			RecommendedSQL: "CREATE INDEX cleanup_new ON public.cleanup_orders(id)",
			Detail:         map[string]any{"drop_ddl": drop}}, state)
		if err != nil || len(state) != 0 {
			t.Fatalf("self/empty snapshot=%#v error=%v", state, err)
		}
	}
	err := e.snapshotSupersededIndex(t.Context(), analyzer.Finding{
		RecommendedSQL: "CREATE INDEX cleanup_new ON public.cleanup_orders(id)",
		Detail: map[string]any{
			"drop_ddl": "DROP INDEX CONCURRENTLY public.missing_cleanup_old",
		}}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("missing target error=%v", err)
	}
}

func TestConcurrentRetainedCleanupExecutesOnce(t *testing.T) {
	e, id, gate := retainedFixture(t, true, "")
	results := make(chan error, 4)
	for range 4 {
		go func() {
			results <- (&executorIndexActions{exec: e}).Retain(
				t.Context(), id, verify.Verdict{Retain: true})
		}()
	}
	for range 4 {
		if err := <-results; err != nil {
			t.Errorf("concurrent retain: %v", err)
		}
	}
	if gate.calls != 1 {
		t.Fatalf("concurrent DROP authorizations=%d", gate.calls)
	}
	assertCleanupIndexes(t, e, true)
}

func mutateRetainedFixture(t *testing.T, e *Executor, id int64, kind string) {
	t.Helper()
	if kind == "replaced" {
		_, err := e.pool.Exec(t.Context(), `DROP INDEX public.cleanup_old;
			CREATE INDEX cleanup_old ON public.cleanup_orders (id)`)
		if err != nil {
			t.Fatal(err)
		}
	}
	if kind == "wrong keys" {
		_, err := e.pool.Exec(t.Context(), `DROP INDEX public.cleanup_new;
 CREATE INDEX cleanup_new ON public.cleanup_orders(value) INCLUDE(id)`)
		if err != nil {
			t.Fatal(err)
		}
	}
	if kind == "unique old" {
		_, err := e.pool.Exec(t.Context(), `DROP INDEX public.cleanup_old;
 CREATE UNIQUE INDEX cleanup_old ON public.cleanup_orders(id)`)
		if err != nil {
			t.Fatal(err)
		}
		// The recorded identity is current: rejection must come from uniqueness,
		// not the independent replaced-index guard.
		_, err = e.pool.Exec(t.Context(), `UPDATE sage.action_log
			SET before_state=jsonb_set(before_state,'{superseded_index_oid}',
			to_jsonb('public.cleanup_old'::regclass::bigint)) WHERE id=$1`, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	if kind == "invalid new" {
		_, err := e.pool.Exec(t.Context(), `DROP INDEX public.cleanup_new`)
		if err != nil {
			t.Fatal(err)
		}
	}
}
