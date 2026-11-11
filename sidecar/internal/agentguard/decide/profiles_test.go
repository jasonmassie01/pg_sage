package decide

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// agentgov-06: the gate's default profiles are agentguard's built-in
// table, so the store's widening check and D4 judge the same grants, and a
// principal whose profile is unknown is told which profiles exist.

func TestDefaultProfilesMirrorTheBuiltInTable(t *testing.T) {
	shipped := DefaultProfiles()
	cat := agentguard.ProfileCatalog()
	if len(shipped) != len(cat) {
		t.Fatalf("decide has %d profiles, agentguard %d", len(shipped), len(cat))
	}
	for _, g := range cat {
		p, ok := shipped.Profile(g.Name)
		if !ok {
			t.Fatalf("profile %q is in agentguard but not in the gate", g.Name)
		}
		if string(p.EnvCeiling) != string(g.EnvCeiling) || p.DirectLane != g.DirectLane {
			t.Fatalf("%s: gate %+v, store %+v", g.Name, p, g)
		}
		if len(p.Classes) != len(g.Classes) {
			t.Fatalf("%s: gate classes %v, store %v", g.Name, p.Classes, g.Classes)
		}
		for i, c := range g.Classes {
			if !Capability(c).Valid() || p.Classes[i] != Capability(c) {
				t.Fatalf("%s: class %q is not the gate's %q", g.Name, c, p.Classes[i])
			}
		}
	}
}

func TestDecideD4UnknownProfileNamesTheProfiles(t *testing.T) {
	p := activePrincipal()
	p.Profile = "codng-agent"
	cfg, _ := fullConfig(p)
	cfg.Profiles = DefaultProfiles()
	v := decideWith(t, cfg, agentRead())
	wantDenied(t, v, ReasonCapability, "D4")
	if !strings.Contains(v.Detail, `"codng-agent"`) {
		t.Fatalf("detail %q does not name the profile", v.Detail)
	}
	if strings.Contains(v.Fix, "agents.profiles") {
		t.Fatalf("fix %q points at a configuration key that does not exist", v.Fix)
	}
	for _, name := range agentguard.ProfileNames() {
		if !strings.Contains(v.Fix, name) {
			t.Fatalf("fix %q does not name profile %s", v.Fix, name)
		}
	}
}
