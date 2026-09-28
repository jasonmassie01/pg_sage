package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

type settingsDispatcher struct{}

func (settingsDispatcher) Dispatch(context.Context, notify.Event) error { return nil }

type settingsIOEvidence struct{}

func (settingsIOEvidence) IOEvidence(context.Context) (verify.IOEvidence, error) {
	return verify.IOEvidence{}, nil
}

type settingsAdapter struct{}

func (settingsAdapter) ApplyParameter(
	context.Context, ManagedConfigChange,
) (ManagedConfigResult, error) {
	return ManagedConfigResult{}, nil
}

func TestRuntimeSettingsNilExecutorIsZero(t *testing.T) {
	var e *Executor
	if got := e.RuntimeSettings(); got != (RuntimeSettings{}) {
		t.Fatalf("nil executor settings = %+v, want zero", got)
	}
}

func TestRuntimeSettingsFreshExecutorReportsNothingWired(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.CloudEnvironment = "rds"
	got := newTestExecutor(cfg, time.Now()).RuntimeSettings()
	want := RuntimeSettings{
		Provider: "rds", TrustLevel: "advisory", ExecutionMode: "auto",
		ExecutorEnabled: true,
	}
	if got != want {
		t.Fatalf("fresh settings = %+v, want %+v", got, want)
	}
}

func TestRuntimeSettingsReflectsEveryWiredComponent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "observation"
	e := newTestExecutor(cfg, time.Now())
	e.WithDatabaseName("orders")
	e.WithAnalyzeSemaphore(make(chan struct{}, 1))
	e.WithActionStore(nil, "approval")
	e.WithDispatcher(settingsDispatcher{})
	e.WithManagedConfigAdapter(settingsAdapter{})
	e.WithPolicyGate(policy.NewGate(policy.GateConfig{}))
	e.WithPostDDLHook(func(context.Context) error { return nil })
	e.WithIOEvidence(settingsIOEvidence{})
	if err := e.SetTrustLevel("autonomous"); err != nil {
		t.Fatal(err)
	}
	e.SetExecutorEnabled(false)
	got := e.RuntimeSettings()
	want := RuntimeSettings{
		DatabaseName: "orders", TrustLevel: "autonomous", ExecutionMode: "approval",
		PolicyGate: true, ManagedConfig: true, IOEvidence: true, AnalyzeSemaphore: true,
		Dispatcher: true, PostDDLHook: true,
	}
	if got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
}

// No concurrent-access test beyond the race detector: RuntimeSettings reads
// under the same locks the setters take.
