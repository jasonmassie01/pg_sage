package migration

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// migration-01: the advisor's measurements reach the finding detail, so
// the case preflight reads them instead of reporting every check unknown.

var measuredAt = time.Date(2026, 11, 9, 12, 30, 0, 0, time.UTC)

// measurementDetailKeys are the keys migration-01 adds to the detail.
var measurementDetailKeys = []string{"lock_level", "requires_rewrite",
	"table_size_bytes", "estimated_rows", "stats_known", "active_queries",
	"longest_query_sec", "pending_locks", "replication_lag",
	"estimated_lock_ms", "affected_table", "measured_at"}

func measuredRisk() *DDLRisk {
	return &DDLRisk{
		Statement: "VACUUM FULL app.orders", RuleID: "ddl_vacuum_full",
		LockLevel: "ACCESS EXCLUSIVE", RequiresRewrite: true,
		TableName: "orders", SchemaName: "app",
		TableSizeBytes: 4 << 20, EstimatedRows: 50000, StatsKnown: true,
		ActiveQueries: 2, LongestQuerySec: 7.5, PendingLocks: 1,
		ReplicationLag: 3, EstimatedLockMs: 120, RiskScore: 0.9,
		Measured: DDLMeasured{TableStats: true, Activity: true,
			PendingLocks: true, ReplicationLag: true},
		MeasuredAt: measuredAt,
	}
}

func TestFindingDetail_WritesEveryMeasurement(t *testing.T) {
	inc := migrationIncident("")
	got, ok := FindingFromIncident(7, "VACUUM FULL app.orders", inc, measuredRisk())
	if !ok {
		t.Fatal("FindingFromIncident returned ok=false")
	}
	want := map[string]any{
		"lock_level": "ACCESS EXCLUSIVE", "requires_rewrite": true,
		"table_size_bytes": int64(4 << 20), "estimated_rows": int64(50000),
		"stats_known": true, "active_queries": 2, "longest_query_sec": 7.5,
		"pending_locks": 1, "replication_lag": 3.0,
		"estimated_lock_ms": int64(120), "affected_table": "app.orders",
		"measured_at": "2026-11-09T12:30:00Z",
	}
	for key, value := range want {
		if got.Detail[key] != value {
			t.Errorf("detail[%q] = %#v (%T), want %#v (%T)",
				key, got.Detail[key], got.Detail[key], value, value)
		}
	}
	if got.Detail["pid"] != 7 || got.Detail["rule_id"] != "ddl_index_not_concurrent" {
		t.Fatalf("existing keys changed: pid=%#v rule_id=%#v",
			got.Detail["pid"], got.Detail["rule_id"])
	}
}

// Nil/empty: without an assessment (LLM fallback, queue-only incident)
// no measurement key is written, so the preflight stays unknown.
func TestFindingDetail_NilRiskWritesNoMeasurement(t *testing.T) {
	got, ok := FindingFromIncident(7, "VACUUM FULL t", migrationIncident(""), nil)
	if !ok {
		t.Fatal("FindingFromIncident returned ok=false")
	}
	for _, key := range measurementDetailKeys {
		if _, present := got.Detail[key]; present {
			t.Errorf("detail[%q] = %#v written without an assessment", key, got.Detail[key])
		}
	}
}

// Error propagation: a live read that failed leaves its value zero; the
// key is absent so the zero is never reported as a passing measurement.
func TestFindingDetail_FailedReadsAreAbsent(t *testing.T) {
	risk := measuredRisk()
	risk.Measured = DDLMeasured{ReplicationLag: true}
	risk.TableSizeBytes, risk.EstimatedRows, risk.StatsKnown = 0, 0, false
	got, _ := FindingFromIncident(7, "VACUUM FULL t", migrationIncident(""), risk)
	for _, key := range []string{"table_size_bytes", "estimated_rows",
		"active_queries", "longest_query_sec", "pending_locks", "estimated_lock_ms"} {
		if _, present := got.Detail[key]; present {
			t.Errorf("detail[%q] = %#v written for a failed read", key, got.Detail[key])
		}
	}
	if got.Detail["replication_lag"] != 3.0 || got.Detail["stats_known"] != false {
		t.Fatalf("replication_lag=%#v stats_known=%#v, want 3 and false",
			got.Detail["replication_lag"], got.Detail["stats_known"])
	}
}

// Boundary: a measured zero is a measurement (an idle table), and an
// assessment without a pool has no measured_at.
func TestFindingDetail_MeasuredZeroAndNoPool(t *testing.T) {
	risk := measuredRisk()
	risk.ActiveQueries, risk.PendingLocks = 0, 0
	got, _ := FindingFromIncident(7, "VACUUM FULL t", migrationIncident(""), risk)
	if got.Detail["active_queries"] != 0 || got.Detail["pending_locks"] != 0 {
		t.Fatalf("measured zeros dropped: active=%#v pending=%#v",
			got.Detail["active_queries"], got.Detail["pending_locks"])
	}
	unmeasured := &DDLRisk{RuleID: "ddl_cluster", LockLevel: "ACCESS EXCLUSIVE",
		RequiresRewrite: true, TableName: "t"}
	got, _ = FindingFromIncident(7, "CLUSTER t", migrationIncident(""), unmeasured)
	if _, present := got.Detail["measured_at"]; present {
		t.Fatalf("measured_at = %#v without a live read", got.Detail["measured_at"])
	}
	if got.Detail["affected_table"] != "public.t" ||
		got.Detail["lock_level"] != "ACCESS EXCLUSIVE" {
		t.Fatalf("affected_table=%#v lock_level=%#v", got.Detail["affected_table"],
			got.Detail["lock_level"])
	}
}

// AnalyzeRisk hands back the winning assessment with the incident; the
// Analyze wrapper returns the same incident.
func TestAdvisor_AnalyzeRiskReturnsWinningRisk(t *testing.T) {
	cfg := &config.MigrationConfig{Enabled: true}
	a := NewAdvisor(nil, cfg, 170000, "db1", func(string, string, ...any) {}, nil)
	inc, risk, err := a.AnalyzeRisk(context.Background(), "VACUUM FULL public.orders")
	if err != nil || inc == nil || risk == nil {
		t.Fatalf("AnalyzeRisk = %v, %v, %v; want incident and risk", inc, risk, err)
	}
	if risk.RuleID != "ddl_vacuum_full" || risk.LockLevel != "ACCESS EXCLUSIVE" ||
		!risk.RequiresRewrite || inc.SignalIDs[0] != risk.RuleID {
		t.Fatalf("risk = %+v, incident signals %v", risk, inc.SignalIDs)
	}
	if !risk.MeasuredAt.IsZero() || risk.Measured != (DDLMeasured{}) {
		t.Fatalf("no pool, yet risk reports measurements: %+v", risk.Measured)
	}
	plain, err := a.Analyze(context.Background(), "VACUUM FULL public.orders")
	if err != nil || plain == nil || plain.RootCause != inc.RootCause {
		t.Fatalf("Analyze = %v, %v; want the same incident as AnalyzeRisk", plain, err)
	}
	inc, risk, err = a.AnalyzeRisk(context.Background(), "SELECT 1")
	if inc != nil || risk != nil || err != nil {
		t.Fatalf("AnalyzeRisk(SELECT 1) = %v, %v, %v; want nils", inc, risk, err)
	}
}

// State transitions: the detector keeps the analysed risk with the
// tracked DDL, so every re-upsert carries the same detection-time values.
func TestDetector_ReUpsertCarriesMeasurements(t *testing.T) {
	ddls := []activeDDL{waitingDDL(300)}
	sink := &captureSink{}
	cfg := &config.MigrationConfig{Enabled: true, ActivityPolling: true}
	advisor := NewAdvisor(nil, cfg, 170000, "db1", func(string, string, ...any) {}, nil)
	d := NewDetector(nil, advisor, cfg, func(string, string, ...any) {}).
		WithFindingSink(sink)
	d.activity = func(context.Context) ([]activeDDL, error) { return ddls, nil }
	analyses := 0
	d.analyze = func(context.Context, activeDDL) (*rca.Incident, *DDLRisk, error) {
		analyses++
		return warningIncident(), measuredRisk(), nil
	}
	for i := 0; i < 2; i++ {
		if _, err := d.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if analyses != 1 || sink.count() != 2 {
		t.Fatalf("analyses=%d upserts=%d, want 1 and 2", analyses, sink.count())
	}
	last := sink.last(t)
	if last.Detail["table_size_bytes"] != int64(4<<20) ||
		last.Detail["measured_at"] != "2026-11-09T12:30:00Z" {
		t.Fatalf("re-upsert detail size=%#v measured_at=%#v",
			last.Detail["table_size_bytes"], last.Detail["measured_at"])
	}
}

// No concurrent access test for FindingFromIncident: it is a pure
// function of its arguments; the detector's tracked map is covered by
// TestDetector_QueuedDDLPIDConcurrent.
