package autonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// migration-09 on real PostgreSQL: a detected type tightening is recorded
// in sage.decision as a moderate recommendation with no proposed SQL, its
// rewrite only as labelled evidence, and is never routed. No concurrent
// access test: one custodian scans at a time per database
// (supervisor-serialized).

// schemaScopedStructuralDetector returns the structural invariants of one
// schema, so other tests' tables do not join the scan.
type schemaScopedStructuralDetector struct {
	detector postgresSchemaDetector
	schema   string
}

func (d schemaScopedStructuralDetector) Detect(
	ctx context.Context,
) ([]schemaguard.Invariant, error) {
	items, err := d.detector.detectStructuralPathologies(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]schemaguard.Invariant, 0, 1)
	for _, item := range items {
		if item.Schema == d.schema {
			result = append(result, item)
		}
	}
	return result, nil
}

type structuralLedgerRow struct {
	riskTier, verdict, reason string
	evidence                  map[string]any
}

func structuralLedgerRows(t *testing.T, pool *pgxpool.Pool, target string) []structuralLedgerRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT risk_tier, verdict, reason, evidence
		FROM sage.decision WHERE feature='schema_guard' AND intent=$1
		AND target_objects @> jsonb_build_array($2::text) ORDER BY id`,
		string(schemaguard.InvariantTypeTightening), target)
	if err != nil {
		t.Fatalf("read structural decisions: %v", err)
	}
	defer rows.Close()
	var result []structuralLedgerRow
	for rows.Next() {
		var row structuralLedgerRow
		var raw []byte
		if err := rows.Scan(&row.riskTier, &row.verdict, &row.reason, &raw); err != nil {
			t.Fatalf("scan structural decision: %v", err)
		}
		if err := json.Unmarshal(raw, &row.evidence); err != nil {
			t.Fatalf("decode structural evidence %s: %v", raw, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate structural decisions: %v", err)
	}
	return result
}

func TestStructuralDecisionIsRecordedAsRecommendationInLedger(t *testing.T) {
	pool := requireAutonomyDB(t)
	schemaName := fmt.Sprintf("mig09_%d", time.Now().UnixNano())
	execAll(t, pool, "CREATE SCHEMA "+schemaName,
		"CREATE TABLE "+schemaName+".orders (id bigint PRIMARY KEY, order_number text)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaName+" CASCADE")
	})
	router := &recordingRouter{}
	policyConfig := schemaguard.DefaultPolicy()
	policyConfig.AllowFKIndexApply, policyConfig.AllowRetentionApply = true, true
	custodian := schemaguard.NewCustodian(
		schemaScopedStructuralDetector{newPostgresSchemaDetector(pool, nil), schemaName},
		postgresSchemaContractSource{pool}, postgresSchemaHistorySource{pool},
		schemaRemediationRouter{database: "testdb", router: router, verifiedIndexes: router},
		schemaDecisionRecorder{ledger.NewService(ledger.NewPostgresRepository(pool))},
		policyConfig)
	first, err := custodian.Scan(context.Background())
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if first.Detected != 1 || first.Routed != 0 || first.Recorded != 1 {
		t.Fatalf("first scan = %+v, want one recorded, none routed", first)
	}
	target := schemaName + ".orders"
	rows := structuralLedgerRows(t, pool, target)
	if len(rows) != 1 {
		t.Fatalf("%d ledger rows for %s, want 1", len(rows), target)
	}
	assertStructuralLedgerRow(t, rows[0], schemaName)
	second, err := custodian.Scan(context.Background())
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if second.Recorded != 0 || second.Unchanged != 1 || second.Routed != 0 {
		t.Fatalf("second scan = %+v, want the decision unchanged", second)
	}
	if n := len(structuralLedgerRows(t, pool, target)); n != 1 {
		t.Fatalf("unchanged decision wrote %d rows, want 1", n)
	}
	if routed := router.routed(); len(routed) != 0 {
		t.Fatalf("structural invariant routed: %#v", routed)
	}
}

func assertStructuralLedgerRow(t *testing.T, row structuralLedgerRow, schemaName string) {
	t.Helper()
	if row.riskTier != "moderate" || row.verdict != string(ledger.VerdictObserveOnly) {
		t.Fatalf("risk_tier=%q verdict=%q, want moderate observe_only", row.riskTier,
			row.verdict)
	}
	if !strings.Contains(row.reason, "online column migration") ||
		strings.Contains(row.reason, "authorization pending") {
		t.Fatalf("reason = %q", row.reason)
	}
	if _, ok := row.evidence["proposed_sql"]; ok {
		t.Fatalf("evidence carries proposed_sql: %v", row.evidence)
	}
	if row.evidence["route"] != string(schemaguard.RouteRecommendation) ||
		row.evidence["disposition"] != string(schemaguard.DispositionRecommend) ||
		row.evidence["subject"] != "order_number" {
		t.Fatalf("evidence = %v", row.evidence)
	}
	want := `ALTER TABLE "` + schemaName + `"."orders" ALTER COLUMN "order_number" ` +
		`TYPE bigint USING "order_number"::bigint`
	if row.evidence["rewrite_statement"] != want {
		t.Fatalf("rewrite_statement = %v, want %q", row.evidence["rewrite_statement"], want)
	}
	note, _ := row.evidence["rewrite_note"].(string)
	if !strings.Contains(note, "ACCESS EXCLUSIVE") {
		t.Fatalf("rewrite_note = %q", note)
	}
}
