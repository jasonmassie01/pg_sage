package migration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migration-01 review: measured_at says the live values were measured at
// detection. When every live read failed nothing was measured, so the
// assessment has no measurement time and the finding detail carries
// neither measured_at nor a lock-hold estimate that would rest on it.
func TestAssess_EveryReadFailedHasNoMeasuredAt(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	pool.Close() // every read now fails with "closed pool"
	ra := NewRiskAssessor(pool, func(string, string, ...any) {})
	risk, err := ra.Assess(context.Background(), DDLClassification{
		Statement: "ALTER TABLE app.orders RENAME TO orders_old", RuleID: "ddl_rename",
		LockLevel: "ACCESS EXCLUSIVE", TableName: "orders", SchemaName: "app",
	})
	if err != nil || risk == nil {
		t.Fatalf("Assess = %v, %v", risk, err)
	}
	if risk.Measured != (DDLMeasured{}) {
		t.Fatalf("Measured = %+v on a closed pool, want nothing measured", risk.Measured)
	}
	if !risk.MeasuredAt.IsZero() {
		t.Errorf("MeasuredAt = %v with every read failed, want zero", risk.MeasuredAt)
	}
	detail := map[string]any{}
	addRiskMeasurements(detail, risk)
	for _, key := range []string{"measured_at", "estimated_lock_ms"} {
		if _, present := detail[key]; present {
			t.Errorf("detail[%q] = %#v with every read failed", key, detail[key])
		}
	}
	if detail["lock_level"] != "ACCESS EXCLUSIVE" || detail["affected_table"] != "app.orders" {
		t.Fatalf("classification keys lost: lock_level=%#v affected_table=%#v",
			detail["lock_level"], detail["affected_table"])
	}
}

// Boundary: one successful read is a measurement, so the time is kept.
func TestMeasuredAny(t *testing.T) {
	if (DDLMeasured{}).any() {
		t.Fatal("empty DDLMeasured reports a measurement")
	}
	for _, m := range []DDLMeasured{{TableStats: true}, {Activity: true},
		{PendingLocks: true}, {ReplicationLag: true}} {
		if !m.any() {
			t.Errorf("%+v reports no measurement", m)
		}
	}
}
