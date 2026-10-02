package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

func TestVerificationOptionsPreserveSafeDefaults(t *testing.T) {
	options := verificationOptions(&config.Config{})
	defaults := verify.DefaultOptions()
	if options.InitialWindow != defaults.InitialWindow ||
		options.HardMax != defaults.HardMax || options.MinSamples != defaults.MinSamples {
		t.Fatalf("zero config erased verify defaults: %#v", options)
	}
	if options.CPUCeilingPct != defaults.CPUCeilingPct {
		t.Fatalf("CPU ceiling = %v, want %v", options.CPUCeilingPct, defaults.CPUCeilingPct)
	}
}

func TestFKCustodianIndexFailsClosedWithoutFullVerificationEvidence(t *testing.T) {
	exec := &Executor{}
	exec.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe,
	}})
	proposal := CustodianProposal{
		Feature:       "fk_index",
		SQL:           "CREATE INDEX CONCURRENTLY orders_customer_idx ON orders(customer_id)",
		TargetObjects: []string{"public.orders"},
	}

	err := exec.SubmitVerifiedIndexProposal(
		context.Background(), proposal, "", []int64{71},
	)
	if !errors.Is(err, ErrVerificationUnavailable) {
		t.Fatalf("missing rollback error = %v", err)
	}
	err = exec.SubmitVerifiedIndexProposal(
		context.Background(), proposal,
		"DROP INDEX CONCURRENTLY orders_customer_idx", nil,
	)
	if !errors.Is(err, ErrVerificationUnavailable) {
		t.Fatalf("missing workload evidence error = %v", err)
	}
}

func TestVerificationOptionsUseConfiguredPolicy(t *testing.T) {
	cfg := &config.Config{
		Verify: config.VerifyConfig{
			WindowMinutes: 10, WindowMaxMinutes: 90, MinSamples: 12,
			MinGainPct: 8, RegressPct: 11, WriteImpactPct: 14,
		},
		Safety: config.SafetyConfig{CPUCeilingPct: 83},
	}
	options := verificationOptions(cfg)
	if options.InitialWindow != 10*time.Minute || options.HardMax != 90*time.Minute ||
		options.MinSamples != 12 || options.CPUCeilingPct != 83 {
		t.Fatalf("verificationOptions() = %#v", options)
	}
	if options.MinGainPct != 8 || options.RegressPct != 11 ||
		options.WriteImpactPct != 14 {
		t.Fatalf("verification thresholds = %#v", options)
	}
}

func TestVerifiedActionForFindingRequiresEvidenceAndRollback(t *testing.T) {
	base := analyzer.Finding{
		RecommendedSQL:   "CREATE INDEX CONCURRENTLY idx_orders ON orders(customer_id)",
		RollbackSQL:      "DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders",
		ObjectIdentifier: "public.orders",
		Detail:           map[string]any{"queryids": []int64{7, 9}},
	}
	action, err := verifiedActionForFinding(base)
	if err != nil {
		t.Fatalf("verifiedActionForFinding() error = %v", err)
	}
	if action.IndexName != "idx_orders" || action.Table != "public.orders" ||
		len(action.QueryIDs) != 2 || action.Criterion.Kind != "per_query_latency" {
		t.Fatalf("verified action = %#v", action)
	}

	for name, mutate := range map[string]func(*analyzer.Finding){
		"queries":  func(f *analyzer.Finding) { f.Detail = nil },
		"rollback": func(f *analyzer.Finding) { f.RollbackSQL = "" },
		"table":    func(f *analyzer.Finding) { f.ObjectIdentifier = "" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			_, gotErr := verifiedActionForFinding(candidate)
			if !errors.Is(gotErr, ErrVerificationUnavailable) {
				t.Fatalf("error = %v, want ErrVerificationUnavailable", gotErr)
			}
		})
	}
}

// C05: optimizer finding identities are "schema.table|<index def>"; the
// verification watch must measure the table, not the identity string.
func TestVerifiedActionForFinding_OptimizerIdentityUsesTable(t *testing.T) {
	f := analyzer.Finding{
		ObjectIdentifier: "public.orders|btree(status)",
		RecommendedSQL:   "CREATE INDEX CONCURRENTLY idx_s ON public.orders (status)",
		RollbackSQL:      `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_s"`,
		Detail:           map[string]any{"queryids": []int64{7}},
	}
	action, err := verifiedActionForFinding(f)
	if err != nil {
		t.Fatalf("verifiedActionForFinding: %v", err)
	}
	if action.Table != "public.orders" || action.IndexName != "idx_s" {
		t.Errorf("table/index = %q/%q, want public.orders/idx_s",
			action.Table, action.IndexName)
	}
}
