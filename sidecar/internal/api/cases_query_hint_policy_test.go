package api

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
)

// earned-04 (cases-08): query-hint cases go through the same policy
// enrichment as finding and incident cases, so their action cards show
// the gate's real verdict instead of "policy pending".

// observationHintManager registers "prod" on the test database with an
// executor in Observation mode over an in-memory standing gate.
func observationHintManager(t *testing.T) (*fleet.DatabaseManager, namedPool) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	cfg := &config.Config{Mode: "fleet", Trust: config.TrustConfig{Level: "observation"}}
	exec := executor.New(nil, cfg, time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	exec.WithEmergencyStopCheck(func(_ context.Context) bool { return false })
	exec.SetExecutionMode("auto")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "prod", Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
		Pool: pool, Executor: exec,
	})
	return mgr, namedPool{name: "prod", pool: pool}
}

func retireHintCandidate(t *testing.T, all []cases.Case) cases.ActionCandidate {
	t.Helper()
	for _, c := range all {
		for _, a := range c.ActionCandidates {
			if a.ActionType == "retire_query_hint" {
				return a
			}
		}
	}
	t.Fatalf("no retire_query_hint candidate among %d cases", len(all))
	return cases.ActionCandidate{}
}

// State transition: in Observation mode the broken hint's retire action
// is held by the gate (observe only, the blocked reason naming it) and
// says so, instead of "policy pending"; the verdict is the one the same
// candidate gets on the finding path.
func TestQueryHintCaseCarriesTheGatesVerdictInObservation(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status) VALUES (9300001, 'SeqScan(t)', 'slow',
		'broken')`); err != nil {
		t.Fatalf("seed broken hint: %v", err)
	}
	mgr, selected := observationHintManager(t)
	res, err := queryDatabaseCases(ctx, mgr, selected)
	if err != nil {
		t.Fatalf("queryDatabaseCases: %v", err)
	}
	got := retireHintCandidate(t, res.cases)
	if got.PolicyDecision == nil {
		t.Fatalf("retire_query_hint has no policy decision (policy pending): %+v", got)
	}
	if got.PolicyDecision.Decision != executor.PolicyDecisionObserveOnly ||
		got.BlockedReason != executor.PolicyDecisionObserveOnly {
		t.Fatalf("decision = %+v (blocked %q), want observe_only held in Observation mode",
			got.PolicyDecision, got.BlockedReason)
	}
	same := cases.Case{DatabaseName: "prod", ActionCandidates: []cases.ActionCandidate{{
		ActionType: "retire_query_hint", RiskTier: "safe",
	}}}
	enrichCaseActionPolicies(&same, mgr, "prod")
	want := same.ActionCandidates[0]
	if want.PolicyDecision == nil ||
		want.PolicyDecision.Decision != got.PolicyDecision.Decision ||
		want.BlockedReason != got.BlockedReason {
		t.Fatalf("hint path %+v disagrees with the finding path %+v",
			got.PolicyDecision, want.PolicyDecision)
	}
}

// Nil/empty: a database with no executor fails closed for its hint
// cases too, as it does for findings.
func TestQueryHintCaseFailsClosedWithoutAnExecutor(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_hints
		(queryid, hint_text, symptom, status) VALUES (9300002, 'SeqScan(u)', 'slow',
		'broken')`); err != nil {
		t.Fatalf("seed broken hint: %v", err)
	}
	mgr := poolManager(map[string]*pgxpool.Pool{"prod": pool})
	res, err := queryDatabaseCases(ctx, mgr, namedPool{name: "prod", pool: pool})
	if err != nil {
		t.Fatalf("queryDatabaseCases: %v", err)
	}
	got := retireHintCandidate(t, res.cases)
	if got.BlockedReason != "standing policy unavailable" {
		t.Fatalf("BlockedReason = %q, want standing policy unavailable", got.BlockedReason)
	}
}
