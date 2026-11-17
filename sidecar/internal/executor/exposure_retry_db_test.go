package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// An AP-15 finding names every exposed relation of its schema. Failures on
// some of them (tables pg_sage's action role does not own) are per
// statement: they must not use up the retries of the finding's other
// tables (fleetpol-02b review).
func TestRevokePublic_FailuresOnOneTableDoNotHoldBackTheOthers(t *testing.T) {
	owned := newExposureFixture(t, "GRANT SELECT ON $T TO PUBLIC")
	ctx := context.Background()
	refused := refusedExposureTables(t, owned)
	actor := exposureActor(t, owned)
	var role string
	if err := actor.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.pool.Exec(ctx, "ALTER TABLE "+owned.qual+" OWNER TO "+role); err != nil {
		t.Fatalf("give the actor the last table: %v", err)
	}
	var evidence []map[string]any
	for _, f := range append(refused, owned) {
		evidence = append(evidence, map[string]any{"source": "pg_class.relacl", "ref": f.qual})
	}
	detail, _ := json.Marshal(map[string]any{"detector": "AP-15", "evidence": evidence})
	if _, err := owned.pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, status)
		VALUES ('agent_posture:AP-15', 'warning', 'schema', 'public', $1, $2, 'open')`,
		"exposure "+owned.table, detail); err != nil {
		t.Fatalf("insert AP-15 finding: %v", err)
	}
	e := exposureExecutor(owned.pool, policy.ExposureModeEnforce, true)
	e.WithActionPool(actor)
	for range 2 {
		e.runExposureRemediations(ctx, false)
	}
	for _, f := range refused {
		acts := f.actions(t)
		if len(acts) != 1 || acts[0]["outcome"] != "failed" {
			t.Fatalf("%s actions = %+v, want one ownership refusal", f.qual, acts)
		}
		if f.publicPrivileges(t) != "SELECT" {
			t.Fatalf("a refused revoke changed the ACL of %s", f.qual)
		}
	}
	if got := owned.publicPrivileges(t); got != "" {
		t.Fatalf("PUBLIC still holds %q on the owned table: the other tables' refusals "+
			"used up its retries", got)
	}
	acts := owned.actions(t)
	if len(acts) != 1 || acts[0]["type"] != ActionTypeRevokePublicGrant ||
		acts[0]["outcome"] == "failed" {
		t.Fatalf("owned table actions = %+v, want one executed revoke", acts)
	}
}

// refusedExposureTables are maxActionRetries tables beside owned, each
// granted to PUBLIC and owned by the test's superuser. One fixture per test
// (requireDB holds the cross-package lock), so they share its pool and are
// named apart from it.
func refusedExposureTables(t *testing.T, owned exposureFixture) []exposureFixture {
	t.Helper()
	ctx := context.Background()
	var refused []exposureFixture
	for i := range maxActionRetries {
		name := fmt.Sprintf("exr%d_%s", i, owned.table[len("exp_"):])
		f := exposureFixture{pool: owned.pool, table: name, qual: "public." + name}
		for _, sql := range []string{"CREATE TABLE " + f.qual + " (id int)",
			"GRANT SELECT ON " + f.qual + " TO PUBLIC"} {
			if _, err := owned.pool.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		t.Cleanup(func() {
			_, _ = owned.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+f.qual)
		})
		refused = append(refused, f)
	}
	return refused
}
