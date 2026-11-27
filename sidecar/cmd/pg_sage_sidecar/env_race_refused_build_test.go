package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// env-race (T4c test round 1): a runtime build refused after its first
// workers started (unmigrated history) cancelled the runtime but did not
// wait for them, so the LLM release read cfg after the refusal returned and
// raced the fixture restoring it (TestMetaRuntimeRefusesUnmigratedHistory,
// ci-19 runner, -race, PG14). A refused build drains its workers first.
// Nil/empty input and the happy path are covered by the existing runtime
// build tests; this file pins the refusal path's drain only.

// holdRegistryOnceTracked locks r once database has a tracked client and
// keeps it locked (the caller unlocks); held closes when it holds the lock.
func holdRegistryOnceTracked(
	r *llmClientRegistry, database string, stop <-chan struct{},
) <-chan struct{} {
	held := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.mu.Lock()
			for _, entry := range r.entries {
				if entry.database == database {
					close(held)
					return // still locked: the release cannot finish
				}
			}
			r.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	return held
}

// State transition: built (clients tracked) -> refused -> drained. The
// refusal must not return while the runtime's LLM release is still pending.
func TestRace_RefusedRuntimeBuildDrainsItsWorkers(t *testing.T) {
	monDSN, metaDSN, mon, _ := historyDBs(t)
	seedHistoryRows(t, mon, 2)
	rec, _ := metaHistoryRuntime(t, monDSN, metaDSN)
	preserveFleetRuntimeGlobals(t) // a fresh registry: no other test's "app"
	registry := llmClients
	stop := make(chan struct{})
	defer close(stop)
	held := holdRegistryOnceTracked(registry, rec.Name, stop)
	done := make(chan error, 1)
	go func() {
		_, err := prepareStoreDatabaseConnection(context.Background(), rec, monDSN,
			memberAction{})
		done <- err
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("the build ended before tracking its LLM clients: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the build never tracked its LLM clients")
	}
	select {
	case err := <-done:
		registry.mu.Unlock()
		t.Fatalf("the refused build returned (%v) while its LLM release was still "+
			"pending: its workers were not drained", err)
	case <-time.After(300 * time.Millisecond):
	}
	registry.mu.Unlock()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the refused build did not return once its release could finish")
	}
	if err == nil || !strings.Contains(err.Error(), "history migrate") {
		t.Fatalf("build error = %v, want the unmigrated-history refusal", err)
	}
	if got := trackedClients(registry, rec.Name); got != 0 {
		t.Fatalf("tracked clients after the refusal = %d, want 0", got)
	}
}
