package migration

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/rca"
)

type recordingFindingSink struct {
	inputs []MigrationSafetyFinding
	err    error
}

func (s *recordingFindingSink) UpsertMigrationSafetyFinding(
	_ context.Context,
	input MigrationSafetyFinding,
) (int64, error) {
	s.inputs = append(s.inputs, input)
	return int64(len(s.inputs)), s.err
}

func TestFindingFromIncidentKeepsExecutableSafeSQL(t *testing.T) {
	incident := migrationIncident("CREATE INDEX CONCURRENTLY idx ON t (id)")

	got, ok := FindingFromIncident(123, "CREATE INDEX idx ON t (id)", incident)

	if !ok {
		t.Fatal("FindingFromIncident returned false")
	}
	if got.RuleID != "ddl_index_not_concurrent" {
		t.Fatalf("RuleID = %q", got.RuleID)
	}
	if got.RecommendedSQL != incident.RecommendedSQL {
		t.Fatalf("RecommendedSQL = %q", got.RecommendedSQL)
	}
	if got.Detail["original_sql"] != "CREATE INDEX idx ON t (id)" {
		t.Fatalf("original_sql detail = %#v", got.Detail["original_sql"])
	}
	if got.ObjectIdentifier == "" {
		t.Fatal("ObjectIdentifier was empty")
	}
}

func TestFindingFromIncidentDropsAdvisoryOnlySafeAlternative(t *testing.T) {
	incident := migrationIncident("New column + trigger + backfill + swap")

	got, ok := FindingFromIncident(123, "ALTER TABLE t ALTER c TYPE bigint", incident)

	if !ok {
		t.Fatal("FindingFromIncident returned false")
	}
	if got.RecommendedSQL != "" {
		t.Fatalf("RecommendedSQL = %q, want empty", got.RecommendedSQL)
	}
	if got.Detail["safe_alternative"] != incident.RecommendedSQL {
		t.Fatalf("safe_alternative detail = %#v", got.Detail["safe_alternative"])
	}
}

func TestDetectorPersistFindingCallsSink(t *testing.T) {
	sink := &recordingFindingSink{}
	detector := &Detector{
		findingSink: sink,
		logFn:       func(string, string, ...any) {},
	}

	detector.persistFinding(
		context.Background(), 321, "CREATE INDEX idx ON t (id)",
		migrationIncident("CREATE INDEX CONCURRENTLY idx ON t (id)"),
	)

	if len(sink.inputs) != 1 {
		t.Fatalf("sink inputs = %d, want 1", len(sink.inputs))
	}
	if sink.inputs[0].Detail["pid"] != 321 {
		t.Fatalf("pid detail = %#v", sink.inputs[0].Detail["pid"])
	}
}

func TestDetectorPersistFindingLogsSinkError(t *testing.T) {
	sink := &recordingFindingSink{err: errors.New("write failed")}
	var logged bool
	detector := &Detector{
		findingSink: sink,
		logFn: func(level, msg string, args ...any) {
			logged = level == "warn" && msg != "" && len(args) == 1
		},
	}

	detector.persistFinding(
		context.Background(), 321, "CREATE INDEX idx ON t (id)",
		migrationIncident("CREATE INDEX CONCURRENTLY idx ON t (id)"),
	)

	if len(sink.inputs) != 1 {
		t.Fatalf("sink inputs = %d, want 1", len(sink.inputs))
	}
	if !logged {
		t.Fatal("expected sink error to be logged")
	}
}

func migrationIncident(safeSQL string) *rca.Incident {
	return &rca.Incident{
		Severity:        "warning",
		RootCause:       "Dangerous DDL: ddl_index_not_concurrent",
		AffectedObjects: []string{"public.t"},
		SignalIDs:       []string{"ddl_index_not_concurrent"},
		RecommendedSQL:  safeSQL,
		ActionRisk:      "risk_score=0.70",
		Source:          "schema_advisor",
		Confidence:      0.7,
		DatabaseName:    "prod",
		CausalChain: []rca.ChainLink{{
			Order:       1,
			Signal:      "ddl_index_not_concurrent",
			Description: "CREATE INDEX without CONCURRENTLY blocks writes",
			Evidence:    "CREATE INDEX idx ON t (id)",
		}},
	}
}
