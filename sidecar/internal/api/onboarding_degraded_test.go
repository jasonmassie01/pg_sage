package api

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// No concurrent access tests: entryFor is a pure function of the report the
// reader returns; the handler's concurrency is the reader's.

func onboardingStep(t *testing.T, db map[string]any, id string) map[string]any {
	t.Helper()
	for _, s := range db["steps"].([]any) {
		if m := s.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("no step %s in %v", id, db["steps"])
	return nil
}

func degradedFake(checks []firstlook.Check) *fakeOnboardingReader {
	f := newFakeOnboarding()
	db := f.dbs["app"]
	report := *db.FirstLook
	report.Items, report.Checks = nil, checks
	db.FirstLook = &report
	f.dbs["app"] = db
	return f
}

func TestOnboardingReportsDegradedCatalogChecks(t *testing.T) {
	f := degradedFake([]firstlook.Check{
		{Rule: firstlook.RuleInvalidIndex, Status: firstlook.CheckOK},
		{Rule: firstlook.RuleNeverScannedIndex, Status: firstlook.CheckFinding,
			Truncated: true, Note: "50 more not shown"},
		{Rule: firstlook.RuleSequenceRunway, Status: firstlook.CheckDegraded,
			Note: "GRANT SELECT ON ALL SEQUENCES IN SCHEMA app TO <pg_sage role>;"},
		{Rule: firstlook.RuleTableBloat, Status: firstlook.CheckDegraded, Note: "timeout"},
		{Rule: "AP-01", Section: firstlook.SectionAgentPosture,
			Status: firstlook.CheckDegraded},
	})
	_, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding?database=app")
	db := body["databases"].([]any)[0].(map[string]any)
	fl := db["first_look"].(map[string]any)
	if fl["ready"] != true || fl["items"] != float64(0) || fl["checks"] != float64(4) ||
		fl["degraded"] != float64(2) || fl["truncated"] != float64(1) {
		t.Fatalf("first look summary = %v, want 4 catalog checks, 2 degraded, 1 truncated", fl)
	}
	s := onboardingStep(t, db, onboarding.StepFirstLook)
	detail := s["detail"].(string)
	if strings.Contains(detail, "No problems found in the catalog") ||
		!strings.Contains(detail, "2 of 4 checks could not run") || s["done"] != true {
		t.Fatalf("first look step = %v", s)
	}
}

func TestOnboardingEveryCatalogCheckDegradedIsNotDone(t *testing.T) {
	f := degradedFake([]firstlook.Check{
		{Rule: firstlook.RuleInvalidIndex, Status: firstlook.CheckDegraded, Note: "x"},
		{Rule: firstlook.RuleXIDRunway, Status: firstlook.CheckDegraded, Note: "y"},
	})
	_, body := obGet(t, onboardingHandler(f), "/api/v1/onboarding")
	db := body["databases"].([]any)[0].(map[string]any)
	if s := onboardingStep(t, db, onboarding.StepFirstLook); s["done"] != false {
		t.Fatalf("every check degraded: step = %v, want not done", s)
	}
}

func TestOnboardingNilChecksKeepsTodaysFields(t *testing.T) {
	// Nil/empty: a stored report from before checks were counted.
	_, body := obGet(t, onboardingHandler(degradedFake(nil)), "/api/v1/onboarding")
	db := body["databases"].([]any)[0].(map[string]any)
	fl := db["first_look"].(map[string]any)
	if fl["checks"] != float64(0) || fl["degraded"] != float64(0) ||
		fl["truncated"] != float64(0) || fl["ready"] != true {
		t.Fatalf("nil checks: first look = %v", fl)
	}
	s := onboardingStep(t, db, onboarding.StepFirstLook)
	if s["done"] != true || !strings.Contains(s["detail"].(string), "No problems found") {
		t.Fatalf("nil checks: step = %v", s)
	}
	_, body = obGet(t, onboardingHandler(newFakeOnboarding()), "/api/v1/onboarding")
	before := body["databases"].([]any)[0].(map[string]any)["first_look"].(map[string]any)
	for _, key := range []string{"ready", "items", "finished_at", "duration_ms"} {
		if _, ok := before[key]; !ok {
			t.Fatalf("existing field %q missing from %v", key, before)
		}
	}
}
