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
	eventually(t, 30*time.Second, "rebuilt runtime to be published", func() bool {
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
	eventually(t, 30*time.Second, "unknown generation's pool to close", func() bool {
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
	eventually(t, 60*time.Second, "runtime builds", func() bool { return b.count() >= n })
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
