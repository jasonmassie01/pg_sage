package fleet

import (
	"strings"
	"testing"
)

// clouds-05a: where no single-runtime rebuild exists (standalone, the YAML
// control database), a re-probe that found the provider says so as a
// readiness blocker until the operator restarts.
func TestReadinessBlocksOnPendingProvider(t *testing.T) {
	caps := BuildProviderCapabilities("unknown", false, nil)
	caps.PendingProvider = "rds"
	snap := EnsureCapabilities(nil, &InstanceStatus{Platform: "unknown", Capabilities: caps})

	want := "provider re-probe found rds; restart pg_sage to apply"
	if !containsString(snap.Capabilities.Blockers, want) {
		t.Fatalf("blockers = %v, want %q", snap.Capabilities.Blockers, want)
	}
	if !containsString(snap.Capabilities.Blockers, "provider unknown") {
		t.Fatalf("blockers = %v, want provider unknown kept", snap.Capabilities.Blockers)
	}
	if snap.Capabilities.ReadyForAutoSafe {
		t.Fatal("a database with a pending provider is ready for auto-safe")
	}
}

func TestReadinessHasNoPendingBlockerByDefault(t *testing.T) {
	caps := BuildProviderCapabilities("self-managed", false, nil)
	for _, blocker := range caps.Blockers {
		if strings.Contains(blocker, "re-probe") {
			t.Fatalf("blockers = %v, want no re-probe blocker", caps.Blockers)
		}
	}
	if caps.PendingProvider != "" {
		t.Fatalf("pending provider = %q, want empty", caps.PendingProvider)
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
