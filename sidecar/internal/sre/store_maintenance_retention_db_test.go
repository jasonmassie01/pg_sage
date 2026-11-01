package sre

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func insertRetentionMaintenance(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	scope Scope, id UUID, state string, daysAgo int) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO sage.sre_maintenance_proposals
		(id, deployment_id, database_id, investigation_id, state, proposal, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5,
		        jsonb_build_object('expires_at', now() - interval '1 hour'),
		        now() - make_interval(days => $6), now() - make_interval(days => $6))`,
		string(NewUUID()), string(scope.DeploymentID), string(scope.DatabaseID),
		string(id), state, daysAgo)
	if err != nil {
		t.Fatalf("insert maintenance: %v", err)
	}
}

// A completed diagnosis can still own an action. In-flight or unresolved
// maintenance cannot lose its evidence or inverse; finalized and expired work
// does not become an unbounded audit exemption.
func TestRetention_MaintenanceStatesHaveBoundedLifecycle(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	for _, tc := range []struct {
		state string
		held  bool
	}{
		{"proposed", false}, {"requested", false}, {"executing", true},
		{"executed", false}, {"uncertain", true}, {"failed", false},
		{"refused", false}, {"rolling_back", true}, {"rolled_back", false},
		{"rollback_failed", true},
	} {
		t.Run(tc.state, func(t *testing.T) {
			scope := testScope(t, ctx, st)
			id := agedConcluded(t, ctx, st, pool, scope, "maintenance:"+tc.state, 400)
			insertRetentionMaintenance(t, ctx, pool, scope, id, tc.state, 400)
			res, err := st.Purge(ctx, scope, testPolicy)
			if err != nil {
				t.Fatal(err)
			}
			_, getErr := st.Get(ctx, scope, id)
			if tc.held {
				if getErr != nil || res != (PurgeResult{}) || countRows(t, ctx, pool, "sre_evidence", id) != 2 {
					t.Fatalf("unresolved maintenance lost state: purge=%+v get=%v", res, getErr)
				}
				if err := st.VerifyEvents(ctx, scope, id); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(getErr, ErrNotFound) || res.InvestigationsPurged != 1 ||
					countRows(t, ctx, pool, "sre_maintenance_proposals", id) != 0 {
					t.Fatalf("finalized maintenance did not cascade: purge=%+v get=%v", res, getErr)
				}
				if tb := tombstoneKinds(t, ctx, st, scope, id)["investigation"]; tb.Reason != "retention" || len(tb.Detail) == 0 {
					t.Fatalf("missing cascade tombstone: %+v", tb)
				}
			}
		})
	}
}

func TestRetention_MaintenanceRestartsEachAgeWindow(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	for _, tc := range []struct {
		name         string
		proposalAge  int
		evidenceGone bool
		timelineGone bool
	}{
		{"recent change", 1, false, false},
		{"evidence window elapsed", 31, true, false},
		{"timeline window elapsed", 91, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := testScope(t, ctx, st)
			id := agedConcluded(t, ctx, st, pool, scope, tc.name, 400)
			insertRetentionMaintenance(t, ctx, pool, scope, id, "rolled_back", tc.proposalAge)
			res, err := st.Purge(ctx, scope, testPolicy)
			if err != nil {
				t.Fatal(err)
			}
			_, getErr := st.Get(ctx, scope, id)
			if errors.Is(getErr, ErrNotFound) != tc.timelineGone || (getErr != nil && !errors.Is(getErr, ErrNotFound)) {
				t.Fatalf("timeline age: purge=%+v get=%v", res, getErr)
			}
			if got := countRows(t, ctx, pool, "sre_evidence", id) == 0; got != tc.evidenceGone {
				t.Fatalf("evidence gone=%v, want %v", got, tc.evidenceGone)
			}
			if !tc.timelineGone {
				if err := st.VerifyEvents(ctx, scope, id); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Investigation UUIDs are unique only inside deployment/database scope. A
// hold in another scope must not retain an unrelated timeline or evidence.
func TestRetention_MaintenanceHoldUsesFullInvestigationScope(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	held := testScope(t, ctx, st)
	id := agedConcluded(t, ctx, st, pool, held, "scoped maintenance", 40)
	insertRetentionMaintenance(t, ctx, pool, held, id, "executing", 400)
	otherDB := testScope(t, ctx, st)
	otherDep := Scope{DeploymentID: NewUUID(), DatabaseID: held.DatabaseID}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_database_bindings
		(deployment_id, database_id, runtime_key, identity_strength, cluster_epoch)
		VALUES ($1, $2, $3, 'configured', 'retention-test')`,
		string(otherDep.DeploymentID), string(otherDep.DatabaseID), string(NewUUID())); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []Scope{otherDB, otherDep} {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_investigations
			(deployment_id, database_id, id, source_case_id, trigger_kind,
			 trigger_fingerprint, state, created_at, updated_at, expires_at)
			SELECT $1, $2, id, source_case_id, trigger_kind, trigger_fingerprint,
			       state, created_at, updated_at, expires_at
			FROM sage.sre_investigations
			WHERE deployment_id=$3 AND database_id=$4 AND id=$5`,
			string(scope.DeploymentID), string(scope.DatabaseID),
			string(held.DeploymentID), string(held.DatabaseID), string(id)); err != nil {
			t.Fatal(err)
		}
		res, err := st.Purge(ctx, scope, testPolicy)
		if err != nil || res.EvidencePurged != 1 || res.InvestigationsPurged != 0 {
			t.Fatalf("foreign scope borrowed maintenance evidence hold: %+v %v", res, err)
		}
		if _, err = pool.Exec(ctx, `UPDATE sage.sre_investigations
			SET created_at=now()-interval '401 days', updated_at=now()-interval '400 days'
			WHERE deployment_id=$1 AND database_id=$2 AND id=$3`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(id)); err != nil {
			t.Fatal(err)
		}
		res, err = st.Purge(ctx, scope, testPolicy)
		if err != nil || res.InvestigationsPurged != 1 {
			t.Fatalf("foreign scope borrowed maintenance timeline hold: %+v %v", res, err)
		}
	}
	res, err := st.Purge(ctx, held, testPolicy)
	if err != nil || res != (PurgeResult{}) {
		t.Fatalf("held scope was purged: %+v %v", res, err)
	}
	if err := st.VerifyEvents(ctx, held, id); err != nil {
		t.Fatal(err)
	}
}

// Parent locking closes the interval between deciding to retain an action and
// recording its state/event. A racing purge skips the entire investigation.
func TestRetention_MaintenanceMutationAndPurgeShareParentLock(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	id := agedConcluded(t, ctx, st, pool, scope, "locked maintenance", 400)
	insertRetentionMaintenance(t, ctx, pool, scope, id, "proposed", 400)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sage.sre_investigations
		WHERE deployment_id=$1 AND database_id=$2 AND id=$3 FOR UPDATE`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)); err != nil {
		t.Fatal(err)
	}
	res, err := st.Purge(ctx, scope, testPolicy)
	if err != nil || res != (PurgeResult{}) {
		t.Fatalf("purge raced locked mutation: %+v %v", res, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sage.sre_maintenance_proposals
		SET state='executing', updated_at=clock_timestamp()
		WHERE deployment_id=$1 AND database_id=$2 AND investigation_id=$3`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	res, err = st.Purge(ctx, scope, RetentionPolicy{EvidenceAge: 24 * time.Hour,
		TimelineAge: 24 * time.Hour, BatchSize: 50})
	if err != nil || res != (PurgeResult{}) {
		t.Fatalf("committed execution lost hold: %+v %v", res, err)
	}
}

// Establish the first purge statement's snapshot, pause it before LockRows,
// then commit maintenance without modifying the parent tuple. The post-lock
// fresh statement must see that child commit; an old-snapshot NOT EXISTS alone
// can otherwise delete the newly active proposal.
func TestRetention_MaintenanceCommitBeforeParentLockIsRechecked(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	id := agedConcluded(t, ctx, st, pool, scope, "snapshot maintenance race", 400)
	insertRetentionMaintenance(t, ctx, pool, scope, id, "proposed", 400)
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	const lockKey = 809204533
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	// Release the gate before a deferred rollback can wait for its query.
	defer gate.Rollback(context.Background())
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	type result struct {
		ids []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		ids, err := agedIDs(ctx, tx, scope, testPolicy.TimelineAge, 50,
			`AND (SELECT true FROM pg_advisory_xact_lock(809204533))`)
		done <- result{ids, err}
	}()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	waiting := false
	for !waiting {
		select {
		case got := <-done:
			t.Fatalf("purge did not wait at snapshot gate: %+v", got)
		case <-deadline.C:
			t.Fatal("purge never reached advisory snapshot gate")
		case <-poll.C:
			if err := pool.QueryRow(ctx, `SELECT COALESCE(wait_event='advisory', false)
				FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
		}
	}
	mutateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := st.WithInvestigationLock(mutateCtx, scope, id, "user:1", func(tx pgx.Tx, emit Emit) error {
		if _, err := tx.Exec(mutateCtx, `UPDATE sage.sre_maintenance_proposals
			SET state='executing', updated_at=clock_timestamp()
			WHERE deployment_id=$1 AND database_id=$2 AND investigation_id=$3`,
			string(scope.DeploymentID), string(scope.DatabaseID), string(id)); err != nil {
			return err
		}
		return emit("action_decided", map[string]any{"decision": "approved", "test": "snapshot race"})
	}); err != nil {
		t.Fatalf("maintenance could not commit before LockRows: %v", err)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || len(got.ids) != 0 {
		t.Fatalf("post-lock check missed committed maintenance: ids=%v err=%v", got.ids, got.err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.VerifyEvents(ctx, scope, id); err != nil {
		t.Fatal(err)
	}
}

// Held oldest rows must be filtered before LIMIT as well as checked freshly
// after locking; otherwise a full batch of holds starves every later case.
func TestRetention_MaintenanceHoldsDoNotStarveEligibleCases(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	var held []UUID
	for i := 0; i < 3; i++ {
		id := agedConcluded(t, ctx, st, pool, scope, "held:"+string(NewUUID()), 400)
		insertRetentionMaintenance(t, ctx, pool, scope, id, "uncertain", 400)
		held = append(held, id)
	}
	timeline := agedConcluded(t, ctx, st, pool, scope, "later terminal maintenance", 100)
	insertRetentionMaintenance(t, ctx, pool, scope, timeline, "rolled_back", 100)
	evidence := agedConcluded(t, ctx, st, pool, scope, "later evidence", 40)
	policy := testPolicy
	policy.BatchSize = 2
	res, err := st.Purge(ctx, scope, policy)
	if err != nil || res.InvestigationsPurged != 1 || res.EvidencePurged != 1 {
		t.Fatalf("held batch starved later eligible rows: %+v %v", res, err)
	}
	if _, err := st.Get(ctx, scope, timeline); !errors.Is(err, ErrNotFound) {
		t.Fatalf("later eligible timeline was not purged: %v", err)
	}
	if n := countRows(t, ctx, pool, "sre_evidence", evidence); n != 0 {
		t.Fatalf("later eligible evidence kept %d rows", n)
	}
	for _, id := range held {
		if n := countRows(t, ctx, pool, "sre_evidence", id); n != 2 {
			t.Fatalf("held case lost evidence: %s has %d rows", id, n)
		}
	}
}
