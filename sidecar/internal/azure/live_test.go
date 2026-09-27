package azure

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

// The live tests change parameters on a real flexible server through ARM
// and restore them. Opt in with PG_SAGE_LIVE_AZURE=1 plus
// SAGE_AZURE_SUBSCRIPTION_ID, SAGE_AZURE_RESOURCE_GROUP and
// SAGE_AZURE_SERVER_NAME (scripts/azure/provision-test-server.sh writes
// them); credentials come from the azidentity chain (az login works).

func liveAdapter(t *testing.T) (*ParameterAdapter, context.Context) {
	t.Helper()
	if os.Getenv("PG_SAGE_LIVE_AZURE") != "1" {
		t.Skip("set PG_SAGE_LIVE_AZURE=1 and SAGE_AZURE_* to run the live Azure parameter test")
	}
	server := Server{SubscriptionID: os.Getenv("SAGE_AZURE_SUBSCRIPTION_ID"),
		ResourceGroup: os.Getenv("SAGE_AZURE_RESOURCE_GROUP"),
		Name:          os.Getenv("SAGE_AZURE_SERVER_NAME")}
	token, err := DefaultTokenSource()
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	adapter, err := NewParameterAdapter(server, token, WithPolling(5*time.Second, 10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	return adapter, ctx
}

// setAndRestore applies target to parameter and registers a restore of the
// original value.
func setAndRestore(
	t *testing.T, adapter *ParameterAdapter, ctx context.Context,
	parameter string, pick func(original string) string,
) executor.ManagedConfigResult {
	t.Helper()
	original, err := adapter.getParameter(ctx, parameter)
	if err != nil {
		t.Fatalf("read %s: %v", parameter, err)
	}
	t.Cleanup(func() {
		restore := executor.ManagedConfigChange{Provider: "azure",
			Mechanism: executor.ManagedServerParameter, Parameter: parameter,
			Value: original.Value}
		if _, err := adapter.ApplyParameter(context.Background(), restore); err != nil {
			t.Errorf("restore %s=%s: %v", parameter, original.Value, err)
		}
	})
	target := pick(original.Value)
	result, err := adapter.ApplyParameter(ctx, executor.ManagedConfigChange{Provider: "azure",
		Mechanism: executor.ManagedServerParameter, Parameter: parameter, Value: target})
	if err != nil {
		t.Fatalf("set %s=%s: %v", parameter, target, err)
	}
	t.Logf("%s (original %s %s)", result.Note, original.Value, original.Unit)
	return result
}

// CHECK-AZ-07: a dynamic parameter is in effect immediately.
func TestAzureLiveServerParameter(t *testing.T) {
	adapter, ctx := liveAdapter(t)
	result := setAndRestore(t, adapter, ctx, "work_mem", func(original string) string {
		if original == "8192" {
			return "16MB"
		}
		return "8MB"
	})
	if !result.InEffect {
		t.Fatalf("work_mem change not in effect: %+v", result)
	}
}

// CHECK-AZ-08: a restart-bound parameter is applied but reported as not in
// effect until restart. The restore is restart-bound too, so the server may
// show "restart pending" afterwards with its original values.
func TestAzureLiveRestartBoundParameter(t *testing.T) {
	adapter, ctx := liveAdapter(t)
	result := setAndRestore(t, adapter, ctx, "max_locks_per_transaction",
		func(original string) string {
			if original == "128" {
				return "96"
			}
			return "128"
		})
	if result.InEffect || !strings.Contains(result.Note, "restart") {
		t.Fatalf("restart-bound change = %+v, want pending restart", result)
	}
}
