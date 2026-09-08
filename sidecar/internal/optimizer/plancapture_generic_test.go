package optimizer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestGenericPlanCapturesUnboundParameters(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	var version int
	if err := pool.QueryRow(t.Context(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 160000 {
		t.Skip("GENERIC_PLAN requires PostgreSQL16+")
	}
	if _, err := pool.Exec(t.Context(),
		"CREATE TABLE generic_plan_parameter_test (id int PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	planner := NewPlanCapture(pool, version, false, false, "generic_plan", noopLog2)
	plans, source := planner.CapturePlans(t.Context(), []collector.QueryStats{{QueryID: 4242,
		Query: "SELECT id FROM public.generic_plan_parameter_test WHERE id=$1"}})
	if source != "generic_plan" || len(plans) != 1 || plans[0].QueryID != 4242 ||
		plans[0].ScanType == "" {
		t.Fatalf("unbound parameter plan missing: source=%s plans=%+v", source, plans)
	}
}
