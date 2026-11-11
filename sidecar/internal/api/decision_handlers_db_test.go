package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/ledger"
)

// policy-07, integration: the decisions route on real PostgreSQL. Database
// a is the test server; b is a pool to a port nothing listens on.

// seedAPIPark records three parks of one fresh candidate and returns its key.
func seedAPIPark(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	tag := fmt.Sprintf("decisions_api_%d", time.Now().UnixNano())
	park := ledger.DecisionInput{Feature: "index", Intent: "create index",
		Evidence:    map[string]any{"budget_detail": "next frees at 18:00"},
		ProposedSQL: "CREATE INDEX CONCURRENTLY " + tag + " ON public.t (c)",
		Verdict:     ledger.VerdictPark, Reason: "rate_limit_exceeded", RiskTier: "safe",
		PolicyVersion: 2, TargetObjects: []string{"public." + tag}}
	park.Fingerprint = ledger.DecisionFingerprint(park)
	key := ledger.CandidateKey(park)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.decision WHERE candidate_key = $1", key)
	})
	service := ledger.NewService(ledger.NewPostgresRepository(pool))
	for range 3 {
		if _, err := service.RecordDecision(ctx, park); err != nil {
			t.Fatalf("park: %v", err)
		}
	}
	return key
}

func TestDecisionsOnPostgresWithAnUnreachableDatabase(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	key := seedAPIPark(t, ctx, pool)
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "a",
		Config: config.DatabaseConfig{Name: "a"}, Pool: pool, Status: &fleet.InstanceStatus{}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "b",
		Config: config.DatabaseConfig{Name: "b"}, Pool: unreachablePool(t),
		Status: &fleet.InstanceStatus{}})
	rec := requestPolicy(decisionsMux(mgr), http.MethodGet,
		"/api/v1/decisions?verdict=parked&reason=rate_limit_exceeded&limit=500", "",
		testOperatorUser())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := decodeDecisions(t, rec.Body.Bytes())
	var found map[string]any
	for _, entry := range body.Decisions {
		if entry["candidate_key"] == key {
			found = entry
		}
		if entry["database"] != "a" || entry["verdict"] != "parked" {
			t.Errorf("entry %v is not a parked decision of a", entry)
		}
	}
	if found == nil || found["repeat_count"] != float64(3) ||
		found["human_reason"] != "rate limit exceeded (next frees at 18:00)" {
		t.Fatalf("entry for %s = %v, want 3 repeats with the human reason", key, found)
	}
	if len(body.Partial) != 1 || body.Partial[0]["database"] != "b" ||
		body.Partial[0]["error"] != "connection" {
		t.Fatalf("partial = %v, want b unreachable (connection)", body.Partial)
	}
}
