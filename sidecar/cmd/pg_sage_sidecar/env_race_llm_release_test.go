package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// env-race: a per-database runtime's LLM clients are untracked by a
// goroutine that waits for the runtime's context. It read the process
// globals cfg and fleetLLMBudget after the runtime had been drained, so it
// raced with whatever replaced them (a fixture restoring globals in CI's
// -race run, 4 of 6 runs). The release is an instance worker now: draining
// the runtime waits for it.

// releaseTestRuntime is a fleet runtime shell with no database: its LLM
// clients are registry-tracked and the policy ceiling is skipped.
func releaseTestRuntime(name string) *databaseRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	allowed := true
	return &databaseRuntime{
		spec: databaseRuntimeSpec{
			Scope: "fleet", Name: name,
			Config: config.DatabaseConfig{Name: name, LLMEnabled: &allowed},
		},
		ctx: ctx, cancel: cancel, workers: &sync.WaitGroup{},
	}
}

func releaseRegistryFixture(t *testing.T) {
	t.Helper()
	preserveFleetRuntimeGlobals(t) // fresh llmClients registry, restored after
	cfg = config.DefaultConfig()
	cfg.Mode = "fleet"
	fleetLLMBudget = nil
}

func trackedClients(r *llmClientRegistry, database string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, entry := range r.entries {
		if entry.database == database {
			n++
		}
	}
	return n
}

func TestRace_LLMClients_ReleaseIsAnInstanceWorker(t *testing.T) {
	releaseRegistryFixture(t)
	rt := releaseTestRuntime("release-a")
	rt.resolveLLM()
	if rt.llmManager == nil || rt.generalLLM == nil {
		t.Fatal("a fleet runtime built no per-database LLM clients")
	}
	if got := trackedClients(llmClients, "release-a"); got != 1 {
		t.Fatalf("tracked clients before the release = %d, want 1", got)
	}
	// Hold the registry so the release cannot finish, then end the runtime.
	llmClients.mu.Lock()
	rt.cancel()
	drained := make(chan struct{})
	go func() { rt.workers.Wait(); close(drained) }()
	select {
	case <-drained:
		llmClients.mu.Unlock()
		t.Fatal("the runtime drained while its LLM clients were still being " +
			"released: the release is not an instance worker")
	case <-time.After(200 * time.Millisecond):
	}
	llmClients.mu.Unlock()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime did not drain once the registry was free")
	}
	if got := trackedClients(llmClients, "release-a"); got != 0 {
		t.Fatalf("tracked clients after the drain = %d, want 0", got)
	}
}

// A shared (standalone) runtime uses the process clients and owns no
// release: nothing is tracked for it and the drain has nothing to wait for.
func TestLLMClientsSharedRuntimeHasNoReleaseWorker(t *testing.T) {
	releaseRegistryFixture(t)
	rt := releaseTestRuntime("shared-a")
	rt.spec.Shared = true
	llmClient, llmMgr = nil, nil
	rt.resolveLLM()
	if rt.llmManager != nil || rt.llmOn {
		t.Fatalf("shared runtime without process clients: manager=%v llmOn=%t",
			rt.llmManager, rt.llmOn)
	}
	llmClients.mu.Lock()
	rt.cancel()
	drained := make(chan struct{})
	go func() { rt.workers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("a shared runtime waited on a release it does not own")
	}
	llmClients.mu.Unlock()
}

// Concurrent runtimes start, end and drain while the parent config is
// reconfigured: every drained runtime has already left the registry, and
// the clients of runtimes still running stay tracked.
func TestRace_LLMClients_ConcurrentRuntimeChurn(t *testing.T) {
	releaseRegistryFixture(t)
	keep := releaseTestRuntime("churn-keep")
	keep.resolveLLM()
	t.Cleanup(func() { keep.cancel(); keep.workers.Wait() })
	stop := make(chan struct{})
	var reconf sync.WaitGroup
	reconf.Add(1)
	go func() {
		defer reconf.Done()
		for {
			select {
			case <-stop:
				return
			default:
				llmClients.reconfigure(cfg.LLM)
			}
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("churn-%d", i)
			rt := releaseTestRuntime(name)
			rt.resolveLLM()
			rt.cancel()
			rt.workers.Wait()
			if got := trackedClients(llmClients, name); got != 0 {
				errs <- fmt.Errorf("%s: %d clients still tracked after its drain", name, got)
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	reconf.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := trackedClients(llmClients, "churn-keep"); got != 1 {
		t.Fatalf("a running runtime's clients = %d after the churn, want 1", got)
	}
}

// No error-propagation case: the release returns nothing and removing an
// untracked client is a no-op (no input surface beyond the runtime).
