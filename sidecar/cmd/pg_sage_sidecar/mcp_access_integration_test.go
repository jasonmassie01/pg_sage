package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/policy"
)

func TestMCPRuntimeReadsEvidenceAndProposalCannotChangeAuthority(t *testing.T) {
	state, _, _ := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx := context.Background()
	current, err := policy.NewStore(state.Pool).Bootstrap(ctx, policy.Scope{}, "staffed", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	access := &fleetMCPAccess{manager: fleetMgr, fallback: state.Pool}
	before, err := access.GetPolicy(ctx, mcp.PolicyRequest{})
	if err != nil || before.Version != current.Version || before.Profile != current.Profile {
		t.Fatalf("policy not read from selected runtime: %+v err=%v", before, err)
	}
	proposal, err := access.ProposePolicyChangeDryRun(ctx,
		mcp.PolicyProposalRequest{Delta: json.RawMessage(`{}`)})
	if err != nil || proposal.ProposalID == 0 {
		t.Fatalf("dry run not durably proposed: %+v err=%v", proposal, err)
	}
	after, err := access.GetPolicy(ctx, mcp.PolicyRequest{})
	if err != nil || after.Version != before.Version || after.Profile != before.Profile {
		t.Fatalf("proposal altered active authority: %+v err=%v", after, err)
	}
	seedRuntimeLedger(t, state.Pool)
	entries, err := access.GetLedger(ctx, mcp.LedgerRequest{
		Filter: json.RawMessage(`{"feature":"runtime_access_fixture","limit":1}`)})
	if err != nil || len(entries.Entries) != 1 || entries.Entries[0].Decision != "parked" ||
		entries.Entries[0].EvidenceID != "runtime-read-fixture" {
		t.Fatalf("ledger filter lost exact evidence: %+v err=%v", entries, err)
	}
	value, err := access.GetValue(ctx)
	if err != nil || len(value) == 0 {
		t.Fatalf("value report absent: keys=%d err=%v", len(value), err)
	}
	guarantees, err := access.GetGuaranteeStatus(ctx)
	if err != nil || guarantees.XID["oldest_database_age"] == nil ||
		guarantees.WAL["registered_consumers"] == nil || guarantees.Schema["declared_contracts"] == nil {
		t.Fatalf("guarantee evidence incomplete: %+v err=%v", guarantees, err)
	}
}

func seedRuntimeLedger(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := ledger.NewService(ledger.NewPostgresRepository(pool)).RecordDecision(
		context.Background(), ledger.DecisionInput{Feature: "runtime_access_fixture",
			Intent: "verify read selection", Verdict: ledger.VerdictPark,
			Reason: "fixture", RiskTier: "moderate", PolicyVersion: 1, EvidenceID: "runtime-read-fixture"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMCPRuntimeMissingAuthorityFailsAllAccessors(t *testing.T) {
	access := &fleetMCPAccess{}
	ctx := context.Background()
	if result, err := access.GetPolicy(ctx, mcp.PolicyRequest{}); err == nil || result.Version != 0 {
		t.Fatalf("policy=%+v err=%v", result, err)
	}
	proposal, proposalErr := access.ProposePolicyChangeDryRun(ctx, mcp.PolicyProposalRequest{})
	if proposalErr == nil || proposal.ProposalID != 0 {
		t.Fatalf("proposal=%+v err=%v", proposal, proposalErr)
	}
	ledger, ledgerErr := access.GetLedger(ctx, mcp.LedgerRequest{})
	if ledgerErr == nil || len(ledger.Entries) != 0 {
		t.Fatalf("ledger=%+v err=%v", ledger, ledgerErr)
	}
	if result, err := access.GetValue(ctx); err == nil || result != nil {
		t.Fatalf("value=%+v err=%v", result, err)
	}
	if result, err := access.GetGuaranteeStatus(ctx); err == nil || result.XID != nil {
		t.Fatalf("guarantees=%+v err=%v", result, err)
	}
	for _, raw := range []string{"", "{", `{"database_id":"bad"}`, `{}`} {
		if id := databaseIDFromLedgerFilter(json.RawMessage(raw)); id != nil {
			t.Fatalf("invalid/empty filter acquired database identity: %s", raw)
		}
	}
	id := databaseIDFromLedgerFilter(json.RawMessage(`{"database_id":42}`))
	if id == nil || *id != 42 {
		t.Fatal("valid filter lost exact database identity")
	}
}
