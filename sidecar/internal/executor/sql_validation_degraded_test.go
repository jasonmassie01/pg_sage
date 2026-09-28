package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// withDegradedASTValidation simulates a build without the parse-tree layer.
// Not parallel-safe: it swaps a package variable.
func withDegradedASTValidation(t *testing.T) {
	t.Helper()
	previous := astValidationAvailable
	astValidationAvailable = func() bool { return false }
	t.Cleanup(func() { astValidationAvailable = previous })
}

func autonomousAnalyzeInput() verdictInput {
	return verdictInput{
		cfg: &config.Config{Trust: config.TrustConfig{
			Level: "autonomous", Tier3Safe: true, Tier3Moderate: true}},
		rampStart: time.Now().Add(-40 * 24 * time.Hour),
	}
}

func TestDegradedBuildQueuesUnattendedMutation(t *testing.T) {
	withDegradedASTValidation(t)
	got := policyVerdict(AnalyzeTableContract(), autonomousAnalyzeInput())
	if got.Decision == PolicyDecisionExecute {
		t.Fatalf("degraded build executed unattended: %#v", got)
	}
	if got.BlockedReason != string(policy.ReasonSQLValidationDegraded) {
		t.Fatalf("reason = %q, want %q", got.BlockedReason, policy.ReasonSQLValidationDegraded)
	}
}

func TestHealthyBuildExecutesUnattendedMutation(t *testing.T) {
	got := policyVerdict(AnalyzeTableContract(), autonomousAnalyzeInput())
	if got.Decision != PolicyDecisionExecute {
		t.Fatalf("healthy build did not execute: %#v", got)
	}
}

func TestDegradedFlagFollowsBuild(t *testing.T) {
	withDegradedASTValidation(t)
	if ASTValidationAvailable() {
		t.Fatal("ASTValidationAvailable ignores the build probe")
	}
}
