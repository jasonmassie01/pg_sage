package agentdb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWave3SizeProfilesReachDryRunAndNativeInputsIdentically(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		params   map[string]any
		wantArgs []string
		assert   func(*testing.T, Deployment)
	}{
		{
			name: "aws", provider: ProviderAWSRDS,
			params: map[string]any{
				"db_instance_class": "db.r7g.large", "allocated_storage": 137,
				"backup_retention_days": 19, "region": "eu-central-1",
			},
			wantArgs: []string{
				"db_instance_class=db.r7g.large", "allocated_storage=137",
				"backup_retention_days=19", "region=eu-central-1",
			},
			assert: assertWave3AWSNativeInput,
		},
		{
			name: "gcp", provider: ProviderGCPCloudSQL,
			params: map[string]any{
				"project": "wave3-project", "region": "europe-west4",
				"tier": "db-custom-4-15360", "storage_size": 211,
				"edition": "ENTERPRISE", "ipv4_enabled": false, "require_ssl": true,
			},
			wantArgs: []string{
				"project=wave3-project", "region=europe-west4",
				"tier=db-custom-4-15360", "storage_size=211",
			},
			assert: assertWave3GCPNativeInput,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dep := provisionWave3Profile(t, tc.provider, tc.params)
			assertWave3PlanArgs(t, dep.ProvisioningPlan, tc.wantArgs)
			assertWave3PersistedParams(t, dep, tc.params)
			tc.assert(t, dep)
		})
	}
}

func TestWave3ProfileParametersOverrideClientMetadata(t *testing.T) {
	trusted := map[string]any{
		"db_instance_class":     "db.r7g.large",
		"allocated_storage":     137,
		"backup_retention_days": 19,
		"region":                "eu-central-1",
	}
	dep := provisionWave3ProfileWithMetadata(t, ProviderAWSRDS, trusted, map[string]any{
		"provider_params": map[string]any{
			"db_instance_class": "db.t4g.micro",
			"allocated_storage": 20,
			"region":            "us-east-1",
		},
	})
	assertWave3PersistedParams(t, dep, trusted)
	assertWave3AWSNativeInput(t, dep)
}

func TestWave3PersistedProfileSnapshotDoesNotTrackLaterProfileMutation(t *testing.T) {
	params := map[string]any{
		"db_instance_class":     "db.r7g.large",
		"allocated_storage":     137,
		"backup_retention_days": 19,
		"region":                "eu-central-1",
	}
	fixture := provisionWave3ProfileFixture(t, ProviderAWSRDS, params, nil)
	changed := SizeProfile{
		ProfileID: fixture.profileID, Provider: ProviderAWSRDS,
		ProvisioningLevel: LevelInstance, Name: fixture.profileID,
		ProviderParams: map[string]any{
			"db_instance_class":     "db.t4g.micro",
			"allocated_storage":     20,
			"backup_retention_days": 1,
			"region":                "us-east-1",
		},
	}
	if _, err := fixture.store.UpsertSizeProfile(fixture.ctx, changed); err != nil {
		t.Fatalf("mutate source profile: %v", err)
	}
	dep, err := fixture.store.Get(fixture.ctx, fixture.deployment.DeploymentID)
	if err != nil {
		t.Fatalf("reload deployment: %v", err)
	}
	assertWave3PersistedParams(t, dep, map[string]any{
		"db_instance_class":     "db.r7g.large",
		"allocated_storage":     137,
		"backup_retention_days": 19,
		"region":                "eu-central-1",
	})
}

func TestWave3ProviderReadinessReportsEffectiveDisabledReasons(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*ProviderReadinessOptions)
		code   string
	}{
		{"runner absent", func(_ *ProviderReadinessOptions) {}, "runner_unavailable"},
		{"runtime disabled", func(o *ProviderReadinessOptions) {
			o.RuntimeEnabled = false
		}, "runtime_disabled"},
		{"global disabled", func(o *ProviderReadinessOptions) {
			o.GlobalPolicy.Policy.LiveProvisioningEnabled = false
		}, "global_disabled"},
		{"provider disabled", func(o *ProviderReadinessOptions) {
			o.ProviderPolicies[ProviderAWSRDS].Policy.ProviderEnabled = false
		}, "provider_disabled"},
		{"credential failure", func(o *ProviderReadinessOptions) {
			check := o.RuntimeChecks[ProviderAWSRDS]
			check.CredentialError = errors.New("credential validation failed")
			o.RuntimeChecks[ProviderAWSRDS] = check
		}, "credentials_invalid"},
		{"configuration failure", func(o *ProviderReadinessOptions) {
			check := o.RuntimeChecks[ProviderAWSRDS]
			check.ConfigurationError = errors.New("provider configuration invalid")
			o.RuntimeChecks[ProviderAWSRDS] = check
		}, "runtime_config_invalid"},
		{"stale provider policy", func(o *ProviderReadinessOptions) {
			o.ProviderPolicies[ProviderAWSRDS].ValidatedAt = now.Add(-2 * time.Hour)
		}, "policy_stale"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := wave3ReadinessOptions(now)
			tc.mutate(&opts)
			got := wave3ReadinessFor(t, ProviderReadinessList(context.Background(), opts))
			if got.Found {
				t.Fatalf("provider reported ready: %+v", got)
			}
			assertWave3DisabledCode(t, got.DisabledReasons, tc.code)
		})
	}
}

func TestWave3ProviderReadinessRequiresRunnerAndEffectivePolicy(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	opts := wave3ReadinessOptions(now)
	opts.Registry.Register(fakeProviderRunner{
		provider: ProviderAWSRDS,
		name:     "wave3-live-rds",
	})
	got := wave3ReadinessFor(t, ProviderReadinessList(context.Background(), opts))
	if !got.Found || len(got.DisabledReasons) != 0 {
		t.Fatalf("ready provider = %+v, want found with no blockers", got)
	}
	if got.PolicyHash == "" || got.PolicyVersion == 0 {
		t.Fatalf("ready provider omitted effective policy provenance: %+v", got)
	}
}

func provisionWave3Profile(
	t *testing.T,
	provider string,
	params map[string]any,
) Deployment {
	t.Helper()
	return provisionWave3ProfileWithMetadata(t, provider, params, nil)
}

func provisionWave3ProfileWithMetadata(
	t *testing.T,
	provider string,
	params map[string]any,
	metadata map[string]any,
) Deployment {
	t.Helper()
	return provisionWave3ProfileFixture(t, provider, params, metadata).deployment
}

type wave3ProfileTestFixture struct {
	store      *Store
	ctx        context.Context
	profileID  string
	deployment Deployment
}

func provisionWave3ProfileFixture(
	t *testing.T,
	provider string,
	params map[string]any,
	metadata map[string]any,
) wave3ProfileTestFixture {
	t.Helper()
	st, ctx, pool := requireAgentDB(t)
	id := fmt.Sprintf("wave3_profile_%s_%d", resourceName(provider), time.Now().UnixNano())
	profileID := id + "_profile"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_deployments WHERE deployment_id=$1", id)
		_, _ = pool.Exec(ctx, "DELETE FROM sage.agent_db_size_profiles WHERE profile_id=$1", profileID)
		pool.Close()
	})
	profile := SizeProfile{
		ProfileID: profileID, Provider: provider, ProvisioningLevel: LevelInstance,
		Name: profileID, StorageGB: 99, ProviderParams: params,
	}
	if _, err := st.UpsertSizeProfile(ctx, profile); err != nil {
		t.Fatalf("upsert size profile: %v", err)
	}
	dep, err := st.Provision(ctx, RegisterRequest{
		DeploymentID: id, TenantID: "wave3", AgentID: "profile",
		Provider: provider, ProvisioningLevel: LevelInstance,
		SizeProfileID: profileID, LeaseSeconds: 3600, Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("provision from size profile: %v", err)
	}
	return wave3ProfileTestFixture{
		store: st, ctx: ctx, profileID: profileID, deployment: dep,
	}
}

func assertWave3PlanArgs(t *testing.T, plan map[string]any, want []string) {
	t.Helper()
	commands, err := commandsFromPlan(plan)
	if err != nil || len(commands) != 1 {
		t.Fatalf("commands = %+v, err = %v", commands, err)
	}
	joined := strings.Join(commands[0].Args, " ")
	for _, value := range want {
		if !strings.Contains(joined, value) {
			t.Errorf("dry-run plan %q missing %q", joined, value)
		}
	}
}

func assertWave3PersistedParams(
	t *testing.T,
	dep Deployment,
	want map[string]any,
) {
	t.Helper()
	got, ok := dep.Metadata["provider_params"].(map[string]any)
	if !ok {
		t.Fatalf("deployment metadata omitted normalized provider_params: %+v", dep.Metadata)
	}
	for key, value := range want {
		if !wave3JSONEqual(got[key], value) {
			t.Errorf("provider_params[%q] = %#v, want %#v", key, got[key], value)
		}
	}
}

func assertWave3AWSNativeInput(t *testing.T, dep Deployment) {
	t.Helper()
	input, err := NewAWSRDSRunner(&fakeRDSClient{}, "fallback-region").createInput(
		ProvisionRequest{Deployment: dep, Policy: LiveProvisionPolicy{AllowPublicIP: true}},
	)
	if err != nil {
		t.Fatalf("build AWS native input: %v", err)
	}
	if input.Class != "db.r7g.large" || input.StorageGB != 137 ||
		input.BackupRetention != 19 || input.Region != "eu-central-1" {
		t.Fatalf("AWS native input lost profile values: %+v", input)
	}
}

func assertWave3GCPNativeInput(t *testing.T, dep Deployment) {
	t.Helper()
	input, err := NewCloudSQLRunner(
		&fakeCloudSQLClient{}, "fallback-project", "fallback-region",
	).createInput(ProvisionRequest{
		Deployment: dep,
		Policy:     LiveProvisionPolicy{AllowPublicIP: false},
	})
	if err != nil {
		t.Fatalf("build GCP native input: %v", err)
	}
	if input.Project != "wave3-project" || input.Region != "europe-west4" ||
		input.Tier != "db-custom-4-15360" || input.StorageGB != 211 ||
		input.IPv4Enabled || !input.RequireSSL {
		t.Fatalf("GCP native input lost profile values: %+v", input)
	}
}

func wave3ReadinessOptions(now time.Time) ProviderReadinessOptions {
	policy := wave3LayerPolicy([]string{"us-east-1"}, 3600, 25, LiveModeApproval)
	return ProviderReadinessOptions{
		Now:            now,
		Registry:       NewRunnerRegistry(DryRunProvisionRunner{}),
		RuntimeEnabled: true,
		GlobalPolicy:   &LivePolicyLayer{Policy: policy, Version: 3},
		ProviderPolicies: map[string]*LivePolicyLayer{
			ProviderAWSRDS: {
				Policy: policy, Version: 9, ValidatedAt: now, ValidationTTL: time.Hour,
			},
		},
		RuntimeChecks: map[string]ProviderRuntimeCheck{},
	}
}

func wave3JSONEqual(left, right any) bool {
	leftNumber, leftOK := left.(float64)
	if leftOK {
		switch value := right.(type) {
		case int:
			return leftNumber == float64(value)
		case int32:
			return leftNumber == float64(value)
		case int64:
			return leftNumber == float64(value)
		}
	}
	return reflect.DeepEqual(left, right)
}

func wave3ReadinessFor(t *testing.T, rows []ProviderReadiness) ProviderReadiness {
	t.Helper()
	for _, row := range rows {
		if row.Provider == ProviderAWSRDS {
			return row
		}
	}
	t.Fatal("AWS RDS readiness row is missing")
	return ProviderReadiness{}
}

func assertWave3DisabledCode(
	t *testing.T,
	reasons []ReadinessDisabledReason,
	want string,
) {
	t.Helper()
	for _, reason := range reasons {
		if reason.Code == want {
			return
		}
	}
	t.Fatalf("disabled reasons = %+v, want code %q", reasons, want)
}
