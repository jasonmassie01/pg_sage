package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration (real Postgres): meta mode with one monitored database
// exports that database's findings, by name, after its analyzer cycle
// refreshes the status; nothing is labelled as the metadata database.
func TestDatabaseHealthMetaModeExportsMonitoredFindings(t *testing.T) {
	// Created first so its drop runs after the target runtime is drained.
	targetDSN := testdb.CreateDatabase(t, "meta_health_target")
	state, input, userID := metaLifecycleFixture(t)
	preserveValueGlobals(t) // restores fleetMgr after its runtime drains (env-race)
	prepareMetaGlobals(t)
	ctx := context.Background()
	parsed, err := url.Parse(targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	input.DatabaseName = strings.TrimPrefix(parsed.Path, "/")
	if _, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID); err != nil {
		t.Fatalf("register target: %v", err)
	}
	target := fleetMgr.GetInstance(input.Name)
	if target == nil || target.Pool == nil {
		t.Fatal("target runtime missing")
	}
	if _, err := target.Pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, recommendation)
		VALUES ('health_probe', 'critical', 'table', 'public.health_probe', 't', '{}', 'r')`,
	); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	cfg.Mode = config.ModeMeta
	pool = state.Pool
	// What the orchestrator runs after every analyzer cycle.
	updateInstanceFindings(ctx, target)

	body := scrapeMetrics(t)
	name := input.Name
	for _, want := range []string{
		fmt.Sprintf(`pg_sage_findings_open{database=%q,severity="critical"} 1`, name),
		fmt.Sprintf(`pg_sage_database_up{database=%q} 1`, name),
		fmt.Sprintf(`pg_sage_analyzer_last_success_timestamp_seconds{database=%q} `, name),
		fmt.Sprintf(`pg_sage_trust_level{database=%q,level="observation"} 1`, name),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("meta scrape lacks %q", want)
		}
	}
	if got := sampleLines(body, "pg_sage_database_up"); len(got) != 1 {
		t.Errorf("database_up = %q, want only the monitored database", got)
	}
	if t.Failed() {
		t.Logf("scrape:\n%s", body)
	}
}
