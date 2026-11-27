package schema

import (
	"strings"
	"testing"
)

// guard-02b: the change feed accepts the provider-event kinds
// (version_change, host_change) and an attribution label, refuses an
// unknown label, and the migration runs twice cleanly on a fresh server.
func TestChangeFeedProviderEvents_KindsAndAttribution(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var v2, old int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE conname = 'sre_change_events_kind_v2'),
		count(*) FILTER (WHERE conname = 'sre_change_events_kind_check')
		FROM pg_constraint WHERE conrelid = 'sage.sre_change_events'::regclass`).
		Scan(&v2, &old); err != nil {
		t.Fatal(err)
	}
	if v2 != 1 || old != 0 {
		t.Fatalf("kind constraints: v2=%d old=%d, want the v2 constraint alone", v2, old)
	}
	dep := "33333333-3333-4333-8333-333333333333"
	insert := `INSERT INTO sage.sre_change_events (deployment_id, id, source, event_id,
		kind, summary, occurred_at, signature_status, change_hash, attribution)
		VALUES ($1, gen_random_uuid(), 'pg_sage', $2, $3, 's', now(), 'internal',
		decode(repeat('00', 32), 'hex'), $4)`
	accepted := []struct{ id, kind string }{{"g02b-v", "version_change"},
		{"g02b-h", "host_change"}, {"g02b-r", "restart"}}
	for _, a := range accepted {
		if _, err := pool.Exec(ctx, insert, dep, a.id, a.kind, "provider"); err != nil {
			t.Errorf("%s: %v", a.kind, err)
		}
	}
	if _, err := pool.Exec(ctx, insert, dep, "g02b-null", "deploy", nil); err != nil {
		t.Errorf("unattributed deploy: %v", err)
	}
	refused := []struct{ id, kind, attribution string }{
		{"g02b-bad-label", "restart", "aliens"},
		{"g02b-bad-kind", "reboot", "provider"},
	}
	for _, r := range refused {
		if _, err := pool.Exec(ctx, insert, dep, r.id, r.kind, r.attribution); err == nil {
			t.Errorf("%s/%s accepted", r.kind, r.attribution)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.sre_change_events
		WHERE deployment_id = $1`, dep); err != nil {
		t.Fatal(err)
	}
}

// The step is registered in the T6 tranche, under lock_timeout, and
// replaces the kind constraint only behind a catalog guard.
func TestChangeFeedProviderEvents_IsATrancheSixStep(t *testing.T) {
	var found bool
	for _, s := range trancheSixMigrations() {
		found = found || s[0] == "ddlChangeFeedProviderEvents"
	}
	if !found {
		t.Fatalf("trancheSixMigrations = %v, want ddlChangeFeedProviderEvents",
			trancheSixMigrations())
	}
	for _, want := range []string{"lock_timeout", "ADD COLUMN IF NOT EXISTS attribution",
		"conname = 'sre_change_events_kind_v2'",
		"'version_change', 'host_change'"} {
		if !strings.Contains(ddlChangeFeedProviderEvents, want) {
			t.Errorf("ddlChangeFeedProviderEvents lacks %q", want)
		}
	}
}
