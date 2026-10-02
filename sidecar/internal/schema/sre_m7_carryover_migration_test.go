package schema

import (
	"testing"
)

// Sage SRE M7 carry-over (coordinator decision 2026-10-02): a ledger row
// records whether its level was carried over from the pre-M7 policy
// (and the decision that granted it) or set by the ledger; the history
// accepts the carry-over and deadline-override events. Additive and
// idempotent on a database the first M7 migration already created.

func TestSREMigrationM7Carry_ProvenanceColumns(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	m7Cleanup(t, ctx)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, family, action_class, level, changed_by, change_reason)
		VALUES ($1, 'wal_retention', 'wal_bound', 1, 'test', 'default provenance')`,
		m7Deployment); err != nil {
		t.Fatal(err)
	}
	var provenance string
	var ref *string
	if err := pool.QueryRow(ctx, `SELECT provenance, carried_ref
		FROM sage.sre_family_autonomy WHERE deployment_id = $1
		  AND family = 'wal_retention' AND action_class = 'wal_bound'`, m7Deployment).
		Scan(&provenance, &ref); err != nil || provenance != "ledger" || ref != nil {
		t.Fatalf("defaults = %q %v (%v), want ledger and no reference", provenance, ref, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_family_autonomy
		SET provenance = 'carried_over', carried_ref = 'spec F4'
		WHERE deployment_id = $1 AND family = 'wal_retention'`, m7Deployment); err != nil {
		t.Fatalf("carried_over refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_family_autonomy
		SET provenance = 'guessed' WHERE deployment_id = $1`, m7Deployment); err == nil {
		t.Fatal("an unknown provenance was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sre_family_autonomy
		SET provenance = 'carried_over', carried_ref = NULL
		WHERE deployment_id = $1`, m7Deployment); err == nil {
		t.Fatal("a carried-over level without its decision reference was accepted")
	}
}

func TestSREMigrationM7Carry_EventTypes(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	insert := func(eventType string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_events
			(deployment_id, family, action_class, event_type, actor, reason)
			VALUES ($1, 'wraparound_runway', 'freeze', $2, 'test', 'event type')`,
			m7Deployment, eventType)
		return err
	}
	for _, ok := range []string{"carried_over", "deadline_override", "capped",
		"auto_executed"} {
		if err := insert(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	if err := insert("carried_away"); err == nil {
		t.Fatal("an unknown event type was accepted")
	}
}
