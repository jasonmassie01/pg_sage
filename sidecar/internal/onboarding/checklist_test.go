package onboarding

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// No concurrent access tests: Checklist is a pure function of its input.

func readyCaps() []firstlook.Capability {
	return []firstlook.Capability{
		{Name: firstlook.CapStatStatements, Status: firstlook.CapabilityOK},
		{Name: firstlook.CapHypoPG, Status: firstlook.CapabilityMissing},
		{Name: firstlook.CapAutoExplain, Status: firstlook.CapabilityOK},
	}
}

func step(steps []Step, id string) Step {
	for _, s := range steps {
		if s.ID == id {
			return s
		}
	}
	return Step{}
}

func TestChecklistOrderAndIDs(t *testing.T) {
	steps := Checklist(ChecklistInput{})
	want := []string{StepConnected, StepExtensions, StepFirstLook, StepMCPToken,
		StepNotifications, StepGrantMore}
	if len(steps) != len(want) {
		t.Fatalf("steps = %+v", steps)
	}
	for i, id := range want {
		if steps[i].ID != id || steps[i].Label == "" {
			t.Fatalf("step %d = %+v, want %s with a label", i, steps[i], id)
		}
	}
	for _, s := range steps {
		if s.Done {
			t.Fatalf("zero input marked %s done", s.ID)
		}
	}
	if !step(steps, StepMCPToken).Optional || !step(steps, StepNotifications).Optional ||
		step(steps, StepGrantMore).Optional {
		t.Fatal("optional flags wrong: MCP and notifications are optional, grant more is not")
	}
}

func TestChecklistAllDone(t *testing.T) {
	in := ChecklistInput{Connected: true, Capabilities: readyCaps(), FirstLookReady: true,
		FirstLookItems: 4, MCPEnabled: true, MCPToken: bp(true), Notifications: bp(true),
		TrustLevel: "advisory", TTFFSeconds: fp(11)}
	for _, s := range Checklist(in) {
		if !s.Done {
			t.Fatalf("step %+v not done", s)
		}
	}
	ext := step(Checklist(in), StepExtensions)
	if !strings.Contains(ext.Detail, "hypopg") {
		t.Fatalf("extensions detail %q does not name the missing optional hypopg", ext.Detail)
	}
	fl := step(Checklist(in), StepFirstLook)
	if !strings.Contains(fl.Detail, "4 findings") || !strings.Contains(fl.Detail, "11 s") {
		t.Fatalf("first look detail = %q", fl.Detail)
	}
}

func TestChecklistReadOnlyAndMissingStatements(t *testing.T) {
	caps := []firstlook.Capability{{Name: firstlook.CapStatStatements,
		Status: firstlook.CapabilityNotLoaded}}
	steps := Checklist(ChecklistInput{Connected: true, Capabilities: caps,
		FirstLookReady: true, TrustLevel: "observation"})
	if s := step(steps, StepExtensions); s.Done ||
		!strings.Contains(s.Detail, "pg_stat_statements") {
		t.Fatalf("extensions = %+v, want not done naming pg_stat_statements", s)
	}
	g := step(steps, StepGrantMore)
	if g.Done || !strings.Contains(strings.ToLower(g.Detail), "read-only") {
		t.Fatalf("grant more = %+v, want not done and read-only stated", g)
	}
	if fl := step(steps, StepFirstLook); !fl.Done ||
		!strings.Contains(fl.Detail, "No problems") {
		t.Fatalf("empty first look = %+v", fl)
	}
}

func TestChecklistUnknownAndDisabledStates(t *testing.T) {
	steps := Checklist(ChecklistInput{Connected: true, MCPEnabled: false})
	if s := step(steps, StepMCPToken); s.Done || !strings.Contains(s.Detail, "mcp.enabled") {
		t.Fatalf("mcp off = %+v, want the config key named", s)
	}
	steps = Checklist(ChecklistInput{Connected: true, MCPEnabled: true})
	if s := step(steps, StepMCPToken); s.Done || !strings.Contains(strings.ToLower(s.Detail),
		"admin") {
		t.Fatalf("mcp unknown = %+v, want admin visibility explained", s)
	}
	if s := step(steps, StepExtensions); s.Done || s.Detail == "" {
		t.Fatalf("extensions before the first look = %+v", s)
	}
	if s := step(steps, StepGrantMore); s.Done {
		t.Fatalf("unknown trust level marked granted: %+v", s)
	}
	for _, s := range steps {
		if s.Link != "" && !strings.HasPrefix(s.Link, "#/") {
			t.Fatalf("step %s link %q is not an in-app route", s.ID, s.Link)
		}
	}
}
