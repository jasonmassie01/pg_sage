package cases_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/rca"
)

// migration-01 contract: what the migration detector writes is what the
// case preflight reads. FindingFromIncident -> JSON -> SourceFinding ->
// preflight, from a real DDLRisk; no measured check may stay unknown.
// The other cases tests feed synthetic keys, which hid this gap.

func contractRisk(measuredAt time.Time) *migration.DDLRisk {
	return &migration.DDLRisk{
		Statement: "CREATE INDEX idx_orders_a ON app.orders (a)",
		RuleID:    "ddl_index_not_concurrent", LockLevel: "SHARE",
		TableName: "orders", SchemaName: "app",
		TableSizeBytes: 64 << 20, EstimatedRows: 900000, StatsKnown: true,
		ActiveQueries: 0, LongestQuerySec: 0, PendingLocks: 0,
		ReplicationLag: 1.5, EstimatedLockMs: 50, RiskScore: 0.55,
		Measured: migration.DDLMeasured{TableStats: true, Activity: true,
			PendingLocks: true, ReplicationLag: true},
		MeasuredAt: measuredAt,
	}
}

func contractIncident() *rca.Incident {
	return &rca.Incident{
		Severity: "warning", Source: "schema_advisor",
		RootCause:       "Dangerous DDL: ddl_index_not_concurrent",
		AffectedObjects: []string{"app.orders"},
		SignalIDs:       []string{"ddl_index_not_concurrent"},
		RecommendedSQL:  "CREATE INDEX CONCURRENTLY idx_orders_a ON app.orders (a)",
		// The advisor marks its own templates; only they are copied.
		RecommendedSQLSource: migration.SQLSourceDeterministic,
		ActionRisk:           "risk_score=0.55", Confidence: 0.55, DatabaseName: "prod",
	}
}

func projectedPreflight(t *testing.T, risk *migration.DDLRisk) cases.ActionCandidate {
	t.Helper()
	f, ok := migration.FindingFromIncident(31, "CREATE INDEX idx_orders_a ON app.orders (a)",
		contractIncident(), risk)
	if !ok {
		t.Fatal("FindingFromIncident returned ok=false")
	}
	raw, err := json.Marshal(f.Detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	c := cases.ProjectFinding(cases.SourceFinding{
		ID: "mig-1", DatabaseName: f.DatabaseName,
		Category: migration.SafetyFindingCategory, Severity: cases.Severity(f.Severity),
		ObjectType: f.ObjectType, ObjectIdentifier: f.ObjectIdentifier,
		RuleID: f.RuleID, Title: f.Title, RecommendedSQL: f.RecommendedSQL,
		Detail: detail, ObservedAt: time.Now(),
	}, executor.ClassifySQL)
	for _, candidate := range c.ActionCandidates {
		if candidate.ActionType == "create_index_concurrently" {
			if candidate.DDLPreflight == nil {
				t.Fatal("create_index_concurrently candidate has no preflight")
			}
			return candidate
		}
	}
	t.Fatalf("no create_index_concurrently candidate: %+v", c.ActionCandidates)
	return cases.ActionCandidate{}
}

func TestDDLPreflightContract_MeasuredChecksAreKnown(t *testing.T) {
	at := time.Now().UTC().Add(-3 * time.Minute).Truncate(time.Second)
	candidate := projectedPreflight(t, contractRisk(at))
	report := candidate.DDLPreflight
	want := map[string]string{
		"lock_level": "pass", "rewrite": "pass", "activity": "pass",
		"pending_locks": "pass", "replica_lag": "pass", "table_size": "pass",
		"lock_timeout": "pass",
	}
	got := map[string]string{}
	for _, check := range report.Checks {
		got[check.Name] = check.Status
	}
	for name, status := range want {
		if got[name] != status {
			t.Errorf("check %s = %q, want %q (checks %+v)", name, got[name], status,
				report.Checks)
		}
	}
	if report.TableSizeBytes != 64<<20 || report.EstimatedRows != 900000 ||
		report.ReplicationLag != 1.5 || report.TableName != "app.orders" {
		t.Fatalf("report values = %+v", report)
	}
	if report.MeasuredAt != at.Format(time.RFC3339) {
		t.Fatalf("MeasuredAt = %q, want %q", report.MeasuredAt, at.Format(time.RFC3339))
	}
	if candidate.BlockedReason != "" {
		t.Fatalf("idle measured table blocked: %q", candidate.BlockedReason)
	}
}

// State transition: a measured active query turns the live preflight to
// warn, and the create_index_concurrently candidate needs review.
func TestDDLPreflightContract_MeasuredActivityBlocksCandidate(t *testing.T) {
	risk := contractRisk(time.Now().UTC())
	risk.ActiveQueries, risk.LongestQuerySec = 3, 42
	candidate := projectedPreflight(t, risk)
	if candidate.BlockedReason != "live DDL preflight has warnings; review required" {
		t.Fatalf("BlockedReason = %q", candidate.BlockedReason)
	}
	for _, check := range candidate.DDLPreflight.Checks {
		if check.Name == "activity" &&
			(check.Status != "warn" || !strings.Contains(check.Detail, "3 active")) {
			t.Fatalf("activity check = %+v", check)
		}
	}
}

// Nil: a finding persisted without an assessment keeps every live check
// unknown, never pass, and is not blocked by a warning it never measured.
func TestDDLPreflightContract_NoAssessmentStaysUnknown(t *testing.T) {
	candidate := projectedPreflight(t, nil)
	for _, check := range candidate.DDLPreflight.Checks {
		switch check.Name {
		case "activity", "pending_locks", "replica_lag", "table_size", "lock_timeout":
			if check.Status != "unknown" {
				t.Errorf("check %s = %q without an assessment, want unknown",
					check.Name, check.Status)
			}
		}
	}
	if candidate.DDLPreflight.MeasuredAt != "" {
		t.Fatalf("MeasuredAt = %q without an assessment", candidate.DDLPreflight.MeasuredAt)
	}
}
