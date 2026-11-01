package schema

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Reproduce the exact deployed nine-state prototype, preserve a real existing
// proposal through Bootstrap, then prove uncertain is accepted and unsupported
// states remain rejected. Repeating Bootstrap must keep the validated check OID.
func TestSREMaintenanceMigration_UpgradesExistingStateCheckWithoutLosingRows(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "maintenance_upgrade"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.sre_maintenance_proposals
		DROP CONSTRAINT sre_maintenance_proposals_state_check;
		ALTER TABLE sage.sre_maintenance_proposals
		ADD CONSTRAINT sre_maintenance_proposals_state_check
		CHECK (state IN ('proposed','requested','executing','executed','failed',
		    'refused','rolling_back','rolled_back','rollback_failed'))`); err != nil {
		t.Fatal(err)
	}
	const dep, db, inv, proposal = "71111111-1111-4111-8111-111111111111",
		"72222222-2222-4222-8222-222222222222", "73333333-3333-4333-8333-333333333333",
		"74444444-4444-4444-8444-444444444444"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertM3Fixture(ctx, tx, dep, db, inv); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_maintenance_proposals
		(id,deployment_id,database_id,investigation_id,state,proposal)
		VALUES ($1,$2,$3,$4,'executed','{"rationale":"preserve-existing-proposal"}')`,
		proposal, dep, db, inv); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var created time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM sage.sre_maintenance_proposals WHERE id=$1`, proposal).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_maintenance_proposals SET state='uncertain' WHERE id=$1`, proposal); err == nil {
		t.Fatal("legacy fixture unexpectedly accepted uncertain")
	}
	var firstOID uint32
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
		var oid uint32
		var validated bool
		if err := pool.QueryRow(ctx, `SELECT oid,convalidated FROM pg_constraint
			WHERE conrelid='sage.sre_maintenance_proposals'::regclass
			  AND conname='sre_maintenance_proposals_state_check'`).Scan(&oid, &validated); err != nil || !validated {
			t.Fatalf("canonical check not validated: oid=%d valid=%v err=%v", oid, validated, err)
		}
		if run == 0 {
			firstOID = oid
		} else if oid != firstOID {
			t.Fatalf("idempotent bootstrap rebuilt its state check: %d -> %d", firstOID, oid)
		}
		var state, rationale string
		var after time.Time
		if err := pool.QueryRow(ctx, `SELECT state,proposal->>'rationale',created_at
			FROM sage.sre_maintenance_proposals WHERE id=$1`, proposal).Scan(&state, &rationale, &after); err != nil || state != "executed" || rationale != "preserve-existing-proposal" || !after.Equal(created) {
			t.Fatalf("existing proposal changed during upgrade: state=%s rationale=%s created=%s err=%v", state, rationale, after, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_maintenance_proposals SET state='uncertain' WHERE id=$1`, proposal); err != nil {
		t.Fatalf("upgraded existing table still rejects uncertain: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE sage.sre_maintenance_proposals SET state='unsupported_future_state' WHERE id=$1`, proposal)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("unsupported state accepted or wrong error: %v", err)
	}
}

// The same state literals do not establish a check's semantics. Bootstrap must
// repair a wrong operator or an extra expression, and validate an unvalidated
// canonical check, before taking its idempotent fast path.
func TestSREMaintenanceMigration_RepairsNoncanonicalChecks(t *testing.T) {
	states := []string{"proposed", "requested", "executing", "executed", "uncertain", "failed", "refused", "rolling_back", "rolled_back", "rollback_failed"}
	list := "'" + strings.Join(states, "','") + "'"
	cases := []struct {
		name, check string
	}{
		{"wrong_operator", "CHECK (state NOT IN (" + list + "))"},
		{"extra_or", "CHECK (state IN (" + list + ") OR true)"},
		{"extra_and", "CHECK (state IN (" + list + ") AND false)"},
		{"not_valid", "CHECK (state IN (" + list + ")) NOT VALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "maintenance_check"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			bootstrapWithRetry(t, ctx, pool)
			if _, err := pool.Exec(ctx, `ALTER TABLE sage.sre_maintenance_proposals
				DROP CONSTRAINT sre_maintenance_proposals_state_check;
				ALTER TABLE sage.sre_maintenance_proposals
				ADD CONSTRAINT sre_maintenance_proposals_state_check `+tc.check); err != nil {
				t.Fatal(err)
			}
			readCheck := func() (uint32, bool) {
				t.Helper()
				var oid uint32
				var valid bool
				if err := pool.QueryRow(ctx, `SELECT oid,convalidated FROM pg_constraint
					WHERE conrelid='sage.sre_maintenance_proposals'::regclass
					AND conname='sre_maintenance_proposals_state_check'`).Scan(&oid, &valid); err != nil {
					t.Fatal(err)
				}
				return oid, valid
			}
			before, _ := readCheck()
			bootstrapWithRetry(t, ctx, pool)
			after, valid := readCheck()
			if before == after || !valid {
				t.Fatalf("check not replaced and validated: before=%d after=%d validated=%v", before, after, valid)
			}
			bootstrapWithRetry(t, ctx, pool)
			repeated, valid := readCheck()
			if repeated != after || !valid {
				t.Fatalf("canonical check not stable: first=%d repeated=%d validated=%v", after, repeated, valid)
			}
			const dep, db, inv, proposal = "75111111-1111-4111-8111-111111111111",
				"75222222-2222-4222-8222-222222222222", "75333333-3333-4333-8333-333333333333",
				"75444444-4444-4444-8444-444444444444"
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := insertM3Fixture(ctx, tx, dep, db, inv); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_maintenance_proposals
				(id,deployment_id,database_id,investigation_id,state,proposal)
				VALUES ($1,$2,$3,$4,'proposed','{}')`, proposal, dep, db, inv); err != nil {
				t.Fatal(err)
			}
			for _, state := range states {
				if _, err := tx.Exec(ctx, `UPDATE sage.sre_maintenance_proposals SET state=$2 WHERE id=$1`, proposal, state); err != nil {
					t.Fatalf("canonical state %s rejected: %v", state, err)
				}
			}
			_, err = tx.Exec(ctx, `UPDATE sage.sre_maintenance_proposals SET state='unsupported_future_state' WHERE id=$1`, proposal)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("unsupported state accepted or wrong error: %v", err)
			}
		})
	}
}

func TestSREMaintenanceMigration_UnsupportedExistingRowPreservesPriorCheck(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "maintenance_invalid"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.sre_maintenance_proposals
		DROP CONSTRAINT sre_maintenance_proposals_state_check;
		ALTER TABLE sage.sre_maintenance_proposals
		ADD CONSTRAINT sre_maintenance_proposals_state_check CHECK (state <> '')`); err != nil {
		t.Fatal(err)
	}
	const dep, db, inv, proposal = "76111111-1111-4111-8111-111111111111",
		"76222222-2222-4222-8222-222222222222", "76333333-3333-4333-8333-333333333333",
		"76444444-4444-4444-8444-444444444444"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := insertM3Fixture(ctx, tx, dep, db, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_maintenance_proposals
		(id,deployment_id,database_id,investigation_id,state,proposal)
		VALUES ($1,$2,$3,$4,'unsupported_future_state','{"rationale":"preserve-unsupported"}')`, proposal, dep, db, inv); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var before uint32
	if err := pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='sage.sre_maintenance_proposals'::regclass
		AND conname='sre_maintenance_proposals_state_check'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	err = Bootstrap(ctx, pool)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("unsupported existing row should fail validation: %v", err)
	}
	var state, rationale, definition string
	var after uint32
	if err := pool.QueryRow(ctx, `SELECT p.state,p.proposal->>'rationale',c.oid,pg_get_constraintdef(c.oid)
		FROM sage.sre_maintenance_proposals p JOIN pg_constraint c
		ON c.conrelid='sage.sre_maintenance_proposals'::regclass AND c.conname='sre_maintenance_proposals_state_check'
		WHERE p.id=$1`, proposal).Scan(&state, &rationale, &after, &definition); err != nil {
		t.Fatal(err)
	}
	if state != "unsupported_future_state" || rationale != "preserve-unsupported" || before != after || definition != "CHECK ((state <> ''::text))" {
		t.Fatalf("failed validation changed prior row/check: state=%s rationale=%s oid=%d->%d definition=%s", state, rationale, before, after, definition)
	}
}
