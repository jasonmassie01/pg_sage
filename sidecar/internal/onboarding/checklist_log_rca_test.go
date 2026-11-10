package onboarding

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// The first look's log_rca capability is not an extension: the extensions
// step neither lists it as missing nor depends on it.
func TestChecklistExtensionsIgnoreLogRCA(t *testing.T) {
	caps := []firstlook.Capability{
		{Name: firstlook.CapStatStatements, Status: firstlook.CapabilityOK},
		{Name: firstlook.CapHypoPG, Status: firstlook.CapabilityOK},
		{Name: firstlook.CapAutoExplain, Status: firstlook.CapabilityOK},
		{Name: firstlook.CapLogRCA, Status: firstlook.CapabilityMissing},
	}
	s := step(Checklist(ChecklistInput{Connected: true, Capabilities: caps}), StepExtensions)
	if !s.Done || strings.Contains(s.Detail, "log_rca") ||
		s.Detail != "pg_stat_statements, hypopg and auto_explain are ready." {
		t.Fatalf("extensions step = %+v, want done without log_rca", s)
	}
	only := []firstlook.Capability{{Name: firstlook.CapLogRCA, Status: firstlook.CapabilityOK}}
	if s := step(Checklist(ChecklistInput{Capabilities: only}), StepExtensions); s.Done {
		t.Fatalf("a ready log source marked the extensions step done: %+v", s)
	}
}
