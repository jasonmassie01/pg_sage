package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// State transition on real PostgreSQL (YAML fleet): database b's provider
// probe fails at startup, so its runtime is "unknown". The re-probe worker
// then finds the real provider and requests exactly one rebuild of b; the
// new generation's executor reports the real provider, and the control
// database is untouched.
func TestDetectCloudEnvReprobeRebuildsFleetRuntime(t *testing.T) {
	builds := stubFirstProviderDetection(t, "reload_b")
	newFleetReloadEnv(t, "ctl", "b")
	control := fleetMgr.GetInstance("ctl")

	first := builds.waitFor(t, 1)[0]
	if first.provider != providerUnknown {
		t.Fatalf("first generation provider = %q, want unknown", first.provider)
	}
	if got := first.inst.Executor.RuntimeSettings().Provider; got != providerUnknown {
		t.Fatalf("first generation executor provider = %q, want unknown", got)
	}
	second := builds.waitFor(t, 2)[1]
	eventually(t, 90*time.Second, "rebuilt runtime to be published", func() bool {
		return fleetMgr.GetInstance("b") == second.inst
	})
	if got := second.inst.Executor.RuntimeSettings().Provider; got != providerSelfManaged {
		t.Fatalf("rebuilt executor provider = %q, want self-managed", got)
	}
	if got := second.inst.SnapshotStatus().Platform; got != providerSelfManaged {
		t.Fatalf("rebuilt platform = %q, want self-managed", got)
	}
	if second.inst.DatabaseID != first.inst.DatabaseID {
		t.Fatalf("rebuild changed database id %d -> %d", first.inst.DatabaseID,
			second.inst.DatabaseID)
	}
	eventually(t, 90*time.Second, "unknown generation's pool to close", func() bool {
		return poolClosed(first.inst.Pool)
	})
	time.Sleep(20 * providerReprobeMin)
	if n := builds.count(); n != 2 {
		t.Fatalf("runtime builds of b = %d, want 2 (exactly one rebuild request)", n)
	}
	if fleetMgr.GetInstance("ctl") != control {
		t.Fatal("the control database was rebuilt")
	}
}

type builtRuntime struct {
	provider string
	inst     *fleet.DatabaseInstance
}

type runtimeBuilds struct {
	mu   sync.Mutex
	runs []builtRuntime
}

func (b *runtimeBuilds) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.runs)
}

func (b *runtimeBuilds) waitFor(t *testing.T, n int) []builtRuntime {
	t.Helper()
	eventually(t, 120*time.Second, "runtime builds", func() bool { return b.count() >= n })
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]builtRuntime(nil), b.runs...)
}

// stubFirstProviderDetection makes the first provider detection on the
// database whose name contains fragment fail as an outage would, records
// every runtime built for it, and shortens the re-probe backoff.
func stubFirstProviderDetection(t *testing.T, fragment string) *runtimeBuilds {
	t.Helper()
	oldDetect, oldBuilt := detectRuntimeProvider, runtimeBuilt
	oldMin, oldMax := providerReprobeMin, providerReprobeMax
	providerReprobeMin, providerReprobeMax = 25*time.Millisecond, 100*time.Millisecond
	builds := &runtimeBuilds{}
	var once sync.Once
	detectRuntimeProvider = func(ctx context.Context, p *pgxpool.Pool) (string, error) {
		failed := false
		if strings.Contains(p.Config().ConnConfig.Database, fragment) {
			once.Do(func() { failed = true })
		}
		if failed {
			return providerUnknown, errors.New("provider probe aurora: simulated outage")
		}
		return detectCloudEnvContext(ctx, p)
	}
	runtimeBuilt = func(rt *databaseRuntime) {
		if strings.Contains(rt.spec.Pool.Config().ConnConfig.Database, fragment) {
			builds.mu.Lock()
			builds.runs = append(builds.runs, builtRuntime{provider: rt.provider, inst: rt.inst})
			builds.mu.Unlock()
		}
		oldBuilt(rt)
	}
	t.Cleanup(func() {
		detectRuntimeProvider, runtimeBuilt = oldDetect, oldBuilt
		providerReprobeMin, providerReprobeMax = oldMin, oldMax
	})
	return builds
}

// State transition on real PostgreSQL (meta-db): a store database whose
// first provider probe failed is rebuilt once through the reconnect path,
// keeps its record id, and the next reconcile pass leaves it alone.
func TestDetectCloudEnvReprobeRebuildsMetaRuntime(t *testing.T) {
	env := newMetaReconcileEnv(t)
	builds := stubFirstProviderDetection(t, "meta_mp")
	rec := env.record(t, "mp")
	if report := env.reconcile(t); len(report.Errors) != 0 {
		t.Fatalf("reconcile = %+v, want mp added", report)
	}
	first := builds.waitFor(t, 1)[0]
	if first.provider != providerUnknown || first.inst.DatabaseID != rec.ID {
		t.Fatalf("first generation = %q id %d, want unknown id %d",
			first.provider, first.inst.DatabaseID, rec.ID)
	}
	second := builds.waitFor(t, 2)[1]
	eventually(t, 90*time.Second, "rebuilt meta runtime to be published", func() bool {
		return fleetMgr.GetInstance("mp") == second.inst
	})
	if got := second.inst.Executor.RuntimeSettings().Provider; got != providerSelfManaged {
		t.Fatalf("rebuilt executor provider = %q, want self-managed", got)
	}
	if second.inst.DatabaseID != rec.ID {
		t.Fatalf("rebuilt database id = %d, want %d", second.inst.DatabaseID, rec.ID)
	}
	eventually(t, 90*time.Second, "unknown generation's pool to close", func() bool {
		return poolClosed(first.inst.Pool)
	})
	if again := env.reconcile(t); !again.empty() || fleetMgr.GetInstance("mp") != second.inst {
		t.Fatalf("reconcile after the rebuild changed the fleet: %+v", again)
	}
	time.Sleep(20 * providerReprobeMin)
	if n := builds.count(); n != 2 {
		t.Fatalf("runtime builds of mp = %d, want 2 (exactly one rebuild request)", n)
	}
}

// A store record disabled while its provider was unknown is not rebuilt:
// the re-probe stops and the runtime stays unknown for reconcile to remove.
func TestProviderReprobeSkipsDisabledMetaRecord(t *testing.T) {
	env := newMetaReconcileEnv(t)
	builds := stubFirstProviderDetection(t, "meta_md")
	providerReprobeMin = time.Hour // the test calls the rebuild itself
	rec := env.record(t, "md")
	env.reconcile(t)
	first := builds.waitFor(t, 1)[0]
	setEnabled(t, env, rec.ID, false)
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Scope: "meta-db", Name: "md",
		DatabaseID: rec.ID, Parent: context.Background()}, inst: first.inst}
	err := rt.rebuildForProvider(providerSelfManaged)
	if !errors.Is(err, fleet.ErrInstanceConflict) || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("rebuild of a disabled record = %v, want a disabled conflict", err)
	}
	if fleetMgr.GetInstance("md") != first.inst || builds.count() != 1 {
		t.Fatalf("disabled record was rebuilt (builds=%d)", builds.count())
	}
}
