package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

// migration-07: a rehearsal that cannot write a receipt creates no clone
// and is recommend-only (no_receipt_store); safety refusals get their own
// reasons; a broken migration runtime is logged, not dropped.

type noStepRunner struct{ calls int }

func (r *noStepRunner) Run(context.Context, string, plan.Plan) (rehearsal.Measurement,
	error) {
	r.calls++
	return rehearsal.Measurement{}, nil
}

func TestRehearsalWithoutReceiptStoreIsRecommendOnly(t *testing.T) {
	adapter := &reaperAdapter{}
	provider, err := receiptingRehearsalProvider(adapter, nil, reaperCloneConfig)
	if err != nil {
		t.Fatal(err)
	}
	runner := &noStepRunner{}
	rehearser := rehearsal.NewOrchestrator(provider, runner, rehearsal.Options{
		MaxCloneAge: time.Hour, RegressionPct: 15})
	result, err := recommendOnlyOnCloneFailure{rehearser}.Rehearse(context.Background(),
		plan.Plan{ExpandSteps: []plan.Step{{SQL: "CREATE INDEX CONCURRENTLY i ON t (id)"}}})
	if err != nil || result.Verdict != rehearsal.VerdictRecommendOnly ||
		result.Reason != rehearsal.ReasonNoReceiptStore {
		t.Fatalf("result %+v err %v, want recommend_only/no_receipt_store", result, err)
	}
	if len(adapter.created) != 0 || runner.calls != 0 {
		t.Fatalf("a clone was created (%v) or a step ran (%d) without a receipt",
			adapter.created, runner.calls)
	}
}

func TestRehearsalFailureReasonForSafetyRefusals(t *testing.T) {
	stepErr := func(cause error) error {
		return fmt.Errorf("%w: rehearse migration: %w", rehearsal.ErrStepFailed, cause)
	}
	for name, tc := range map[string]struct {
		err  error
		want rehearsal.Reason
	}{
		"no receipt store": {fmt.Errorf("%w: create rehearsal clone: %w",
			rehearsal.ErrCloneUnavailable, clone.ErrNoReceiptStore),
			rehearsal.ReasonNoReceiptStore},
		"production": {stepErr(rehearsal.ErrTargetIsProduction), "target_is_production"},
		"unverified": {stepErr(rehearsal.ErrTargetUnverified), "target_unverified"},
		"step sqlstate": {stepErr(&pgconn.PgError{Code: "42P01"}),
			"rehearsal_failed:42P01"},
		"step":     {stepErr(errors.New("boom")), "rehearsal_failed"},
		"provider": {errors.New("dle: 503"), "clone_unavailable"},
	} {
		if got := rehearsalFailureReason(tc.err); got != tc.want {
			t.Errorf("%s: reason %q, want %q", name, got, tc.want)
		}
	}
}

type errRehearser struct{ err error }

func (r errRehearser) Rehearse(context.Context, plan.Plan) (rehearsal.Result, error) {
	return rehearsal.Result{}, r.err
}

func TestRehearsalOnProductionIsLoggedAsError(t *testing.T) {
	var result rehearsal.Result
	out := captureStderr(t, func() {
		result, _ = recommendOnlyOnCloneFailure{errRehearser{fmt.Errorf("%w: %w",
			rehearsal.ErrStepFailed, rehearsal.ErrTargetIsProduction)}}.Rehearse(
			context.Background(), plan.Plan{})
	})
	if result.Reason != rehearsal.ReasonTargetIsProduction ||
		!strings.Contains(out, "[ERROR] [migration]") ||
		!strings.Contains(out, "returned the production server") {
		t.Fatalf("result %+v log %q", result, out)
	}
}

func TestFleetMCPApplyMigrationLogsRuntimeError(t *testing.T) {
	parsetree.Require(t)
	pool := agentWiringPool(t)
	executor := &fleetIntentExecutor{
		access: &fleetMCPAccess{fallback: pool},
		gate: &mcpGateRecorder{decision: policy.Decision{
			Verdict: policy.VerdictExecute, EvidenceID: "ev-runtime-error",
		}},
		cloneConfig: config.CloneProviderConfig{Provider: "dle"},
		migrationFactory: func(*pgxpool.Pool, policy.Gate,
			config.CloneProviderConfig) (mcp.MigrationRuntime, error) {
			return nil, errors.New("DLE endpoint must be an absolute URL")
		},
	}
	var result any
	var err error
	out := captureStderr(t, func() {
		result, err = executor.ExecuteConcrete(context.Background(), policy.ActionRequest{
			Contract: &policy.ActionContract{ActionType: "apply_migration"},
			Arguments: json.RawMessage(`{"table":"public.users","cycle":4,` +
				`"sql":"ALTER TABLE public.users ADD CONSTRAINT users_email_key ` +
				`UNIQUE (email)"}`),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome, ok := result.(mcp.MigrationOutcome); !ok || outcome.Verdict != "recommend_only" {
		t.Fatalf("outcome %#v", result)
	}
	if !strings.Contains(out, "[ERROR] [mcp] apply_migration is recommend-only") ||
		!strings.Contains(out, "DLE endpoint must be an absolute URL") {
		t.Fatalf("runtime error not logged: %q", out)
	}
}
