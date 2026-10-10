package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/upkeep"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestAgentUpkeepConfigMapsTheAgentsSettings(t *testing.T) {
	c := config.DefaultConfig()
	c.Agents.Roles.RetireGraceDays = 3
	c.Agents.Broker.RotationDays = 2
	got := agentUpkeepConfig(c)
	want := upkeep.ConfigFrom(3, 2)
	want.Roles = agentRoleConfig(c) // the drift reconciler's expected limits
	if got != want {
		t.Fatalf("agentUpkeepConfig = %+v, want %+v", got, want)
	}
	rc := agentRoleConfig(c)
	if rc.BrokerCredentialRotation.Hours() != 48 {
		t.Fatalf("broker rotation = %v, want 48h", rc.BrokerCredentialRotation)
	}
	if err := rc.Validate(); err != nil {
		t.Fatalf("role config from the defaults is invalid: %v", err)
	}
}

func TestStartAgentUpkeepWithoutControlDoesNothing(t *testing.T) {
	// No control database: posture-only, no runner and no goroutines.
	if r := startAgentUpkeep(context.Background(), nil, nil, governanceLeader{}); r != nil {
		t.Fatalf("runner = %v, want nil without a control database", r)
	}
}

func TestUpkeepFenceWithoutElectionIsUnfenced(t *testing.T) {
	f, ok := upkeepFence(governanceLeader{}, "agent role upkeep")
	if !ok || f != (upkeep.Fence{}) {
		t.Fatalf("fence = %+v, %v; want an empty fence that may write", f, ok)
	}
}

func TestBackendViolationLinesOnlyWhenTheSetChanges(t *testing.T) {
	var noted backendNotes
	rep := upkeep.BackendReport{Clusters: 1, Violations: []upkeep.ClusterViolations{{
		ClusterKey: "k", Databases: []string{"app"},
		Findings: []agentguard.BackendFinding{{Role: "sage_agentb_aaaaaaaaaa", Sessions: 2,
			Problems: []string{"is not a registered agent role"}}}}}}
	first := noted.changes(rep)
	if len(first) != 1 || !strings.Contains(first[0], "sage_agentb_aaaaaaaaaa") ||
		!strings.Contains(first[0], "not a registered agent role") {
		t.Fatalf("first pass lines = %v", first)
	}
	if again := noted.changes(rep); len(again) != 0 {
		t.Fatalf("an unchanged violation is logged once, got %v", again)
	}
	cleared := noted.changes(upkeep.BackendReport{Clusters: 1})
	if len(cleared) != 1 || !strings.Contains(cleared[0], "no longer") {
		t.Fatalf("a cleared violation is logged once: %v", cleared)
	}
	if again := noted.changes(upkeep.BackendReport{Clusters: 1}); len(again) != 0 {
		t.Fatalf("nothing changed: %v", again)
	}
}

func TestUpkeepReportLines(t *testing.T) {
	rep := upkeep.Report{
		Done:       []upkeep.Outcome{{PrincipalID: "agp_a", ClusterKey: "k"}},
		Skipped:    []upkeep.Outcome{{PrincipalID: "agp_b", Reason: "no_approver", Detail: "d"}},
		Incomplete: []upkeep.Outcome{{PrincipalID: "agp_c", Fix: "REVOKE x"}},
		Withheld:   []upkeep.Outcome{{PrincipalID: "agp_d", Reason: "trust_level"}},
		Failed:     []upkeep.Outcome{{PrincipalID: "agp_e", Detail: "boom"}},
	}
	info, warn := upkeepLines(upkeep.JobRetireGrace, rep)
	if len(info) != 1 || !strings.Contains(info[0], "1") {
		t.Fatalf("info = %v", info)
	}
	joined := strings.Join(warn, "\n")
	for _, want := range []string{"agp_b", "no_approver", "agp_c", "revoke_incomplete",
		"agp_d", "trust_level", "agp_e", "boom"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("warnings %q miss %q", joined, want)
		}
	}
	if info, warn := upkeepLines(upkeep.JobBrokerRotation, upkeep.Report{}); len(info)+
		len(warn) != 0 {
		t.Fatalf("an empty pass logs nothing: %v %v", info, warn)
	}
}

func TestDriftLines(t *testing.T) {
	rep := upkeep.DriftReport{Drift: []upkeep.RoleDrift{{Role: "sage_agentb_a",
		Database: "app", Widening: []string{"has SELECT on relation public.t"},
		Corrected: []string{"REVOKE SELECT ON TABLE public.t FROM sage_agentb_a"}}},
		Failed: map[string]string{"billing/sage_agentb_a": "no PUBLIC baseline"}}
	lines := driftLines(rep)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"sage_agentb_a", "public.t", "corrected", "billing",
		"baseline"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("lines %q miss %q", joined, want)
		}
	}
	if len(driftLines(upkeep.DriftReport{})) != 0 {
		t.Fatal("a clean pass logs nothing")
	}
}
