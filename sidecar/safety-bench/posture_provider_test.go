package safetybench

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentposture"
)

// No concurrent-access tests here: the mapping and scoping functions are
// pure, and DetectorProvider holds only its immutable Config.

func TestPostureFindings_MapsEveryField(t *testing.T) {
	in := []agentposture.Finding{
		{Detector: "AP-03", Severity: agentposture.Critical, ObjectType: "table",
			Object: "sb_ps_exposed.profiles", Title: "t", Detail: "anon reads it"},
		{Detector: "AP-07", Severity: agentposture.Warning, ObjectType: "schema",
			Object: "sb_ps_public", Title: "t2", Detail: "PUBLIC: CREATE"},
	}
	got := postureFindings(in)
	want := []PostureFinding{
		{DetectorID: "AP-03", Severity: "critical", Object: "sb_ps_exposed.profiles",
			Detail: "anon reads it"},
		{DetectorID: "AP-07", Severity: "warning", Object: "sb_ps_public",
			Detail: "PUBLIC: CREATE"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPostureFindings_NilAndEmpty(t *testing.T) {
	if got := postureFindings(nil); len(got) != 0 {
		t.Fatalf("nil findings mapped to %+v", got)
	}
	if got := postureFindings([]agentposture.Finding{}); len(got) != 0 {
		t.Fatalf("empty findings mapped to %+v", got)
	}
}

func TestScopedFindings(t *testing.T) {
	found := []PostureFinding{
		{DetectorID: "AP-03", Object: "sb_ps_exposed.profiles"},
		{DetectorID: "AP-07", Object: "public"},
		{DetectorID: "AP-07", Object: "sb_ps_public"},
	}
	if got := scopedFindings("", found); len(got) != 3 {
		t.Errorf("empty scope kept %d findings, want all 3", len(got))
	}
	got := scopedFindings("sb_ps_public", found)
	if len(got) != 1 || got[0].Object != "sb_ps_public" {
		t.Errorf("scope sb_ps_public kept %+v, want only sb_ps_public", got)
	}
	if got := scopedFindings("sb_ps_absent", found); len(got) != 0 {
		t.Errorf("scope with no match kept %+v", got)
	}
	if got := scopedFindings("sb_ps_public", nil); len(got) != 0 {
		t.Errorf("nil findings kept %+v", got)
	}
}

// A finding outside the scenario's scope must not score as a match: the
// detectors read the whole database, and another scenario's object (or the
// server's own public schema on PostgreSQL 14) would otherwise pass it.
func TestScoring_OutOfScopeFindingIsMissing(t *testing.T) {
	found := []PostureFinding{{DetectorID: "AP-07", Object: "public"}}
	matched, missing := scoreDetectors([]string{"AP-07"},
		scopedFindings("sb_ps_public", found))
	if len(matched) != 0 || len(missing) != 1 || missing[0] != "AP-07" {
		t.Fatalf("matched=%v missing=%v, want AP-07 missing", matched, missing)
	}
}

func TestDetectorProvider_Name(t *testing.T) {
	p := NewDetectorProvider()
	if p.Name() != "agentposture" {
		t.Fatalf("Name() = %q, want agentposture", p.Name())
	}
	if p.Name() == (NotConnectedProvider{}).Name() {
		t.Fatal("the real provider must not report itself as not connected")
	}
	if len(p.Config.ClientPatterns) == 0 {
		t.Fatal("NewDetectorProvider must use the default client patterns")
	}
}

func TestDetectorProvider_NilPoolIsAnError(t *testing.T) {
	got, err := NewDetectorProvider().Findings(context.Background(), nil)
	if !errors.Is(err, agentposture.ErrNoPool) {
		t.Fatalf("Findings(nil pool) err = %v, want agentposture.ErrNoPool", err)
	}
	if got != nil {
		t.Fatalf("Findings(nil pool) returned findings %+v", got)
	}
}

var benchAgentRole = regexp.MustCompile(`sage_agentb_[a-z0-9_]*`)
var registeredAgentRole = regexp.MustCompile(`^sage_agentb_[a-z2-7]{10}$`)

// Every scenario is scoped to its own objects, and the agent scenarios use
// role names the detectors count as registered agents, dropped afterwards.
func TestPostureScenarios_ScopesAndAgentRoles(t *testing.T) {
	scenarios, err := PostureScenarios()
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	agentScenarios := 0
	for _, sc := range scenarios {
		if sc.Scope == "" {
			t.Errorf("%s has no scope", sc.ID)
		}
		if sc.SetupSQL == "" {
			t.Errorf("%s has no setup SQL", sc.ID)
		}
		roles := benchAgentRole.FindAllString(sc.SetupSQL, -1)
		if len(roles) == 0 {
			continue
		}
		agentScenarios++
		for _, r := range roles {
			if !registeredAgentRole.MatchString(r) {
				t.Errorf("%s: role %q is not a registered agent name", sc.ID, r)
			}
			if !strings.Contains(sc.TeardownSQL, "DROP ROLE IF EXISTS "+r) {
				t.Errorf("%s: teardown does not drop %s", sc.ID, r)
			}
		}
	}
	if agentScenarios < 2 {
		t.Fatalf("%d scenarios use agent roles, want at least 2", agentScenarios)
	}
}
