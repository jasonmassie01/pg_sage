package agentdb

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWave3EffectivePolicyUsesRestrictiveMergeRules(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	input := wave3PolicyInput(now)
	input.Runtime.Policy = wave3LayerPolicy(
		[]string{"us-east-1", "us-west-2"}, 7200, 30, LiveModeAutoWithinPolicy,
	)
	input.Global.Policy = wave3LayerPolicy(
		[]string{"*"}, 3600, 20, LiveModeAutoWithinPolicy,
	)
	input.Provider.Policy = wave3LayerPolicy(
		[]string{"us-east-1", "eu-west-1"}, 1800, 10, LiveModeApproval,
	)
	input.Global.Policy.AllowPublicIP = false
	input.Provider.Policy.RequireBackupBeforeDrop = true

	got := ResolveEffectiveLiveProvisionPolicy(input)
	if !got.Decision.Allowed {
		t.Fatalf("effective policy denied valid request: %+v", got.Decision)
	}
	if !reflect.DeepEqual(got.Policy.AllowedRegions, []string{"us-east-1"}) {
		t.Fatalf("regions = %v, want intersection [us-east-1]", got.Policy.AllowedRegions)
	}
	if got.Policy.MaxTTLSeconds != 1800 || got.Policy.MaxEstimatedCostUSD != 10 {
		t.Fatalf("ceilings = ttl %d cost %.2f, want 1800 and 10",
			got.Policy.MaxTTLSeconds, got.Policy.MaxEstimatedCostUSD)
	}
	if got.Policy.AllowPublicIP || !got.Policy.RequireBackupBeforeDrop {
		t.Fatalf("boolean restrictions were weakened: %+v", got.Policy)
	}
	if got.Policy.ExecutionMode != LiveModeApproval {
		t.Fatalf("mode = %q, want %q", got.Policy.ExecutionMode, LiveModeApproval)
	}
}

func TestWave3EffectivePolicyPrecedenceMatrix(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*EffectiveLivePolicyInput)
		reason string
	}{
		{"runtime disabled", func(in *EffectiveLivePolicyInput) {
			in.Runtime.Enabled = false
		}, "runtime"},
		{"runner unavailable", func(in *EffectiveLivePolicyInput) {
			in.Runtime.RunnerAvailable = false
		}, "runner"},
		{"global disabled", func(in *EffectiveLivePolicyInput) {
			in.Global.Policy.LiveProvisioningEnabled = false
		}, "global"},
		{"provider disabled", func(in *EffectiveLivePolicyInput) {
			in.Provider.Policy.ProviderEnabled = false
		}, "provider"},
		{"operation unauthorized", func(in *EffectiveLivePolicyInput) {
			in.Authorization.Allowed = false
		}, "authorization"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := wave3PolicyInput(now)
			tc.mutate(&input)
			assertWave3PolicyDenied(t, ResolveEffectiveLiveProvisionPolicy(input), tc.reason)
		})
	}
}

func TestWave3EffectivePolicyAllowlistIntersectionAndDenyAll(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	input := wave3PolicyInput(now)
	input.Runtime.Policy.AllowedRegions = []string{"us-east-1", "us-west-2"}
	input.Global.Policy.AllowedRegions = []string{"*"}
	input.Provider.Policy.AllowedRegions = []string{"us-west-2"}
	input.Request.Region = "us-east-1"
	assertWave3PolicyDenied(
		t, ResolveEffectiveLiveProvisionPolicy(input), "region",
	)

	input.Request.Region = "us-west-2"
	if got := ResolveEffectiveLiveProvisionPolicy(input); !got.Decision.Allowed {
		t.Fatalf("allowlisted intersection denied: %+v", got.Decision)
	}

	input.Global.Policy.AllowedRegions = nil
	assertWave3PolicyDenied(
		t, ResolveEffectiveLiveProvisionPolicy(input), "allowlist",
	)
}

func TestWave3EffectivePolicyFailsClosedForUnavailableSources(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*EffectiveLivePolicyInput)
		reason string
	}{
		{"missing runtime", func(in *EffectiveLivePolicyInput) {
			in.Runtime = nil
		}, "runtime"},
		{"missing global", func(in *EffectiveLivePolicyInput) {
			in.Global = nil
		}, "global"},
		{"missing provider", func(in *EffectiveLivePolicyInput) {
			in.Provider = nil
		}, "provider"},
		{"missing authorization", func(in *EffectiveLivePolicyInput) {
			in.Authorization = nil
		}, "authorization"},
		{"policy store error", func(in *EffectiveLivePolicyInput) {
			in.Provider.Err = errors.New("meta policy read failed")
		}, "policy"},
		{"stale validation", func(in *EffectiveLivePolicyInput) {
			in.Provider.ValidatedAt = now.Add(-2 * time.Hour)
			in.Provider.ValidationTTL = time.Hour
		}, "stale"},
		{"unknown provider", func(in *EffectiveLivePolicyInput) {
			in.Request.Provider = "unknown_cloud"
		}, "provider"},
		{"contradictory provider", func(in *EffectiveLivePolicyInput) {
			in.Provider.Policy.Provider = ProviderGCPCloudSQL
		}, "provider"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := wave3PolicyInput(now)
			tc.mutate(&input)
			assertWave3PolicyDenied(t, ResolveEffectiveLiveProvisionPolicy(input), tc.reason)
		})
	}
}

func TestWave3EffectivePolicyRequiresExplicitPositiveCeilings(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	for _, field := range []string{"ttl", "cost"} {
		t.Run(field, func(t *testing.T) {
			input := wave3PolicyInput(now)
			if field == "ttl" {
				input.Global.Policy.MaxTTLSeconds = 0
			} else {
				input.Provider.Policy.MaxEstimatedCostUSD = 0
			}
			assertWave3PolicyDenied(
				t, ResolveEffectiveLiveProvisionPolicy(input), field,
			)
		})
	}
}

func TestWave3PolicyTighteningAppliesAfterAuthorization(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	input := wave3PolicyInput(now)
	before := ResolveEffectiveLiveProvisionPolicy(input)
	if !before.Decision.Allowed {
		t.Fatalf("initial policy denied: %+v", before.Decision)
	}

	input.Provider.Policy.ProviderEnabled = false
	input.Provider.Version++
	after := ResolveEffectiveLiveProvisionPolicy(input)
	assertWave3PolicyDenied(t, after, "provider")
	if after.PolicyHash == before.PolicyHash {
		t.Fatal("provider tightening did not change the effective policy hash")
	}
}

func TestWave3ProviderPolicyCannotBroadenGlobalCeiling(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	input := wave3PolicyInput(now)
	input.Global.Policy.AllowedRegions = []string{"us-east-1"}
	input.Global.Policy.MaxTTLSeconds = 900
	input.Global.Policy.MaxEstimatedCostUSD = 5
	input.Provider.Policy.AllowedRegions = []string{"*"}
	input.Provider.Policy.MaxTTLSeconds = 7200
	input.Provider.Policy.MaxEstimatedCostUSD = 100
	input.Request.Region = "us-west-2"
	input.Request.TTLSeconds = 1800
	input.Request.EstimatedCostUSD = 6

	got := ResolveEffectiveLiveProvisionPolicy(input)
	if got.Decision.Allowed {
		t.Fatalf("provider policy widened hard global ceiling: %+v", got)
	}
	if got.Policy.MaxTTLSeconds != 900 || got.Policy.MaxEstimatedCostUSD != 5 {
		t.Fatalf("effective ceilings = ttl %d cost %.2f",
			got.Policy.MaxTTLSeconds, got.Policy.MaxEstimatedCostUSD)
	}
}

func TestWave3ManualModeDisablesLiveMutation(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	input := wave3PolicyInput(now)
	input.Provider.Policy.ExecutionMode = LiveModeManual
	got := ResolveEffectiveLiveProvisionPolicy(input)
	assertWave3PolicyDenied(t, got, "manual")
	if got.Policy.ExecutionMode != LiveModeManual {
		t.Fatalf("effective mode = %q, want manual", got.Policy.ExecutionMode)
	}
}

func wave3PolicyInput(now time.Time) EffectiveLivePolicyInput {
	policy := wave3LayerPolicy([]string{"*"}, 3600, 25, LiveModeAutoWithinPolicy)
	return EffectiveLivePolicyInput{
		Now: now,
		Runtime: &RuntimeLiveCapability{
			Enabled: true, RunnerAvailable: true, Policy: policy,
		},
		Global: &LivePolicyLayer{Policy: policy, Version: 7},
		Provider: &LivePolicyLayer{
			Policy: policy, Version: 11, ValidatedAt: now, ValidationTTL: time.Hour,
		},
		Authorization: &LiveOperationAuthorization{
			Allowed: true, Provider: ProviderAWSRDS, Operation: ProvisionOpCreate,
			DeploymentID: "wave3-policy", PlanHash: "plan-v1", ExpiresAt: now.Add(time.Hour),
		},
		Operation:    ProvisionOpCreate,
		DeploymentID: "wave3-policy",
		PlanHash:     "plan-v1",
		Request: LiveProvisionRequest{
			Provider: ProviderAWSRDS, Region: "us-east-1", Account: "123456789012",
			TTLSeconds: 900, EstimatedCostUSD: 3,
		},
	}
}

func wave3LayerPolicy(
	regions []string,
	ttl int,
	cost float64,
	mode string,
) LiveProvisionPolicy {
	return LiveProvisionPolicy{
		LiveProvisioningEnabled: true,
		ProviderEnabled:         true,
		Provider:                ProviderAWSRDS,
		AllowedRegions:          regions,
		AllowedAccounts:         []string{"*"},
		AllowedProjects:         []string{"*"},
		AllowedWorkspaces:       []string{"*"},
		AllowPublicIP:           true,
		MaxTTLSeconds:           ttl,
		MaxEstimatedCostUSD:     cost,
		ExecutionMode:           mode,
	}
}

func assertWave3PolicyDenied(
	t *testing.T,
	got EffectiveLivePolicyResult,
	reason string,
) {
	t.Helper()
	if got.Decision.Allowed {
		t.Fatalf("policy allowed unexpectedly: %+v", got)
	}
	joined := strings.ToLower(strings.Join(got.Decision.DisabledReasons, " "))
	if !strings.Contains(joined, strings.ToLower(reason)) {
		t.Fatalf("disabled reasons %q do not identify %q", joined, reason)
	}
}
