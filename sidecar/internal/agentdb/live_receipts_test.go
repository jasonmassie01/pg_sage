package agentdb

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func liveDeploymentID(prefix string) string {
	stamp := time.Now().UTC().Format("20060102150405")
	return prefix + "_" + stamp
}

func liveEnv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func liveEnvDefault(key string, fallback string) string {
	if value := liveEnv(key); value != "" {
		return value
	}
	return fallback
}

func requireLiveEnv(t *testing.T, key string) string {
	t.Helper()
	value := liveEnv(key)
	if value == "" {
		t.Fatalf("%s is required for this live test", key)
	}
	return value
}

func liveReceipt(t *testing.T, provider string, receipt map[string]any) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	out := filepath.Join(filepath.Dir(file), "..", "..", "test-output")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("create live receipt dir: %v", err)
	}
	receipt["provider"] = provider
	receipt["recorded_at"] = time.Now().UTC().Format(time.RFC3339)
	line, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal live receipt: %v", err)
	}
	path := filepath.Join(out, "agentdb-live-receipts.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open live receipt file: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatalf("write live receipt: %v", err)
	}
}

func waitProvisionStatus(
	ctx context.Context,
	interval time.Duration,
	check func() ProvisionResult,
	ready func(ProvisionResult) bool,
) (ProvisionResult, time.Duration, error) {
	start := time.Now()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last ProvisionResult
	for {
		last = check()
		if ready(last) {
			return last, time.Since(start), nil
		}
		if last.Error != nil && errors.Is(publicProviderError(last.Error), ErrNotFound) {
			return last, time.Since(start), ErrNotFound
		}
		select {
		case <-ctx.Done():
			return last, time.Since(start), ctx.Err()
		case <-ticker.C:
		}
	}
}
