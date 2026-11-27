package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// providerGate is an autonomous standing gate whose runtime provider is the
// executor's own (cfg.CloudEnvironment), as the production gate reads it.
func providerGate(exec *Executor, now time.Time) policy.Gate {
	doc := policy.UnattendedProfile()
	return policy.NewGate(policy.GateConfig{
		Runtime: func(ctx context.Context, req policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{
				ExecutorEnabled: true, TrustLevel: policy.TrustAutonomous,
				ExecutionMode: policy.ExecutionAuto, Tier3Safe: true, Tier3Moderate: true,
				RampStart: now.Add(-90 * 24 * time.Hour), InConfiguredWindow: true,
				Provider: exec.standingRuntimeState(ctx, req).Provider,
			}, nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		RecordDecision: func(
			context.Context, policy.ActionRequest, policy.Decision,
		) (string, error) {
			return "recorded", nil
		},
		Now: func() time.Time { return now },
	})
}

func explainAlterSystem(t *testing.T, provider string) ActionPolicyDecision {
	t.Helper()
	cfg := autonomousTestConfig()
	cfg.CloudEnvironment = provider
	exec := New(nil, cfg, time.Time{}, noopExecLog)
	exec.WithPolicyGate(providerGate(exec, time.Now()))
	contract, ok := ContractForActionType("alter_system_guc")
	if !ok {
		t.Fatal("missing alter_system_guc contract")
	}
	got := exec.ExplainFamilies(context.Background(), []ActionContract{contract}, false)
	if len(got) != 1 {
		t.Fatalf("family decisions = %+v, want one", got)
	}
	return got[0]
}

// clouds-05a: a provider probe that errored reads as unknown, and unknown
// must not inherit self-managed's ALTER SYSTEM permission.
func TestAlterSystemBlockedForUnknownProvider(t *testing.T) {
	got := explainAlterSystem(t, "unknown")
	if got.Decision != PolicyDecisionBlocked {
		t.Fatalf("unknown provider: decision = %+v, want blocked", got)
	}
	if got.BlockedReason != string(policy.ReasonProviderUnsupported) {
		t.Fatalf("blocked reason = %q, want %q", got.BlockedReason,
			policy.ReasonProviderUnsupported)
	}
}

// The control: self-managed is not blocked by the provider check, so the
// test above fails for the provider and not for some other reason.
func TestAlterSystemNotProviderBlockedForSelfManaged(t *testing.T) {
	got := explainAlterSystem(t, "self-managed")
	if strings.Contains(got.BlockedReason, "provider") {
		t.Fatalf("self-managed: blocked reason = %q, want no provider block", got.BlockedReason)
	}
	if got.Decision == "" {
		t.Fatalf("self-managed: empty decision %+v", got)
	}
}

func TestAlterSystemContractExcludesUnknown(t *testing.T) {
	contract, _ := ContractForActionType("alter_system_guc")
	if len(contract.ProviderSupport) == 0 {
		t.Fatal("alter_system_guc has no provider restriction; every provider could run it")
	}
	for _, provider := range contract.ProviderSupport {
		if strings.EqualFold(provider, "unknown") || strings.EqualFold(provider, "self-managed") {
			t.Fatalf("provider support %v lists %q", contract.ProviderSupport, provider)
		}
	}
}
