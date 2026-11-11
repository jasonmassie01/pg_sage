package onboarding

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// firstlook-05: a missing pg_stat_statements points at the re-check, which
// updates the step without a restart; a ready one does not.
func TestExtensionsStepPointsAtTheRecheck(t *testing.T) {
	missing := extensionsStep(ChecklistInput{Capabilities: []firstlook.Capability{
		{Name: firstlook.CapStatStatements, Status: firstlook.CapabilityMissing}}})
	if missing.Done || !strings.Contains(missing.Detail, "Re-check updates this step "+
		"without a restart") || !strings.Contains(missing.Detail, "pg_stat_statements is missing") {
		t.Fatalf("missing step = %+v, want not done with the re-check named", missing)
	}
	ready := extensionsStep(ChecklistInput{Capabilities: []firstlook.Capability{
		{Name: firstlook.CapStatStatements, Status: firstlook.CapabilityOK}}})
	if !ready.Done || strings.Contains(ready.Detail, "Re-check") {
		t.Fatalf("ready step = %+v, want done without the re-check note", ready)
	}
}
