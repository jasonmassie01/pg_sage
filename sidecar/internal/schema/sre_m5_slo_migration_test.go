package schema

import (
	"strings"
	"testing"
)

// Sage SRE M5 (SLOs and change events): the tables install idempotently
// on a fresh or existing schema, and their constraints refuse malformed
// rows (bad > eligible, an unknown state or change kind, a duplicate
// source event id).
func TestSREMigrationM5_TablesAndConstraints(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"sre_service_slos", "sre_slo_transitions",
		"sre_sli_samples", "sre_change_events", "sre_change_feed_state"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.'||$1) IS NOT NULL`,
			table).Scan(&ok); err != nil || !ok {
			t.Fatalf("sage.%s missing (err %v)", table, err)
		}
	}
	dep := "11111111-1111-4111-8111-111111111111"
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_sli_samples
		(deployment_id, slo_name, series, observed_at, bad, eligible)
		VALUES ($1, 'm5-test', 's', now(), 1, 10)`, dep); err != nil {
		t.Fatalf("valid sample: %v", err)
	}
	refusals := map[string]string{
		"bad > eligible": `INSERT INTO sage.sre_sli_samples (deployment_id, slo_name, series,
			observed_at, bad, eligible) VALUES ('` + dep + `', 'm5-test', 's',
			now() - interval '1 second', 11, 10)`,
		"negative": `INSERT INTO sage.sre_sli_samples (deployment_id, slo_name, series,
			observed_at, bad, eligible) VALUES ('` + dep + `', 'm5-test', 's',
			now() - interval '2 second', -1, 10)`,
		"unknown kind": `INSERT INTO sage.sre_change_events (deployment_id, id, source,
			event_id, kind, summary, occurred_at, signature_status, change_hash)
			VALUES ('` + dep + `', gen_random_uuid(), 'ci', 'e1', 'reboot', 's', now(),
			'verified', decode(repeat('00', 32), 'hex'))`,
		"unknown signature": `INSERT INTO sage.sre_change_events (deployment_id, id, source,
			event_id, kind, summary, occurred_at, signature_status, change_hash)
			VALUES ('` + dep + `', gen_random_uuid(), 'ci', 'e2', 'deploy', 's', now(),
			'unsigned', decode(repeat('00', 32), 'hex'))`,
	}
	for name, sql := range refusals {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// One row per source event id: a second insert is a unique violation.
func TestSREMigrationM5_UniqueSourceEvent(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	dep := "22222222-2222-4222-8222-222222222222"
	insert := `INSERT INTO sage.sre_change_events (deployment_id, id, source, event_id,
		kind, summary, occurred_at, signature_status, change_hash)
		VALUES ($1, gen_random_uuid(), 'ci', 'dup-1', 'deploy', 's', now(), 'verified',
		decode(repeat('00', 32), 'hex'))`
	if _, err := pool.Exec(ctx, insert, dep); err != nil {
		t.Fatalf("first change event: %v", err)
	}
	_, err := pool.Exec(ctx, insert, dep)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate source/event id: err = %v", err)
	}
}
