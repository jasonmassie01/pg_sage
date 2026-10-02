package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
)

// The runtime adapter forwards the owner-declared retention column to the
// executor, which marks the gate request as owner-authorized only then.
func TestExecuteRetentionForwardsDeclaredColumn(t *testing.T) {
	gate := &runtimePolicyRecorder{}
	exec := executor.New(nil, config.DefaultConfig(), time.Now(), nil)
	exec.WithPolicyGate(gate)
	router := executorProposalRouter{
		executor: exec, isReplica: func(context.Context) bool { return false },
	}

	_ = router.executeRetention(context.Background(), autonomy.RetentionIntent{
		Schema: "public", Table: "events", Column: "created_at",
		DeclaredColumn: "created_at", Window: time.Hour, Cutoff: time.Now(),
	}, nil)

	if !gate.request.OwnerDeclared {
		t.Fatal("declared retention column was not forwarded as owner authority")
	}

	_ = router.executeRetention(context.Background(), autonomy.RetentionIntent{
		Schema: "public", Table: "events", Column: "created_at",
		Window: time.Hour, Cutoff: time.Now(),
	}, nil)

	if gate.request.OwnerDeclared {
		t.Fatal("a contract without a declared column was treated as owner authority")
	}
}
