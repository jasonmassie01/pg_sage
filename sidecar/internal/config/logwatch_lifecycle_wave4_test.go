package config

import (
	"context"
	"testing"
)

func TestR14LogwatchReconfigurationWithoutOwnerStaysPending(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	candidate := controller.Active().Config
	candidate.LogWatch.LogDirectory = t.TempDir()

	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("apply logwatch change: %v", err)
	}
	if wave4ContainsPath(result.Applied, "logwatch.log_directory") {
		t.Fatalf("unacknowledged logwatch change became active: %+v", result)
	}
	if !wave4ContainsPath(result.PendingRestart, "logwatch.log_directory") {
		t.Fatalf("missing pending-restart gate: %+v", result)
	}
	if got := controller.Active().Config.LogWatch.LogDirectory; got != "" {
		t.Fatalf("active log directory = %q before owner acknowledgment", got)
	}
}

func TestR14LogwatchLifecycleIsRestartBound(t *testing.T) {
	metadata, ok := LookupFieldLifecycle("logwatch.log_directory")
	if !ok {
		t.Fatal("logwatch.log_directory lifecycle metadata missing")
	}
	if metadata.Lifecycle != LifecycleRestart || metadata.Owner != "" {
		t.Fatalf("logwatch lifecycle = %+v, want restart-bound without owner", metadata)
	}
}

func wave4ContainsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
