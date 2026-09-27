package azure

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

// TestAzureLiveServerParameter changes work_mem on a real flexible server
// through ARM and restores it. Opt in with PG_SAGE_LIVE_AZURE=1 plus
// SAGE_AZURE_SUBSCRIPTION_ID, SAGE_AZURE_RESOURCE_GROUP and
// SAGE_AZURE_SERVER_NAME; credentials come from the azidentity chain
// (az login works). work_mem is dynamic, so no restart is needed.
func TestAzureLiveServerParameter(t *testing.T) {
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
	defer cancel()
	original, err := adapter.getParameter(ctx, "work_mem")
	if err != nil {
		t.Fatalf("read work_mem: %v", err)
	}
	t.Cleanup(func() {
		restore := executor.ManagedConfigChange{Provider: "azure",
			Mechanism: executor.ManagedServerParameter, Parameter: "work_mem",
			Value: original.Value}
		if _, err := adapter.ApplyParameter(context.Background(), restore); err != nil {
			t.Errorf("restore work_mem=%s: %v", original.Value, err)
		}
	})
	target := "8MB"
	if original.Value == "8192" {
		target = "16MB"
	}
	result, err := adapter.ApplyParameter(ctx, executor.ManagedConfigChange{Provider: "azure",
		Mechanism: executor.ManagedServerParameter, Parameter: "work_mem", Value: target})
	if err != nil || !result.InEffect {
		t.Fatalf("set work_mem=%s: result=%+v err=%v", target, result, err)
	}
	t.Logf("%s (original %s %s)", result.Note, original.Value, original.Unit)
}
