package autoexplain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

func TestParseObservedPlanPreservesActualExecution(t *testing.T) {
	e := logwatch.LogEntry{Database: "postgres", Timestamp: time.Now(),
		Message: `duration: 12.345 ms  plan: {"Query Text":"SELECT 1","Query Identifier":42,` +
			`"Plan":{"Node Type":"Result","Total Cost":0.01,"Actual Rows":1}}`}
	p, err := ParseObservedPlan(e, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if p.QueryID != 42 || p.Query != "SELECT 1" || p.ExecutionMS != 12.345 ||
		p.TotalCost != 0.01 || p.CapturedAt != e.Timestamp || !json.Valid(p.JSON) {
		t.Fatalf("observed plan = %#v", p)
	}
}

func TestParseObservedPlanRejectsWrongIdentityAndInvalidPlans(t *testing.T) {
	for _, message := range []string{"", "duration: 1 ms statement: SELECT 1",
		"duration: 1 ms plan: {", `duration: 1 ms plan: {"Query Text":"SELECT 1"}`,
		`duration: 1 ms plan: {"Plan":{"Total Cost":1}}`,
		`duration: -1 ms plan: {"Query Text":"SELECT 1","Plan":{"Total Cost":1}}`} {
		e := logwatch.LogEntry{Database: "postgres", Timestamp: time.Now(), Message: message}
		if _, err := ParseObservedPlan(e, "postgres"); err == nil {
			t.Errorf("accepted %q", message)
		}
	}
	e := logwatch.LogEntry{Database: "other", Timestamp: time.Now(),
		Message: `duration: 1 ms plan: {"Query Text":"SELECT 1","Plan":{"Total Cost":1}}`}
	if _, err := ParseObservedPlan(e, "postgres"); err == nil {
		t.Fatal("cross-database plan")
	}
}

// No concurrent access tests: parsing is stateless and retains no shared references.
