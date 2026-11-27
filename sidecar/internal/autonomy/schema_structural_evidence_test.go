package autonomy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// migration-09: a type-tightening decision proposes no SQL. The rewrite
// ALTER TYPE is evidence only, labelled as a full-table rewrite.

// capturingRepository records the decisions the schema recorder writes.
type capturingRepository struct {
	mu     sync.Mutex
	inputs []ledger.DecisionInput
	err    error
}

func (r *capturingRepository) InsertDecision(
	_ context.Context, input ledger.DecisionInput,
) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	r.inputs = append(r.inputs, input)
	return int64(len(r.inputs)), nil
}

func (r *capturingRepository) FindAuditViolations(
	context.Context,
) ([]ledger.AuditViolation, error) {
	return nil, nil
}

// plannedRecord plans one invariant with the production policy and wraps it
// as the ledger row the custodian would write.
func plannedRecord(t *testing.T, invariant schemaguard.Invariant) schemaguard.DecisionRecord {
	t.Helper()
	policyConfig := schemaguard.DefaultPolicy()
	policyConfig.AllowFKIndexApply, policyConfig.AllowRetentionApply = true, true
	decision, err := schemaguard.Plan(context.Background(), schemaguard.Request{
		Invariant: invariant, Policy: policyConfig})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return schemaguard.DecisionRecord{
		Remediation: schemaguard.Remediation{Invariant: invariant, Decision: decision},
		Targets:     []string{invariant.Target()}, Identity: "id", Hash: "hash"}
}

func TestTypeTighteningDecisionShowsRewriteOnlyAsEvidence(t *testing.T) {
	items := structuralInvariants(map[uint32]structuralTable{
		1: {schema: "app", name: "orders", columns: 2, textColumns: 1,
			tightening: []string{"order_number"}},
	})
	if len(items) != 1 || items[0].Kind != schemaguard.InvariantTypeTightening {
		t.Fatalf("structural invariants = %+v, want one type tightening", items)
	}
	if items[0].ProposedSQL != "" {
		t.Fatalf("type tightening proposes %q, want no SQL", items[0].ProposedSQL)
	}
	repository := &capturingRepository{}
	recorder := schemaDecisionRecorder{ledger.NewService(repository)}
	if err := recorder.Record(context.Background(), plannedRecord(t, items[0])); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(repository.inputs) != 1 {
		t.Fatalf("recorded %d decisions, want 1", len(repository.inputs))
	}
	input := repository.inputs[0]
	if input.ProposedSQL != "" || input.RiskTier != "moderate" ||
		input.Verdict != ledger.VerdictObserveOnly {
		t.Fatalf("decision input = %+v", input)
	}
	if _, ok := input.Evidence["proposed_sql"]; ok {
		t.Fatalf("evidence carries proposed_sql: %v", input.Evidence)
	}
	want := `ALTER TABLE "app"."orders" ALTER COLUMN "order_number" TYPE bigint ` +
		`USING "order_number"::bigint`
	if got := input.Evidence["rewrite_statement"]; got != want {
		t.Fatalf("rewrite_statement = %v, want %q", got, want)
	}
	note, _ := input.Evidence["rewrite_note"].(string)
	if !strings.Contains(note, "ACCESS EXCLUSIVE") || !strings.Contains(note, "not to run") {
		t.Fatalf("rewrite_note = %q", note)
	}
	if input.Evidence["route"] != schemaguard.RouteRecommendation {
		t.Fatalf("route = %v, want recommendation", input.Evidence["route"])
	}
	if !strings.Contains(input.Reason, "online column migration") ||
		strings.Contains(input.Reason, "authorization pending") {
		t.Fatalf("reason = %q", input.Reason)
	}
}

// Identifiers are quoted in the reference statement, so a hostile column
// name cannot end the statement (it is never run, but may be copied).
func TestTypeTighteningRewriteQuotesIdentifiers(t *testing.T) {
	got := typeTighteningRewrite(schemaguard.Invariant{
		Kind: schemaguard.InvariantTypeTightening, Schema: `a"b`, Table: "t",
		Subject: `x"; DROP TABLE t; --_id`})
	want := `ALTER TABLE "a""b"."t" ALTER COLUMN "x""; DROP TABLE t; --_id" TYPE bigint ` +
		`USING "x""; DROP TABLE t; --_id"::bigint`
	if got != want {
		t.Fatalf("rewrite = %q, want %q", got, want)
	}
}

// Only a type tightening with a column carries rewrite evidence.
func TestRewriteEvidenceOnlyForTypeTighteningWithAColumn(t *testing.T) {
	for name, invariant := range map[string]schemaguard.Invariant{
		"everything text": {Kind: schemaguard.InvariantEverythingText,
			Schema: "app", Table: "orders"},
		"missing FK index": {Kind: schemaguard.InvariantMissingFKIndex,
			Schema: "app", Table: "orders", Subject: "orders_fk",
			ProposedSQL: "CREATE INDEX CONCURRENTLY i ON app.orders (c)"},
		"tightening without column": {Kind: schemaguard.InvariantTypeTightening,
			Schema: "app", Table: "orders"},
	} {
		t.Run(name, func(t *testing.T) {
			evidence := schemaDecisionEvidence(plannedRecord(t, invariant))
			if _, ok := evidence["rewrite_statement"]; ok {
				t.Fatalf("evidence = %v, want no rewrite_statement", evidence)
			}
			if _, ok := evidence["rewrite_note"]; ok {
				t.Fatalf("evidence = %v, want no rewrite_note", evidence)
			}
		})
	}
}

func TestSchemaDecisionReasonDefaultsOnlyForRoutedDispositions(t *testing.T) {
	const pending = "standing policy authorization pending"
	tests := []struct {
		name     string
		decision schemaguard.Decision
		pending  bool
	}{
		{"apply", schemaguard.Decision{Disposition: schemaguard.DispositionApply}, true},
		{"dry run", schemaguard.Decision{Disposition: schemaguard.DispositionDryRun}, true},
		{"recommend", schemaguard.Decision{Disposition: schemaguard.DispositionRecommend,
			Route: schemaguard.RouteVerifyIndex}, false},
		{"zero", schemaguard.Decision{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reason := schemaDecisionReason(test.decision)
			if strings.TrimSpace(reason) == "" {
				t.Fatal("empty decision reason")
			}
			if strings.Contains(reason, pending) != test.pending {
				t.Fatalf("reason = %q, want pending=%v", reason, test.pending)
			}
		})
	}
	explicit := schemaguard.Decision{Disposition: schemaguard.DispositionApply,
		Reason: "owner said so"}
	if got := schemaDecisionReason(explicit); got != "owner said so" {
		t.Fatalf("explicit reason replaced: %q", got)
	}
}

// A recommendation (or the removed clone_rehearsal route) that reaches the
// router fails closed: nothing is routed and the error names the route.
func TestSchemaRouterRefusesRecommendationRoutes(t *testing.T) {
	for _, route := range []schemaguard.Route{schemaguard.RouteRecommendation,
		schemaguard.Route("clone_rehearsal")} {
		t.Run(string(route), func(t *testing.T) {
			router := &recordingRouter{}
			adapter := schemaRemediationRouter{database: "orders", router: router,
				verifiedIndexes: router}
			err := adapter.Route(context.Background(), schemaguard.Remediation{
				Invariant: schemaguard.Invariant{Kind: schemaguard.InvariantTypeTightening,
					Schema: "app", Table: "orders", Subject: "order_number",
					ProposedSQL: "ALTER TABLE app.orders ALTER COLUMN order_number TYPE bigint"},
				Decision: schemaguard.Decision{Route: route,
					Disposition: schemaguard.DispositionRecommend},
			})
			if err == nil || !strings.Contains(err.Error(), string(route)) {
				t.Fatalf("Route err = %v, want unsupported route %q", err, route)
			}
			if routed := router.routed(); len(routed) != 0 {
				t.Fatalf("recommendation routed: %#v", routed)
			}
		})
	}
}

func TestSchemaRecorderPropagatesLedgerErrors(t *testing.T) {
	cause := errors.New("connection refused")
	recorder := schemaDecisionRecorder{ledger.NewService(&capturingRepository{err: cause})}
	err := recorder.Record(context.Background(), plannedRecord(t, schemaguard.Invariant{
		Kind: schemaguard.InvariantTypeTightening, Schema: "app", Table: "orders",
		Subject: "order_number"}))
	if !errors.Is(err, cause) {
		t.Fatalf("Record err = %v, want the ledger cause", err)
	}
}

// No concurrent-access test: the evidence and reason builders are pure; the
// custodian serializes scans per database (supervisor).
