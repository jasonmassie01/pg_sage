package agentdb

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestHostedBlueprintRejectsUnknownMode(t *testing.T) {
	for _, provider := range []string{ProviderNeon, ProviderSupabase} {
		files, err := RenderTerraformFromBlueprint(BlueprintSpec{
			Provider: provider, ProvisioningLevel: LevelInstance, HostedMode: "brnach",
		})
		if !errors.Is(err, ErrInvalid) || len(files) != 0 {
			t.Fatalf("%s: unknown mode generated resources: files=%d err=%v", provider, len(files), err)
		}
	}
}

func TestHostedBlueprintReportsUnconfiguredSizing(t *testing.T) {
	for _, provider := range []string{ProviderNeon, ProviderSupabase} {
		spec := NormalizeBlueprintSpec(BlueprintSpec{Provider: provider}, "")
		if spec.StorageGB != 0 {
			t.Fatalf("%s: invented storage allocation: %d", provider, spec.StorageGB)
		}
		spec.StorageGB, spec.DatabaseVersion, spec.InstanceClass = 100, "17", "custom"
		spec.Extensions = []string{"vector"}
		findings := strings.Join(hostedBlueprintFindings(spec), " ")
		for _, field := range []string{"storage", "version", "compute", "extensions"} {
			if !strings.Contains(findings, field) {
				t.Fatalf("%s: silently ignored requested %s: %s", provider, field, findings)
			}
		}
	}
}

func TestHostedBlueprintProjectTemplates(t *testing.T) {
	for _, provider := range []string{ProviderNeon, ProviderSupabase} {
		t.Run(provider, func(t *testing.T) {
			got, err := NewHeuristicBlueprintGenerator().GenerateBlueprint(context.Background(),
				BlueprintDraftRequest{Intent: "Create a " + provider + " project"})
			if err != nil {
				t.Fatal(err)
			}
			if got.Spec.Provider != provider || !got.Spec.PublicIP || len(got.Files) != 1 {
				t.Fatalf("invalid generated specification: %#v", got.Spec)
			}
			body := got.Files[0].Body
			for _, required := range []string{"required_providers", provider + "_project",
				"var.organization"} {
				if !strings.Contains(body, required) {
					t.Fatalf("missing %s", required)
				}
			}
			if provider == ProviderSupabase && !strings.Contains(body, "sensitive = true") {
				t.Fatal("database password variable must be sensitive")
			}
			if len(got.PolicyFindings) == 0 {
				t.Fatal("public access policy was bypassed")
			}
		})
	}
}

func TestHostedBlueprintBranchAndUnsupportedClaims(t *testing.T) {
	for _, provider := range []string{ProviderNeon, ProviderSupabase} {
		got, err := NewHeuristicBlueprintGenerator().GenerateBlueprint(context.Background(),
			BlueprintDraftRequest{Intent: "Create a private multi-az " + provider + " branch"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.Files[0].Body, provider+"_branch") {
			t.Fatal("branch request silently became project creation")
		}
		findings := strings.Join(got.PolicyFindings, " ")
		if !strings.Contains(findings, "not configured") {
			t.Fatal("unsupported private networking and HA were silently accepted")
		}
	}
}
