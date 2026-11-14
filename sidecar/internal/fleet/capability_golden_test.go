package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
	prov "github.com/pg-sage/sidecar/internal/provider"
)

// Characterization of capability readiness before and after the
// capabilities.go split (fleet-30). The golden file was written from the
// unsplit code; a pure move must reproduce it byte for byte.

var updateCapabilityGolden = flag.Bool("update-capability-golden", false,
	"rewrite testdata/capability_readiness.golden.json from the current code")

const capabilityGoldenPath = "testdata/capability_readiness.golden.json"

// goldenProviders are the self-managed, RDS, Cloud SQL, Azure and Lakebase
// fixtures the entry names, plus an unrecognised name.
var goldenProviders = []string{
	prov.Postgres, prov.RDS, prov.CloudSQL, prov.Azure, prov.Lakebase, "mystery",
}

// executeExplainer lets every family through, approval keyed to the
// contract's risk tier, so readiness reaches its execute branches.
func executeExplainer(
	_ context.Context, contracts []executor.ActionContract, _ bool,
) []executor.ActionPolicyDecision {
	out := make([]executor.ActionPolicyDecision, len(contracts))
	for i, c := range contracts {
		out[i] = executor.ActionPolicyDecision{
			Decision: executor.PolicyDecisionExecute, RiskTier: c.BaseRiskTier,
			RequiresApproval: c.BaseRiskTier != "safe", Guardrails: c.Guardrails,
		}
	}
	return out
}

type capabilityGoldenCase struct {
	Name         string                `json:"name"`
	Capabilities ProviderCapabilities  `json:"capabilities"`
	Summary      FleetReadinessSummary `json:"summary"`
}

type capabilityGolden struct {
	Cases        []capabilityGoldenCase `json:"cases"`
	FleetSummary FleetReadinessSummary  `json:"fleet_summary"`
	NilSummary   FleetReadinessSummary  `json:"nil_summary"`
}

func buildCapabilityGolden() capabilityGolden {
	explainers := []struct {
		name    string
		explain FamilyExplainer
	}{{"nil-executor", nil}, {"execute-all", executeExplainer}}
	var golden capabilityGolden
	var fleet []DatabaseStatus
	for _, provider := range goldenProviders {
		for _, ex := range explainers {
			for _, replica := range []bool{false, true} {
				caps := BuildProviderCapabilities(provider, replica, ex.explain)
				name := provider + "/" + ex.name
				if replica {
					name += "/replica"
				}
				db := DatabaseStatus{Name: name, Status: &InstanceStatus{Capabilities: caps}}
				fleet = append(fleet, db)
				golden.Cases = append(golden.Cases, capabilityGoldenCase{
					Name: name, Capabilities: caps,
					Summary: SummarizeReadiness([]DatabaseStatus{db}),
				})
			}
		}
	}
	fleet = append(fleet, DatabaseStatus{Name: "no-status"}, readyDatabase())
	golden.FleetSummary = SummarizeReadiness(fleet)
	golden.NilSummary = SummarizeReadiness(nil)
	return golden
}

// readyDatabase has every evidence field known, so the summary counts it
// ready rather than unknown or blocked.
func readyDatabase() DatabaseStatus {
	caps := BuildProviderCapabilities(prov.Postgres, false, executeExplainer)
	caps.Permissions = map[string]CapabilityStatus{"analyze": {Status: "available"}}
	caps.Extensions = map[string]string{"pg_stat_statements": "available"}
	caps.Blockers = readinessBlockers(caps)
	caps.ReadyForAutoSafe = readyForAutoSafe(caps)
	return DatabaseStatus{Name: "ready", Status: &InstanceStatus{Capabilities: caps}}
}

func TestCapabilityReadinessGolden(t *testing.T) {
	got, err := json.MarshalIndent(buildCapabilityGolden(), "", "  ")
	if err != nil {
		t.Fatalf("marshal capability golden: %v", err)
	}
	got = append(got, '\n')
	if *updateCapabilityGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(capabilityGoldenPath, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", capabilityGoldenPath, err)
		}
	}
	want, err := os.ReadFile(capabilityGoldenPath)
	if err != nil {
		t.Fatalf("read %s (run with -update-capability-golden on unsplit code): %v",
			capabilityGoldenPath, err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Fatalf("capability readiness drifted from %s:\n%s", capabilityGoldenPath, got)
	}
}

// The golden must actually exercise both outcomes, or a regression that
// blocked (or readied) everything would be pinned as correct.
func TestCapabilityGoldenCoversReadyBlockedAndUnknown(t *testing.T) {
	g := buildCapabilityGolden()
	s := g.FleetSummary
	if s.TotalDatabases != len(g.Cases)+2 {
		t.Fatalf("total = %d, want %d", s.TotalDatabases, len(g.Cases)+2)
	}
	if s.ReadyForAutoSafe != 1 || s.Unknown == 0 {
		t.Fatalf("fleet summary = %+v, want exactly 1 ready and some unknown", s)
	}
	if s.ReadyForAutoSafe+s.Blocked+s.Unknown != s.TotalDatabases {
		t.Fatalf("summary buckets do not add up: %+v", s)
	}
	if g.NilSummary != (FleetReadinessSummary{}) {
		t.Fatalf("SummarizeReadiness(nil) = %+v, want zero", g.NilSummary)
	}
	sawDirect := false
	for _, f := range g.Cases[0].Capabilities.ActionFamilies {
		if f.ActionType == "promote_role_work_mem" {
			sawDirect = f.BlockedReason == "direct execution is not implemented" &&
				f.Decision == executor.PolicyDecisionBlocked
		}
	}
	if !sawDirect {
		t.Fatal("promote_role_work_mem must stay blocked: direct execution is not implemented")
	}
}
