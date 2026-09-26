package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// Regression test for G4-B14: custodian proposals must carry the HA
// replica / safe-mode state so the gate refuses mutations on a standby.
func TestAutonomyRouterMarksReplicaProposals(t *testing.T) {
	gate := &runtimePolicyRecorder{}
	exec := executor.New(nil, config.DefaultConfig(), nil, time.Now(), nil)
	exec.WithPolicyGate(gate)
	router := executorProposalRouter{
		executor: exec, isReplica: func(context.Context) bool { return true },
	}
	proposal := autonomy.Proposal{Database: "fixture", Feature: "freeze",
		SQL:           `VACUUM (FREEZE) "public"."orders"`,
		TargetObjects: []string{"public.orders"}}

	_ = router.Route(context.Background(), proposal)
	if !gate.request.IsReplica {
		t.Fatal("custodian proposal on a replica was not marked IsReplica")
	}
	_ = router.RouteVerifiedIndex(context.Background(), autonomy.Proposal{
		Feature: "fk_index", TargetObjects: []string{"public.orders"},
		SQL: "CREATE INDEX CONCURRENTLY fixture_idx ON public.orders(customer_id)",
	}, "DROP INDEX CONCURRENTLY public.fixture_idx", []int64{41})
	if !gate.request.IsReplica {
		t.Fatal("verified-index proposal on a replica was not marked IsReplica")
	}
}

func TestAutonomyRouterWithoutReplicaCheckFailsClosed(t *testing.T) {
	gate := &runtimePolicyRecorder{}
	exec := executor.New(nil, config.DefaultConfig(), nil, time.Now(), nil)
	exec.WithPolicyGate(gate)
	router := executorProposalRouter{executor: exec}

	_ = router.Route(context.Background(), autonomy.Proposal{
		Feature: "freeze", SQL: `VACUUM (FREEZE) "public"."orders"`,
		TargetObjects: []string{"public.orders"},
	})

	if !gate.request.IsReplica {
		t.Fatal("router without HA evidence treated the target as a primary")
	}
}
