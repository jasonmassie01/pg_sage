package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests: the classifier is a pure function of its SQL.

// executor-08: a VACUUM that names no relation or several, and a REINDEX
// of a whole schema or database, have no typed contract: they classify as
// "" and the gate parks them as no_typed_contract instead of running a
// database-wide maintenance pass as a one-table vacuum_table.
func TestClassifyRefusesUnscopedVacuumAndReindex(t *testing.T) {
	exec := New(nil, &config.Config{}, time.Time{}, nopLog)
	exec.WithPolicyGate(fullyEnabledGate(time.Now()))
	for _, sql := range []string{
		"VACUUM",
		"VACUUM;",
		"VACUUM (FREEZE)",
		"VACUUM (FREEZE);",
		"VACUUM ANALYZE",
		"VACUUM a, b",
		"VACUUM (FREEZE) public.a, public.b;",
		"VACUUM public.a,public.b",
		"REINDEX SCHEMA CONCURRENTLY public",
		"REINDEX DATABASE CONCURRENTLY x",
		"REINDEX SYSTEM CONCURRENTLY x",
	} {
		if got := ActionTypeForSQL(sql); got != "" {
			t.Errorf("ActionTypeForSQL(%q) = %q, want \"\" (no contract)", sql, got)
		}
		finding := analyzer.Finding{RecommendedSQL: sql, ObjectType: "table"}
		decision := exec.evaluateFindingPolicy(context.Background(), finding, false)
		if decision.Decision != PolicyDecisionParked ||
			decision.BlockedReason != "no_typed_contract" {
			t.Errorf("%q: decision = %s/%s, want parked/no_typed_contract",
				sql, decision.Decision, decision.BlockedReason)
		}
	}
}

// Boundary: one named relation, in every spelling the generators and an
// operator use, keeps its contract.
func TestClassifyKeepsSingleRelationVacuumAndReindex(t *testing.T) {
	cases := map[string]string{
		"VACUUM public.t":                          "vacuum_table",
		"VACUUM public.t;":                         "vacuum_table",
		`VACUUM "public"."orders";`:                "vacuum_table",
		`VACUUM (FREEZE) "public"."hot";`:          "vacuum_table",
		"VACUUM (FREEZE, VERBOSE) public.t":        "vacuum_table",
		"VACUUM ANALYZE public.t (a, b)":           "vacuum_table",
		"VACUUM VERBOSE ANALYZE t":                 "vacuum_table",
		`VACUUM "odd,name"`:                        "vacuum_table",
		`VACUUM "My Schema"."My Table"`:            "vacuum_table",
		"VACUUM FULL public.t":                     "",
		"VACUUM (FULL) public.t":                   "",
		"VACUUM public.t (a) public.u":             "",
		"VACUUM (FREEZE public.t":                  "",
		"VACUUM public.t (a":                       "",
		`VACUUM "unterminated`:                     "",
		`VACUUM "a""b"`:                            "vacuum_table",
		"REINDEX INDEX CONCURRENTLY public.idx_x":  "reindex_concurrently",
		"REINDEX TABLE CONCURRENTLY public.orders": "reindex_concurrently",
		"REINDEX INDEX public.idx_x":               "",
		"REINDEX INDEX CONCURRENTLY":               "",
	}
	for sql, want := range cases {
		if got := ActionTypeForSQL(sql); got != want {
			t.Errorf("ActionTypeForSQL(%q) = %q, want %q", sql, got, want)
		}
	}
}

// The single-relation VACUUM a generator writes still executes under a
// fully enabled gate (its contract is untouched by the narrowing).
func TestSingleRelationVacuumStillAuthorized(t *testing.T) {
	exec := New(nil, &config.Config{}, time.Time{}, nopLog)
	exec.WithPolicyGate(fullyEnabledGate(time.Now()))
	finding := analyzer.Finding{
		Category: "table_bloat", ObjectType: "table", ObjectIdentifier: "public.orders",
		RecommendedSQL: `VACUUM "public"."orders";`, ActionRisk: "safe",
	}
	decision := exec.evaluateFindingPolicy(context.Background(), finding, false)
	if decision.Decision != PolicyDecisionExecute {
		t.Fatalf("single-table VACUUM decision = %s/%s, want execute",
			decision.Decision, decision.BlockedReason)
	}
}

// CHECK-02: Ask Sage cannot queue an unscoped VACUUM or REINDEX for
// approval (no typed contract), while the one-table form stays proposable.
func TestAskSageRefusesUnscopedMaintenance(t *testing.T) {
	for _, sql := range []string{"VACUUM", "VACUUM (FREEZE);", "VACUUM a, b",
		"REINDEX DATABASE CONCURRENTLY app"} {
		_, _, err := typedProposal(7, proposableFinding{sql: sql})
		if !errors.Is(err, ErrNotProposable) ||
			!strings.Contains(err.Error(), "no typed action contract") {
			t.Errorf("typedProposal(%q) error = %v, want not proposable (no contract)", sql, err)
		}
	}
	proposal, _, err := typedProposal(7, proposableFinding{sql: "VACUUM public.orders"})
	if err != nil || proposal.ActionType != "vacuum_table" {
		t.Fatalf("one-table VACUUM proposal = %+v, %v; want vacuum_table", proposal, err)
	}
}
