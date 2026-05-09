package analyzer

import "testing"

func TestClassifyAgentWorkload_AttributedApplication(t *testing.T) {
	got := ClassifyAgentWorkload(AgentWorkloadSignal{
		ApplicationName: "agent:planner-7",
		QueryText:       "select 1",
	})
	if got.Kind != AgentWorkloadAttributed {
		t.Fatalf("expected attributed, got %#v", got)
	}
	if got.AgentID != "agent:planner-7" {
		t.Fatalf("expected stable agent id, got %q", got.AgentID)
	}
}

func TestClassifyAgentWorkload_AttributedSQLComment(t *testing.T) {
	got := ClassifyAgentWorkload(AgentWorkloadSignal{
		QueryText: "/* agent_id=researcher_2 */ select * from docs",
	})
	if got.Kind != AgentWorkloadAttributed {
		t.Fatalf("expected attributed SQL comment, got %#v", got)
	}
	if got.AgentID != "agent:researcher_2" {
		t.Fatalf("expected SQL comment agent id, got %q", got.AgentID)
	}
}

func TestClassifyAgentWorkload_Unattributed(t *testing.T) {
	got := ClassifyAgentWorkload(AgentWorkloadSignal{QueryText: "select * from orders"})
	if got.Kind != AgentWorkloadUnattributed {
		t.Fatalf("expected unattributed workload, got %#v", got)
	}
	if len(got.Evidence) == 0 {
		t.Fatal("expected evidence for unattributed workload")
	}
}

func TestClassifyAgentWorkload_Ephemeral(t *testing.T) {
	got := ClassifyAgentWorkload(AgentWorkloadSignal{
		ApplicationName: "sandbox-agent",
		QueryText:       "create temporary table t as select 1",
	})
	if got.Kind != AgentWorkloadEphemeral {
		t.Fatalf("expected ephemeral workload, got %#v", got)
	}
}

func TestAgentWorkloadFinding(t *testing.T) {
	finding := AgentWorkloadFinding(AgentWorkloadSignal{
		DatabaseName: "agent_db",
		QueryText:    "select * from generated_table",
	})
	if finding == nil {
		t.Fatal("expected unattributed finding")
	}
	if finding.Category != "agent_workload" {
		t.Fatalf("expected agent_workload category, got %q", finding.Category)
	}
	if finding.ActionRisk != "" || finding.RecommendedSQL != "" {
		t.Fatalf("expected advisory-only finding, got %#v", finding)
	}
}

func TestAgentWorkloadFinding_AttributedIsQuiet(t *testing.T) {
	finding := AgentWorkloadFinding(AgentWorkloadSignal{
		ApplicationName: "agent:stable-writer",
		QueryText:       "insert into events values (1)",
	})
	if finding != nil {
		t.Fatalf("expected attributed workload to stay quiet, got %#v", finding)
	}
}
