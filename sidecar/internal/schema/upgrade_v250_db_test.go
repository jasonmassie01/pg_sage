package schema

import (
	"context"
	_ "embed"
	"slices"
	"strings"
	"testing"
	"time"
)

// v250SageDump is pg_dump --schema=sage --inserts --no-owner
// --no-privileges --no-comments of a database pg_sage v2.5.0 bootstrapped
// twice (PG 14 pg_dump, so it restores on PG 14 to 18); see
// testdata/README.md. plan-01a made it: the first T5 item to need it.
//
//go:embed testdata/v2.5.0_sage_dump.sql
var v250SageDump string

// Upgrade: a v2.5.0 sage schema, with its rows, boots twice to the catalog
// of a fresh install (every T5 step, the plan tables included).
func TestBootstrap_UpgradeFromV250MatchesAFreshInstall(t *testing.T) {
	if !strings.Contains(v250SageDump, "CREATE TABLE sage.action_log") ||
		strings.Contains(v250SageDump, "CREATE TABLE sage.plan_step") {
		t.Fatal("testdata dump is not a v2.5.0 sage schema")
	}
	fresh := freshSchemaPool(t, "schema_up_fresh250")
	pool := newPool(t, "schema_up_v250")
	restoreDump(t, pool, v250SageDump)
	before := sageCatalog(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("upgrade boot %d: %v", run, err)
		}
	}
	got := sageCatalog(t, pool)
	if d := diffCatalogs(sageCatalog(t, fresh), got); d != "" {
		t.Fatalf("v2.5.0 upgraded differs from a fresh install:%s", d)
	}
	if slices.Equal(before, got) {
		t.Fatal("the upgrade changed nothing: the dump is not from an older release")
	}
	if !slices.ContainsFunc(got, func(s string) bool {
		return strings.Contains(s, "plan_approval_live")
	}) {
		t.Fatal("the upgraded catalog lacks the plan tables")
	}
	if err := CheckReady(ctx, pool); err != nil {
		t.Fatalf("CheckReady after the upgrade: %v", err)
	}
}
