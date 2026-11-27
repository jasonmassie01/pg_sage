package fleet

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// tuning-04: the capability report carries the pg_hint_plan
// application-effect facts, and a limitation names the missing one.

func TestHintPlanLimitation(t *testing.T) {
	if got := hintPlanLimitation(nil); got != "" {
		t.Fatalf("nil facts: %q", got)
	}
	if got := hintPlanLimitation(&tuner.HintPlanAvailability{ApplicationEffect: true}); got != "" {
		t.Fatalf("ready: %q", got)
	}
	got := hintPlanLimitation(&tuner.HintPlanAvailability{
		Reasons: []string{"enable_hint_table off", "privilege"}})
	for _, want := range []string{"pg_hint_plan", "application sessions",
		"enable_hint_table off; privilege", "advisory"} {
		if !strings.Contains(got, want) {
			t.Fatalf("limitation %q lacks %q", got, want)
		}
	}
}

// Nil pool: no probe, no invented facts.
func TestRuntimeCapabilitiesNilPoolHasNoHintFacts(t *testing.T) {
	caps := CollectProviderCapabilities(t.Context(), nil, "rds", nil)
	if caps.HintPlan != nil {
		t.Fatalf("nil pool invented hint facts: %+v", caps.HintPlan)
	}
}

// Integration: the CI server preloads pg_hint_plan with enable_hint_table
// off, so the report shows the facts and the limitation.
func TestRuntimeCapabilitiesReportsHintReadiness(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "tuning04_fleet")
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var installed bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM pg_extension WHERE extname = 'pg_hint_plan')`).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Skip("pg_hint_plan not installable on this server")
	}
	caps := CollectProviderCapabilities(t.Context(), pool, "self-managed", nil)
	hp := caps.HintPlan
	if hp == nil || hp.ApplicationEffect || hp.HintTableOn || !hp.Preload {
		t.Fatalf("hint facts = %+v", hp)
	}
	found := false
	for _, l := range caps.Limitations {
		found = found || strings.Contains(l, "enable_hint_table off")
	}
	if !found {
		t.Fatalf("limitations %q lack enable_hint_table off", caps.Limitations)
	}
	doc, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"hint_plan":{`, `"application_effect":false`,
		`"hint_table_on":false`, `"preload":true`} {
		if !strings.Contains(string(doc), want) {
			t.Fatalf("report JSON lacks %s: %s", want, doc)
		}
	}
}
